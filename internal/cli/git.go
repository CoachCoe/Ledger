package cli

import (
	"context"
	"encoding/json/v2"
	"errors"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/atomicfile"
	"github.com/CoachCoe/Ledger/internal/bridge"
	"github.com/CoachCoe/Ledger/internal/gitx"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/store"
)

// bridgeConfig is the Git repository a native repository was imported
// from: the default source of git import and the repository git mirror
// pushes from. It is local configuration, not a signed record.
type bridgeConfig struct {
	Source string `json:"source"`
	Ref    string `json:"ref"`
	Format string `json:"format"`
}

func bridgeConfigPath(ledgerDir string) string { return filepath.Join(ledgerDir, "bridge.json") }

func readBridgeConfig(ledgerDir string) (bridgeConfig, bool, error) {
	var c bridgeConfig
	raw, err := os.ReadFile(bridgeConfigPath(ledgerDir))
	if errors.Is(err, fs.ErrNotExist) {
		return c, false, nil
	}
	if err != nil {
		return c, false, err
	}
	return c, true, json.Unmarshal(raw, &c, json.RejectUnknownMembers(true))
}

func writeBridgeConfig(ledgerDir string, c bridgeConfig) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return atomicfile.Write(bridgeConfigPath(ledgerDir), append(b, '\n'), 0o600)
}

func stagingDir(ledgerDir string) string { return filepath.Join(ledgerDir, "runtime", "bridge") }

// gitSource resolves a Git repository path given on the command line.
func gitSource(ctx context.Context, p string) (string, string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", "", err
	}
	format, err := bridge.ObjectFormat(ctx, abs)
	if err != nil {
		return "", "", err
	}
	return abs, format, nil
}

func cmdGit(args []string) (any, error) {
	if len(args) == 0 {
		return nil, apierr.New("usage", "ledger git import|export|mirror|sync ...")
	}
	switch args[0] {
	case "import":
		return cmdGitImport(args[1:])
	case "export":
		return cmdGitExport(args[1:])
	case "mirror":
		return cmdGitMirror(args[1:])
	case "sync":
		return cmdGitSync(args[1:])
	}
	return nil, apierr.New("usage", "unknown git operation %q", args[0])
}

// withBridge opens the native repository of the working directory, its
// store and root key, and the staging repository for format, which is the
// bridged repository's unless given.
func withBridge(format string, fn func(context.Context, *bridge.Bridge, string) (any, error)) (result any, err error) {
	ctx := context.Background()
	s, _, err := openStore(ctx, false)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := s.Close(); err == nil {
			err = cerr
		}
	}()
	c, err := readConfig(s.Dir)
	if err != nil {
		return nil, err
	}
	if format == "" {
		bc, ok, err := readBridgeConfig(s.Dir)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, apierr.New("not_bridged", "this repository was not imported from Git; name a Git repository")
		}
		format = bc.Format
	}
	br, err := openBridge(ctx, s, c, format)
	if err != nil {
		return nil, err
	}
	return fn(ctx, br, s.Dir)
}

func openBridge(ctx context.Context, s *record.Store, c config, format string) (*bridge.Bridge, error) {
	keyDir, err := identity.DefaultKeyDir()
	if err != nil {
		return nil, err
	}
	root, err := identity.LocalKeys{Dir: keyDir}.Root()
	if err != nil {
		return nil, err
	}
	if root.ID() != c.RootFingerprint {
		return nil, apierr.New("root_mismatch", "repository root does not match the configured key")
	}
	objects, err := store.Open(filepath.Join(s.Dir, record.StoreDir, "objects"), c.CaseSensitive)
	if err != nil {
		return nil, err
	}
	staging, err := bridge.OpenStaging(ctx, stagingDir(s.Dir), format)
	if err != nil {
		return nil, err
	}
	return &bridge.Bridge{Records: s, Objects: objects, Staging: staging, Root: root}, nil
}

// cmdGitImport imports another ref's first-parent history: ledger git
// import [--from PATH] REF. It maps commits and moves no line.
func cmdGitImport(args []string) (any, error) {
	fl := flag.NewFlagSet("git import", flag.ContinueOnError)
	from := fl.String("from", "", "Git repository (default: the one this repository was imported from)")
	if err := fl.Parse(args); err != nil || fl.NArg() != 1 {
		return nil, apierr.New("usage", "ledger git import [--from PATH] REF")
	}
	ref := fl.Arg(0)
	src, format := "", ""
	if *from != "" {
		var err error
		if src, format, err = gitSource(context.Background(), *from); err != nil {
			return nil, err
		}
	}
	return withBridge(format, func(ctx context.Context, br *bridge.Bridge, ledgerDir string) (any, error) {
		if src == "" {
			bc, _, err := readBridgeConfig(ledgerDir)
			if err != nil {
				return nil, err
			}
			src = bc.Source
		}
		f, err := br.Staging.Convert(ctx, br.Objects, src, ref)
		if err != nil {
			return nil, err
		}
		return br.Publish(ctx, f)
	})
}

// cmdGitExport writes a line's commits into a Git repository: ledger git
// export --line LINE --to PATH [--ref REF]. The ref only fast-forwards.
func cmdGitExport(args []string) (any, error) {
	var line, to, ref string
	var proposals bool
	const usage = "ledger git export [--line LINE] --to PATH [--ref REF] [--proposals]"
	if err := parseFlags("git export", usage, args, func(fl *flag.FlagSet) {
		fl.StringVar(&line, "line", "main", "line to export")
		fl.StringVar(&to, "to", "", "Git repository to write the line into")
		fl.StringVar(&ref, "ref", "", "ref to update (default refs/heads/LINE)")
		fl.BoolVar(&proposals, "proposals", false, "also export each open proposal as a branch ledger/INTENT/WORKSPACE")
	}); err != nil || to == "" {
		return nil, apierr.New("usage", usage)
	}
	target, format, err := gitSource(context.Background(), to)
	if err != nil {
		return nil, err
	}
	if ref == "" {
		ref = "refs/heads/" + line
	}
	return withBridge(format, func(ctx context.Context, br *bridge.Bridge, _ string) (any, error) {
		report, err := br.ExportLine(ctx, line)
		if err != nil {
			return nil, err
		}
		if report.Tip == "" {
			return nil, apierr.New("nothing_to_export", "line %s has no commits to export", line)
		}
		if err := fetchInto(ctx, target, br.Staging.Dir, bridge.LineRef(line), ref); err != nil {
			return nil, err
		}
		out := map[string]any{"export": report, "repository": target, "ref": ref}
		if proposals {
			pr, err := br.ExportProposals(ctx, report)
			if err != nil {
				return nil, err
			}
			refused := []map[string]string{}
			for _, p := range pr.Proposals {
				if _, err := placeProposal(ctx, br, target, p, p.Branch); err != nil {
					refused = append(refused, map[string]string{"workspace_id": p.WorkspaceID, "branch": p.Branch, "code": apierr.As(err).Code})
				}
			}
			out["proposals"], out["refused"] = pr, refused
		}
		return out, nil
	})
}

// fetchInto fast-forwards ref in the Git repository at target to the
// staging repository's src ref.
func fetchInto(ctx context.Context, target, staging, src, ref string) error {
	_, err := gitx.Run(ctx, target, nil, []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}, "fetch", "-q", "--no-tags", "--no-write-fetch-head", "--", staging, src+":"+ref)
	if ge, ok := errors.AsType[*gitx.Error](err); ok {
		switch {
		case strings.Contains(ge.Stderr, "checked out"):
			return apierr.New("ref_checked_out", "%s is checked out in %s; export to another ref with --ref", ref, target).With("ref", ref)
		case strings.Contains(ge.Stderr, "non-fast-forward") || strings.Contains(ge.Stderr, "rejected"):
			return apierr.New("ref_moved", "%s in %s has moved and does not lead to the exported line; nothing was changed", ref, target).With("ref", ref)
		}
	}
	return err
}

// cmdGitMirror exports a line and pushes it with an ordinary git push from
// the bridged repository: ledger git mirror --remote NAME [--line LINE]
// [--repo PATH]. It never force-pushes. The push uses the user's Git
// configuration for credentials, with hooks disabled.
func cmdGitMirror(args []string) (any, error) {
	var line, remote, repo string
	var proposals bool
	const usage = "ledger git mirror --remote NAME [--line LINE] [--repo PATH] [--proposals]"
	if err := parseFlags("git mirror", usage, args, func(fl *flag.FlagSet) {
		fl.StringVar(&line, "line", "main", "line to mirror")
		fl.StringVar(&remote, "remote", "", "remote of the Git repository to push to")
		fl.StringVar(&repo, "repo", "", "Git repository whose remote is used (default: the one this repository was imported from)")
		fl.BoolVar(&proposals, "proposals", false, "also push each open proposal as a branch ledger/INTENT/WORKSPACE")
	}); err != nil || remote == "" || strings.HasPrefix(remote, "-") {
		return nil, apierr.New("usage", usage)
	}
	format := ""
	if repo != "" {
		var err error
		if repo, format, err = gitSource(context.Background(), repo); err != nil {
			return nil, err
		}
	}
	return withBridge(format, func(ctx context.Context, br *bridge.Bridge, ledgerDir string) (any, error) {
		if repo == "" {
			bc, _, err := readBridgeConfig(ledgerDir)
			if err != nil {
				return nil, err
			}
			repo = bc.Source
		}
		report, err := br.ExportLine(ctx, line)
		if err != nil {
			return nil, err
		}
		if report.Tip == "" {
			return nil, apierr.New("nothing_to_export", "line %s has no commits to export", line)
		}
		local := "refs/ledger/lines/" + line
		if err := fetchInto(ctx, repo, br.Staging.Dir, bridge.LineRef(line), local); err != nil {
			return nil, err
		}
		_, err = gitx.Run(ctx, repo, nil, nil, "push", "-q", "--no-verify", "--", remote, local+":refs/heads/"+line)
		if ge, ok := errors.AsType[*gitx.Error](err); ok && (strings.Contains(ge.Stderr, "rejected") || strings.Contains(ge.Stderr, "non-fast-forward")) {
			return nil, apierr.New("ref_moved", "%s on %s has moved and does not lead to the exported line; nothing was pushed", line, remote).With("remote", remote)
		}
		if err != nil {
			return nil, err
		}
		out := map[string]any{"export": report, "repository": repo, "remote": remote, "ref": "refs/heads/" + line}
		if proposals {
			pr, err := br.ExportProposals(ctx, report)
			if err != nil {
				return nil, err
			}
			refused := []map[string]string{}
			for _, p := range pr.Proposals {
				if err := pushProposal(ctx, br, repo, remote, p); err != nil {
					refused = append(refused, map[string]string{"workspace_id": p.WorkspaceID, "branch": p.Branch, "code": apierr.As(err).Code})
				}
			}
			out["proposals"], out["refused"] = pr, refused
		}
		return out, nil
	})
}

// proposalRef is where the bridged repository keeps the proposal it last
// mirrored, the lease for the next push of that branch.
func proposalRef(p bridge.ProposalExport) string {
	return "refs/ledger/proposals/" + p.IntentID + "/" + p.WorkspaceID
}

// pushProposal pushes one proposal branch to remote. The push replaces the
// remote branch only while it still holds the commit last mirrored from
// here, so a re-proposal updates the branch and nobody's push is lost.
func pushProposal(ctx context.Context, br *bridge.Bridge, repo, remote string, p bridge.ProposalExport) error {
	lease, err := placeProposal(ctx, br, repo, p, proposalRef(p))
	if err != nil {
		return err
	}
	_, err = gitx.Run(ctx, repo, nil, nil, "push", "-q", "--no-verify", "--force-with-lease="+p.Branch+":"+lease, "--", remote, proposalRef(p)+":"+p.Branch)
	if ge, ok := errors.AsType[*gitx.Error](err); ok && (strings.Contains(ge.Stderr, "rejected") || strings.Contains(ge.Stderr, "stale info")) {
		return apierr.New("ref_moved", "%s on %s has moved since it was last mirrored; nothing was pushed", p.Branch, remote).With("ref", p.Branch)
	}
	return err
}

// placeProposal sets ref in the Git repository at target to the exported
// proposal p and returns what ref held before ("" for nothing). A ref that
// holds a commit Ledger did not export for the same workspace is left
// alone (ref_moved), as is the branch checked out there.
func placeProposal(ctx context.Context, br *bridge.Bridge, target string, p bridge.ProposalExport, ref string) (string, error) {
	hardened := []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	current := ""
	if out, err := gitx.Run(ctx, target, nil, hardened, "rev-parse", "-q", "--verify", ref+"^{commit}"); err == nil {
		current = strings.TrimSpace(string(out))
	}
	if current == p.Commit {
		return current, nil
	}
	if head, err := gitx.Run(ctx, target, nil, hardened, "symbolic-ref", "-q", "HEAD"); err == nil && strings.TrimSpace(string(head)) == ref {
		return "", apierr.New("ref_checked_out", "%s is checked out in %s", ref, target).With("ref", ref)
	}
	if current != "" {
		ours, err := exportedFor(ctx, br, current, p.WorkspaceID)
		if err != nil {
			return "", err
		}
		if !ours {
			return "", apierr.New("ref_moved", "%s in %s holds a commit Ledger did not export for %s; nothing was changed", ref, target, p.WorkspaceID).With("ref", ref)
		}
	}
	if _, err := gitx.Run(ctx, target, nil, hardened, "fetch", "-q", "--no-tags", "--no-write-fetch-head", "--", br.Staging.Dir, p.Staging); err != nil {
		return "", err
	}
	if _, err := gitx.Run(ctx, target, nil, hardened, "update-ref", "-m", "ledger git export", ref, p.Commit, current); err != nil {
		return "", apierr.New("ref_moved", "%s in %s changed during the export; nothing was changed", ref, target).With("ref", ref)
	}
	return current, nil
}

// exportedFor reports whether commit is a proposal Ledger exported for
// workspace.
func exportedFor(ctx context.Context, br *bridge.Bridge, commit, workspace string) (bool, error) {
	known, err := br.Records.Index.MappingsForCommit(ctx, commit)
	if err != nil {
		return false, err
	}
	for _, m := range known {
		if m.Direction != "export" || m.Manifest == nil {
			continue
		}
		cm, err := br.Records.Index.Manifest(ctx, *m.Manifest)
		if err != nil {
			return false, err
		}
		if cm.WorkspaceID == workspace {
			return true, nil
		}
	}
	return false, nil
}

// cmdGitSync accepts commits made in Git onto a line: ledger git sync
// [--line LINE] [--from PATH] [--ref REF]. REF defaults to refs/heads/LINE
// of the repository this one was imported from; to take a remote's
// commits, fetch them there first and name the remote-tracking ref.
func cmdGitSync(args []string) (any, error) {
	var line, from, ref string
	if err := parseFlags("git sync", "ledger git sync [--line LINE] [--from PATH] [--ref REF]", args, func(fl *flag.FlagSet) {
		fl.StringVar(&line, "line", "main", "line to bring up to the Git ref")
		fl.StringVar(&from, "from", "", "Git repository (default: the one this repository was imported from)")
		fl.StringVar(&ref, "ref", "", "Git ref to take commits from (default refs/heads/LINE)")
	}); err != nil {
		return nil, err
	}
	if ref == "" {
		ref = "refs/heads/" + line
	}
	src, format := "", ""
	if from != "" {
		var err error
		if src, format, err = gitSource(context.Background(), from); err != nil {
			return nil, err
		}
	}
	return withBridge(format, func(ctx context.Context, br *bridge.Bridge, ledgerDir string) (any, error) {
		if src == "" {
			bc, _, err := readBridgeConfig(ledgerDir)
			if err != nil {
				return nil, err
			}
			src = bc.Source
		}
		return br.Sync(ctx, src, ref, line)
	})
}

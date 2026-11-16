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
		return nil, apierr.New("usage", "ledger git import|export|mirror ...")
	}
	switch args[0] {
	case "import":
		return cmdGitImport(args[1:])
	case "export":
		return cmdGitExport(args[1:])
	case "mirror":
		return cmdGitMirror(args[1:])
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
	if err := parseFlags("git export", "ledger git export [--line LINE] --to PATH [--ref REF]", args, func(fl *flag.FlagSet) {
		fl.StringVar(&line, "line", "main", "line to export")
		fl.StringVar(&to, "to", "", "Git repository to write the line into")
		fl.StringVar(&ref, "ref", "", "ref to update (default refs/heads/LINE)")
	}); err != nil || to == "" {
		return nil, apierr.New("usage", "ledger git export [--line LINE] --to PATH [--ref REF]")
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
		return map[string]any{"export": report, "repository": target, "ref": ref}, nil
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
	if err := parseFlags("git mirror", "ledger git mirror --remote NAME [--line LINE] [--repo PATH]", args, func(fl *flag.FlagSet) {
		fl.StringVar(&line, "line", "main", "line to mirror")
		fl.StringVar(&remote, "remote", "", "remote of the Git repository to push to")
		fl.StringVar(&repo, "repo", "", "Git repository whose remote is used (default: the one this repository was imported from)")
	}); err != nil || remote == "" || strings.HasPrefix(remote, "-") {
		return nil, apierr.New("usage", "ledger git mirror --remote NAME [--line LINE] [--repo PATH]")
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
		return map[string]any{"export": report, "repository": repo, "remote": remote, "ref": "refs/heads/" + line}, nil
	})
}

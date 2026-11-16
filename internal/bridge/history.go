package bridge

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/store"
)

// Bridge acts on one native repository's records, store and staging
// repository as its root.
type Bridge struct {
	Records *record.Store
	Objects *store.Store
	Staging *Staging
	Root    identity.Signer
}

// ImportReport describes one import.
type ImportReport struct {
	Ref        string `json:"ref"`
	Tip        string `json:"tip"`
	NativeTree string `json:"native_tree"`
	Commits    int    `json:"commits"`
	Mapped     int    `json:"mapped"`
	Receipt    string `json:"receipt"`
}

// Fetched is a ref's first-parent history converted into the store.
type Fetched struct {
	Ref     string
	Commits []Imported
}

// Tip returns the converted tip of the history.
func (f Fetched) Tip() Imported { return f.Commits[len(f.Commits)-1] }

// Convert fetches ref from the Git repository at src and converts its
// first-parent history into native trees, without publishing anything.
func (s *Staging) Convert(ctx context.Context, objects *store.Store, src, ref string) (Fetched, error) {
	tip, err := s.Fetch(ctx, src, ref)
	if err != nil {
		return Fetched{}, err
	}
	shas, err := s.FirstParents(ctx, tip)
	if err != nil {
		return Fetched{}, err
	}
	b := objects.Batch()
	im, err := s.NewImporter(ctx, b)
	if err != nil {
		return Fetched{}, err
	}
	f := Fetched{Ref: ref}
	for _, sha := range shas {
		c, err := im.Commit(sha)
		if err != nil {
			_ = im.Close()
			return Fetched{}, err
		}
		f.Commits = append(f.Commits, c)
	}
	if err := im.Close(); err != nil {
		return Fetched{}, err
	}
	if err := b.Commit(); err != nil {
		return Fetched{}, err
	}
	if _, err := s.Git(ctx, nil, "update-ref", importRef(tip), tip); err != nil {
		return Fetched{}, err
	}
	return f, nil
}

// importRef keeps an imported commit reachable in the staging repository,
// so export can build on it.
func importRef(sha string) string { return "refs/bridge/imported/" + sha }

// Publish signs one import mapping for every commit of f that has none,
// in one operation. Imported history carries no intent, capability or
// evidence: a mapping says only that the tree and the commit agree.
func (br *Bridge) Publish(ctx context.Context, f Fetched) (ImportReport, error) {
	tip := f.Tip()
	report := ImportReport{Ref: f.Ref, Tip: tip.SHA, NativeTree: tip.NativeTree, Commits: len(f.Commits)}
	shas := make([]string, 0, len(f.Commits))
	for _, c := range f.Commits {
		shas = append(shas, c.SHA)
	}
	digest, err := canon.Marshal(map[string]any{"operation": "bridge.import", "ref": f.Ref, "commits": shas})
	if err != nil {
		return report, err
	}
	op := record.Op{ID: schema.NewID(schema.PrefixOperation), Name: "bridge.import", Digest: canon.Hash(digest), Signer: br.Root}
	var mapped int
	res, err := br.Records.Do(ctx, op, func(idx *index.Index) (any, []envelope.Object, error) {
		var members []envelope.Object
		for _, c := range f.Commits {
			known, err := idx.MappingsForCommit(ctx, c.SHA)
			if err != nil {
				return nil, nil, err
			}
			if slices.ContainsFunc(known, func(m index.Mapping) bool { return m.Direction == "import" && m.NativeTree == c.NativeTree }) {
				continue
			}
			o, err := envelope.SealVersion(2, schema.KindBridgeMapping, schema.BridgeMapping{NativeTree: c.NativeTree, GitCommit: c.SHA, GitTree: c.Tree,
				GitParents: c.Parents, Direction: "import", Author: c.Author, Committer: c.Committer, Message: c.Message}, br.Root)
			if err != nil {
				return nil, nil, err
			}
			members = append(members, o)
		}
		mapped = len(members)
		if mapped == 0 {
			return nil, nil, errNothing
		}
		return map[string]any{"mapped": mapped}, members, nil
	})
	if apierr.HasCode(err, "nothing_to_publish") {
		return report, nil
	}
	if err != nil {
		return report, err
	}
	report.Mapped, report.Receipt = mapped, res.Receipt
	return report, nil
}

var errNothing = apierr.New("nothing_to_publish", "every commit is already mapped")

// ExportReport describes one line export.
type ExportReport struct {
	Line     string   `json:"line"`
	Base     string   `json:"base"`
	Tip      string   `json:"tip"`
	Advances int      `json:"advances"`
	Commits  []string `json:"commits"`
	Mapped   int      `json:"mapped"`
	Ref      string   `json:"staging_ref"`
}

// LineRef is where the staging repository holds an exported line.
func LineRef(line string) string { return "refs/bridge/lines/" + line }

// ExportLine writes one Git commit per advance of line onto the commit of
// the repository's initial tree, and signs an export mapping for each new
// commit (ADR 0028). Author, committer, dates and message come from the
// signed records, so the same records always produce the same commits.
func (br *Bridge) ExportLine(ctx context.Context, line string) (ExportReport, error) {
	idx := br.Records.Index
	report := ExportReport{Line: line, Ref: LineRef(line), Commits: []string{}}
	repo, err := idx.Repository(ctx)
	if err != nil {
		return report, err
	}
	advances, err := idx.LineAdvances(ctx, line)
	if err != nil {
		return report, err
	}
	report.Advances = len(advances)
	var outs []Out
	base, err := br.baseCommit(ctx, repo)
	if err != nil {
		return report, err
	}
	parent, parentTree := base, repo.Tree
	if base == "" {
		outs = append(outs, Out{Tree: repo.Tree, Author: rootIdent(repo.CreatedAt, br.Root.ID()), Committer: rootIdent(repo.CreatedAt, br.Root.ID()),
			Message: fmt.Sprintf("Initial tree of %s\n\nLedger-Repository: %s\n", repo.ID, repo.ID)})
		parent = ":1"
	}
	for _, a := range advances {
		o, err := br.advanceCommit(ctx, a)
		if err != nil {
			return report, err
		}
		o.Parent, o.ParentTree = parent, parentTree
		outs = append(outs, o)
		parent, parentTree = fmt.Sprintf(":%d", len(outs)), o.Tree
	}
	written, err := br.Staging.Export(ctx, br.Objects, outs)
	if err != nil {
		return report, err
	}
	tip := base
	if len(written) > 0 {
		tip = written[len(written)-1].SHA
	}
	if tip == "" {
		return report, nil
	}
	report.Base, report.Tip = base, tip
	if base == "" {
		report.Base = written[0].SHA
	}
	if _, err := br.Staging.Git(ctx, nil, "update-ref", LineRef(line), tip); err != nil {
		return report, err
	}
	mappings := make([]schema.BridgeMapping, 0, len(written))
	off := len(written) - len(advances)
	for i, w := range written {
		report.Commits = append(report.Commits, w.SHA)
		m := schema.BridgeMapping{NativeTree: outs[i].Tree, GitCommit: w.SHA, GitTree: w.Tree, GitParents: []string{}, Direction: "export",
			Author: outs[i].Author, Committer: outs[i].Committer, Message: outs[i].Message}
		switch {
		case i > 0:
			m.GitParents = []string{written[i-1].SHA}
		case base != "":
			m.GitParents = []string{base}
		}
		if i >= off {
			h := advances[i-off].Hash
			m.LineAdvance = &h
		}
		mappings = append(mappings, m)
	}
	report.Mapped, err = br.publishExports(ctx, line, mappings)
	return report, err
}

// publishExports signs the export mappings not already recorded.
func (br *Bridge) publishExports(ctx context.Context, line string, mappings []schema.BridgeMapping) (int, error) {
	commits := make([]string, 0, len(mappings))
	for _, m := range mappings {
		commits = append(commits, m.GitCommit)
	}
	digest, err := canon.Marshal(map[string]any{"operation": "bridge.export", "line": line, "commits": commits})
	if err != nil {
		return 0, err
	}
	op := record.Op{ID: schema.NewID(schema.PrefixOperation), Name: "bridge.export", Digest: canon.Hash(digest), Signer: br.Root}
	n := 0
	_, err = br.Records.Do(ctx, op, func(idx *index.Index) (any, []envelope.Object, error) {
		var members []envelope.Object
		for _, m := range mappings {
			known, err := idx.MappingsForCommit(ctx, m.GitCommit)
			if err != nil {
				return nil, nil, err
			}
			if slices.ContainsFunc(known, func(k index.Mapping) bool { return k.Direction == "export" }) {
				continue
			}
			o, err := envelope.SealVersion(2, schema.KindBridgeMapping, m, br.Root)
			if err != nil {
				return nil, nil, err
			}
			members = append(members, o)
		}
		n = len(members)
		if n == 0 {
			return nil, nil, errNothing
		}
		return map[string]any{"mapped": n}, members, nil
	})
	if apierr.HasCode(err, "nothing_to_publish") {
		return 0, nil
	}
	return n, err
}

// baseCommit is the imported commit of the repository's initial tree, or
// "" when none was imported. Of several commits with that tree, it is the
// one no other of them has as a parent (the latest of a run of commits
// that left the tree unchanged), the least by id if that is not unique.
func (br *Bridge) baseCommit(ctx context.Context, repo schema.Repository) (string, error) {
	known, err := br.Records.Index.MappingsForTree(ctx, repo.Tree)
	if err != nil {
		return "", err
	}
	candidates := map[string]bool{}
	for _, m := range known {
		if m.Direction == "import" {
			candidates[m.GitCommit] = true
		}
	}
	for _, m := range known {
		if m.Direction == "import" {
			for _, p := range m.GitParents {
				if candidates[p] && p != m.GitCommit {
					delete(candidates, p)
				}
			}
		}
	}
	if len(candidates) == 0 {
		return "", nil
	}
	base := slices.Sorted(maps.Keys(candidates))[0]
	if _, err := br.Staging.Git(ctx, nil, "cat-file", "-e", base+"^{commit}"); err != nil {
		return "", apierr.New("bridge_source_unavailable", "imported commit %s is not in the staging repository; run ledger git import again", base).With("commit", base)
	}
	return base, nil
}

// advanceCommit is the commit for one advance: authored by the manifest's
// author and committed by the advance's actor, both at the advance's time,
// with the plan (or the intent's description) and the Ledger trailers.
func (br *Bridge) advanceCommit(ctx context.Context, a index.Advance) (Out, error) {
	idx := br.Records.Index
	m, err := idx.Manifest(ctx, a.Manifest)
	if err != nil {
		return Out{}, err
	}
	cr, err := idx.Creation(ctx, m.IntentID)
	if err != nil {
		return Out{}, err
	}
	var intent schema.Intent
	if err := cr.Decode(&intent); err != nil {
		return Out{}, err
	}
	author, err := br.ident(ctx, m.AuthorSession, a.At)
	if err != nil {
		return Out{}, err
	}
	committer, err := br.ident(ctx, a.Actor, a.At)
	if err != nil {
		return Out{}, err
	}
	subject := intent.Description
	if m.Plan != nil && strings.TrimSpace(*m.Plan) != "" {
		subject = *m.Plan
	}
	msg := fmt.Sprintf("%s\n\nLedger-Line-Advance: %s\nLedger-Manifest: %s\nLedger-Intent: %s\nLedger-Selection: %s\n",
		strings.TrimSpace(subject), a.Hash, a.Manifest, m.IntentID, a.Selection)
	return Out{Tree: m.Tree, Author: author, Committer: committer, Message: msg}, nil
}

// ident is the Git ident of principal at time at: a session by its agent
// name, or root by its fingerprint, with an address that cannot be
// mistaken for a person's.
func (br *Bridge) ident(ctx context.Context, principal, at string) (string, error) {
	if !strings.HasPrefix(principal, schema.PrefixSession) {
		return rootIdent(at, principal), nil
	}
	cr, err := br.Records.Index.Creation(ctx, principal)
	if err != nil {
		return "", err
	}
	var s schema.Session
	if err := cr.Decode(&s); err != nil {
		return "", err
	}
	name := principal
	if s.AgentName != nil {
		name = cmp.Or(clean(*s.AgentName), principal)
	}
	return gitIdent(name, principal+"@ledger.invalid", at), nil
}

func rootIdent(at, fingerprint string) string {
	return gitIdent("Ledger root", fingerprint[:16]+"@ledger.invalid", at)
}

func gitIdent(name, email, at string) string {
	t, err := schema.ParseTimestamp(at)
	if err != nil {
		return fmt.Sprintf("%s <%s> 0 +0000", name, email)
	}
	return fmt.Sprintf("%s <%s> %d +0000", name, email, t.Unix())
}

// clean drops the characters an ident name cannot hold.
func clean(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '<' || r == '>' || r == '\n' || r == 0 {
			return -1
		}
		return r
	}, s))
}

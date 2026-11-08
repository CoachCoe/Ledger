package verify

import (
	"context"
	"os"
	"slices"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/gitx"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// Source is what Verify reads: a corpus of records and, if available, a
// Git repository holding the retained proposal commits.
type Source struct {
	Name          string
	Corpus        *Corpus
	Repo          *gitx.Repo
	BundleVersion int
	Inventory     *Inventory
	// Strict marks a bundle, whose records are all supposed to be committed.
	Strict bool
	// Missing and Problems carry findings made while reading the source.
	Missing  []string
	Problems []Problem
	Size     Size
	Staged   *Staged
}

// Close removes any staging directory.
func (s *Source) Close() {
	if s != nil && s.Staged != nil {
		s.Staged.Remove()
	}
}

// OpenRepoSource reads a repository's records from refs/ledger/records, or
// from .ledger/objects when the ref does not exist yet. It opens no SQLite
// index and needs no daemon.
func OpenRepoSource(ctx context.Context, repo *gitx.Repo) (*Source, error) {
	c, err := LoadRecordsRef(ctx, repo)
	name := RecordsRef
	if apierr.HasCode(err, "records_ref_missing") {
		c, err = LoadObjectsDir(repo.MainDir + "/.ledger/objects")
		name = ".ledger/objects"
	}
	if err != nil {
		return nil, err
	}
	return &Source{Name: name, Corpus: c, Repo: repo, Size: Size{Records: len(c.Objects)}}, nil
}

// OpenBundleSource stages a bundle in a private temporary directory and
// loads it. The caller must Close the source.
func OpenBundleSource(ctx context.Context, path string, lim Limits) (*Source, error) {
	dir, err := os.MkdirTemp("", "ledger-verify-")
	if err != nil {
		return nil, err
	}
	return openStaged(ctx, path, dir, lim)
}

func openStaged(ctx context.Context, path, dir string, lim Limits) (*Source, error) {
	st, err := Stage(ctx, path, dir, lim)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	c, err := st.Corpus()
	if err != nil {
		st.Remove()
		return nil, err
	}
	inv := st.Inventory
	src := &Source{Name: path, Corpus: c, Repo: st.Repo, BundleVersion: inv.BundleVersion, Inventory: &inv, Strict: true,
		Missing: st.Missing(), Problems: st.Problems, Size: st.Size, Staged: st}
	if !inv.Complete {
		src.Missing = append(src.Missing, inv.Missing...)
		if len(inv.Missing) == 0 {
			src.Missing = append(src.Missing, "incomplete")
		}
	}
	return src, nil
}

// Options configures Verify.
type Options struct {
	// Trusted is the local trust list; nil trusts nothing. Keys carried by a
	// source are never added to it.
	Trusted *identity.TrustList
}

// RootTrust is one root key present in the source and whether the local
// trust list names it.
type RootTrust struct {
	Fingerprint string `json:"fingerprint"`
	Trusted     bool   `json:"trusted"`
}

// ManifestReport verifies one change manifest and everything attached to it.
type ManifestReport struct {
	Hash          string            `json:"hash"`
	ChangeID      string            `json:"change_id"`
	IntentID      string            `json:"intent_id"`
	AuthorSession string            `json:"author_session"`
	Commit        string            `json:"commit"`
	Integrity     string            `json:"integrity"`
	Trust         string            `json:"trust"`
	Binding       Binding           `json:"binding"`
	Evidence      []EvidenceReport  `json:"evidence"`
	Selections    []SelectionReport `json:"selections"`
	Components    map[string]string `json:"components"`
	Detail        string            `json:"detail,omitempty"`
}

// Report separates integrity (do the records hold together and verify?)
// from trust (does the local policy trust the roots that vouch for them?).
// Unavailable dependencies are never reported as invalid or verified.
type Report struct {
	BundleVersion int              `json:"bundle_version"`
	Source        string           `json:"source"`
	Integrity     string           `json:"integrity"`
	Trust         string           `json:"trust"`
	Complete      bool             `json:"complete"`
	Roots         []RootTrust      `json:"roots"`
	Manifests     []ManifestReport `json:"manifests"`
	Runs          []RunReport      `json:"runs"`
	Conflicts     []Conflict       `json:"conflicts"`
	Missing       []string         `json:"missing"`
	Invalid       []Problem        `json:"invalid"`
	Pending       []string         `json:"pending"`
	Unattached    []string         `json:"unattached"`
	Size          Size             `json:"size"`
}

func bindingState(v string) string {
	switch v {
	case BindingOK:
		return Verified
	case BindingMismatch:
		return Invalid
	}
	return Unavailable
}

// Verify checks src and reports. It reads no SQLite and executes nothing
// from the source. An unsupported schema version never reaches a report: it
// fails loading.
func Verify(ctx context.Context, src *Source, opts Options) (*Report, error) {
	c := src.Corpus
	sensitive := src.Inventory == nil || src.Inventory.CaseSensitive
	g, err := newGraph(valuesOf(c.Objects), sensitive)
	if err != nil {
		return nil, err
	}
	rep := &Report{
		BundleVersion: src.BundleVersion, Source: src.Name, Roots: []RootTrust{}, Manifests: []ManifestReport{}, Runs: []RunReport{},
		Conflicts: []Conflict{}, Missing: []string{}, Invalid: []Problem{}, Pending: nilToEmpty(c.Pending), Unattached: []string{}, Size: src.Size,
	}
	rep.Invalid = append(rep.Invalid, src.Problems...)
	rep.Invalid = append(rep.Invalid, c.Problems...)
	sigProblems, sigMissing := g.rawSignatures(c)
	rep.Invalid = append(rep.Invalid, sigProblems...)
	rep.Missing = append(rep.Missing, c.Missing...)
	rep.Missing = append(rep.Missing, src.Missing...)
	rep.Missing = append(rep.Missing, sigMissing...)

	trusted := opts.Trusted.Fingerprints()
	for _, h := range sortedKeys(g.objects) {
		if o := g.objects[h]; o.Envelope.Kind == schema.KindRootKey && c.Committed[h] {
			rep.Roots = append(rep.Roots, RootTrust{Fingerprint: o.Envelope.Signer, Trusted: opts.Trusted.Contains(o.Envelope.Signer)})
		}
	}
	rep.Roots = slices.CompactFunc(rep.Roots, func(a, b RootTrust) bool { return a.Fingerprint == b.Fingerprint })

	commits := map[string]string{}
	trusts := map[string]string{}
	for _, h := range sortedKeys(g.objects) {
		o := g.objects[h]
		if o.Envelope.Kind != schema.KindChangeManifest {
			continue
		}
		m, err := decode[schema.ChangeManifest](o)
		if err != nil {
			rep.Invalid = append(rep.Invalid, Problem{Hash: h, Code: apierr.As(err).Code, Error: apierr.As(err).Message})
			continue
		}
		mr := ManifestReport{Hash: h, ChangeID: m.ChangeID, IntentID: m.IntentID, AuthorSession: m.AuthorSession,
			Evidence: []EvidenceReport{}, Selections: []SelectionReport{}, Components: map[string]string{}}
		res := g.manifest(h, trusted)
		mr.Components["authority"] = res.Integrity
		trusts[h] = res.Trust
		if res.Error != nil {
			mr.Detail = res.Error.Message
			mr.Components["authority"] = stateOf(res.Error)
		}
		commit, cerr := commitFor(ctx, src.Repo, h)
		if cerr != nil {
			return nil, cerr
		}
		mr.Commit = commit.SHA
		commits[h] = commit.SHA
		mr.Binding = CheckBinding(commit, h, m)
		if commit.SHA == "" {
			rep.Missing = append(rep.Missing, "commit:"+h)
		}
		mr.Components["trailer"], mr.Components["tree"], mr.Components["parent"] = bindingState(mr.Binding.Trailer), bindingState(mr.Binding.Tree), bindingState(mr.Binding.Parent)
		rep.Manifests = append(rep.Manifests, mr)
	}
	byManifest := map[string]*ManifestReport{}
	for i := range rep.Manifests {
		byManifest[rep.Manifests[i].Hash] = &rep.Manifests[i]
	}
	for _, h := range sortedKeys(g.objects) {
		o := g.objects[h]
		switch o.Envelope.Kind {
		case schema.KindEvidenceAttestation:
			att, err := decode[schema.EvidenceAttestation](o)
			if err != nil {
				continue
			}
			mr := byManifest[att.Manifest]
			if mr == nil {
				rep.Unattached = append(rep.Unattached, h)
				rep.Missing = append(rep.Missing, "manifest:"+att.Manifest)
				continue
			}
			mr.Evidence = append(mr.Evidence, g.evidence(o, commits[att.Manifest]))
			if _, ok := g.objects[att.CheckDefinition]; !ok {
				rep.Missing = append(rep.Missing, att.CheckDefinition)
			}
		case schema.KindSelectionDecision:
			d, err := decode[schema.SelectionDecision](o)
			if err != nil {
				continue
			}
			mr := byManifest[d.Manifest]
			if mr == nil {
				rep.Unattached = append(rep.Unattached, h)
				rep.Missing = append(rep.Missing, "manifest:"+d.Manifest)
				continue
			}
			mr.Selections = append(mr.Selections, g.selection(o, commits[d.Manifest]))
			for _, h := range append(slices.Clone(d.Evidence), d.Competitors...) {
				if _, ok := g.objects[h]; !ok {
					rep.Missing = append(rep.Missing, h)
				}
			}
		}
	}
	rep.Runs = g.runs()
	rep.Conflicts = append(rep.Conflicts, conflicts(c)...)
	if src.Inventory != nil && src.Repo != nil {
		for _, ic := range src.Inventory.Git.Commits {
			if sha, err := src.Repo.ResolveRef(ctx, ProposalRefPrefix+ic.Manifest); err == nil && sha != "" && sha != ic.Commit {
				rep.Invalid = append(rep.Invalid, Problem{Hash: ic.Manifest, Code: "inventory_mismatch", Error: "retained commit differs from the inventory"})
			}
		}
	}
	integrity := []string{}
	for i := range rep.Manifests {
		mr := &rep.Manifests[i]
		if len(mr.Evidence) > 0 {
			mr.Components["evidence"] = worstEvidence(mr.Evidence)
		}
		if len(mr.Selections) > 0 {
			mr.Components["selections"] = worstSelections(mr.Selections)
		}
		mr.Integrity = worst(valuesOf(mr.Components)...)
		mr.Trust = Unknown
		if mr.Integrity == Verified {
			mr.Trust = trustOf(trusts[mr.Hash])
		}
		integrity = append(integrity, mr.Integrity)
	}
	for _, r := range rep.Runs {
		integrity = append(integrity, r.Integrity)
	}
	slices.Sort(rep.Missing)
	rep.Missing = slices.Compact(rep.Missing)
	slices.SortFunc(rep.Invalid, func(a, b Problem) int {
		if a.Hash != b.Hash {
			return compareStrings(a.Hash, b.Hash)
		}
		return compareStrings(a.Code, b.Code)
	})
	rep.Complete = len(rep.Missing) == 0 && (src.Inventory == nil || src.Inventory.Complete)
	switch {
	case len(rep.Invalid) > 0 || slices.Contains(integrity, Invalid):
		rep.Integrity = Invalid
	case len(rep.Missing) > 0 || slices.Contains(integrity, Unavailable) || !rep.Complete || (src.Strict && len(rep.Pending) > 0):
		rep.Integrity = Unavailable
	default:
		rep.Integrity = Verified
	}
	rep.Trust = Unknown
	if rep.Integrity == Verified && len(rep.Roots) > 0 {
		rep.Trust = Trusted
		for _, r := range rep.Roots {
			if !r.Trusted {
				rep.Trust = Untrusted
			}
		}
		for _, m := range rep.Manifests {
			if m.Trust != Trusted {
				rep.Trust = Untrusted
			}
		}
	}
	return rep, nil
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func trustOf(s string) string {
	if s == Trusted {
		return Trusted
	}
	return Untrusted
}

func worstEvidence(es []EvidenceReport) string {
	states := make([]string, len(es))
	for i, e := range es {
		states[i] = e.Integrity
	}
	return worst(states...)
}

func worstSelections(ss []SelectionReport) string {
	states := make([]string, len(ss))
	for i, s := range ss {
		states[i] = s.Integrity
	}
	return worst(states...)
}

// commitFor returns the commit retained for manifest, or the zero Commit
// when the ref or the commit object is unavailable.
func commitFor(ctx context.Context, repo *gitx.Repo, manifest string) (gitx.Commit, error) {
	if repo == nil {
		return gitx.Commit{}, nil
	}
	sha, err := repo.ResolveRef(ctx, ProposalRefPrefix+manifest)
	if err != nil {
		return gitx.Commit{}, err
	}
	if sha == "" {
		return gitx.Commit{}, nil
	}
	c, err := repo.ReadCommit(ctx, sha)
	if apierr.HasCode(err, "dependency_unavailable") {
		return gitx.Commit{}, nil
	}
	return c, err
}

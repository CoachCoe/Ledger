package verify

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/atomicfile"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/gitx"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// Generator names the producer in every inventory.
const Generator = "ledger-bundle/1"

// ProposalRefPrefix is where proposal revisions are retained.
const ProposalRefPrefix = "refs/ledger/proposals/"

// Export stages, in order. ExportOptions.Fault is called before each.
const (
	StageExportInventory = "export_inventory"
	StageExportGit       = "export_git"
	StageExportWrite     = "export_write"
	StageExportRename    = "export_rename"
)

// Inventory is bundle.json, the first entry of every bundle.
type Inventory struct {
	BundleVersion int               `json:"bundle_version"`
	Generator     string            `json:"generator"`
	CreatedAt     string            `json:"created_at"`
	CaseSensitive bool              `json:"case_sensitive"`
	Complete      bool              `json:"complete"`
	IDs           map[string]string `json:"ids"`
	Objects       []InvObject       `json:"objects"`
	Keys          []InvKey          `json:"keys"`
	Roots         []string          `json:"roots"`
	Missing       []string          `json:"missing,omitempty"`
	Git           InvGit            `json:"git"`
	// Store and OpLogs are the native parts of a version 2 bundle.
	Store  []InvStoreObject `json:"store,omitzero"`
	OpLogs []InvOpLog       `json:"oplogs,omitzero"`
}

// InvStoreObject lists one store object file in a version 2 bundle.
type InvStoreObject struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Size int64  `json:"size"`
}

// InvOpLog lists one workspace's operation log, cut at the last head an
// exported manifest pinned.
type InvOpLog struct {
	Workspace string `json:"workspace"`
	Head      string `json:"head"`
	Entries   int    `json:"entries"`
	Size      int64  `json:"size"`
}

// InvObject lists one record file in the archive.
type InvObject struct {
	Hash string `json:"hash"`
	Kind string `json:"kind"`
	Size int64  `json:"size"`
}

// InvKey is a public key carried for verification. Carrying a key never
// makes it trusted.
type InvKey struct {
	Fingerprint string `json:"fingerprint"`
	PublicKey   string `json:"public_key"`
	Role        string `json:"role"`
	SessionID   string `json:"session_id,omitempty"`
}

// InvGit describes the Git object bundle.
type InvGit struct {
	File    string      `json:"file"`
	SHA256  string      `json:"sha256"`
	Size    int64       `json:"size"`
	Refs    []string    `json:"refs"`
	Commits []InvCommit `json:"commits"`
}

// InvCommit names the commit retained for a manifest.
type InvCommit struct {
	Manifest string `json:"manifest"`
	Commit   string `json:"commit"`
	Tree     string `json:"tree"`
	Base     string `json:"base"`
}

// Size is the honest size of a bundle.
type Size struct {
	BundleBytes int64 `json:"bundle_bytes"`
	RecordBytes int64 `json:"record_bytes"`
	GitBytes    int64 `json:"git_bytes"`
	Records     int   `json:"records"`
	Commits     int   `json:"commits"`
	// StoreBytes and StoreObjects size a version 2 bundle's store part.
	StoreBytes   int64 `json:"store_bytes,omitzero"`
	StoreObjects int   `json:"store_objects,omitzero"`
}

func unmarshalInventory(raw []byte, inv *Inventory) error {
	if err := json.Unmarshal(raw, inv, json.RejectUnknownMembers(true)); err != nil {
		return apierr.New("schema_invalid", "bundle_inventory: %v", err)
	}
	return nil
}

// ExportOptions selects and places a bundle.
type ExportOptions struct {
	Out string
	// Manifests and Intents select a closure; both empty exports every
	// committed group.
	Manifests, Intents []string
	// AllowPartial writes a bundle whose inventory says complete:false when
	// a dependency is missing, instead of failing export_incomplete.
	AllowPartial bool
	// Fault, if set, is called before each export stage; an error stops the
	// export there, as a crash would.
	Fault func(stage string) error
	Now   func() time.Time
}

// ExportReport describes a written bundle.
type ExportReport struct {
	Out       string   `json:"out"`
	Complete  bool     `json:"complete"`
	Missing   []string `json:"missing,omitempty"`
	Manifests []string `json:"manifests"`
	Size      Size     `json:"size"`
}

func fault(o ExportOptions, stage string) error {
	if o.Fault == nil {
		return nil
	}
	return o.Fault(stage)
}

// Export writes a portable bundle from the record files under
// .ledger/objects and the retained proposal refs. It reads no SQLite and
// needs no daemon. Local trust lists, private keys, journals, runtime state
// and uncommitted records are never exported. The bundle appears at
// opts.Out only after it is complete and durable.
func Export(ctx context.Context, repo *gitx.Repo, opts ExportOptions) (*ExportReport, error) {
	objDir := filepath.Join(repo.MainDir, ".ledger", "objects")
	sel, err := selectRecords(objDir, &opts)
	if err != nil {
		return nil, err
	}
	c, missing := sel.corpus, sel.missing
	commits := []InvCommit{}
	refs := []string{}
	for _, m := range sel.manifests {
		ref := ProposalRefPrefix + m
		sha, err := repo.ResolveRef(ctx, ref)
		if err != nil {
			return nil, err
		}
		if sha == "" {
			missing = append(missing, "ref:"+ref)
			continue
		}
		commit, err := repo.ReadCommit(ctx, sha)
		if err != nil {
			return nil, err
		}
		if len(commit.Parents) != 1 {
			missing = append(missing, "ref:"+ref)
			continue
		}
		commits = append(commits, InvCommit{Manifest: m, Commit: sha, Tree: commit.Tree, Base: commit.Parents[0]})
		refs = append(refs, ref)
	}
	complete := len(missing) == 0
	if !complete && !opts.AllowPartial {
		return nil, apierr.New("export_incomplete", "%d dependencies are missing; export would not verify as complete", len(missing)).With("missing", missing)
	}
	if err := fault(opts, StageExportGit); err != nil {
		return nil, err
	}
	hashes := sortedKeys(sel.set)
	tmp, err := os.MkdirTemp("", "ledger-export-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	gitFile, gitRefs, err := buildGitBundle(ctx, repo, tmp, refs, hashes, objDir)
	if err != nil {
		return nil, err
	}
	inv := Inventory{
		BundleVersion: schema.BundleVersion, Generator: Generator, CreatedAt: schema.Timestamp(opts.Now()),
		CaseSensitive: caseSensitive(repo), Complete: complete, Missing: nilToEmpty(missing),
		Git: InvGit{Refs: nilToEmpty(gitRefs), Commits: commits},
	}
	var parts []part
	var gitBytes int64
	if gitFile != "" {
		sum, size, err := fileDigest(gitFile)
		if err != nil {
			return nil, err
		}
		inv.Git.File, inv.Git.SHA256, inv.Git.Size, gitBytes = GitName, sum, size, size
		parts = append(parts, part{GitName, func() ([]byte, error) { return os.ReadFile(gitFile) }}) //nolint:gosec // a file Ledger just wrote
	}
	rep, err := writeBundle(opts, inv, c, hashes, parts)
	if err != nil {
		return nil, err
	}
	rep.Missing, rep.Manifests = missing, sel.manifests
	rep.Size.GitBytes, rep.Size.Commits = gitBytes, len(commits)
	return rep, nil
}

// chosen is the set of committed records an export carries.
type chosen struct {
	corpus    *Corpus
	set       map[string]bool
	missing   []string
	manifests []string
}

// selectRecords loads the records in objDir and picks the provenance
// closure opts asks for, or every committed group.
func selectRecords(objDir string, opts *ExportOptions) (chosen, error) {
	if opts.Out == "" {
		return chosen{}, apierr.New("usage", "export needs an output file")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if _, err := os.Lstat(opts.Out); err == nil {
		return chosen{}, apierr.New("export_exists", "%s already exists", opts.Out)
	}
	c, err := LoadObjectsDir(objDir)
	if err != nil {
		return chosen{}, err
	}
	g, err := newGraph(valuesOf(c.Objects), true)
	if err != nil {
		return chosen{}, err
	}
	if err := fault(*opts, StageExportInventory); err != nil {
		return chosen{}, err
	}
	sel := chosen{corpus: c, manifests: []string{}}
	if len(opts.Manifests)+len(opts.Intents) == 0 {
		sel.set = map[string]bool{}
		for h := range c.Committed {
			sel.set[h] = true
		}
		sel.missing = slices.Clone(c.Missing)
		for _, p := range c.Problems {
			sel.missing = append(sel.missing, "corrupt:"+p.Hash)
		}
	} else {
		ex := newExporter(g, c)
		starts, err := ex.starts(opts.Manifests, opts.Intents)
		if err != nil {
			return chosen{}, err
		}
		for _, h := range starts {
			ex.include(h)
		}
		// A native repository's identity travels with every bundle, so an
		// import can refuse records from another repository up front.
		for _, h := range sortedKeys(c.Objects) {
			if c.Objects[h].Envelope.Kind == schema.KindRepository && c.Committed[h] {
				ex.include(h)
			}
		}
		ex.run()
		sel.set, sel.missing = ex.set, ex.missingList()
	}
	slices.Sort(sel.missing)
	sel.missing = slices.Compact(sel.missing)
	for _, h := range sortedKeys(sel.set) {
		if c.Objects[h].Envelope.Kind == schema.KindChangeManifest {
			sel.manifests = append(sel.manifests, h)
		}
	}
	return sel, nil
}

// part is an archive entry after the records, read only when written.
type part struct {
	name string
	read func() ([]byte, error)
}

// writeBundle lists the records in inv, then writes the archive beside
// opts.Out and renames it into place once it is durable.
func writeBundle(opts ExportOptions, inv Inventory, c *Corpus, hashes []string, parts []part) (*ExportReport, error) {
	inv.IDs, inv.Objects, inv.Keys, inv.Roots = map[string]string{}, []InvObject{}, []InvKey{}, []string{}
	if len(inv.Missing) == 0 {
		inv.Missing = nil
	}
	var recordBytes int64
	for _, h := range hashes {
		o := c.Objects[h]
		inv.Objects = append(inv.Objects, InvObject{Hash: h, Kind: string(o.Envelope.Kind), Size: int64(len(o.Bytes))})
		recordBytes += int64(len(o.Bytes))
		switch o.Envelope.Kind {
		case schema.KindRootKey:
			r, err := decode[schema.RootKey](o)
			if err != nil {
				return nil, err
			}
			inv.Roots = append(inv.Roots, o.Envelope.Signer)
			inv.Keys = append(inv.Keys, InvKey{Fingerprint: o.Envelope.Signer, PublicKey: r.PublicKey, Role: "root"})
		case schema.KindSession:
			s, err := decode[schema.Session](o)
			if err != nil {
				return nil, err
			}
			if k, err := identity.DecodePublicKey(s.PublicKey); err == nil {
				inv.Keys = append(inv.Keys, InvKey{Fingerprint: identity.Fingerprint(k), PublicKey: s.PublicKey, Role: "session", SessionID: s.ID})
			}
			setID(inv.IDs, s.ID, h)
		case schema.KindCapability, schema.KindWorkspace, schema.KindIntent, schema.KindSpeculationRun:
			var p struct {
				ID string `json:"id"`
			}
			if err := decodeInto(o, &p); err == nil {
				setID(inv.IDs, p.ID, h)
			}
		}
	}
	slices.Sort(inv.Roots)
	inv.Roots = slices.Compact(inv.Roots)
	invRaw, err := canon.Marshal(inv)
	if err != nil {
		return nil, err
	}
	if err := schema.ValidateBundle(invRaw); err != nil {
		return nil, err
	}
	if err := fault(opts, StageExportWrite); err != nil {
		return nil, err
	}
	partial := opts.Out + ".partial"
	if err := writeArchive(partial, invRaw, c, hashes, parts); err != nil {
		_ = os.Remove(partial)
		return nil, err
	}
	if err := fault(opts, StageExportRename); err != nil {
		return nil, err
	}
	if err := os.Rename(partial, opts.Out); err != nil {
		return nil, err
	}
	if err := atomicfile.SyncDir(filepath.Dir(opts.Out)); err != nil {
		return nil, err
	}
	fi, err := os.Stat(opts.Out)
	if err != nil {
		return nil, err
	}
	return &ExportReport{Out: opts.Out, Complete: inv.Complete,
		Size: Size{BundleBytes: fi.Size(), RecordBytes: recordBytes, Records: len(hashes)}}, nil
}

func nilToEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// setID records a stable id's creation; with a fork the smallest hash is
// listed and every creation record remains in objects.
func setID(ids map[string]string, id, hash string) {
	if old, ok := ids[id]; !ok || hash < old {
		ids[id] = hash
	}
}

func valuesOf[V any](m map[string]V) []V {
	out := make([]V, 0, len(m))
	for _, k := range sortedKeys(m) {
		out = append(out, m[k])
	}
	return out
}

func caseSensitive(repo *gitx.Repo) bool {
	raw, err := os.ReadFile(filepath.Join(repo.MainDir, ".ledger", "config.json"))
	if err != nil {
		return true
	}
	var cfg struct {
		CaseSensitive *bool `json:"case_sensitive"`
	}
	if json.Unmarshal(raw, &cfg) != nil || cfg.CaseSensitive == nil {
		return true
	}
	return *cfg.CaseSensitive
}

func fileDigest(path string) (string, int64, error) {
	f, err := os.Open(path) //nolint:gosec // a file Ledger just wrote
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// writeArchive writes the tar to path and makes it durable.
func writeArchive(path string, inv []byte, c *Corpus, hashes []string, parts []part) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // the caller names the output
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	tw := tar.NewWriter(f)
	put := func(name string, data []byte) error {
		hdr := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err := put(InventoryName, inv); err != nil {
		return err
	}
	for _, h := range hashes {
		if err := put(recordsPrefix+h[:2]+"/"+h[2:]+".json", c.Objects[h].Bytes); err != nil {
			return err
		}
	}
	for _, p := range parts {
		raw, err := p.read()
		if err != nil {
			return err
		}
		if err := put(p.name, raw); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return f.Sync()
}

// buildGitBundle creates a self-contained Git bundle (no prerequisites) in
// a throwaway bare repository so the export holds exactly the retained
// proposal revisions and a records ref listing exactly the exported
// records, whatever else the source repository contains.
func buildGitBundle(ctx context.Context, repo *gitx.Repo, tmp string, proposalRefs, hashes []string, objDir string) (file string, refs []string, err error) {
	if len(proposalRefs) == 0 && len(hashes) == 0 {
		return "", nil, nil
	}
	bare := filepath.Join(tmp, "export.git")
	if _, err := gitx.Run(ctx, tmp, nil, gitEnv, "init", "--bare", "-q", bare); err != nil {
		return "", nil, err
	}
	if len(proposalRefs) > 0 {
		args := []string{"fetch", "--no-tags", "-q", repo.Dir}
		for _, r := range proposalRefs {
			args = append(args, "+"+r+":"+r)
		}
		if _, err := gitx.Run(ctx, bare, nil, gitEnv, args...); err != nil {
			return "", nil, err
		}
		refs = append(refs, proposalRefs...)
	}
	if len(hashes) > 0 {
		var paths strings.Builder
		for _, h := range hashes {
			paths.WriteString(filepath.Join(objDir, h[:2], h[2:]+".json") + "\n")
		}
		out, err := gitx.Run(ctx, bare, strings.NewReader(paths.String()), gitEnv, "hash-object", "-w", "--stdin-paths")
		if err != nil {
			return "", nil, err
		}
		oids := strings.Fields(string(out))
		if len(oids) != len(hashes) {
			return "", nil, apierr.New("export_failed", "git stored %d of %d records", len(oids), len(hashes))
		}
		var info strings.Builder
		for i, h := range hashes {
			info.WriteString("100644 " + oids[i] + "\t" + h[:2] + "/" + h[2:] + ".json\n")
		}
		idx := append(slices.Clone(gitEnv), "GIT_INDEX_FILE="+filepath.Join(tmp, "records.index"))
		if _, err := gitx.Run(ctx, bare, strings.NewReader(info.String()), idx, "update-index", "--add", "--index-info"); err != nil {
			return "", nil, err
		}
		tree, err := gitx.Run(ctx, bare, nil, idx, "write-tree")
		if err != nil {
			return "", nil, err
		}
		who := append(slices.Clone(gitEnv), "GIT_AUTHOR_NAME=Ledger", "GIT_AUTHOR_EMAIL=ledger@localhost", "GIT_COMMITTER_NAME=Ledger", "GIT_COMMITTER_EMAIL=ledger@localhost")
		commit, err := gitx.Run(ctx, bare, strings.NewReader("ledger records\n"), who, "commit-tree", strings.TrimSpace(string(tree)), "-F", "-")
		if err != nil {
			return "", nil, err
		}
		if _, err := gitx.Run(ctx, bare, nil, gitEnv, "update-ref", RecordsRef, strings.TrimSpace(string(commit))); err != nil {
			return "", nil, err
		}
		refs = append(refs, RecordsRef)
	}
	file = filepath.Join(tmp, "objects.bundle")
	if _, err := gitx.Run(ctx, bare, nil, gitEnv, append([]string{"bundle", "create", "-q", file}, refs...)...); err != nil {
		return "", nil, err
	}
	return file, refs, nil
}

// exporter computes the provenance closure of the selected manifests.
type exporter struct {
	g       *graph
	c       *Corpus
	set     map[string]bool
	missing map[string]bool
	queue   []string

	memberOf  map[string][]string // record -> committed receipts naming it
	eventsOf  map[string][]string // subject id -> lifecycle events
	bindOf    map[string][]string // workspace id -> bindings
	evidence  map[string][]string // manifest -> attestations
	selection map[string][]string // manifest -> decisions
	attempts  map[string][]string // run id and manifest -> attempt records
	runOf     map[string][]string // workspace id -> runs binding it
	runSel    map[string][]string // run id -> its selections
	intentRun map[string][]string // intent id -> runs
	manifests map[string][]string // workspace id -> manifests
}

func newExporter(g *graph, c *Corpus) *exporter {
	e := &exporter{g: g, c: c, set: map[string]bool{}, missing: map[string]bool{},
		memberOf: map[string][]string{}, eventsOf: map[string][]string{}, bindOf: map[string][]string{},
		evidence: map[string][]string{}, selection: map[string][]string{}, attempts: map[string][]string{},
		runOf: map[string][]string{}, runSel: map[string][]string{}, intentRun: map[string][]string{}, manifests: map[string][]string{}}
	for _, h := range sortedKeys(c.Objects) {
		o := c.Objects[h]
		switch o.Envelope.Kind {
		case schema.KindOperationReceipt:
			if r, err := decode[schema.OperationReceipt](o); err == nil && c.Committed[h] {
				for _, m := range r.Members {
					e.memberOf[m] = append(e.memberOf[m], h)
				}
			}
		case schema.KindLifecycleEvent:
			if ev, err := decode[schema.LifecycleEvent](o); err == nil {
				e.eventsOf[ev.SubjectID] = append(e.eventsOf[ev.SubjectID], h)
			}
		case schema.KindWorkspaceBinding:
			if b, err := decode[schema.WorkspaceBinding](o); err == nil {
				e.bindOf[b.WorkspaceID] = append(e.bindOf[b.WorkspaceID], h)
			}
		case schema.KindEvidenceAttestation:
			if a, err := decode[schema.EvidenceAttestation](o); err == nil {
				e.evidence[a.Manifest] = append(e.evidence[a.Manifest], h)
			}
		case schema.KindSelectionDecision:
			if d, err := decode[schema.SelectionDecision](o); err == nil {
				e.selection[d.Manifest] = append(e.selection[d.Manifest], h)
				if d.Selector.Kind == "speculation_run" {
					e.runSel[d.Selector.ID] = append(e.runSel[d.Selector.ID], h)
				}
			}
		case schema.KindChangeManifest:
			if m, err := decode[schema.ChangeManifest](o); err == nil {
				e.manifests[m.WorkspaceID] = append(e.manifests[m.WorkspaceID], h)
			}
		case schema.KindAttemptRecord:
			if a, err := decode[schema.AttemptRecord](o); err == nil {
				e.attempts[a.RunID] = append(e.attempts[a.RunID], h)
				if a.Manifest != nil {
					e.attempts[*a.Manifest] = append(e.attempts[*a.Manifest], h)
				}
			}
		case schema.KindSpeculationRun:
			if r, err := decode[schema.SpeculationRun](o); err == nil {
				for _, a := range r.Attempts {
					e.runOf[a.WorkspaceID] = append(e.runOf[a.WorkspaceID], r.ID)
				}
				e.intentRun[r.IntentID] = append(e.intentRun[r.IntentID], h)
			}
		}
	}
	return e
}

func (e *exporter) starts(manifests, intents []string) ([]string, error) {
	var out []string
	for _, m := range manifests {
		o, ok := e.c.Objects[m]
		if !ok || o.Envelope.Kind != schema.KindChangeManifest || !e.c.Committed[m] {
			return nil, apierr.New("not_found", "no committed manifest %s", m)
		}
		out = append(out, m)
	}
	for _, id := range intents {
		found := false
		for _, h := range e.intentRun[id] {
			if e.c.Committed[h] {
				out = append(out, h)
				found = true
			}
		}
		for _, h := range sortedKeys(e.c.Objects) {
			o := e.c.Objects[h]
			if o.Envelope.Kind != schema.KindChangeManifest || !e.c.Committed[h] {
				continue
			}
			if m, err := decode[schema.ChangeManifest](o); err == nil && m.IntentID == id {
				out = append(out, h)
				found = true
			}
		}
		if !found {
			return nil, apierr.New("not_found", "no committed manifest for intent %s", id)
		}
	}
	return out, nil
}

func (e *exporter) include(h string) {
	if e.set[h] {
		return
	}
	_, ok := e.c.Objects[h]
	switch {
	case !ok:
		e.missing[h] = true
		return
	case !e.c.Committed[h]:
		e.missing["pending:"+h] = true
		return
	}
	e.set[h] = true
	e.queue = append(e.queue, h)
	for _, r := range e.memberOf[h] {
		e.include(r)
	}
}

func (e *exporter) entity(id string) {
	objs := e.g.entities[id]
	if len(objs) == 0 {
		e.missing["entity:"+id] = true
		return
	}
	for _, o := range objs {
		e.include(o.Hash)
	}
	for _, h := range e.eventsOf[id] {
		e.include(h)
	}
	for _, h := range e.bindOf[id] {
		e.include(h)
	}
}

func (e *exporter) missingList() []string {
	out := make([]string, 0, len(e.missing))
	for h := range e.missing {
		out = append(out, h)
	}
	return out
}

func (e *exporter) run() {
	for len(e.queue) > 0 {
		h := e.queue[0]
		e.queue = e.queue[1:]
		o := e.c.Objects[h]
		if o.Envelope.Kind != schema.KindRootKey && o.Envelope.Kind != schema.KindOperationReceipt {
			e.entity(o.Envelope.Signer)
		}
		e.deps(o)
	}
}

func (e *exporter) deps(o envelope.Object) {
	switch o.Envelope.Kind {
	case schema.KindOperationReceipt:
		if r, err := decode[schema.OperationReceipt](o); err == nil {
			for _, m := range r.Members {
				e.include(m)
			}
		}
	case schema.KindChangeManifest:
		if m, err := decode[schema.ChangeManifest](o); err == nil {
			e.entity(m.AuthorSession)
			e.entity(m.WorkspaceID)
			e.entity(m.IntentID)
			for _, run := range e.runOf[m.WorkspaceID] {
				e.entity(run)
			}
		}
		for _, h := range e.evidence[o.Hash] {
			e.include(h)
		}
		for _, h := range e.attempts[o.Hash] {
			e.include(h)
		}
		for _, h := range e.selection[o.Hash] {
			e.include(h)
		}
	case schema.KindWorkspace:
		if w, err := decode[schema.Workspace](o); err == nil {
			e.entity(w.SessionID)
			e.entity(w.CapabilityID)
			e.entity(w.IntentID)
		}
	case schema.KindSession:
		if s, err := decode[schema.Session](o); err == nil {
			e.entity(s.CapabilityID)
		}
	case schema.KindCapability:
		if c, err := decode[schema.Capability](o); err == nil {
			e.entity(c.SessionID)
			if c.Issuer.Kind == "root" {
				e.entity(c.Issuer.Fingerprint)
			} else {
				e.entity(c.Issuer.CapabilityID)
			}
		}
	case schema.KindIntent:
		if in, err := decode[schema.Intent](o); err == nil {
			if in.ParentID != nil {
				e.entity(*in.ParentID)
			}
			for _, c := range in.Criteria {
				if c.Verification.DefinitionHash != "" {
					e.include(c.Verification.DefinitionHash)
				}
			}
		}
	case schema.KindWorkspaceBinding:
		if b, err := decode[schema.WorkspaceBinding](o); err == nil {
			e.entity(b.WorkspaceID)
			e.entity(b.SessionID)
			e.entity(b.CapabilityID)
		}
	case schema.KindLifecycleEvent:
		if ev, err := decode[schema.LifecycleEvent](o); err == nil {
			e.include(ev.Creation)
			if ev.ReplacementID != nil {
				e.entity(*ev.ReplacementID)
			}
		}
	case schema.KindEvidenceAttestation:
		if a, err := decode[schema.EvidenceAttestation](o); err == nil {
			e.include(a.Manifest)
			e.include(a.CheckDefinition)
			e.entity(a.VerifierSession)
		}
	case schema.KindSelectionDecision:
		if d, err := decode[schema.SelectionDecision](o); err == nil {
			e.include(d.Manifest)
			for _, h := range d.Evidence {
				e.include(h)
			}
			for _, h := range d.Competitors {
				e.include(h)
			}
			e.entity(d.IntentID)
			if d.Selector.Kind == "speculation_run" {
				e.entity(d.Selector.ID)
			}
		}
	case schema.KindSpeculationRun:
		if r, err := decode[schema.SpeculationRun](o); err == nil {
			e.entity(r.IntentID)
			for _, h := range r.Checks {
				e.include(h)
			}
			for _, a := range r.Attempts {
				e.entity(a.WorkspaceID)
				for _, h := range e.manifests[a.WorkspaceID] {
					e.include(h)
				}
			}
			for _, h := range e.attempts[r.ID] {
				e.include(h)
			}
			for _, h := range e.runSel[r.ID] {
				e.include(h)
			}
		}
	case schema.KindAttemptRecord:
		if a, err := decode[schema.AttemptRecord](o); err == nil {
			e.entity(a.RunID)
			e.entity(a.WorkspaceID)
			if a.Manifest != nil {
				e.include(*a.Manifest)
			}
			for _, h := range a.Evidence {
				e.include(h)
			}
		}
	}
}

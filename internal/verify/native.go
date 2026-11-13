package verify

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/atomicfile"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/oplog"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/store"
)

// Native repository layout under .ledger, as the record and workspace
// packages create it.
const (
	nativeObjects = "store/objects"
	nativeOpLogs  = "oplog"
)

// Version 2 bundle entry prefixes: store/<aa>/<62 hex> holds a store
// object file as the store writes it, and oplog/<workspace>.log a
// workspace's operation log.
const (
	storePrefix = "store/"
	opLogPrefix = "oplog/"
)

func storeName(rel string) (string, bool) {
	dir, file, ok := strings.Cut(rel, "/")
	if !ok || len(dir) != 2 {
		return "", false
	}
	return dir + file, isHex64(dir + file)
}

func opLogName(rel string) (string, bool) {
	w, ok := strings.CutSuffix(rel, ".log")
	return w, ok && workspaceID.MatchString(w)
}

var workspaceID = regexp.MustCompile(`^wsp_[0-9a-v]{26}$`)

// OpenNativeSource reads a native repository's records from
// .ledger/objects, with its content store and operation logs. It opens no
// SQLite index and needs no daemon.
func OpenNativeSource(root string) (*Source, error) {
	dir := filepath.Join(root, ".ledger")
	c, err := LoadObjectsDir(filepath.Join(dir, "objects"))
	if err != nil {
		return nil, err
	}
	sensitive := casePolicy(c)
	objects, err := openStore(filepath.Join(dir, nativeObjects), sensitive)
	if err != nil {
		return nil, err
	}
	return &Source{Name: ".ledger/objects", Corpus: c, Objects: objects, OpLogs: filepath.Join(dir, nativeOpLogs),
		Insensitive: !sensitive, Size: Size{Records: len(c.Objects)}}, nil
}

// openStore opens an existing store read-only in effect: it refuses to
// create the directory, which store.Open would.
func openStore(dir string, sensitive bool) (*store.Store, error) {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil, apierr.New("not_initialized", "%s is not a content store", dir)
	}
	return store.Open(dir, sensitive)
}

// repositoryID is the id of the corpus's committed repository record, or
// "" if it has none.
func repositoryID(c *Corpus) string {
	for _, h := range sortedKeys(c.Objects) {
		o := c.Objects[h]
		if o.Envelope.Kind != schema.KindRepository || !c.Committed[h] {
			continue
		}
		if r, err := decode[schema.Repository](o); err == nil {
			return r.ID
		}
	}
	return ""
}

// casePolicy is the committed repository record's case policy; a corpus
// without one is case-sensitive, as Phase 1 repositories are by default.
func casePolicy(c *Corpus) bool {
	for _, h := range sortedKeys(c.Objects) {
		o := c.Objects[h]
		if o.Envelope.Kind != schema.KindRepository || !c.Committed[h] {
			continue
		}
		if r, err := decode[schema.Repository](o); err == nil {
			return r.CasePolicy != "insensitive"
		}
	}
	return true
}

// nativeCheck is the outcome of the checks that bind a v2 manifest to its
// code, in place of Phase 1's commit trailer, tree and parent.
type nativeCheck struct {
	components map[string]string
	problems   []Problem
	missing    []string
}

func (n *nativeCheck) set(component, hash string, err error, missing string) {
	switch {
	case err == nil:
		n.components[component] = Verified
	case apierr.HasCode(err, "object_missing") || apierr.HasCode(err, "dependency_unavailable"):
		n.components[component] = Unavailable
		n.missing = append(n.missing, missing)
	default:
		e := apierr.As(err)
		n.components[component] = Invalid
		n.problems = append(n.problems, Problem{Hash: hash, Code: e.Code, Error: e.Message})
	}
}

// native checks manifest h: its tree is complete and intact in the store,
// its file list is exactly the change from its base, the operation log it
// pinned is unbroken up to a propose entry of that tree, and each symbol
// map it names describes that path's bytes in the tree.
func (src *Source) native(h string, m schema.ChangeManifest) nativeCheck {
	n := nativeCheck{components: map[string]string{}}
	if src.Objects == nil {
		for _, c := range []string{"tree", "files", "symbols"} {
			n.components[c] = Unavailable
		}
		n.missing = append(n.missing, "store:"+h)
	} else {
		_, err := src.Objects.Closure(m.Tree)
		n.set("tree", h, err, "object:"+m.Tree)
		n.set("files", h, src.files(m), "object:"+m.BaseTree)
		n.set("symbols", h, src.symbolMaps(m), "symbols:"+h)
	}
	n.set("oplog", h, src.opLog(m), "oplog:"+m.WorkspaceID)
	return n
}

func (src *Source) files(m schema.ChangeManifest) error {
	if _, err := src.Objects.Closure(m.BaseTree); err != nil {
		return err
	}
	changes, err := src.Objects.Diff(m.BaseTree, m.Tree)
	if err != nil {
		return err
	}
	got := make([]string, len(changes))
	for i, c := range changes {
		got[i] = c.Path
	}
	slices.Sort(got)
	if !slices.Equal(got, slices.Sorted(slices.Values(m.Files))) {
		return apierr.New("files_mismatch", "the manifest's files are not the change from %s to %s", m.BaseTree, m.Tree)
	}
	return nil
}

func (src *Source) symbolMaps(m schema.ChangeManifest) error {
	if len(m.SymbolMaps) == 0 {
		return nil
	}
	files, err := src.Objects.Files(m.Tree)
	if err != nil {
		return err
	}
	blobs := make(map[string]string, len(files))
	for _, f := range files {
		blobs[f.Path] = f.ID
	}
	for _, p := range sortedKeys(m.SymbolMaps) {
		id := m.SymbolMaps[p]
		kind, data, err := src.Objects.Get(id)
		if err != nil {
			return err
		}
		var sm struct {
			Path string `json:"path"`
			Blob string `json:"blob"`
		}
		if kind != store.KindSymbolMap || json.Unmarshal(data, &sm) != nil {
			return apierr.New("symbol_map_invalid", "object %s is not a symbol map", id)
		}
		if sm.Path != p || blobs[p] == "" || sm.Blob != blobs[p] {
			return apierr.New("symbol_map_invalid", "symbol map %s does not describe %s in the manifest tree", id, p)
		}
	}
	return nil
}

// readOpLog reads workspace w's log from dir. A live log may end in a
// line still being appended; only whole lines are entries.
func readOpLog(dir, w string) ([]oplog.Entry, error) {
	if dir == "" {
		return nil, apierr.New("dependency_unavailable", "no operation logs")
	}
	raw, err := os.ReadFile(oplog.Path(dir, w))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, apierr.New("dependency_unavailable", "no operation log for %s", w)
	}
	if err != nil {
		return nil, err
	}
	return oplog.Decode(bytes.NewReader(raw[:bytes.LastIndexByte(raw, '\n')+1]))
}

// opLog checks the operation log the manifest pinned. Entries after the
// pinned head are not covered by the manifest and are not read.
func (src *Source) opLog(m schema.ChangeManifest) error {
	entries, err := readOpLog(src.OpLogs, m.WorkspaceID)
	if err != nil {
		return err
	}
	pinned, err := oplog.Prefix(entries, m.OpHead)
	if err != nil {
		return err
	}
	for _, e := range pinned {
		if e.Workspace != m.WorkspaceID {
			return apierr.New("oplog_broken", "entry %d belongs to workspace %s", e.Seq, e.Workspace)
		}
	}
	head := pinned[len(pinned)-1]
	if head.Kind != oplog.Propose || head.Tree != m.Tree || head.Base != m.BaseTree {
		return apierr.New("oplog_mismatch", "the pinned entry is not a proposal of the manifest's tree and base")
	}
	return nil
}

// ExportNative writes a version 2 bundle from a native repository: the
// selected records, every store object their manifests reach (tree, base
// tree and symbol maps), and each workspace's operation log up to the last
// head an exported manifest pinned. Later log entries stay private. Like
// Export it reads no SQLite and the bundle appears only once durable.
func ExportNative(root string, opts ExportOptions) (*ExportReport, error) {
	dir := filepath.Join(root, ".ledger")
	sel, err := selectRecords(filepath.Join(dir, "objects"), &opts)
	if err != nil {
		return nil, err
	}
	c, missing := sel.corpus, sel.missing
	objects, err := openStore(filepath.Join(dir, nativeObjects), casePolicy(c))
	if err != nil {
		return nil, err
	}
	reached := map[string]bool{}
	heads := map[string][]string{}
	for _, h := range sel.manifests {
		o := c.Objects[h]
		if o.Envelope.SchemaVersion != 2 {
			missing = append(missing, "git:"+h)
			continue
		}
		m, err := decode[schema.ChangeManifest](o)
		if err != nil {
			return nil, err
		}
		for _, id := range append([]string{m.Tree, m.BaseTree}, valuesOf(m.SymbolMaps)...) {
			ids, err := objects.Closure(id)
			if apierr.HasCode(err, "object_missing") || apierr.HasCode(err, "object_corrupt") {
				missing = append(missing, "object:"+id)
				continue
			}
			if err != nil {
				return nil, err
			}
			for _, id := range ids {
				reached[id] = true
			}
		}
		heads[m.WorkspaceID] = append(heads[m.WorkspaceID], m.OpHead)
	}
	logs := map[string][]oplog.Entry{}
	for _, w := range sortedKeys(heads) {
		entries, err := readOpLog(filepath.Join(dir, nativeOpLogs), w)
		if err != nil && !apierr.HasCode(err, "dependency_unavailable") && !apierr.HasCode(err, "oplog_broken") {
			return nil, err
		}
		for _, head := range heads[w] {
			pinned, perr := oplog.Prefix(entries, head)
			if err != nil || perr != nil {
				missing = append(missing, "oplog:"+w)
				delete(logs, w)
				break
			}
			if len(pinned) > len(logs[w]) {
				logs[w] = pinned
			}
		}
	}
	slices.Sort(missing)
	missing = slices.Compact(missing)
	complete := len(missing) == 0
	if !complete && !opts.AllowPartial {
		return nil, apierr.New("export_incomplete", "%d dependencies are missing; export would not verify as complete", len(missing)).With("missing", missing)
	}
	inv := Inventory{
		BundleVersion: schema.BundleVersionNative, Generator: Generator, CreatedAt: schema.Timestamp(opts.Now()),
		CaseSensitive: casePolicy(c), Complete: complete, Missing: nilToEmpty(missing),
		Git: InvGit{Refs: []string{}, Commits: []InvCommit{}}, Store: []InvStoreObject{}, OpLogs: []InvOpLog{},
	}
	var parts []part
	var storeBytes int64
	for _, id := range sortedKeys(reached) {
		kind, data, err := objects.Get(id)
		if err != nil {
			return nil, err
		}
		size := int64(len(kind) + 1 + len(data))
		inv.Store = append(inv.Store, InvStoreObject{ID: id, Kind: kind, Size: size})
		storeBytes += size
		parts = append(parts, part{storePrefix + id[:2] + "/" + id[2:], func() ([]byte, error) {
			kind, data, err := objects.Get(id)
			return append([]byte(kind+"\n"), data...), err
		}})
	}
	for _, w := range sortedKeys(logs) {
		var raw []byte
		for _, e := range logs[w] {
			line, err := canon.Marshal(e)
			if err != nil {
				return nil, err
			}
			raw = append(append(raw, line...), '\n')
		}
		head, err := oplog.Hash(logs[w][len(logs[w])-1])
		if err != nil {
			return nil, err
		}
		inv.OpLogs = append(inv.OpLogs, InvOpLog{Workspace: w, Head: head, Entries: len(logs[w]), Size: int64(len(raw))})
		parts = append(parts, part{opLogPrefix + w + ".log", func() ([]byte, error) { return raw, nil }})
	}
	rep, err := writeBundle(opts, inv, c, sortedKeys(sel.set), parts)
	if err != nil {
		return nil, err
	}
	rep.Missing, rep.Manifests = missing, sel.manifests
	rep.Size.StoreBytes, rep.Size.StoreObjects = storeBytes, len(inv.Store)
	return rep, nil
}

// ImportNative stages and verifies a version 2 bundle and adds it to the
// native repository at root: store objects first, then operation logs,
// then records, so no record is published before what it names. Nothing
// is added unless verification finds the bundle valid and complete. An
// operation log is only ever created; a local log that does not already
// start with the bundled entries is reported as oplog_conflict and kept.
// Importing again changes nothing.
func ImportNative(ctx context.Context, bundle string, dst Publisher, root string, opts ImportOptions) (*ImportReport, error) {
	if opts.Limits == (Limits{}) {
		opts.Limits = DefaultLimits()
	}
	dir := filepath.Join(root, ".ledger")
	staging := filepath.Join(dir, "runtime", "import")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return nil, err
	}
	stage := filepath.Join(staging, schema.NewID(schema.PrefixOperation))
	if err := os.Mkdir(stage, 0o700); err != nil {
		return nil, err
	}
	src, err := openStaged(ctx, bundle, stage, opts.Limits)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	if src.BundleVersion != schema.BundleVersionNative {
		return nil, apierr.New("unsupported_bundle_version", "a native repository imports only version %d bundles", schema.BundleVersionNative)
	}
	if err := importFault(opts, StageImportStaged); err != nil {
		return nil, err
	}
	rep, err := Verify(ctx, src, Options{Trusted: opts.Trusted})
	if err != nil {
		return nil, err
	}
	switch {
	case rep.Integrity == Invalid:
		return nil, apierr.New("bundle_invalid", "the bundle failed verification; nothing was imported").With("report", rep)
	case !rep.Complete || len(rep.Missing) > 0 || len(rep.Pending) > 0:
		return nil, apierr.New("bundle_incomplete", "the bundle is missing dependencies; nothing was imported").With("report", rep)
	}
	local, err := LoadObjectsDir(filepath.Join(dir, "objects"))
	if err != nil {
		return nil, err
	}
	if theirs, ours := repositoryID(src.Corpus), repositoryID(local); theirs == "" || theirs != ours {
		return nil, apierr.New("repository_mismatch", "the bundle is from repository %q, not this repository %q; nothing was imported", theirs, ours)
	}
	out := &ImportReport{Verification: rep, Refs: []RefResult{}, Records: len(src.Corpus.Objects), OpLogs: []OpLogResult{}}
	if err := importFault(opts, StageImportStore); err != nil {
		return nil, err
	}
	objects, err := openStore(filepath.Join(dir, nativeObjects), !src.Insensitive && src.Inventory.CaseSensitive)
	if err != nil {
		return nil, err
	}
	b := objects.Batch()
	for _, id := range sortedKeys(src.Staged.Objects) {
		kind, data, err := src.Objects.Get(id)
		if err != nil {
			return nil, err
		}
		if _, err := b.Put(kind, data); err != nil {
			return nil, err
		}
	}
	if err := b.Commit(); err != nil {
		return nil, err
	}
	out.Objects = b.Written()
	if err := importFault(opts, StageImportOpLogs); err != nil {
		return nil, err
	}
	for _, w := range sortedKeys(src.Staged.OpLogs) {
		r, err := installOpLog(src.OpLogs, filepath.Join(dir, nativeOpLogs), w)
		if err != nil {
			return nil, err
		}
		out.OpLogs = append(out.OpLogs, r)
	}
	if err := importFault(opts, StageImportRecords); err != nil {
		return nil, err
	}
	objs := make([]envelope.Object, 0, len(src.Corpus.Objects))
	for _, h := range sortedKeys(src.Corpus.Objects) {
		objs = append(objs, src.Corpus.Objects[h])
	}
	res, err := dst.Publish(ctx, objs)
	if err != nil {
		return nil, err
	}
	out.Publish = res
	out.AlreadyPresent = res.Written == 0 && res.Indexed == 0 && out.Objects == 0 &&
		!slices.ContainsFunc(out.OpLogs, func(r OpLogResult) bool { return r.Status == "created" })
	return out, nil
}

func installOpLog(from, to, w string) (OpLogResult, error) {
	bundled, err := readOpLog(from, w)
	if err != nil {
		return OpLogResult{}, err
	}
	r := OpLogResult{Workspace: w}
	if r.Head, err = oplog.Hash(bundled[len(bundled)-1]); err != nil {
		return r, err
	}
	local, err := readOpLog(to, w)
	switch {
	case apierr.HasCode(err, "dependency_unavailable"):
		raw, err := os.ReadFile(oplog.Path(from, w))
		if err != nil {
			return r, err
		}
		if err := os.MkdirAll(to, 0o700); err != nil {
			return r, err
		}
		r.Status = "created"
		return r, atomicfile.Write(oplog.Path(to, w), raw, 0o600)
	case err != nil && !apierr.HasCode(err, "oplog_broken"):
		return r, err
	case err == nil:
		if _, perr := oplog.Prefix(local, r.Head); perr == nil {
			r.Status = "present"
			return r, nil
		}
	}
	r.Status = "oplog_conflict"
	return r, nil
}

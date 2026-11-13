package verify_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/CoachCoe/Ledger/internal/api"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/core"
	"github.com/CoachCoe/Ledger/internal/cow"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/store"
	"github.com/CoachCoe/Ledger/internal/verify"
)

var errCrash = errors.New("simulated crash")

// site is one initialized native repository with its own root key.
type site struct {
	t     *testing.T
	dir   string
	store *record.Store
	keys  identity.LocalKeys
	root  identity.Signer
	svc   *core.Service
	// pristine is a copy of the repository as init left it, for earlier
	// copies that import what this one later exports.
	pristine string
}

// newSite makes a native repository holding a.ts, with its root key and
// repository records published, as `ledger init` leaves it.
func newSite(t *testing.T) *site {
	return newSiteWith(t, record.Options{}, identity.LocalKeys{})
}

// newSiteWith reuses keys (when its Dir is set) so several repositories
// share one root.
func newSiteWith(t *testing.T, opts record.Options, keys identity.LocalKeys) *site {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.ts"), []byte("export const x=1;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(dir, record.DirName)
	if err := os.Mkdir(ledger, 0o700); err != nil {
		t.Fatal(err)
	}
	objects, err := store.Open(filepath.Join(ledger, "store", "objects"), true)
	if err != nil {
		t.Fatal(err)
	}
	m, err := cow.New(cow.Config{Store: objects, Runtime: filepath.Join(ledger, "runtime"), Logs: filepath.Join(ledger, "oplog"), CaseSensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	tree, err := m.Import(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ledger, "config.json"), []byte(`{"case_sensitive":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if keys.Dir == "" {
		keys = identity.LocalKeys{Dir: filepath.Join(t.TempDir(), "keys")}
	}
	s := &site{t: t, dir: dir, keys: keys}
	root, _, err := s.keys.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	s.root = root
	s.open(opts)
	rk, err := envelope.SealVersion(2, schema.KindRootKey, schema.RootKey{PublicKey: identity.EncodePublicKey(root.PublicKey()), CreatedAt: "2026-11-05T12:00:00Z"}, root)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := envelope.SealVersion(2, schema.KindRepository, schema.Repository{ID: schema.NewID(schema.PrefixRepository), HashAlgorithm: "sha256",
		CasePolicy: "sensitive", Analyzers: []string{}, Tree: tree, CreatedAt: "2026-11-05T12:00:00Z"}, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.Do(ctx, record.Op{ID: "op_" + root.ID()[:26], Name: "test.init", Digest: canon.Hash([]byte("init")), Signer: root}, func(*index.Index) (any, []envelope.Object, error) {
		return map[string]any{}, []envelope.Object{rk, repo}, nil
	}); err != nil {
		t.Fatal(err)
	}
	s.pristine = filepath.Join(t.TempDir(), "pristine")
	copyTree(t, dir, s.pristine)
	return s
}

// earlier opens a fresh copy of the repository as init left it: a
// teammate's copy from before any of this site's work.
func (s *site) earlier() *site {
	s.t.Helper()
	return s.earlierWith(record.Options{})
}

func (s *site) earlierWith(opts record.Options) *site {
	s.t.Helper()
	dir := filepath.Join(s.t.TempDir(), "earlier")
	copyTree(s.t, s.pristine, dir)
	e := &site{t: s.t, dir: dir, keys: s.keys, root: s.root, pristine: s.pristine}
	e.open(opts)
	if _, err := e.store.Recover(context.Background()); err != nil {
		s.t.Fatal(err)
	}
	return e
}

// copyTree copies a repository without its runtime state or index.
func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		switch filepath.ToSlash(rel) {
		case ".ledger/runtime", ".ledger/lock", ".ledger/index.db", ".ledger/index.db-wal", ".ledger/index.db-shm":
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// open (re)opens the store, closing any previous one: a daemon restart.
func (s *site) open(opts record.Options) {
	s.t.Helper()
	if s.store != nil {
		s.store.Close()
	}
	store, err := record.OpenNative(context.Background(), s.dir, opts)
	if err != nil {
		s.t.Fatal(err)
	}
	s.store = store
	s.t.Cleanup(func() { store.Close() })
}

// source reads the repository's own records, store and logs for verification.
func (s *site) source() *verify.Source {
	s.t.Helper()
	src, err := verify.OpenNativeSource(s.dir)
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { src.Close() })
	return src
}

func (s *site) service() *core.Service {
	s.t.Helper()
	if s.svc == nil {
		svc, err := core.New(context.Background(), s.store, s.keys)
		if err != nil {
			s.t.Fatal(err)
		}
		s.svc = svc
	}
	return s.svc
}

func (s *site) call(op, sid string, input any) map[string]any {
	s.t.Helper()
	raw, _ := canon.Marshal(input)
	r := api.Request{Version: 2, Operation: op, SessionID: sid, OperationID: schema.NewID(schema.PrefixOperation), Input: raw}
	result, err := s.service().Handle(context.Background(), r)
	if err != nil {
		s.t.Fatalf("%s: %v", op, err)
	}
	b, err := canon.Marshal(result)
	if err != nil {
		s.t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		s.t.Fatal(err)
	}
	if inner, ok := v["result"].(map[string]any); ok {
		inner["_receipt"] = v["receipt"]
		return inner
	}
	return v
}

// chain is a full provenance chain: root, scoped session, intent, workspace,
// proposal, independent evidence, a human judgment and a selection.
type chain struct {
	*site
	sid, cap, wid, wdir, intent                string
	manifest, tree, evidence, judgment, choice string
}

func newChain(t *testing.T, command string) *chain {
	t.Helper()
	s := newSite(t)
	def := schema.CheckDefinition{Name: "unit", Category: "mechanical", Command: command, WorkingDirectory: ".", EnvAllowlist: []string{}, TimeoutSeconds: 10, OutputLimitBytes: 1024}
	raw, _ := canon.Marshal([]schema.CheckDefinition{def})
	if err := os.WriteFile(filepath.Join(s.store.Dir, "checks.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	c := &chain{site: s}
	ses := s.call("session.create", "", map[string]any{"agent_name": "author", "allow": []string{"a.ts"}})
	c.sid, c.cap = ses["session_id"].(string), ses["capability_id"].(string)
	in := s.call("intent.create", "", map[string]any{"description": "portable", "criteria": []any{
		map[string]any{"id": "tests", "statement": "unit passes", "verification": map[string]any{"kind": "check", "check": "unit"}},
		map[string]any{"id": "review", "statement": "looks good", "verification": map[string]any{"kind": "judgment", "reviewer": "human"}},
	}})
	c.intent = in["intent_id"].(string)
	ws := s.call("workspace.fork", c.sid, map[string]any{"intent_id": c.intent, "capability_id": c.cap})
	c.wid, c.wdir = ws["workspace_id"].(string), ws["path"].(string)
	c.propose("export const x=2;\n")
	bound := s.call("evidence.resolve", "", map[string]any{"manifest": c.manifest})
	defHash := bound["checks"].([]any)[0].(map[string]any)["definition_hash"].(string)
	run := s.call("evidence.run", "", map[string]any{"manifest": c.manifest, "check_definition": defHash})
	c.evidence = run["attestation"].(string)
	j := s.call("evidence.judge", "", map[string]any{"manifest": c.manifest, "criterion_id": "review", "result": "pass"})
	c.judgment = j["attestation"].(string)
	sel := s.call("selection.record", "", map[string]any{"manifest": c.manifest, "evidence": []string{c.evidence, c.judgment}})
	c.choice = sel["selection"].(string)
	return c
}

func (c *chain) propose(content string) {
	c.t.Helper()
	if err := os.WriteFile(filepath.Join(c.wdir, "a.ts"), []byte(content), 0o600); err != nil {
		c.t.Fatal(err)
	}
	p := c.call("workspace.propose", c.sid, map[string]any{"workspace_id": c.wid})
	c.manifest, c.tree = p["manifest"].(string), p["tree"].(string)
}

// export writes a bundle of the whole repository (or the given selection).
func (s *site) export(opts verify.ExportOptions) (*verify.ExportReport, error) {
	s.t.Helper()
	if opts.Out == "" {
		opts.Out = filepath.Join(s.t.TempDir(), "out.ledger")
	}
	return verify.ExportNative(s.dir, opts)
}

func (s *site) mustExport() string {
	s.t.Helper()
	rep, err := s.export(verify.ExportOptions{})
	if err != nil {
		s.t.Fatal(err)
	}
	if !rep.Complete {
		s.t.Fatalf("export is incomplete: %v", rep.Missing)
	}
	return rep.Out
}

func verifyBundle(t *testing.T, path string, trust ...string) *verify.Report {
	t.Helper()
	rep, err := tryVerify(path, trust...)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func tryVerify(path string, trust ...string) (*verify.Report, error) {
	ctx := context.Background()
	src, err := verify.OpenBundleSource(ctx, path, verify.DefaultLimits())
	if err != nil {
		return nil, err
	}
	defer src.Close()
	list := identity.NewTrustList()
	if err := list.Merge(trust); err != nil {
		return nil, err
	}
	return verify.Verify(ctx, src, verify.Options{Trusted: list})
}

// parts is a bundle taken apart so tests can tamper with any piece.
type parts struct {
	Inv     map[string]any
	Records map[string][]byte // record hash -> bytes
	Files   map[string][]byte // store object and operation log entries by name
}

func readParts(t *testing.T, path string) *parts {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p := &parts{Records: map[string][]byte{}, Files: map[string][]byte{}}
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case hdr.Name == verify.InventoryName:
			if err := json.Unmarshal(data, &p.Inv); err != nil {
				t.Fatal(err)
			}
		case strings.HasPrefix(hdr.Name, "records/"):
			p.Records[strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(hdr.Name, "records/"), ".json"), "/", "")] = data
		default:
			p.Files[hdr.Name] = data
		}
	}
	return p
}

// write assembles a bundle. With refresh, the inventory is recomputed so it
// is consistent with whatever the test changed; without it the inventory is
// written as it was read.
func (p *parts) write(t *testing.T, refresh bool) string {
	t.Helper()
	if refresh {
		objs := []any{}
		for _, h := range sortedHashes(p.Records) {
			kind := "unknown"
			if o, err := envelope.Parse(p.Records[h]); err == nil {
				kind = string(o.Envelope.Kind)
			}
			objs = append(objs, map[string]any{"hash": h, "kind": kind, "size": len(p.Records[h])})
		}
		p.Inv["objects"] = objs
		if raw, ok := p.Files[verify.GitName]; ok {
			git := p.Inv["git"].(map[string]any)
			git["file"], git["size"], git["sha256"] = verify.GitName, len(raw), sha(raw)
		}
	}
	inv, err := canon.Marshal(p.Inv)
	if err != nil {
		t.Fatal(err)
	}
	return writeTar(t, []tarEntry{{Name: verify.InventoryName, Data: inv}}, p.entries())
}

func (p *parts) entries() []tarEntry {
	var out []tarEntry
	for _, h := range sortedHashes(p.Records) {
		out = append(out, tarEntry{Name: "records/" + h[:2] + "/" + h[2:] + ".json", Data: p.Records[h]})
	}
	for _, name := range slices.Sorted(maps.Keys(p.Files)) {
		out = append(out, tarEntry{Name: name, Data: p.Files[name]})
	}
	return out
}

// byKind returns the hashes of records of one kind.
func (p *parts) byKind(t *testing.T, kind schema.Kind) []string {
	t.Helper()
	var out []string
	for _, h := range sortedHashes(p.Records) {
		if o, err := envelope.Parse(p.Records[h]); err == nil && o.Envelope.Kind == kind {
			out = append(out, h)
		}
	}
	return out
}

// forge replaces record h with the result of edit applied to its envelope,
// re-hashed and re-named so only the signature or group no longer fit.
func (p *parts) forge(t *testing.T, h string, edit func(env map[string]any)) {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(p.Records[h], &env); err != nil {
		t.Fatal(err)
	}
	edit(env)
	raw, err := canon.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	delete(p.Records, h)
	nh := canon.Hash(raw)
	p.Records[nh] = raw
}

func (p *parts) payload(t *testing.T, h string) map[string]any {
	t.Helper()
	var env struct {
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(p.Records[h], &env); err != nil {
		t.Fatal(err)
	}
	return env.Payload
}

type tarEntry struct {
	Name     string
	Data     []byte
	Type     byte
	Linkname string
	Size     int64 // declared size when different from len(Data)
}

func writeTar(t *testing.T, lists ...[]tarEntry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x.ledger")
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, list := range lists {
		for _, e := range list {
			typ := e.Type
			if typ == 0 {
				typ = tar.TypeReg
			}
			size := int64(len(e.Data))
			if e.Size != 0 {
				size = e.Size
			}
			if typ != tar.TypeReg {
				size = 0
			}
			if err := tw.WriteHeader(&tar.Header{Name: e.Name, Mode: 0o600, Size: size, Typeflag: typ, Linkname: e.Linkname, ModTime: time.Unix(0, 0), Format: tar.FormatUSTAR}); err != nil {
				t.Fatal(err)
			}
			if typ == tar.TypeReg {
				data := e.Data
				if int64(len(data)) > size {
					data = data[:size]
				}
				if _, err := tw.Write(data); err != nil && !errors.Is(err, tar.ErrWriteTooLong) {
					t.Fatal(err)
				}
			}
		}
	}
	_ = tw.Close() // a deliberately short entry cannot be closed cleanly
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

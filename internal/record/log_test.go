package record

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/schema"
)

func newNativeDir(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, DirName), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func openNative(t testing.TB, dir string, opts Options) *Store {
	t.Helper()
	if opts.Now == nil {
		opts.Now = func() time.Time { return fixedNow }
	}
	s, err := OpenNative(ctx, dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newNative(t testing.TB) (*Store, string) {
	t.Helper()
	dir := newNativeDir(t)
	s := openNative(t, dir, Options{})
	publishRootKey(t, s)
	return s, dir
}

// checkLog fails unless the log lists exactly the indexed receipts, once
// each.
func checkLog(t testing.TB, s *Store) []LogEntry {
	t.Helper()
	entries, err := s.Log()
	if err != nil {
		t.Fatal(err)
	}
	var logged []string
	for _, e := range entries {
		logged = append(logged, e.Receipt)
	}
	indexed, err := s.Index.Receipts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(logged)
	if !slices.Equal(logged, indexed) {
		t.Fatalf("log lists %d receipts, index %d", len(logged), len(indexed))
	}
	return entries
}

func TestNativeStoreLogsEachGroupInOrder(t *testing.T) {
	s, dir := newNative(t)
	var receipts []string
	for range 3 {
		res, err := s.Do(ctx, op("create"), members(intentRecord(t, schema.NewID(schema.PrefixIntent))))
		if err != nil {
			t.Fatal(err)
		}
		receipts = append(receipts, res.Receipt)
	}
	entries := checkLog(t, s)
	if len(entries) != 4 {
		t.Fatalf("%d log entries, want 4", len(entries))
	}
	for i, r := range receipts {
		if entries[i+1].Receipt != r {
			t.Fatalf("entry %d is %s, want the receipt of operation %d", i+2, entries[i+1].Receipt, i+1)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("native store touched Git: %v", err)
	}
	if _, err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	before := digest(t, s)
	if r, err := s.Reindex(ctx); err != nil || r.Logged != 0 {
		t.Fatalf("reindex logged %+v, %v", r, err)
	}
	if digest(t, s) != before || len(checkLog(t, s)) != 4 {
		t.Fatal("reindex changed the native store")
	}
}

func TestNativeCrashAtEveryStageConverges(t *testing.T) {
	for _, stage := range []string{StageJournal, StageObjects, StageReceipt, StageLog, StageIndex, StageDone} {
		t.Run(stage, func(t *testing.T) {
			_, dir := newNative(t)
			crash := errors.New("crash")
			crashing := openNative(t, dir, Options{Fault: func(st string) error {
				if st == stage {
					return crash
				}
				return nil
			}})
			id := schema.NewID(schema.PrefixIntent)
			o := op("create")
			if _, err := crashing.Do(ctx, o, members(intentRecord(t, id))); !errors.Is(err, crash) {
				t.Fatalf("Do = %v, want crash", err)
			}
			restarted := openNative(t, dir, Options{})
			if _, err := restarted.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			committed := subject(t, restarted, id) != nil
			if committed == (stage == StageJournal) {
				t.Fatalf("after crash at %s committed = %v", stage, committed)
			}
			res, err := restarted.Do(ctx, o, members(intentRecord(t, id)))
			if err != nil {
				t.Fatal(err)
			}
			if res.Replayed != committed {
				t.Fatalf("retry Replayed = %v, want %v", res.Replayed, committed)
			}
			if n := len(checkLog(t, restarted)); n != 2 {
				t.Fatalf("%d log entries, want 2", n)
			}
		})
	}
}

func TestNativeMultiProcessWriters(t *testing.T) {
	const procs, opsEach = 6, 30
	s, dir := newNative(t)
	var wg sync.WaitGroup
	for p := range procs {
		cmd := exec.Command(os.Args[0], "-test.run=^TestNativeWriterProcess$")
		cmd.Env = append(os.Environ(), "LEDGER_NATIVE_WRITER="+dir, "LEDGER_WRITER_OPS="+strconv.Itoa(opsEach))
		wg.Go(func() {
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("writer %d: %v\n%s", p, err, out)
			}
		})
	}
	wg.Wait()
	if n := len(checkLog(t, s)); n != 1+procs*opsEach {
		t.Fatalf("%d log entries, want %d", n, 1+procs*opsEach)
	}
	if r, err := s.Recover(ctx); err != nil || r.Logged != 0 || r.Indexed != 0 || len(r.Missing) != 0 {
		t.Fatalf("writers left work for recovery: %+v, %v", r, err)
	}
}

func TestNativeWriterProcess(t *testing.T) {
	dir := os.Getenv("LEDGER_NATIVE_WRITER")
	if dir == "" {
		t.Skip("helper process for TestNativeMultiProcessWriters")
	}
	n, _ := strconv.Atoi(os.Getenv("LEDGER_WRITER_OPS"))
	s := openNative(t, dir, Options{})
	for range n {
		if _, err := s.Do(ctx, op("write"), members(intentRecord(t, schema.NewID(schema.PrefixIntent)))); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNativeLogTornTailIsCut(t *testing.T) {
	s, dir := newNative(t)
	path := filepath.Join(dir, DirName, LogName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"previous":"ab`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	s.Close()
	again := openNative(t, dir, Options{})
	if _, err := again.Do(ctx, op("after"), members(intentRecord(t, schema.NewID(schema.PrefixIntent)))); err != nil {
		t.Fatal(err)
	}
	if n := len(checkLog(t, again)); n != 2 {
		t.Fatalf("%d log entries, want 2", n)
	}
}

func TestNativeLogEditBreaksTheChain(t *testing.T) {
	s, dir := newNative(t)
	if _, err := s.Do(ctx, op("create"), members(intentRecord(t, schema.NewID(schema.PrefixIntent)))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, DirName, LogName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := splitLines(raw)
	// Drop the first entry: the second still names it as previous.
	if err := os.WriteFile(path, []byte(lines[1]+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Log(); !apierr.HasCode(err, "record_log_broken") {
		t.Fatalf("open process accepted a shrunken log: %v", err)
	}
	if _, err := OpenNative(ctx, dir, Options{}); !apierr.HasCode(err, "record_log_broken") {
		t.Fatalf("OpenNative = %v, want record_log_broken", err)
	}
}

func splitLines(raw []byte) []string {
	var out []string
	for len(raw) > 0 {
		i := slices.Index(raw, '\n')
		out, raw = append(out, string(raw[:i])), raw[i+1:]
	}
	return out
}

func TestNativeLostReceiptIsReportedNotForgotten(t *testing.T) {
	s, _ := newNative(t)
	res, err := s.Do(ctx, op("create"), members(intentRecord(t, schema.NewID(schema.PrefixIntent))))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(s.ObjectPath(res.Receipt)); err != nil {
		t.Fatal(err)
	}
	r, err := s.Reindex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Missing) != 1 || r.Missing[0].Hash != res.Receipt || r.Missing[0].Code != "record_missing" {
		t.Fatalf("missing = %+v", r.Missing)
	}
	entries, err := s.Log()
	if err != nil || len(entries) != 2 || entries[1].Receipt != res.Receipt {
		t.Fatalf("log after loss: %+v, %v", entries, err)
	}
}

func TestNativeImportAppendsToLog(t *testing.T) {
	s, _ := newNative(t)
	g := externalGroup(t, intentRecord(t, schema.NewID(schema.PrefixIntent)))
	r, err := s.ImportRecords(ctx, append([]envelope.Object{g.Receipt}, g.Members...))
	if err != nil {
		t.Fatal(err)
	}
	if r.Indexed != 1 || r.Logged != 1 {
		t.Fatalf("import report %+v", r)
	}
	entries := checkLog(t, s)
	if entries[len(entries)-1].Receipt != g.Receipt.Hash {
		t.Fatal("imported group is not the log head")
	}
	if r, err := s.ImportRecords(ctx, append([]envelope.Object{g.Receipt}, g.Members...)); err != nil || r.Logged != 0 {
		t.Fatalf("second import %+v, %v", r, err)
	}
}

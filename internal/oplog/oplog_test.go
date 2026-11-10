package oplog

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/CoachCoe/Ledger/internal/apierr"
)

const wsp = "wsp_00000000000000000000000000"

func tree(c byte) string { return strings.Repeat(string(c), 64) }

func appendAll(t *testing.T, l *Log, entries ...Entry) []string {
	t.Helper()
	var hashes []string
	for _, e := range entries {
		_, h, ok, err := l.Append(e, e.Kind == Propose)
		if err != nil || !ok {
			t.Fatalf("append %+v: %v %v", e, ok, err)
		}
		hashes = append(hashes, h)
	}
	return hashes
}

func TestAppendChainsAndReloads(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir, wsp)
	if err != nil {
		t.Fatal(err)
	}
	hashes := appendAll(t, l,
		Entry{Kind: Snapshot, Tree: tree('a'), Base: tree('0'), Trigger: "watch"},
		Entry{Kind: Snapshot, Tree: tree('b'), Base: tree('0'), Trigger: "watch"},
	)
	if _, h, ok, err := l.Append(Entry{Kind: Snapshot, Tree: tree('b'), Base: tree('0'), Trigger: "poll"}, false); ok || err != nil || h != hashes[1] {
		t.Fatalf("unchanged tree appended: %v %v", ok, err)
	}
	src := hashes[0]
	hashes = append(hashes, appendAll(t, l,
		Entry{Kind: Restore, Tree: tree('a'), Base: tree('0'), Source: &src, Trigger: "call"},
		Entry{Kind: Propose, Tree: tree('a'), Base: tree('0'), Trigger: "call"},
	)...)
	again, err := Open(dir, wsp)
	if err != nil {
		t.Fatal(err)
	}
	_, head, _ := again.Head()
	if head != hashes[3] || len(again.Entries()) != 4 {
		t.Fatalf("reload head %s, %d entries", head, len(again.Entries()))
	}
	if err := Verify(again.Entries(), head); err != nil {
		t.Fatal(err)
	}
	if e, ok := again.Get(hashes[0]); !ok || e.Tree != tree('a') {
		t.Fatal("entry lookup by hash failed")
	}
	if _, _, _, err := l.Append(Entry{Kind: Restore, Tree: tree('a'), Base: tree('0')}, false); !apierr.HasCode(err, "invalid_operation") {
		t.Fatalf("restore without source: %v", err)
	}
}

// P2-AC-013, the log half: the manifest half is P2-M2.
func TestHandEditsBreakTheChain(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir, wsp)
	if err != nil {
		t.Fatal(err)
	}
	hashes := appendAll(t, l,
		Entry{Kind: Snapshot, Tree: tree('a'), Base: tree('0'), Trigger: "watch"},
		Entry{Kind: Snapshot, Tree: tree('b'), Base: tree('0'), Trigger: "watch"},
		Entry{Kind: Propose, Tree: tree('b'), Base: tree('0'), Trigger: "call"},
	)
	head := hashes[2]
	raw, err := os.ReadFile(Path(dir, wsp))
	if err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func([]byte) []byte{
		"change a middle tree": func(b []byte) []byte { return bytes.Replace(b, []byte(tree('a')), []byte(tree('c')), 1) },
		"drop the first entry": func(b []byte) []byte { return b[bytes.IndexByte(b, '\n')+1:] },
		"change the last entry": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"trigger":"call"`), []byte(`"trigger":"poll"`), 1)
		},
		"reformat a line": func(b []byte) []byte { return bytes.Replace(b, []byte(`{"at"`), []byte(`{ "at"`), 1) },
	} {
		t.Run(name, func(t *testing.T) {
			edited := edit(bytes.Clone(raw))
			if bytes.Equal(edited, raw) {
				t.Fatal("edit changed nothing")
			}
			entries, err := Decode(bytes.NewReader(edited))
			if err == nil {
				err = Verify(entries, head)
			}
			if !apierr.HasCode(err, "oplog_broken") && !apierr.HasCode(err, "oplog_head_mismatch") {
				t.Fatalf("edited log verified: %v", err)
			}
		})
	}
}

func TestTornTailIsCutOff(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir, wsp)
	if err != nil {
		t.Fatal(err)
	}
	appendAll(t, l, Entry{Kind: Snapshot, Tree: tree('a'), Base: tree('0'), Trigger: "watch"})
	f, err := os.OpenFile(Path(dir, wsp), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"at":"2026`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	l, err = Open(dir, wsp)
	if err != nil {
		t.Fatal(err)
	}
	appendAll(t, l, Entry{Kind: Snapshot, Tree: tree('b'), Base: tree('0'), Trigger: "watch"})
	if l, err = Open(dir, wsp); err != nil || len(l.Entries()) != 2 {
		t.Fatalf("reopen after torn tail: %v", err)
	}
}

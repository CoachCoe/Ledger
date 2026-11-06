package verify

import (
	"slices"
	"testing"

	"github.com/CoachCoe/Ledger/internal/gitx"
	"github.com/CoachCoe/Ledger/internal/schema"
)

func TestAC057_ThreeConditionsReportedSeparately(t *testing.T) {
	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const other = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	m := schema.ChangeManifest{Tree: "1111111111111111111111111111111111111111", BaseCommit: "2222222222222222222222222222222222222222"}
	good := gitx.Commit{SHA: "3333333333333333333333333333333333333333", Tree: m.Tree, Parents: []string{m.BaseCommit}, Message: "Ledger: x\n\nLedger-Change: chg_x\nLedger-Manifest: " + hash + "\n"}
	if b := CheckBinding(good, hash, m); !b.OK() || len(b.Detail) != 0 {
		t.Fatalf("good binding: %+v", b)
	}
	tests := []struct {
		name                  string
		mutate                func(*gitx.Commit)
		trailer, tree, parent string
	}{
		{"trailer names another manifest", func(c *gitx.Commit) { c.Message = "x\n\nLedger-Manifest: " + other + "\n" }, "mismatch", "ok", "ok"},
		{"trailer missing", func(c *gitx.Commit) { c.Message = "x\n" }, "mismatch", "ok", "ok"},
		{"trailer only in the body paragraph", func(c *gitx.Commit) { c.Message = "Ledger-Manifest: " + hash + "\n\nfooter\n" }, "mismatch", "ok", "ok"},
		{"duplicate trailer", func(c *gitx.Commit) {
			c.Message = "x\n\nLedger-Manifest: " + hash + "\nLedger-Manifest: " + hash + "\n"
		}, "mismatch", "ok", "ok"},
		{"tree differs", func(c *gitx.Commit) { c.Tree = "4444444444444444444444444444444444444444" }, "ok", "mismatch", "ok"},
		{"parent differs", func(c *gitx.Commit) { c.Parents = []string{"5555555555555555555555555555555555555555"} }, "ok", "ok", "mismatch"},
		{"extra parent", func(c *gitx.Commit) { c.Parents = []string{m.BaseCommit, "5555555555555555555555555555555555555555"} }, "ok", "ok", "mismatch"},
		{"no parent", func(c *gitx.Commit) { c.Parents = nil }, "ok", "ok", "mismatch"},
		{"all three", func(c *gitx.Commit) {
			*c = gitx.Commit{SHA: c.SHA, Tree: "4444444444444444444444444444444444444444", Message: "none\n"}
		}, "mismatch", "mismatch", "mismatch"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := good
			c.Parents = slices.Clone(good.Parents)
			tc.mutate(&c)
			b := CheckBinding(c, hash, m)
			if b.Trailer != tc.trailer || b.Tree != tc.tree || b.Parent != tc.parent || b.OK() {
				t.Fatalf("got %+v", b)
			}
			if len(b.Detail) == 0 {
				t.Fatal("failure without detail")
			}
		})
	}
	// An unavailable commit is never a mismatch.
	if b := CheckBinding(gitx.Commit{}, hash, m); b.Trailer != BindingUnavailable || b.Tree != BindingUnavailable || b.Parent != BindingUnavailable {
		t.Fatalf("unavailable: %+v", b)
	}
}

package verify_test

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/verify"
)

// P2-AC-021: a bundle exported by the Phase 1 binary from demos/m3.sh still
// verifies, with the same report it produced then. The binary can no longer
// write v1 records, so the bundle is kept as a file.
func TestPhase1BundleVerifiesUnchanged(t *testing.T) {
	raw, err := os.ReadFile("testdata/m3-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var want struct {
		Root   string `json:"root"`
		Report any    `json:"report"`
	}
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	src, err := verify.OpenBundleSource(ctx, "testdata/m3-v1.ledger", verify.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	trust := identity.NewTrustList()
	if err := trust.Merge([]string{want.Root}); err != nil {
		t.Fatal(err)
	}
	rep, err := verify.Verify(ctx, src, verify.Options{Trusted: trust})
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	delete(got, "source")
	if !reflect.DeepEqual(got, want.Report) {
		t.Fatalf("report changed:\n got %s\nwant %v", out, want.Report)
	}
}

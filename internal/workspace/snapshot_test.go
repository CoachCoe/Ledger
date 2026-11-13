package workspace

import (
	"context"
	"testing"

	"github.com/CoachCoe/Ledger/internal/apierr"
)

func TestM2ManifestPathsNormalizeWithoutLosingStoredNames(t *testing.T) {
	raw := "src/Café.ts"
	snap := Snapshot{Files: []string{raw}}
	files, err := snap.RecordFiles()
	if err != nil || len(files) != 1 || files[0] != "src/Café.ts" || snap.Files[0] != raw {
		t.Fatalf("normalized record / raw lookup: %v %v", files, err)
	}
	snap.Files = append(snap.Files, "src/Café.ts")
	if _, err := snap.RecordFiles(); !apierr.HasCode(err, "path_alias") {
		t.Fatal("ambiguous aliases accepted", err)
	}
}

// links is a TreeSource whose symlink targets are given by id.
type links map[string]string

func (links) Entries(context.Context, string) ([]TreeEntry, error) { return nil, nil }

func (l links) LinkTarget(_ context.Context, id string) (string, error) { return l[id], nil }

func TestM2FrozenSymlinkResolution(t *testing.T) {
	ctx := context.Background()
	src := links{"out": "../../outside", "safe": "../src/safe"}
	entries := map[string]treeEntry{"src/café": {link: true, id: "out"}, "src/escape": {link: true, id: "out"}, "src/safe": {}}
	for _, p := range []string{"src/Café", "src/escape/../safe"} {
		if _, err := resolveTreePath(ctx, src, p, entries, false); err == nil {
			t.Fatal("allowed aliased or cleaned escape", p)
		}
	}
	entries["src/link"] = treeEntry{link: true, id: "safe"}
	resolved, err := resolveTreePath(ctx, src, "src/link", entries, true)
	if err != nil || resolved != "src/safe" {
		t.Fatalf("safe relative link: %q %v", resolved, err)
	}
}

package store

import (
	"slices"
	"testing"
)

func TestDiffReportsEveryKindOfChange(t *testing.T) {
	s := open(t, true)
	b := s.Batch()
	blob := func(data string) string {
		id, err := b.PutFile([]byte(data))
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	tree := func(files ...File) string {
		id, err := b.BuildTree(files)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	x, y := blob("x"), blob("y")
	before := tree(
		File{"same/a.txt", TypeFile, x},
		File{"edit.txt", TypeFile, x},
		File{"mode.sh", TypeFile, x},
		File{"gone.txt", TypeFile, x},
		File{"dir/inner.txt", TypeFile, x},
		File{"becomes-dir", TypeFile, x},
	)
	after := tree(
		File{"same/a.txt", TypeFile, x},
		File{"edit.txt", TypeFile, y},
		File{"mode.sh", TypeExecutable, x},
		File{"new.txt", TypeFile, y},
		File{"dir", TypeFile, y},
		File{"becomes-dir/child.txt", TypeFile, y},
	)
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	changes, err := s.Diff(before, after)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range changes {
		got = append(got, c.Status+" "+c.Path)
	}
	want := []string{"D becomes-dir", "A becomes-dir/child.txt", "A dir", "D dir/inner.txt", "M edit.txt", "D gone.txt", "T mode.sh", "A new.txt"}
	if !slices.Equal(got, want) {
		t.Fatalf("diff:\n got %q\nwant %q", got, want)
	}
	if none, err := s.Diff(after, after); err != nil || len(none) != 0 {
		t.Fatalf("diff of a tree with itself: %v, %v", none, err)
	}
	if c := changes[4]; c.Old.ID != x || c.New.ID != y {
		t.Fatalf("edit change %+v", c)
	}
}

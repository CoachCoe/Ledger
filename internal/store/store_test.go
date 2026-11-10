package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/CoachCoe/Ledger/internal/apierr"
)

func open(t *testing.T, sensitive bool) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "objects"), sensitive)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func read(t *testing.T, s *Store, id string) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := s.WriteFile(id, &b); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// text is deterministic source-like data, independent of math/rand, so
// the chunk-boundary golden values never move with a Go release.
func text(n int, seed uint64) []byte {
	words := []string{"invoice", "total", "ledger", "return", "const", "function", "await", "export"}
	x := seed*0x9e3779b97f4a7c15 + 1
	var b bytes.Buffer
	for line := 0; b.Len() < n; line++ {
		fmt.Fprintf(&b, "%d", line)
		for range 8 {
			x ^= x << 13
			x ^= x >> 7
			x ^= x << 17
			b.WriteByte(' ')
			b.WriteString(words[x%uint64(len(words))])
		}
		b.WriteByte('\n')
	}
	return b.Bytes()[:n]
}

func random(n int, seed uint64) []byte {
	b := make([]byte, 0, n+sha256.Size)
	for i := uint64(0); len(b) < n; i++ {
		sum := sha256.Sum256(binary.BigEndian.AppendUint64(binary.BigEndian.AppendUint64(nil, seed), i))
		b = append(b, sum[:]...)
	}
	return b[:n]
}

// P2-AC-001
func TestFilesRoundTripByteForByte(t *testing.T) {
	s := open(t, true)
	b := s.Batch()
	cases := map[string][]byte{
		"empty":            {},
		"crlf":             []byte("one\r\ntwo\r\n"),
		"invalid utf-8":    {0xff, 0xfe, 'a', 0xc3, 0x28, '\n'},
		"nul":              {0, 0, 0},
		"leading newline":  []byte("\nkind\n"),
		"exactly 4 MiB":    text(ChunkThreshold, 1),
		"over 4 MiB":       text(ChunkThreshold+1, 2),
		"9 MiB of zeroes":  make([]byte, 9<<20),
		"random 6 MiB":     random(6<<20, 3),
		"no final newline": []byte("x"),
	}
	for name, data := range cases {
		id, err := b.PutFile(data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		kind, _, err := s.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if want := map[bool]string{true: KindChunkedBlob, false: KindBlob}[len(data) > ChunkThreshold]; kind != want {
			t.Errorf("%s: stored as %s, want %s", name, kind, want)
		}
		if got := read(t, s, id); !bytes.Equal(got, data) {
			t.Errorf("%s: round trip differs (%d bytes, want %d)", name, len(got), len(data))
		}
	}
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestMaterializeRoundTripsTree(t *testing.T) {
	s := open(t, true)
	b := s.Batch()
	src := t.TempDir()
	files := map[string][]byte{
		"README.md":           []byte("# r\r\n"),
		"src/a.ts":            []byte("export const a = 1\n"),
		"src/deep/b/c.bin":    random(5<<20, 4),
		"src/deep/b/empty":    {},
		"bin/run":             []byte("#!/bin/sh\n"),
		"café/menu.txt":       []byte("menu"),
		"src/link-to-a":       nil,
		"src/deep/b/link-dir": nil,
	}
	var list []File
	for p, data := range files {
		full := filepath.Join(src, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		typ, mode := TypeFile, fs.FileMode(0o644)
		switch p {
		case "bin/run":
			typ, mode = TypeExecutable, 0o755
		case "src/link-to-a", "src/deep/b/link-dir":
			target := map[string]string{"src/link-to-a": "a.ts", "src/deep/b/link-dir": "../../.."}[p]
			if err := os.Symlink(target, full); err != nil {
				t.Fatal(err)
			}
			id, err := b.Put(KindBlob, []byte(target))
			if err != nil {
				t.Fatal(err)
			}
			list = append(list, File{Path: p, Type: TypeSymlink, ID: id})
			continue
		}
		if err := os.WriteFile(full, data, mode); err != nil {
			t.Fatal(err)
		}
		id, err := b.PutFile(data)
		if err != nil {
			t.Fatal(err)
		}
		list = append(list, File{Path: p, Type: typ, ID: id})
	}
	root, err := b.BuildTree(list)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	slices.Reverse(list)
	again, err := s.Batch().BuildTree(list)
	if err != nil || again != root {
		t.Fatalf("root id depends on listing order: %s vs %s (%v)", again, root, err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	if err := s.Materialize(root, dst); err != nil {
		t.Fatal(err)
	}
	for p, data := range files {
		full := filepath.Join(dst, p)
		info, err := os.Lstat(full)
		if err != nil {
			t.Fatal(err)
		}
		switch p {
		case "src/link-to-a", "src/deep/b/link-dir":
			if info.Mode()&fs.ModeSymlink == 0 {
				t.Errorf("%s is not a symlink", p)
			}
			continue
		case "bin/run":
			if info.Mode().Perm()&0o100 == 0 {
				t.Errorf("%s lost its executable bit", p)
			}
		}
		got, err := os.ReadFile(full)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, data) {
			t.Errorf("%s differs after materialize", p)
		}
	}
}

// P2-AC-002
func TestCollidingNamesAreRejected(t *testing.T) {
	blob := func(s *Store) string {
		id, err := s.Batch().PutFile([]byte("x"))
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	for _, c := range []struct {
		name      string
		sensitive bool
		paths     []string
	}{
		{"NFC and NFD file", true, []string{"café.txt", "café.txt"}},
		{"NFC and NFD directory", true, []string{"café/a", "café/b"}},
		{"case, insensitive policy", false, []string{"README.md", "readme.md"}},
		{"case in directory, insensitive policy", false, []string{"Src/a.ts", "src/b.ts"}},
		{"case folding beyond ASCII", false, []string{"straße", "STRASSE"}},
		{"file and directory", true, []string{"a", "a/b"}},
		{"duplicate path", true, []string{"a", "a"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := open(t, c.sensitive)
			id := blob(s)
			var files []File
			for _, p := range c.paths {
				files = append(files, File{Path: p, Type: TypeFile, ID: id})
			}
			if _, err := s.Batch().BuildTree(files); !apierr.HasCode(err, "path_collision") {
				t.Fatalf("got %v, want path_collision", err)
			}
		})
	}
	s := open(t, true)
	id := blob(s)
	if _, err := s.Batch().BuildTree([]File{{Path: "README.md", Type: TypeFile, ID: id}, {Path: "readme.md", Type: TypeFile, ID: id}}); err != nil {
		t.Fatalf("case-sensitive policy rejected distinct names: %v", err)
	}
	if _, err := s.Batch().WriteTree([]Entry{{Name: "café", Type: TypeFile, ID: id}}); !apierr.HasCode(err, "invalid_path") {
		t.Fatalf("tree accepted a non-NFC name: %v", err)
	}
	for _, p := range []string{"", "a//b", "./a", "a/..", "/a", "a\x00b", string([]byte{0xff})} {
		if _, err := s.Batch().BuildTree([]File{{Path: p, Type: TypeFile, ID: id}}); !apierr.HasCode(err, "invalid_path") {
			t.Errorf("%q: got %v, want invalid_path", p, err)
		}
	}
}

// P2-AC-004
func TestOneLineEditStoresOnlyNearbyChunks(t *testing.T) {
	s := open(t, true)
	const size = 50 << 20
	original := text(size, 5)
	b := s.Batch()
	if _, err := b.PutFile(original); err != nil {
		t.Fatal(err)
	}
	before := b.Written()
	mid := bytes.IndexByte(original[size/2:], '\n') + size/2 + 1
	end := bytes.IndexByte(original[mid:], '\n') + mid
	edited := slices.Concat(original[:mid], []byte("one line changed by a longer replacement than before"), original[end:])
	id, err := b.PutFile(edited)
	if err != nil {
		t.Fatal(err)
	}
	// One or two new chunks around the edit, plus the new chunk list.
	if fresh := b.Written() - before; fresh < 2 || fresh > 3 {
		t.Fatalf("edit stored %d new objects", fresh)
	}
	if !bytes.Equal(read(t, s, id), edited) {
		t.Fatal("edited file does not round-trip")
	}
}

func TestChunkBoundariesAreStable(t *testing.T) {
	var sizes []int
	for _, c := range chunks(text(1<<20, 6)) {
		sizes = append(sizes, len(c))
	}
	// Chunk boundaries feed chunked_blob ids; changing them is a format change.
	want := []int{66341, 97661, 74821, 125186, 86135, 38969, 58055, 67375, 81096, 102106, 57706, 53199, 46956, 70961, 22009}
	if fmt.Sprint(sizes) != fmt.Sprint(want) {
		t.Fatalf("chunk sizes %v, want %v", sizes, want)
	}
}

func TestCorruptObjectsAreDetectedAndRecovered(t *testing.T) {
	s := open(t, true)
	b := s.Batch()
	good, err := b.PutFile([]byte("good"))
	if err != nil {
		t.Fatal(err)
	}
	bad, err := b.PutFile([]byte("will be truncated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(s.path(bad), 8); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(s.path(good)), ".tmp-123"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(bad); !apierr.HasCode(err, "object_corrupt") {
		t.Fatalf("read of truncated object: %v", err)
	}
	removed, err := s.Recover()
	if err != nil || removed != 2 {
		t.Fatalf("recover removed %d (%v), want 2", removed, err)
	}
	if _, _, err := s.Get(bad); !apierr.HasCode(err, "object_missing") {
		t.Fatalf("truncated object survived recovery: %v", err)
	}
	if _, _, err := s.Get(good); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path(good), []byte("blob\ntampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A later process finds the damaged copy on its first write of it.
	if s, err = Open(s.dir, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Batch().PutFile([]byte("good")); err != nil {
		t.Fatal(err)
	}
	if got := read(t, s, good); string(got) != "good" {
		t.Fatalf("rewrite kept the corrupt copy: %q", got)
	}
}

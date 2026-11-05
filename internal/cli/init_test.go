package cli

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestProbeCaseSensitive(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := os.Stat(filepath.Join(dir, "X"))
	want := errors.Is(err, fs.ErrNotExist)
	got, err := probeCaseSensitive(dir)
	if err != nil || got != want {
		t.Fatalf("probeCaseSensitive = %v, %v; want %v", got, err, want)
	}
}

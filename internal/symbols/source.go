package symbols

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/CoachCoe/Ledger/internal/gitx"
	"github.com/CoachCoe/Ledger/internal/store"
)

// maxSource is the largest file the engine parses.
const maxSource = 4 << 20

// Entry is one file of a frozen tree. Size is -1 when the source learns it
// only by reading the file.
type Entry struct {
	Path    string
	ID      string
	Regular bool
	Size    int64
}

// Source reads the files of frozen trees.
type Source interface {
	Entries(ctx context.Context, tree string) ([]Entry, error)
	// Read returns the bytes of each entry, or nil for one larger than
	// maxSource.
	Read(ctx context.Context, entries []Entry) ([][]byte, error)
}

// GitSource reads Git trees of a Phase 1 repository.
type GitSource struct{ Repo *gitx.Repo }

func (g GitSource) Entries(ctx context.Context, tree string) ([]Entry, error) {
	list, err := g.Repo.ListTreeDetails(ctx, tree)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, len(list))
	for i, e := range list {
		out[i] = Entry{Path: e.Path, ID: e.Blob, Regular: e.Mode == "100644" || e.Mode == "100755", Size: e.Size}
	}
	return out, nil
}

func (g GitSource) Read(ctx context.Context, entries []Entry) ([][]byte, error) {
	var input strings.Builder
	for _, entry := range entries {
		input.WriteString(entry.ID + "\n")
	}
	raw, err := gitx.Run(ctx, g.Repo.Dir, strings.NewReader(input.String()), nil, "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	r := bufio.NewReader(bytes.NewReader(raw))
	out := make([][]byte, len(entries))
	for i, entry := range entries {
		header, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[1] != "blob" {
			return nil, fmt.Errorf("unavailable blob %s", entry.ID)
		}
		n, err := strconv.Atoi(fields[2])
		if err != nil || int64(n) != entry.Size {
			return nil, fmt.Errorf("invalid blob size")
		}
		out[i] = make([]byte, n)
		if _, err := io.ReadFull(r, out[i]); err != nil {
			return nil, err
		}
		if _, err := r.ReadByte(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// StoreSource reads trees of a native store. A file over maxSource is a
// chunked_blob there, so its size is known without reading its chunks.
type StoreSource struct{ Store *store.Store }

func (s StoreSource) Entries(_ context.Context, tree string) ([]Entry, error) {
	files, err := s.Store.Files(tree)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, len(files))
	for i, f := range files {
		out[i] = Entry{Path: f.Path, ID: f.ID, Regular: f.Type == store.TypeFile || f.Type == store.TypeExecutable, Size: -1}
	}
	return out, nil
}

func (s StoreSource) Read(_ context.Context, entries []Entry) ([][]byte, error) {
	out := make([][]byte, len(entries))
	for i, e := range entries {
		kind, data, err := s.Store.Get(e.ID)
		if err != nil {
			return nil, err
		}
		if kind == store.KindBlob {
			out[i] = data
		}
	}
	return out, nil
}

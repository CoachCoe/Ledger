package symbols

import (
	"context"

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

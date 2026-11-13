package workspace

import (
	"bytes"
	"context"

	"github.com/CoachCoe/Ledger/internal/store"
)

// TreeEntry is one non-directory path of a frozen tree.
type TreeEntry struct {
	Path string
	Link bool
	ID   string
}

// TreeSource lists frozen trees and reads their symlink targets, so path
// authorization never looks at live disk state.
type TreeSource interface {
	Entries(ctx context.Context, tree string) ([]TreeEntry, error)
	LinkTarget(ctx context.Context, id string) (string, error)
}

// StoreTrees reads trees from the native store.
type StoreTrees struct{ Store *store.Store }

func (s StoreTrees) Entries(_ context.Context, tree string) ([]TreeEntry, error) {
	files, err := s.Store.Files(tree)
	if err != nil {
		return nil, err
	}
	out := make([]TreeEntry, len(files))
	for i, f := range files {
		out[i] = TreeEntry{Path: f.Path, Link: f.Type == store.TypeSymlink, ID: f.ID}
	}
	return out, nil
}

func (s StoreTrees) LinkTarget(_ context.Context, id string) (string, error) {
	var b bytes.Buffer
	err := s.Store.WriteFile(id, &b)
	return b.String(), err
}

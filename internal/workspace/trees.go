package workspace

import (
	"bytes"
	"context"
	"strings"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/gitx"
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

// GitTrees reads trees from the Git repository at Dir.
type GitTrees struct{ Dir string }

func (g GitTrees) Entries(ctx context.Context, tree string) ([]TreeEntry, error) {
	raw, err := gitx.Run(ctx, g.Dir, nil, nil, "ls-tree", "-r", "-z", tree, "--")
	if err != nil {
		return nil, err
	}
	var out []TreeEntry
	for row := range strings.SplitSeq(string(raw), "\x00") {
		if row == "" {
			continue
		}
		meta, name, ok := strings.Cut(row, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return nil, apierr.New("git_output", "malformed tree")
		}
		out = append(out, TreeEntry{Path: name, Link: fields[0] == "120000", ID: fields[2]})
	}
	return out, nil
}

func (g GitTrees) LinkTarget(ctx context.Context, id string) (string, error) {
	raw, err := gitx.Run(ctx, g.Dir, nil, nil, "cat-file", "blob", id)
	return string(raw), err
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

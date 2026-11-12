package workspace

import (
	"context"
	"strings"
	"time"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/capability"
	"github.com/CoachCoe/Ledger/internal/schema"
)

type treeEntry struct {
	link bool
	id   string
}

// Violations authorizes the frozen candidate and base trees, never live disk
// symlinks. Both sides matter for deletion, replacement and rename. Trees
// contain no directory symlinks: resolution walks every component explicitly.
func Violations(ctx context.Context, src TreeSource, base string, snap Snapshot, chain []schema.Capability, sensitive bool, at time.Time) (map[string]error, error) {
	bad := map[string]error{}
	trees := []map[string]treeEntry{}
	for _, tree := range []string{base, snap.Tree} {
		list, err := src.Entries(ctx, tree)
		if err != nil {
			return nil, err
		}
		entries := map[string]treeEntry{}
		for _, e := range list {
			normalized, err := capability.Normalize(e.Path, sensitive)
			if err != nil {
				continue
			} // unmodified unsupported names cannot be traversal components
			if _, exists := entries[normalized]; exists {
				return nil, apierr.New("path_alias", "ambiguous tree path %q", e.Path)
			}
			entries[normalized] = treeEntry{e.Link, e.ID}
		}
		trees = append(trees, entries)
	}
	for _, p := range snap.Files {
		if err := capability.Authorize(chain, p, sensitive, at); err != nil {
			bad[p] = err
			continue
		}
		for _, entries := range trees {
			normalized, _ := capability.Normalize(p, sensitive)
			if _, exists := entries[normalized]; !exists {
				continue
			}
			resolved, err := resolveTreePath(ctx, src, p, entries, sensitive)
			if err == nil {
				err = capability.Authorize(chain, resolved, sensitive, at)
			}
			if err != nil {
				bad[p] = err
				break
			}
		}
	}
	return bad, nil
}

// Resolve components in order: cleaning `link/../file` before following link
// would incorrectly erase a symlink that leads outside the workspace.
func resolveTreePath(ctx context.Context, src TreeSource, p string, entries map[string]treeEntry, sensitive bool) (string, error) {
	pending := strings.Split(p, "/")
	stack := []string{}
	links := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if len(stack) == 0 {
				return "", apierr.New("scope_violation", "symlink escapes workspace")
			}
			stack = stack[:len(stack)-1]
			continue
		}
		normalized, err := capability.Normalize(part, sensitive)
		if err != nil {
			return "", err
		}
		stack = append(stack, normalized)
		prefix := strings.Join(stack, "/")
		entry := entries[prefix]
		if !entry.link {
			continue
		}
		links++
		if links > 40 {
			return "", apierr.New("scope_violation", "symlink cycle at %q", prefix)
		}
		target, err := src.LinkTarget(ctx, entry.id)
		if err != nil {
			return "", err
		}
		if target == "" || strings.HasPrefix(target, "/") || strings.ContainsAny(target, "\\:\x00") {
			return "", apierr.New("scope_violation", "symlink %q has unsafe target", prefix)
		}
		stack = stack[:len(stack)-1]
		pending = append(strings.Split(target, "/"), pending...)
	}
	if len(stack) == 0 {
		return "", apierr.New("scope_violation", "symlink targets workspace root")
	}
	return strings.Join(stack, "/"), nil
}

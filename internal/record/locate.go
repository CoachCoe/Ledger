package record

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/gitx"
)

// StoreDir is the content store's directory inside .ledger. Its presence
// is what makes a repository native.
const StoreDir = "store"

// Repository is a located Ledger repository.
type Repository struct {
	// Root is the directory holding .ledger.
	Root string
	// Git is a Phase 1 repository's Git repository, nil for a native one.
	Git *gitx.Repo
}

// Native reports whether the repository runs on the native store.
func (r Repository) Native() bool { return r.Git == nil }

// APIVersion is the api_version the repository's daemon answers.
func (r Repository) APIVersion() int {
	if r.Native() {
		return 2
	}
	return 1
}

// Open opens the repository's RecordStore.
func (r Repository) Open(ctx context.Context, opts Options) (*Store, error) {
	if r.Native() {
		return OpenNative(ctx, r.Root, opts)
	}
	return Open(ctx, r.Git, opts)
}

// IsNative reports whether root holds a native repository.
func IsNative(root string) (bool, error) {
	fi, err := os.Stat(filepath.Join(root, DirName, StoreDir))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return fi.IsDir(), nil
}

// Locate finds the repository dir belongs to. The nearest ancestor with a
// .ledger directory wins, so a native repository needs no Git and may sit
// inside a Git working tree. A .ledger without a content store is a Phase 1
// repository, as is one at the main working tree of dir's Git repository,
// which a linked worktree outside it reaches only through Git.
func Locate(ctx context.Context, dir string) (Repository, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return Repository{}, err
	}
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		return Repository{}, err
	}
	phase1 := false
	for d := dir; ; {
		fi, err := os.Stat(filepath.Join(d, DirName))
		if err == nil && fi.IsDir() {
			native, err := IsNative(d)
			if err != nil {
				return Repository{}, err
			}
			if native {
				return Repository{Root: d}, nil
			}
			phase1 = true
			break
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return Repository{}, err
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	repo, err := gitx.Open(ctx, dir)
	if err != nil && phase1 {
		return Repository{}, err
	}
	if err != nil {
		return Repository{}, apierr.New("not_initialized", "%s is not inside a Ledger repository; run ledger init", dir)
	}
	if fi, err := os.Stat(filepath.Join(repo.MainDir, DirName)); err != nil || !fi.IsDir() {
		return Repository{}, apierr.New("not_initialized", "%s is not inside a Ledger repository; run ledger init", dir)
	}
	return Repository{Root: repo.MainDir, Git: repo}, nil
}

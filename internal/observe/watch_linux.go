//go:build linux

package observe

import (
	"context"
	"os"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
)

type watch struct {
	w    *fsnotify.Watcher
	done chan struct{}
}

func (w *watch) Close() error { err := w.w.Close(); <-w.done; return err }

// dirLister is a Filter that can list the directories to watch faster
// than asking it about each one.
type dirLister interface {
	WatchDirs(ctx context.Context) ([]string, error)
}

func watchDirs(ctx context.Context, root string, filter Filter) ([]string, error) {
	if l, ok := filter.(dirLister); ok {
		return l.WatchDirs(ctx)
	}
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && filter.Ignored(ctx, path) {
			return filepath.SkipDir
		}
		dirs = append(dirs, path)
		return nil
	})
	return dirs, err
}

func start(root string, filter Filter, notify func(), report func(error)) (WorkspaceObserver, error) {
	ctx := context.Background()
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	dirs, err := watchDirs(ctx, root, filter)
	if err != nil {
		_ = w.Close()
		return nil, err
	}
	for _, dir := range dirs {
		if err := w.Add(dir); err != nil && !os.IsNotExist(err) {
			_ = w.Close()
			return nil, err
		}
	}
	result := &watch{w: w, done: make(chan struct{})}
	go func() {
		defer close(result.done)
		for {
			select {
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				report(err)
			case event, ok := <-w.Events:
				if !ok {
					return
				}
				if filter.Ignored(ctx, event.Name) {
					continue
				}
				if event.Has(fsnotify.Create) {
					if fi, err := os.Stat(event.Name); err == nil && fi.IsDir() {
						err := filepath.WalkDir(event.Name, func(path string, d os.DirEntry, err error) error {
							if err != nil {
								return err
							}
							if !d.IsDir() {
								return nil
							}
							if filter.Ignored(ctx, path) {
								return filepath.SkipDir
							}
							return w.Add(path)
						})
						if err != nil {
							report(err)
						}
					}
				}
				notify()
			}
		}
	}()
	return result, nil
}

//go:build linux

package observe

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"

	"github.com/CoachCoe/Ledger/internal/gitx"
)

type watch struct {
	w    *fsnotify.Watcher
	done chan struct{}
}

func (w *watch) Close() error { err := w.w.Close(); <-w.done; return err }
func start(root string, notify func(), report func(error)) (WorkspaceObserver, error) {
	ctx := context.Background()
	filter, err := newFilter(root)
	if err != nil {
		return nil, err
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	fail := func(err error) (WorkspaceObserver, error) { _ = w.Close(); return nil, err }
	out, err := gitx.Run(ctx, root, nil, nil, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return fail(err)
	}
	dirs := map[string]bool{root: true}
	for _, p := range strings.Split(string(out), "\x00") {
		for d := filepath.Dir(p); d != "." && d != ""; d = filepath.Dir(d) {
			dirs[filepath.Join(root, d)] = true
		}
	}
	for dir := range dirs {
		if err := w.Add(dir); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fail(err)
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
				if filter.ignored(ctx, event.Name) {
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
							if filter.ignored(ctx, path) {
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

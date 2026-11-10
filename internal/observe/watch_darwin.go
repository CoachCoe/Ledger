//go:build darwin

package observe

import (
	"context"
	"time"

	"github.com/fsnotify/fsevents"
)

type stream struct {
	events *fsevents.EventStream
	cancel context.CancelFunc
	done   chan struct{}
}

func (s *stream) Close() error { s.cancel(); s.events.Stop(); <-s.done; return nil }

// start watches path with one recursive FSEvents stream. FSEvents reports
// no asynchronous errors, so the error callback is unused.
func start(path string, filter Filter, notify func(), _ func(error)) (WorkspaceObserver, error) {
	ctx, cancel := context.WithCancel(context.Background())
	events := &fsevents.EventStream{Paths: []string{path}, Latency: 50 * time.Millisecond, Flags: fsevents.FileEvents | fsevents.NoDefer}
	if err := events.Start(); err != nil {
		cancel()
		return nil, err
	}
	s := &stream{events: events, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		for {
			select {
			case <-ctx.Done():
				return
			case batch, ok := <-events.Events:
				if !ok {
					return
				}
				for _, e := range batch {
					if !filter.Ignored(ctx, e.Path) {
						notify()
						break
					}
				}
			}
		}
	}()
	return s, nil
}

//go:build darwin

package observe

import (
	"context"
	"github.com/fsnotify/fsevents"
	"time"
)

type stream struct {
	events *fsevents.EventStream
	cancel context.CancelFunc
	done   chan struct{}
}

func (s *stream) Close() error { s.cancel(); s.events.Stop(); <-s.done; return nil }
func start(path string, notify func(), report func(error)) (WorkspaceObserver, error) {
	filter, err := newFilter(path)
	if err != nil {
		return nil, err
	}
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
					if !filter.ignored(ctx, e.Path) {
						notify()
						break
					}
				}
			}
		}
	}()
	return s, nil
}

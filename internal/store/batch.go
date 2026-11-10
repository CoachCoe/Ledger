package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// Batch writes objects without waiting for stable storage; Commit makes
// all of them durable with one barrier. Nothing may name an object of the
// batch durably before Commit returns. A Batch is safe for concurrent use.
type Batch struct {
	s     *Store
	mu    sync.Mutex
	dirty map[string]bool // directories whose entries need writing out
	ids   []string        // objects to record as durable at Commit
	// written counts objects this batch created.
	written int
}

func (s *Store) Batch() *Batch { return &Batch{s: s, dirty: map[string]bool{}} }

// Written returns how many objects the batch created so far.
func (b *Batch) Written() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.written
}

func (b *Batch) mark(id string, dirs ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, d := range dirs {
		b.dirty[d] = true
	}
	if id != "" {
		b.ids = append(b.ids, id)
	}
}

// Put stores data as kind and returns its id. An object already present is
// kept only if it verifies; a copy truncated by an earlier crash is replaced.
func (b *Batch) Put(kind string, data []byte) (string, error) {
	id := ID(kind, data)
	if _, ok := b.s.durable.Load(id); ok {
		return id, nil
	}
	p := b.s.path(id)
	fan := filepath.Dir(p)
	if ok, err := b.s.Has(id); err != nil {
		return "", err
	} else if ok {
		// Another batch may have renamed it into place without its barrier.
		b.mark(id, fan)
		return id, nil
	}
	if err := os.Mkdir(fan, 0o700); err == nil {
		b.mark("", b.s.dir)
	} else if !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	f, err := os.CreateTemp(fan, ".tmp-*")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, err = f.Write([]byte(kind + "\n"))
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = writeout(f)
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return "", err
	}
	if err := os.Rename(tmp, p); err != nil {
		return "", err
	}
	b.mark(id, fan)
	b.mu.Lock()
	b.written++
	b.mu.Unlock()
	return id, nil
}

// Commit writes out every touched directory and issues one barrier, after
// which every object the batch put is durable. A batch that found every
// object already durable needs no barrier.
func (b *Batch) Commit() error {
	b.mu.Lock()
	dirs := make([]string, 0, len(b.dirty))
	for d := range b.dirty {
		dirs = append(dirs, d)
	}
	ids := b.ids
	b.ids = nil
	b.mu.Unlock()
	if len(dirs) == 0 {
		return nil
	}
	for _, d := range dirs {
		f, err := os.Open(d)
		if err != nil {
			return err
		}
		err = writeout(f)
		_ = f.Close()
		if err != nil {
			return err
		}
	}
	if err := barrier(b.s.dir); err != nil {
		return err
	}
	b.mu.Lock()
	for _, d := range dirs {
		delete(b.dirty, d)
	}
	b.mu.Unlock()
	for _, id := range ids {
		b.s.durable.Store(id, true)
	}
	return nil
}

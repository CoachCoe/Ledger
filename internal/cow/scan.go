package cow

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/CoachCoe/Ledger/internal/ignore"
	"github.com/CoachCoe/Ledger/internal/store"
)

// stat is what a file must still match for its cached id to be reused.
// Ino 0 means unknown, as in a cache copied to a fresh clone.
type stat struct {
	Size  int64  `json:"s"`
	Mtime int64  `json:"m"`
	Ino   uint64 `json:"i"`
	Type  string `json:"t"`
}

type cached struct {
	stat
	ID string `json:"id"`
}

// statCache maps each path of the last snapshot, as named on disk, to its
// stat and object id. At is when that scan began.
type statCache struct {
	At    int64             `json:"at"`
	Files map[string]cached `json:"files"`
}

func loadCache(p string) (statCache, error) {
	c := statCache{Files: map[string]cached{}}
	raw, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if json.Unmarshal(raw, &c) != nil || c.Files == nil {
		// A damaged cache costs one full rehash, never a wrong tree.
		return statCache{Files: map[string]cached{}}, nil
	}
	return c, nil
}

func saveCache(p string, c statCache) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".tmp-cache-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(raw)
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), p)
}

// trusted reports whether a cached id may be reused for a file with stat
// now. An entry modified at or after the scan that recorded it began may
// have changed again within the same timestamp, so it is rehashed.
func (c statCache) trusted(rel string, now stat) (string, bool) {
	e, ok := c.Files[rel]
	if !ok || e.Size != now.Size || e.Mtime != now.Mtime || e.Type != now.Type || e.Mtime >= c.At {
		return "", false
	}
	if e.Ino != 0 && e.Ino != now.Ino {
		return "", false
	}
	return e.ID, true
}

type scanResult struct {
	files    []store.File
	cache    statCache
	rehashed int
}

// scan observes root: every file that is not ignored, plus every path in
// tracked even if a rule now ignores it, as Git keeps tracked files.
// Dependency directories linked to shared copies under shared are never
// part of the tree. Files whose stat matches prev reuse their ids; the rest are
// hashed and stored in b by workers goroutines.
func scan(root string, rules *ignore.Tree, tracked map[string]bool, shared string, prev statCache, b *store.Batch, workers int) (scanResult, error) {
	start := time.Now().UnixNano()
	trackedDirs := map[string]bool{}
	for p := range tracked {
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			trackedDirs[d] = true
		}
	}
	type job struct {
		i   int
		rel string
	}
	res := scanResult{cache: statCache{At: start, Files: map[string]cached{}}}
	var stats []stat
	var jobs []job
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(p, root+string(filepath.Separator)))
		ignored, err := rules.Ignored(rel, d.IsDir())
		if err != nil {
			return err
		}
		if ignored && !tracked[rel] && (!d.IsDir() || !trackedDirs[rel]) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if shared != "" {
				if _, err := os.Lstat(filepath.Join(p, sharedMarker)); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		var typ string
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			typ = store.TypeSymlink
		case info.Mode().IsRegular() && info.Mode().Perm()&0o100 != 0:
			typ = store.TypeExecutable
		case info.Mode().IsRegular():
			typ = store.TypeFile
		default:
			return nil // sockets, pipes and devices are not versioned
		}
		st := stat{Size: info.Size(), Mtime: info.ModTime().UnixNano(), Type: typ}
		if sys, ok := info.Sys().(*syscall.Stat_t); ok {
			st.Ino = sys.Ino
		}
		i := len(res.files)
		res.files = append(res.files, store.File{Path: rel, Type: typ})
		stats = append(stats, st)
		if id, ok := prev.trusted(rel, st); ok {
			res.files[i].ID = id
		} else {
			jobs = append(jobs, job{i, rel})
		}
		return nil
	})
	if err != nil {
		return scanResult{}, err
	}
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
		next  = make(chan job)
	)
	for range max(workers, 1) {
		wg.Go(func() {
			for j := range next {
				id, err := hashOne(root, j.rel, res.files[j.i].Type, b)
				if errors.Is(err, fs.ErrNotExist) {
					continue // deleted since the walk; left out like any deletion
				}
				if err != nil {
					mu.Lock()
					first = errors.Join(first, err)
					mu.Unlock()
					continue
				}
				res.files[j.i].ID = id
			}
		})
	}
	for _, j := range jobs {
		next <- j
	}
	close(next)
	wg.Wait()
	if first != nil {
		return scanResult{}, first
	}
	kept := res.files[:0]
	for i, f := range res.files {
		if f.ID == "" {
			continue
		}
		res.cache.Files[f.Path] = cached{stats[i], f.ID}
		kept = append(kept, f)
	}
	res.files = kept
	res.rehashed = len(jobs)
	return res, nil
}

func hashOne(root, rel, typ string, b *store.Batch) (string, error) {
	p := filepath.Join(root, filepath.FromSlash(rel))
	if typ == store.TypeSymlink {
		target, err := os.Readlink(p)
		if err != nil {
			return "", err
		}
		return b.Put(store.KindBlob, []byte(target))
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return b.PutFile(data)
}

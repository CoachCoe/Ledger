package record

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/atomicfile"
	"github.com/CoachCoe/Ledger/internal/canon"
)

// LogName is a native repository's record head log under .ledger: one
// entry per accepted receipt group, in the order this repository accepted
// them (ADR 0027). It replaces refs/ledger/records.
const LogName = "records.log"

// LogEntry is one accepted group, named by its receipt.
type LogEntry struct {
	Seq      int64   `json:"seq"`
	Previous *string `json:"previous"`
	Receipt  string  `json:"receipt"`
}

// headLog is the in-memory copy of the log. It is only read or appended
// under the RecordStore lock, and reloaded whenever the file has grown
// since this process last saw it, since other processes append too.
type headLog struct {
	path    string
	entries []LogEntry
	hashes  []string
	logged  map[string]bool
	size    int64
	linked  bool
}

func openLog(dir string) (*headLog, error) {
	l := &headLog{path: filepath.Join(dir, LogName)}
	return l, l.load()
}

// load rereads the file if its size changed. A torn final line, which
// only a crash during an append can leave, is cut off; any other damage
// fails with record_log_broken.
func (l *headLog) load() error {
	fi, err := os.Stat(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		if l.size != 0 {
			return apierr.New("record_log_broken", "%s disappeared", l.path)
		}
		l.logged = map[string]bool{}
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Size() == l.size && l.logged != nil {
		return nil
	}
	raw, err := os.ReadFile(l.path)
	if err != nil {
		return err
	}
	if n := bytes.LastIndexByte(raw, '\n') + 1; n < len(raw) {
		if err := os.Truncate(l.path, int64(n)); err != nil {
			return err
		}
		raw = raw[:n]
	}
	if int64(len(raw)) < l.size {
		return apierr.New("record_log_broken", "%s shrank from %d to %d bytes", l.path, l.size, len(raw))
	}
	entries, hashes, err := decodeLog(raw)
	if err != nil {
		return err
	}
	for i := range l.hashes {
		if hashes[i] != l.hashes[i] {
			return apierr.New("record_log_broken", "entry %d of %s was rewritten", i+1, l.path)
		}
	}
	l.entries, l.hashes, l.size = entries, hashes, int64(len(raw))
	l.logged = make(map[string]bool, len(entries))
	for _, e := range entries {
		l.logged[e.Receipt] = true
	}
	return nil
}

func decodeLog(raw []byte) ([]LogEntry, []string, error) {
	var entries []LogEntry
	var hashes []string
	var prev *string
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		line := sc.Bytes()
		n := len(entries) + 1
		c, err := canon.Canonical(line)
		if err != nil || !bytes.Equal(c, line) {
			return nil, nil, apierr.New("record_log_broken", "entry %d is not canonical JSON", n).With("seq", n)
		}
		var e LogEntry
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&e); err != nil || !isHash(e.Receipt) {
			return nil, nil, apierr.New("record_log_broken", "entry %d is malformed", n).With("seq", n)
		}
		if e.Seq != int64(n) || !samePtr(e.Previous, prev) {
			return nil, nil, apierr.New("record_log_broken", "entry %d does not follow the entry before it", n).With("seq", n)
		}
		h := canon.Hash(line)
		entries, hashes = append(entries, e), append(hashes, h)
		prev = &hashes[len(hashes)-1]
	}
	return entries, hashes, sc.Err()
}

func samePtr(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

// append logs receipts that are not logged yet, in the order given, and
// syncs the file before returning. The receipts and their members must
// already be durable.
func (l *headLog) append(receipts ...string) error {
	if err := l.load(); err != nil {
		return err
	}
	var buf []byte
	entries, hashes := slices.Clone(l.entries), slices.Clone(l.hashes)
	added := map[string]bool{}
	for _, r := range receipts {
		if l.logged[r] || added[r] {
			continue
		}
		e := LogEntry{Seq: int64(len(entries) + 1), Receipt: r}
		if n := len(hashes); n > 0 {
			e.Previous = &hashes[n-1]
		}
		raw, err := canon.Marshal(e)
		if err != nil {
			return err
		}
		entries, hashes = append(entries, e), append(hashes, canon.Hash(raw))
		buf = slices.Concat(buf, raw, []byte("\n"))
		added[r] = true
	}
	if len(buf) == 0 {
		return nil
	}
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(buf)
	if err == nil {
		err = f.Sync()
	}
	if err == nil && !l.linked {
		err = atomicfile.SyncDir(filepath.Dir(l.path))
		l.linked = err == nil
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		// Never leave a partial line for the next append to follow.
		if terr := os.Truncate(l.path, l.size); terr != nil && !errors.Is(terr, fs.ErrNotExist) {
			return errors.Join(err, terr)
		}
		return err
	}
	l.entries, l.hashes, l.size = entries, hashes, l.size+int64(len(buf))
	for r := range added {
		l.logged[r] = true
	}
	return nil
}

// Log returns the record head log of a native repository, or nil for a
// Phase 1 repository, which keeps refs/ledger/records instead.
func (s *Store) Log() ([]LogEntry, error) {
	if s.log == nil {
		return nil, nil
	}
	unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := s.log.load(); err != nil {
		return nil, err
	}
	return slices.Clone(s.log.entries), nil
}

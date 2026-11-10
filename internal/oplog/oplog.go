// Package oplog is a workspace's append-only, hash-chained operation log
// (ADR 0026). Entries are unsigned; a v2 manifest pins the head, so its
// signature covers every entry up to the proposal.
package oplog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/atomicfile"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// Entry kinds.
const (
	Snapshot = "snapshot" // observed change in the workspace
	Write    = "write"    // write through the API
	Restore  = "restore"  // return to an earlier entry's tree
	Rebase   = "rebase"   // move to a new base
	Propose  = "propose"
	Rework   = "rework"
)

// Entry is one operation. Tree and Base are store tree ids.
type Entry struct {
	Workspace string  `json:"workspace"`
	Seq       int64   `json:"seq"`
	Previous  *string `json:"previous"`
	Kind      string  `json:"kind"`
	Tree      string  `json:"tree"`
	Base      string  `json:"base"`
	// Source is the entry a restore returned to.
	Source  *string `json:"source,omitzero"`
	At      string  `json:"at"`
	Trigger string  `json:"trigger"`
	// Session is null for entries made by root.
	Session *string `json:"session"`
}

// Hash is the entry's identity, which the next entry names as previous.
func Hash(e Entry) (string, error) {
	raw, err := canon.Marshal(e)
	if err != nil {
		return "", err
	}
	return canon.Hash(raw), nil
}

// Log is one workspace's log, stored as one canonical JSON entry per line.
type Log struct {
	mu        sync.Mutex
	path      string
	workspace string
	entries   []Entry
	hashes    []string
	index     map[string]int
	size      int64
	// linked is whether the file's directory entry is known durable.
	linked bool
}

func Path(dir, workspace string) string { return filepath.Join(dir, workspace+".log") }

// Open loads the log of workspace in dir. A torn final line, which only a
// crash during an append that was never made durable can leave, is cut
// off; any other damage fails verification.
func Open(dir, workspace string) (*Log, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	l := &Log{path: Path(dir, workspace), workspace: workspace, index: map[string]int{}}
	raw, err := os.ReadFile(l.path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if n := bytes.LastIndexByte(raw, '\n') + 1; n < len(raw) {
		if err := os.Truncate(l.path, int64(n)); err != nil {
			return nil, err
		}
		raw = raw[:n]
	}
	entries, err := Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	hashes, err := Chain(entries)
	if err != nil {
		return nil, err
	}
	for i, e := range entries {
		if e.Workspace != workspace {
			return nil, apierr.New("oplog_broken", "entry %d of %s belongs to workspace %s", e.Seq, workspace, e.Workspace)
		}
		l.index[hashes[i]] = i
	}
	l.entries, l.hashes, l.size = entries, hashes, int64(len(raw))
	return l, nil
}

// Decode reads canonical entries, one per line.
func Decode(r io.Reader) ([]Entry, error) {
	var out []Entry
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		c, err := canon.Canonical(line)
		if err != nil || !bytes.Equal(c, line) {
			return nil, apierr.New("oplog_broken", "entry %d is not canonical JSON", len(out)+1)
		}
		var e Entry
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&e); err != nil {
			return nil, apierr.New("oplog_broken", "entry %d is malformed", len(out)+1)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// Chain checks that entries form one chain from sequence 1 and returns
// their hashes. Editing any entry breaks the link the next one holds; a
// manifest's pinned head covers the last.
func Chain(entries []Entry) ([]string, error) {
	hashes := make([]string, len(entries))
	var prev *string
	for i, e := range entries {
		if e.Seq != int64(i+1) || !samePtr(e.Previous, prev) {
			return nil, apierr.New("oplog_broken", "entry %d does not follow the entry before it", i+1).With("seq", i+1)
		}
		raw, err := canon.Marshal(e)
		if err != nil {
			return nil, err
		}
		h := canon.Hash(raw)
		hashes[i] = h
		prev = &hashes[i]
	}
	return hashes, nil
}

// Verify checks entries against head, the hash a manifest pinned.
func Verify(entries []Entry, head string) error {
	hashes, err := Chain(entries)
	if err != nil {
		return err
	}
	if len(hashes) == 0 || hashes[len(hashes)-1] != head {
		return apierr.New("oplog_head_mismatch", "operation log does not end at the pinned head %s", head).With("head", head)
	}
	return nil
}

func samePtr(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

// Head returns the last entry and its hash, or false for an empty log.
func (l *Log) Head() (Entry, string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.entries) == 0 {
		return Entry{}, "", false
	}
	return l.entries[len(l.entries)-1], l.hashes[len(l.hashes)-1], true
}

// Entries returns a copy of the log.
func (l *Log) Entries() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Entry(nil), l.entries...)
}

// Get returns the entry with hash h.
func (l *Log) Get(h string) (Entry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	i, ok := l.index[h]
	if !ok {
		return Entry{}, false
	}
	return l.entries[i], true
}

// Append adds e after the head, filling its workspace, sequence, previous
// hash and time. A snapshot or write whose tree equals the head's is not
// appended; the head is returned with false. The objects e names must be
// durable already (ADR 0025). Durable entries are fully synced before
// Append returns, as a proposal's must be before a manifest pins it;
// others may be lost in a crash, which loses only local history.
func (l *Log) Append(e Entry, durable bool) (Entry, string, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e.Workspace, e.Seq, e.Previous = l.workspace, int64(len(l.entries)+1), nil
	if n := len(l.entries); n > 0 {
		head := l.entries[n-1]
		if (e.Kind == Snapshot || e.Kind == Write) && e.Tree == head.Tree && e.Base == head.Base {
			return head, l.hashes[n-1], false, nil
		}
		prev := l.hashes[n-1]
		e.Previous = &prev
	}
	switch e.Kind {
	case Snapshot, Write, Restore, Rebase, Propose, Rework:
	default:
		return Entry{}, "", false, apierr.New("invalid_operation", "unknown operation kind %q", e.Kind)
	}
	if (e.Kind == Restore) != (e.Source != nil) {
		return Entry{}, "", false, apierr.New("invalid_operation", "only a restore names a source entry")
	}
	if e.At == "" {
		e.At = schema.Timestamp(time.Now())
	}
	raw, err := canon.Marshal(e)
	if err != nil {
		return Entry{}, "", false, err
	}
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return Entry{}, "", false, err
	}
	line := slices.Concat(raw, []byte("\n"))
	_, err = f.Write(line)
	if err == nil && durable {
		err = f.Sync()
		if err == nil && !l.linked {
			err = atomicfile.SyncDir(filepath.Dir(l.path))
			l.linked = err == nil
		}
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		// Never leave a partial line for the next append to follow.
		if terr := os.Truncate(l.path, l.size); terr != nil {
			return Entry{}, "", false, errors.Join(err, terr)
		}
		return Entry{}, "", false, err
	}
	l.size += int64(len(line))
	h := canon.Hash(raw)
	l.entries = append(l.entries, e)
	l.hashes = append(l.hashes, h)
	l.index[h] = len(l.entries) - 1
	return e, h, true, nil
}

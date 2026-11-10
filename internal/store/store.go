// Package store is Ledger's native content-addressed object store
// (ADR 0025): exact file bytes, chunked large files, canonical trees and
// symbol maps, stored as loose uncompressed objects.
package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/CoachCoe/Ledger/internal/apierr"
)

const (
	KindBlob        = "blob"
	KindChunkedBlob = "chunked_blob"
	KindTree        = "tree"
	KindSymbolMap   = "symbol_map"
)

const idPrefix = "ledger-object-v2\n"

// Store holds objects under dir/<aa>/<rest of id>. Each file holds the
// kind, a newline and the object's canonical bytes.
type Store struct {
	dir string
	// sensitive is the repository's case policy, detected at init.
	sensitive bool
}

// Open returns the store in dir, creating it if needed.
func Open(dir string, caseSensitive bool) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir, sensitive: caseSensitive}, nil
}

// ID returns the object id of data stored as kind.
func ID(kind string, data []byte) string {
	h := sha256.New()
	h.Write([]byte(idPrefix + kind + "\n"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func validID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (s *Store) path(id string) string { return filepath.Join(s.dir, id[:2], id[2:]) }

// Get returns the kind and bytes of object id after checking them against
// the id.
func (s *Store) Get(id string) (string, []byte, error) {
	if !validID(id) {
		return "", nil, apierr.New("invalid_object_id", "invalid object id %q", id)
	}
	raw, err := os.ReadFile(s.path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil, apierr.New("object_missing", "object %s is not in the store", id).With("object", id)
	}
	if err != nil {
		return "", nil, err
	}
	kind, data, ok := decode(raw)
	if !ok || ID(kind, data) != id {
		return "", nil, apierr.New("object_corrupt", "object %s does not match its id", id).With("object", id)
	}
	return kind, data, nil
}

// Has reports whether a valid copy of object id is stored.
func (s *Store) Has(id string) (bool, error) {
	_, _, err := s.Get(id)
	if apierr.HasCode(err, "object_missing") || apierr.HasCode(err, "object_corrupt") {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) getKind(id, kind string) ([]byte, error) {
	k, data, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if k != kind {
		return nil, apierr.New("object_kind", "object %s is a %s, not a %s", id, k, kind).With("object", id)
	}
	return data, nil
}

func decode(raw []byte) (string, []byte, bool) {
	kind, data, ok := bytes.Cut(raw, []byte("\n"))
	return string(kind), data, ok
}

// Recover removes temporary files and objects that fail verification, as
// a crash before a batch's barrier can leave them. Nothing committed names
// such an object, and the next write of it recreates it.
func (s *Store) Recover() (removed int, err error) {
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return 0, err
	}
	defer func() { _ = root.Close() }()
	fans, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, err
	}
	for _, fan := range fans {
		if !fan.IsDir() {
			continue
		}
		names, err := os.ReadDir(filepath.Join(s.dir, fan.Name()))
		if err != nil {
			return removed, err
		}
		for _, n := range names {
			id := fan.Name() + n.Name()
			if !strings.HasPrefix(n.Name(), ".tmp-") && validID(id) {
				if _, _, err := s.Get(id); !apierr.HasCode(err, "object_corrupt") {
					if err != nil {
						return removed, err
					}
					continue
				}
			}
			if err := root.Remove(filepath.Join(fan.Name(), n.Name())); err != nil {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}

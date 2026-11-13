package verify

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/gitx"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// RecordsRef is the Git ref that mirrors the record files.
const RecordsRef = "refs/ledger/records"

// Problem is a record that is present but cannot be accepted: corrupt
// bytes, a name that is not its hash, a malformed payload or a bad
// signature. Problems make integrity invalid, never unavailable.
type Problem struct {
	Hash  string `json:"hash"`
	Code  string `json:"code"`
	Error string `json:"error"`
}

// Corpus is a set of record objects read from files, the records ref or a
// bundle, with the committed groups derived from their receipts. Loading
// never consults SQLite.
type Corpus struct {
	Objects  map[string]envelope.Object
	Problems []Problem
	// Missing lists records named by a receipt but not present, sorted.
	Missing []string
	// Pending lists present records that belong to no complete group.
	Pending []string
	// Committed holds the receipts and members of complete groups.
	Committed map[string]bool
}

func newCorpus() *Corpus {
	return &Corpus{Objects: map[string]envelope.Object{}, Committed: map[string]bool{}}
}

// add parses one record stored under name (its claimed hash). Unreadable
// records are recorded as problems; an unsupported schema version stops
// loading, because such a record can never be interpreted safely.
func (c *Corpus) add(name string, raw []byte) error {
	o, err := envelope.ParseStored(raw, name)
	if err != nil {
		if apierr.HasCode(err, "unsupported_schema_version") {
			return err
		}
		e := apierr.As(err)
		c.Problems = append(c.Problems, Problem{Hash: name, Code: e.Code, Error: e.Message})
		return nil
	}
	c.Objects[o.Hash] = o
	return nil
}

// finish derives groups: a receipt and all its members present is
// committed; anything else is pending and the absent members are missing.
func (c *Corpus) finish() {
	missing := map[string]bool{}
	for h, o := range c.Objects {
		if o.Envelope.Kind != schema.KindOperationReceipt {
			continue
		}
		var r schema.OperationReceipt
		if err := o.Decode(&r); err != nil {
			e := apierr.As(err)
			c.Problems = append(c.Problems, Problem{Hash: h, Code: e.Code, Error: e.Message})
			continue
		}
		complete := true
		for _, m := range r.Members {
			if _, ok := c.Objects[m]; !ok {
				complete = false
				missing[m] = true
			}
		}
		if complete {
			c.Committed[h] = true
			for _, m := range r.Members {
				c.Committed[m] = true
			}
		}
	}
	for h := range missing {
		c.Missing = append(c.Missing, h)
	}
	slices.Sort(c.Missing)
	for h := range c.Objects {
		if !c.Committed[h] {
			c.Pending = append(c.Pending, h)
		}
	}
	slices.Sort(c.Pending)
	slices.SortFunc(c.Problems, func(a, b Problem) int { return strings.Compare(a.Hash+a.Code, b.Hash+b.Code) })
}

// objectName reports the record hash named by a path of the form
// aa/<62 hex>.json.
func objectName(rel string) (string, bool) {
	dir, file, ok := strings.Cut(rel, "/")
	if !ok || len(dir) != 2 || !strings.HasSuffix(file, ".json") || strings.Contains(file, "/") {
		return "", false
	}
	h := dir + strings.TrimSuffix(file, ".json")
	return h, isHex64(h)
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// LoadObjectsDir reads every well-named record file under dir (normally
// .ledger/objects).
func LoadObjectsDir(dir string) (*Corpus, error) {
	c := newCorpus()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		name, ok := objectName(filepath.ToSlash(rel))
		if !ok || !d.Type().IsRegular() {
			return nil
		}
		raw, err := os.ReadFile(p) //nolint:gosec // regular files under the caller's objects directory
		if err != nil {
			return err
		}
		return c.add(name, raw)
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, apierr.New("not_initialized", "%s does not exist", dir)
	}
	if err != nil {
		return nil, err
	}
	c.finish()
	return c, nil
}

// LoadRecordsRef reads the tree of refs/ledger/records. The error code
// records_ref_missing means the repository has no such ref.
func LoadRecordsRef(ctx context.Context, repo *gitx.Repo) (*Corpus, error) {
	tip, err := repo.ResolveRef(ctx, RecordsRef)
	if err != nil {
		return nil, err
	}
	if tip == "" {
		return nil, apierr.New("records_ref_missing", "%s does not exist", RecordsRef)
	}
	entries, err := repo.ListTree(ctx, tip)
	if err != nil {
		return nil, err
	}
	var blobs []string
	var names []string
	for _, e := range entries {
		if name, ok := objectName(e.Path); ok {
			blobs = append(blobs, e.Blob)
			names = append(names, name)
		}
	}
	contents, err := catBlobs(ctx, repo, blobs)
	if err != nil {
		return nil, err
	}
	c := newCorpus()
	for i, name := range names {
		if err := c.add(name, contents[i]); err != nil {
			return nil, err
		}
	}
	c.finish()
	return c, nil
}

// catBlobs reads many blobs with one `git cat-file --batch`.
func catBlobs(ctx context.Context, repo *gitx.Repo, blobs []string) ([][]byte, error) {
	if len(blobs) == 0 {
		return nil, nil
	}
	out, err := gitx.Run(ctx, repo.Dir, strings.NewReader(strings.Join(blobs, "\n")+"\n"), nil, "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	r := bufio.NewReader(bytes.NewReader(out))
	res := make([][]byte, 0, len(blobs))
	for range blobs {
		header, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		f := strings.Fields(header)
		if len(f) != 3 || f[1] != "blob" {
			return nil, apierr.New("dependency_unavailable", "blob unavailable: %s", strings.TrimSpace(header))
		}
		n, err := strconv.Atoi(f[2])
		if err != nil {
			return nil, err
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		if _, err := r.ReadByte(); err != nil {
			return nil, err
		}
		res = append(res, buf)
	}
	return res, nil
}

// decode unmarshals an object's payload as T, rejecting unknown members.
func decode[T any](o envelope.Object) (T, error) {
	var v T
	if err := o.Decode(&v); err != nil {
		return v, apierr.New("record_malformed", "%s: %v", o.Hash, err)
	}
	return v, nil
}

// decodeInto reads members of a payload leniently, for the fields every
// version shares, such as an entity id.
func decodeInto(o envelope.Object, v any) error {
	return json.Unmarshal(o.Envelope.Payload, v)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

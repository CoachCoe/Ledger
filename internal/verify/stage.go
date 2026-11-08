package verify

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/gitx"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// Bundle entry names.
const (
	InventoryName = "bundle.json"
	GitName       = "git/objects.bundle"
	recordsPrefix = "records/"
)

// Limits bound what a bundle may make Ledger read or write.
type Limits struct {
	MaxEntries        int
	MaxRecordBytes    int64
	MaxInventoryBytes int64
	MaxTotalBytes     int64
	MaxGitBytes       int64
}

// DefaultLimits are generous for real repositories and small enough that a
// hostile bundle cannot exhaust memory or disk.
func DefaultLimits() Limits {
	return Limits{MaxEntries: 200_000, MaxRecordBytes: 8 << 20, MaxInventoryBytes: 64 << 20, MaxTotalBytes: 4 << 30, MaxGitBytes: 4 << 30}
}

// Staged is an inert, validated extraction of a bundle. Records stay in
// memory; the Git objects live only in a bare repository inside Dir. No
// check, hook or configuration from the bundle ever runs or is read.
type Staged struct {
	Dir       string
	Inventory Inventory
	Records   map[string][]byte
	GitFile   string
	// Repo is the staged bare repository, nil if the Git part is absent or
	// could not be unpacked.
	Repo     *gitx.Repo
	Problems []Problem
	Size     Size
}

// Remove deletes the staging directory.
func (s *Staged) Remove() {
	if s != nil && s.Dir != "" {
		_ = os.RemoveAll(s.Dir)
	}
}

func unsafe(format string, args ...any) error {
	return apierr.New("bundle_unsafe", format, args...)
}

// Stage reads the bundle at path into the existing directory dir, which
// must be empty and private. Structural violations (unsafe names, entry
// types, sizes over limits, a truncated archive, an invalid inventory or an
// unknown version) fail before anything is published. Records whose bytes
// do not match their name are reported as Problems for the verifier.
func Stage(ctx context.Context, path, dir string, lim Limits) (*Staged, error) {
	if strings.HasSuffix(path, ".partial") {
		return nil, apierr.New("bundle_partial", "%s is an unfinished export and is never verified or imported", path)
	}
	f, err := os.Open(path) //nolint:gosec // the caller names the bundle
	if err != nil {
		return nil, apierr.New("bundle_unreadable", "%v", err)
	}
	defer f.Close()
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // a directory needs the owner execute bit
		return nil, err
	}
	st := &Staged{Dir: dir, Records: map[string][]byte{}}
	tr := tar.NewReader(f)
	seen := map[string]bool{}
	folded := map[string]bool{}
	var total int64
	var invRaw []byte
	var gitSHA string
	for n := 0; ; n++ {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			// archive/tar reports a clean EOF when a file is cut inside an
			// entry's padding, so require the end-of-archive marker.
			if err := endMarker(f); err != nil {
				return nil, err
			}
			break
		}
		if err != nil {
			return nil, apierr.New("bundle_corrupt", "reading archive: %v", err)
		}
		if n >= lim.MaxEntries {
			return nil, apierr.New("bundle_too_large", "more than %d entries", lim.MaxEntries)
		}
		name := hdr.Name
		switch {
		case hdr.Typeflag != tar.TypeReg:
			return nil, unsafe("entry %q is not a regular file", name)
		case len(hdr.PAXRecords) > 0 || hdr.Format == tar.FormatGNU:
			return nil, unsafe("entry %q uses extended headers", name)
		case hdr.Size < 0:
			return nil, unsafe("entry %q has a negative size", name)
		}
		if n == 0 && name != InventoryName {
			return nil, unsafe("the first entry must be %s", InventoryName)
		}
		var kind, hash string
		switch {
		case name == InventoryName:
			kind = "inventory"
		case name == GitName:
			kind = "git"
		case strings.HasPrefix(name, recordsPrefix):
			h, ok := objectName(strings.TrimPrefix(name, recordsPrefix))
			if !ok {
				return nil, unsafe("entry %q is not a record name", name)
			}
			kind, hash = "record", h
		default:
			return nil, unsafe("entry %q is not allowed", name)
		}
		if seen[name] || folded[strings.ToLower(name)] {
			return nil, unsafe("entry %q is a duplicate or alias", name)
		}
		seen[name], folded[strings.ToLower(name)] = true, true
		limit := map[string]int64{"inventory": lim.MaxInventoryBytes, "record": lim.MaxRecordBytes, "git": lim.MaxGitBytes}[kind]
		if hdr.Size > limit {
			return nil, apierr.New("bundle_too_large", "entry %q declares %d bytes, over the %d limit", name, hdr.Size, limit)
		}
		if total += hdr.Size; total > lim.MaxTotalBytes {
			return nil, apierr.New("bundle_too_large", "bundle exceeds %d bytes", lim.MaxTotalBytes)
		}
		switch kind {
		case "git":
			gitSHA, err = copyGit(tr, filepath.Join(dir, "objects.bundle"), hdr.Size)
			if err != nil {
				return nil, err
			}
			st.GitFile = filepath.Join(dir, "objects.bundle")
			st.Size.GitBytes = hdr.Size
		default:
			raw, err := io.ReadAll(io.LimitReader(tr, limit+1))
			if err != nil {
				return nil, apierr.New("bundle_corrupt", "reading %q: %v", name, err)
			}
			if int64(len(raw)) != hdr.Size {
				return nil, apierr.New("bundle_corrupt", "entry %q declares %d bytes but holds %d", name, hdr.Size, len(raw))
			}
			if kind == "inventory" {
				invRaw = raw
			} else {
				st.Records[hash] = raw
				st.Size.RecordBytes += hdr.Size
			}
		}
	}
	if invRaw == nil {
		return nil, apierr.New("bundle_corrupt", "no %s", InventoryName)
	}
	if err := schema.ValidateBundle(invRaw); err != nil {
		return nil, err
	}
	if err := unmarshalInventory(invRaw, &st.Inventory); err != nil {
		return nil, err
	}
	st.Size.Records = len(st.Records)
	st.Size.Commits = len(st.Inventory.Git.Commits)
	st.Size.BundleBytes = total
	st.checkInventory(gitSHA)
	if st.GitFile != "" && st.Inventory.Git.File != "" {
		st.unpackGit(ctx)
	}
	return st, nil
}

// endMarker requires the archive to end with the two zero blocks tar writers
// emit, so a file truncated inside an entry's padding is corrupt, not short.
func endMarker(f *os.File) error {
	st, err := f.Stat()
	if err != nil {
		return apierr.New("bundle_unreadable", "%v", err)
	}
	const marker = 2 * 512
	if st.Size() < marker || st.Size()%512 != 0 {
		return apierr.New("bundle_corrupt", "the archive is truncated")
	}
	tail := make([]byte, marker)
	if _, err := f.ReadAt(tail, st.Size()-marker); err != nil {
		return apierr.New("bundle_corrupt", "reading archive end: %v", err)
	}
	for _, b := range tail {
		if b != 0 {
			return apierr.New("bundle_corrupt", "the archive has no end-of-archive marker")
		}
	}
	return nil
}

func copyGit(r io.Reader, dst string, size int64) (string, error) {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // inside the private staging directory
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(r, size+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", apierr.New("bundle_corrupt", "reading the Git part: %v", err)
	}
	if n != size {
		return "", apierr.New("bundle_corrupt", "the Git part declares %d bytes but holds %d", size, n)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// checkInventory compares the inventory with the entries. Records the
// inventory lists but the archive lacks are missing (unavailable); entries
// the inventory does not list, or that disagree with it, are problems.
func (s *Staged) checkInventory(gitSHA string) {
	listed := map[string]InvObject{}
	for _, o := range s.Inventory.Objects {
		listed[o.Hash] = o
	}
	for _, h := range sortedKeys(s.Records) {
		raw := s.Records[h]
		e, ok := listed[h]
		switch {
		case !ok:
			s.Problems = append(s.Problems, Problem{Hash: h, Code: "unlisted_record", Error: "record is not in the inventory"})
		case int64(len(raw)) != e.Size:
			s.Problems = append(s.Problems, Problem{Hash: h, Code: "inventory_mismatch", Error: "record size differs from the inventory"})
		}
	}
	switch g := s.Inventory.Git; {
	case g.File == "" && s.GitFile != "":
		s.Problems = append(s.Problems, Problem{Hash: "git", Code: "unlisted_git", Error: "the archive has a Git part the inventory does not list"})
	case g.File != "" && s.GitFile != "" && (g.SHA256 != gitSHA || g.Size != s.Size.GitBytes):
		s.Problems = append(s.Problems, Problem{Hash: "git", Code: "git_digest_mismatch", Error: "the Git part differs from its inventory digest"})
		s.GitFile = ""
	}
}

// Missing lists records the inventory names that the archive lacks.
func (s *Staged) Missing() []string {
	var out []string
	for _, o := range s.Inventory.Objects {
		if _, ok := s.Records[o.Hash]; !ok {
			out = append(out, o.Hash)
		}
	}
	if s.Inventory.Git.File != "" && s.GitFile == "" && !slices.ContainsFunc(s.Problems, func(p Problem) bool { return p.Hash == "git" }) {
		out = append(out, "git")
	}
	slices.Sort(out)
	return out
}

// gitEnv isolates Git from user and system configuration.
var gitEnv = []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0"}

// unpackGit verifies the Git part and fetches it into a bare repository
// inside the staging directory, with hooks off and object checking on.
func (s *Staged) unpackGit(ctx context.Context) {
	bare := filepath.Join(s.Dir, "repo.git")
	problem := func(err error) {
		s.Problems = append(s.Problems, Problem{Hash: "git", Code: "git_bundle_invalid", Error: err.Error()})
	}
	if _, err := gitx.Run(ctx, s.Dir, nil, gitEnv, "init", "--bare", "-q", bare); err != nil {
		problem(err)
		return
	}
	c := []string{"-c", "transfer.fsckObjects=true", "-c", "core.hooksPath=/dev/null"}
	if _, err := gitx.Run(ctx, bare, nil, gitEnv, append(c, "bundle", "verify", s.GitFile)...); err != nil {
		problem(err)
		return
	}
	if _, err := gitx.Run(ctx, bare, nil, gitEnv, append(c, "fetch", "--no-tags", "-q", s.GitFile, "+refs/*:refs/*")...); err != nil {
		problem(err)
		return
	}
	s.Repo = &gitx.Repo{Dir: bare, CommonDir: bare, MainDir: bare}
}

// Corpus builds the verification corpus from the staged records.
func (s *Staged) Corpus() (*Corpus, error) {
	c := newCorpus()
	for _, h := range sortedKeys(s.Records) {
		if err := c.add(h, s.Records[h]); err != nil {
			return nil, err
		}
	}
	c.finish()
	return c, nil
}

package bridge

import (
	"bytes"
	"context"
	"encoding/hex"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/store"
)

// Commit is a Git commit as the bridge records it.
type Commit struct {
	SHA       string
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

// Imported is one commit of imported history and its native tree.
type Imported struct {
	Commit
	NativeTree string
}

// fetchRef is where Fetch leaves the fetched tip in the staging repository.
const fetchRef = "refs/bridge/fetch"

// Fetch copies ref and its history from the Git repository at src into
// the staging repository and returns the tip commit. Only local
// repositories are read; nothing configured in src runs.
func (s *Staging) Fetch(ctx context.Context, src, ref string) (string, error) {
	if strings.HasPrefix(ref, "-") || strings.ContainsAny(ref, ":\x00\n ") {
		return "", apierr.New("invalid_ref", "invalid ref %q", ref)
	}
	if _, err := s.Git(ctx, nil, "fetch", "-q", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", "--", src, "+"+ref+":"+fetchRef); err != nil {
		return "", apierr.New("ref_unavailable", "cannot fetch %s from %s: %v", ref, src, err)
	}
	out, err := s.Git(ctx, nil, "rev-parse", "--verify", fetchRef+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// FirstParents returns the first-parent history of commit, oldest first.
func (s *Staging) FirstParents(ctx context.Context, commit string) ([]string, error) {
	out, err := s.Git(ctx, nil, "rev-list", "--first-parent", "--reverse", commit)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// Importer converts Git commits in the staging repository into native
// trees. Each Git tree is converted once.
type Importer struct {
	staging *Staging
	batch   *store.Batch
	cat     *catFile
	trees   map[string]string // Git tree → native tree
	blobs   map[string]string // Git blob → native file
}

// NewImporter starts reading from the staging repository into batch. The
// caller commits the batch and closes the importer.
func (s *Staging) NewImporter(ctx context.Context, batch *store.Batch) (*Importer, error) {
	cat, err := s.catFile(ctx)
	if err != nil {
		return nil, err
	}
	return &Importer{staging: s, batch: batch, cat: cat, trees: map[string]string{}, blobs: map[string]string{}}, nil
}

// Close stops the importer's reader.
func (im *Importer) Close() error { return im.cat.close() }

// Commit converts one commit.
func (im *Importer) Commit(sha string) (Imported, error) {
	typ, data, err := im.cat.read(sha)
	if err != nil {
		return Imported{}, err
	}
	if typ != "commit" {
		return Imported{}, apierr.New("invalid_commit", "%s is a %s, not a commit", sha, typ)
	}
	c, err := parseCommit(sha, data)
	if err != nil {
		return Imported{}, err
	}
	tree, err := im.tree(c.Tree, "")
	if err != nil {
		return Imported{}, err
	}
	return Imported{Commit: c, NativeTree: tree}, nil
}

// parseCommit reads a commit object. Text that is not UTF-8 is made
// valid, since records hold UTF-8 only; the tree is what must round-trip.
func parseCommit(sha string, data []byte) (Commit, error) {
	headers, message, _ := bytes.Cut(data, []byte("\n\n"))
	c := Commit{SHA: sha, Parents: []string{}, Message: strings.ToValidUTF8(string(message), "�")}
	for line := range strings.SplitSeq(string(headers), "\n") {
		key, value, _ := strings.Cut(line, " ")
		switch key {
		case "tree":
			c.Tree = value
		case "parent":
			c.Parents = append(c.Parents, value)
		case "author":
			c.Author = strings.ToValidUTF8(value, "�")
		case "committer":
			c.Committer = strings.ToValidUTF8(value, "�")
		}
	}
	if c.Tree == "" || c.Author == "" || c.Committer == "" {
		return Commit{}, apierr.New("invalid_commit", "commit %s lacks a tree, author or committer", sha)
	}
	return c, nil
}

// tree converts the Git tree sha at dir (for error messages) into a native
// tree. Names must already be NFC: changing one would change the tree, and
// the round trip must be exact.
func (im *Importer) tree(sha, dir string) (string, error) {
	if id, ok := im.trees[sha]; ok {
		return id, nil
	}
	typ, data, err := im.cat.read(sha)
	if err != nil {
		return "", err
	}
	if typ != "tree" {
		return "", apierr.New("invalid_tree", "%s is a %s, not a tree", sha, typ)
	}
	idLen := 20
	if im.staging.Format == "sha256" {
		idLen = 32
	}
	var entries []store.Entry
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		nul := bytes.IndexByte(data, 0)
		if sp < 0 || nul < sp || len(data) < nul+1+idLen {
			return "", apierr.New("invalid_tree", "Git tree %s is malformed", sha)
		}
		mode, name := string(data[:sp]), string(data[sp+1:nul])
		id := hex.EncodeToString(data[nul+1 : nul+1+idLen])
		data = data[nul+1+idLen:]
		p := name
		if dir != "" {
			p = dir + "/" + name
		}
		if !utf8.ValidString(name) || !norm.NFC.IsNormalString(name) {
			return "", apierr.New("unsupported_path", "path %q is not NFC UTF-8, which the native store requires", p).With("path", p)
		}
		e := store.Entry{Name: name}
		switch mode {
		case "100644", "100664":
			e.Type = store.TypeFile
		case "100755":
			e.Type = store.TypeExecutable
		case "120000":
			e.Type = store.TypeSymlink
		case "40000":
			e.Type = store.TypeTree
		case "160000":
			e.Type, e.ID = store.TypeGitlink, id
		default:
			return "", apierr.New("unsupported_mode", "path %q has Git mode %s", p, mode).With("path", p)
		}
		switch e.Type {
		case store.TypeTree:
			if e.ID, err = im.tree(id, p); err != nil {
				return "", err
			}
		case store.TypeFile, store.TypeExecutable, store.TypeSymlink:
			if e.ID, err = im.blob(id); err != nil {
				return "", err
			}
		}
		entries = append(entries, e)
	}
	out, err := im.batch.WriteTree(entries)
	if err != nil {
		return "", err
	}
	im.trees[sha] = out
	return out, nil
}

func (im *Importer) blob(sha string) (string, error) {
	if id, ok := im.blobs[sha]; ok {
		return id, nil
	}
	typ, data, err := im.cat.read(sha)
	if err != nil {
		return "", err
	}
	if typ != "blob" {
		return "", apierr.New("invalid_tree", "%s is a %s, not a blob", sha, typ)
	}
	id, err := im.batch.PutFile(data)
	if err != nil {
		return "", err
	}
	im.blobs[sha] = id
	return id, nil
}

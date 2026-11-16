package bridge

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/store"
)

// Out is one commit to write: Tree on top of Parent, a Git commit whose
// tree is the native ParentTree, or a root commit when Parent is empty.
type Out struct {
	Tree       string
	Parent     string
	ParentTree string
	Author     string
	Committer  string
	Message    string
}

// Written is a commit the exporter wrote.
type Written struct {
	SHA  string
	Tree string // Git tree
}

// Export writes outs into the staging repository, in order, with git
// fast-import. A commit may name an earlier commit of the same call as its
// parent with ":<index+1>". The same inputs always produce the same
// commits: every identity, date and message comes from outs.
func (s *Staging) Export(ctx context.Context, objects *store.Store, outs []Out) ([]Written, error) {
	if len(outs) == 0 {
		return nil, nil
	}
	tmp, err := os.MkdirTemp(s.Dir, "export-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	marks := filepath.Join(tmp, "marks")
	var w bytes.Buffer
	blobs := map[string]int{}
	next := len(outs) + 1
	blob := func(id string) (string, error) {
		if m, ok := blobs[id]; ok {
			return fmt.Sprintf(":%d", m), nil
		}
		var data bytes.Buffer
		if err := objects.WriteFile(id, &data); err != nil {
			return "", err
		}
		m := next
		next++
		blobs[id] = m
		fmt.Fprintf(&w, "blob\nmark :%d\ndata %d\n", m, data.Len())
		w.Write(data.Bytes())
		w.WriteString("\n")
		return fmt.Sprintf(":%d", m), nil
	}
	modes := map[string]string{store.TypeFile: "100644", store.TypeExecutable: "100755", store.TypeSymlink: "120000", store.TypeGitlink: "160000"}
	for i, o := range outs {
		if err := ident(o.Author); err != nil {
			return nil, err
		}
		if err := ident(o.Committer); err != nil {
			return nil, err
		}
		var changes []store.Change
		if o.ParentTree == "" {
			files, err := objects.Files(o.Tree)
			if err != nil {
				return nil, err
			}
			for _, f := range files {
				changes = append(changes, store.Change{Path: f.Path, Status: "A", New: f})
			}
		} else if changes, err = objects.Diff(o.ParentTree, o.Tree); err != nil {
			return nil, err
		}
		var files strings.Builder
		for _, c := range changes {
			if c.Status == "D" {
				fmt.Fprintf(&files, "D %s\n", quote(c.Path))
				continue
			}
			if c.Status == "T" {
				fmt.Fprintf(&files, "D %s\n", quote(c.Path))
			}
			ref := c.New.ID
			if c.New.Type != store.TypeGitlink {
				if ref, err = blob(c.New.ID); err != nil {
					return nil, err
				}
			}
			fmt.Fprintf(&files, "M %s %s %s\n", modes[c.New.Type], ref, quote(c.Path))
		}
		fmt.Fprintf(&w, "reset %s\n", exportRef)
		fmt.Fprintf(&w, "commit %s\nmark :%d\nauthor %s\ncommitter %s\ndata %d\n%s\n", exportRef, i+1, o.Author, o.Committer, len(o.Message), o.Message)
		if o.Parent != "" {
			fmt.Fprintf(&w, "from %s\n", o.Parent)
		}
		w.WriteString(files.String())
		w.WriteString("\n")
	}
	w.WriteString("done\n")
	if _, err := s.Git(ctx, &w, "fast-import", "--quiet", "--done", "--date-format=raw", "--export-marks="+marks); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(marks)
	if err != nil {
		return nil, err
	}
	byMark := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		mark, sha, _ := strings.Cut(line, " ")
		byMark[mark] = sha
	}
	out := make([]Written, len(outs))
	for i := range outs {
		sha := byMark[fmt.Sprintf(":%d", i+1)]
		tree, err := s.Git(ctx, nil, "rev-parse", "--verify", sha+"^{tree}")
		if err != nil {
			return nil, err
		}
		out[i] = Written{SHA: sha, Tree: strings.TrimSpace(string(tree))}
	}
	return out, nil
}

// exportRef is the staging ref fast-import commits on; each commit names
// its parent explicitly.
const exportRef = "refs/bridge/export"

// ident checks a Git ident line: "Name <email> seconds zone".
func ident(s string) error {
	if strings.ContainsAny(s, "\n\x00") || !strings.Contains(s, "<") || !strings.Contains(s, ">") {
		return apierr.New("invalid_ident", "invalid Git ident %q", s)
	}
	return nil
}

// quote writes a path for fast-import, quoting it C-style when it would
// otherwise be misread.
func quote(p string) string {
	if !strings.ContainsAny(p, "\"\\\n") && !strings.HasPrefix(p, " ") {
		return p
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := range len(p) {
		switch c := p[i]; c {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

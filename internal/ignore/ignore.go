// Package ignore matches paths against gitignore-syntax rule files
// (.gitignore and .ledgerignore) without Git.
package ignore

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
)

// Files are the rule files read in each directory, lowest precedence first.
var Files = []string{".gitignore", ".ledgerignore"}

type rule struct {
	re      *regexp.Regexp
	negate  bool
	dirOnly bool
	// base matches the final path component only: the pattern had no
	// slash except a trailing one.
	base bool
	dir  string // directory of the rule file, "" for the root
}

// parse compiles the rules of one file found in directory dir.
func parse(dir string, content []byte, fold bool) []rule {
	var out []rule
	for line := range strings.SplitSeq(string(content), "\n") {
		line = strings.TrimSuffix(line, "\r")
		line = trimTrailingSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		r := rule{dir: dir}
		if line[0] == '!' {
			r.negate, line = true, line[1:]
		} else if strings.HasPrefix(line, `\!`) || strings.HasPrefix(line, `\#`) {
			line = line[1:]
		}
		if strings.HasSuffix(line, "/") {
			r.dirOnly, line = true, strings.TrimSuffix(line, "/")
		}
		if line == "" {
			continue
		}
		r.base = !strings.Contains(line, "/")
		line = strings.TrimPrefix(line, "/")
		expr := "^" + translate(line) + "$"
		if fold {
			expr = "(?i)" + expr
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			continue // Git ignores a pattern it cannot parse
		}
		r.re = re
		out = append(out, r)
	}
	return out
}

func trimTrailingSpace(s string) string {
	for strings.HasSuffix(s, " ") && !strings.HasSuffix(s, `\ `) {
		s = s[:len(s)-1]
	}
	return s
}

// translate turns a wildmatch pattern into a regular expression. "**" is
// special only as a whole path component: "**/x", "x/**" and "x/**/y".
func translate(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '*' && strings.HasPrefix(p[i:], "**") && (i == 0 || p[i-1] == '/') && (i+2 == len(p) || p[i+2] == '/'):
			switch {
			case i+2 == len(p):
				b.WriteString(".*")
				i++
			default: // "**/": zero or more directories
				b.WriteString("(?:.*/)?")
				i += 2
			}
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		case c == '\\' && i+1 < len(p):
			i++
			b.WriteString(regexp.QuoteMeta(p[i : i+1]))
		case c == '[':
			end := classEnd(p, i)
			if end < 0 {
				b.WriteString(`\[`)
				continue
			}
			class := p[i+1 : end]
			if strings.HasPrefix(class, "!") || strings.HasPrefix(class, "^") {
				class = "^" + class[1:]
			}
			class = strings.ReplaceAll(class, `\`, `\\`)
			b.WriteString("[" + class + "]")
			i = end
		default:
			b.WriteString(regexp.QuoteMeta(p[i : i+1]))
		}
	}
	return b.String()
}

// classEnd returns the index of the "]" closing the class opened at i.
func classEnd(p string, i int) int {
	j := i + 1
	if j < len(p) && (p[j] == '!' || p[j] == '^') {
		j++
	}
	if j < len(p) && p[j] == ']' {
		j++
	}
	for ; j < len(p); j++ {
		switch {
		case p[j] == ']':
			return j
		case strings.HasPrefix(p[j:], "[:"):
			if k := strings.Index(p[j+2:], ":]"); k >= 0 {
				j += k + 3
			}
		case p[j] == '\\':
			j++
		}
	}
	return -1
}

// match reports whether rel (relative to the root) is decided by r, and
// whether it is then ignored.
func (r rule) match(rel string, isDir bool) bool {
	if r.dirOnly && !isDir {
		return false
	}
	if r.dir != "" {
		var ok bool
		if rel, ok = strings.CutPrefix(rel, r.dir+"/"); !ok {
			return false
		}
	}
	if r.base {
		rel = path.Base(rel)
	}
	return r.re.MatchString(rel)
}

// Tree answers ignore questions for one directory tree, loading each
// directory's rule files once. A path is ignored when it, or any
// directory above it, is: Git never descends into an ignored directory,
// so nothing inside one can be re-included.
type Tree struct {
	root   string
	fold   bool
	always map[string]bool
	mu     sync.Mutex
	rules  map[string][]rule // directory → its own rules
}

// New returns a Tree for root. Patterns match case-insensitively unless
// caseSensitive, as Git's core.ignorecase does. always names top-level
// entries that are ignored whatever the rules say, such as .git and .ledger.
func New(root string, caseSensitive bool, always ...string) *Tree {
	t := &Tree{root: root, fold: !caseSensitive, always: map[string]bool{}, rules: map[string][]rule{}}
	for _, a := range always {
		t.always[a] = true
	}
	return t
}

// Invalidate forgets cached rules, after a rule file changed.
func (t *Tree) Invalidate() {
	t.mu.Lock()
	defer t.mu.Unlock()
	clear(t.rules)
}

func (t *Tree) dirRules(dir string) ([]rule, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if rs, ok := t.rules[dir]; ok {
		return rs, nil
	}
	var rs []rule
	for _, name := range Files {
		raw, err := os.ReadFile(filepath.Join(t.root, filepath.FromSlash(dir), name))
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			continue
		}
		if err != nil {
			return nil, err
		}
		rs = append(rs, parse(dir, raw, t.fold)...)
	}
	t.rules[dir] = rs
	return rs, nil
}

// Ignored reports whether rel, a slash-separated path relative to the
// root, is ignored. isDir says whether rel itself is a directory.
func (t *Tree) Ignored(rel string, isDir bool) (bool, error) {
	parts := strings.Split(rel, "/")
	if t.always[parts[0]] {
		return true, nil
	}
	var active []rule
	for i := range parts {
		dir := strings.Join(parts[:i], "/")
		rs, err := t.dirRules(dir)
		if err != nil {
			return false, err
		}
		active = append(active, rs...)
		if decide(active, strings.Join(parts[:i+1], "/"), i < len(parts)-1 || isDir) {
			return true, nil
		}
	}
	return false, nil
}

// decide applies rules in order; the last one that matches wins.
func decide(rules []rule, rel string, isDir bool) bool {
	for i := len(rules) - 1; i >= 0; i-- {
		if rules[i].match(rel, isDir) {
			return !rules[i].negate
		}
	}
	return false
}

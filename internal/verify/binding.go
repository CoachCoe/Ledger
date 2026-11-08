package verify

import (
	"strconv"
	"strings"

	"github.com/CoachCoe/Ledger/internal/gitx"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// Condition results for one commit binding check.
const (
	BindingOK          = "ok"
	BindingMismatch    = "mismatch"
	BindingUnavailable = "unavailable"
)

// Binding reports the three independent conditions that bind a commit to a
// manifest (AC-057). Each is "ok", "mismatch" or "unavailable".
type Binding struct {
	Trailer string   `json:"trailer"`
	Tree    string   `json:"tree"`
	Parent  string   `json:"parent"`
	Detail  []string `json:"detail,omitempty"`
}

// OK reports whether all three conditions hold.
func (b Binding) OK() bool {
	return b.Trailer == BindingOK && b.Tree == BindingOK && b.Parent == BindingOK
}

// CheckBinding evaluates the trailer, tree and parent conditions
// separately. A zero commit (object not available) makes all three
// "unavailable", never "mismatch".
func CheckBinding(c gitx.Commit, manifestHash string, m schema.ChangeManifest) Binding {
	if c.SHA == "" {
		return Binding{Trailer: BindingUnavailable, Tree: BindingUnavailable, Parent: BindingUnavailable, Detail: []string{"commit object unavailable"}}
	}
	b := Binding{Trailer: BindingOK, Tree: BindingOK, Parent: BindingOK}
	if n, ok := trailer(c.Message, manifestHash); !ok {
		b.Trailer = BindingMismatch
		b.Detail = append(b.Detail, "Ledger-Manifest trailer does not name the manifest exactly once ("+strconv.Itoa(n)+" trailers)")
	}
	if c.Tree != m.Tree {
		b.Tree = BindingMismatch
		b.Detail = append(b.Detail, "commit tree "+c.Tree+" differs from manifest tree "+m.Tree)
	}
	if len(c.Parents) != 1 || c.Parents[0] != m.BaseCommit {
		b.Parent = BindingMismatch
		b.Detail = append(b.Detail, "commit parents do not consist solely of the manifest base commit")
	}
	return b
}

// trailer parses the last paragraph literally (no git trailer
// configuration) and reports the number of Ledger-Manifest lines and
// whether there is exactly one naming hash.
func trailer(message, hash string) (int, bool) {
	paragraph := strings.TrimRight(message, "\n")
	if i := strings.LastIndex(paragraph, "\n\n"); i >= 0 {
		paragraph = paragraph[i+2:]
	}
	count, match := 0, false
	for line := range strings.SplitSeq(paragraph, "\n") {
		if value, ok := strings.CutPrefix(line, "Ledger-Manifest:"); ok {
			count++
			match = strings.TrimSpace(value) == hash
		}
	}
	return count, count == 1 && match
}

package index

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"slices"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// LineHead is where a line stands: the last accepted advance, or none, and
// the tree it leaves the line at.
type LineHead struct {
	Advance *string `json:"advance"`
	Tree    string  `json:"tree"`
}

// Repository returns the native repository's creation record.
func (x *Index) Repository(ctx context.Context) (schema.Repository, error) {
	return repository(ctx, x.conn())
}

// LineHead returns the current head of line.
func (x *Index) LineHead(ctx context.Context, line string) (LineHead, error) {
	return lineHead(ctx, x.conn(), line)
}

func repository(ctx context.Context, q querier) (schema.Repository, error) {
	var r schema.Repository
	rows, err := q.QueryContext(ctx, `SELECT version, payload FROM creations WHERE kind = 'repository'`)
	if err != nil {
		return r, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var version int
		var payload string
		if err := rows.Scan(&version, &payload); err != nil {
			return r, err
		}
		if err := schema.Decode(version, schema.KindRepository, []byte(payload), &r); err != nil {
			return r, err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return r, err
	}
	switch n {
	case 0:
		return r, apierr.New("not_native", "this repository has no repository record")
	case 1:
		return r, nil
	}
	return r, apierr.New("conflicting_creation", "%d repository records", n)
}

// Advance is a committed line advance and its object hash.
type Advance struct {
	Hash string
	schema.LineAdvance
}

// LineAdvances returns line's advances from its first to its head.
func (x *Index) LineAdvances(ctx context.Context, line string) ([]Advance, error) {
	return lineAdvances(ctx, x.conn(), line)
}

// lineAdvances follows line's advances from its start. Each advance names
// its predecessor and the validator admits one successor per head, so two
// advances from one head arrive only by import and leave the line forked.
func lineAdvances(ctx context.Context, q querier, line string) ([]Advance, error) {
	rows, err := q.QueryContext(ctx, `SELECT hash, payload FROM objects WHERE kind = 'line_advance' AND json_extract(payload, '$.line') = ?`, line)
	if err != nil {
		return nil, err
	}
	next := map[string]Advance{}
	for rows.Next() {
		var a Advance
		var payload string
		if err := rows.Scan(&a.Hash, &payload); err != nil {
			rows.Close()
			return nil, err
		}
		if err := json.Unmarshal([]byte(payload), &a.LineAdvance); err != nil {
			rows.Close()
			return nil, err
		}
		from := ""
		if a.Previous != nil {
			from = *a.Previous
		}
		if other, ok := next[from]; ok {
			rows.Close()
			return nil, apierr.New("line_fork", "line %s has two advances from one head: %s and %s", line, min(a.Hash, other.Hash), max(a.Hash, other.Hash)).With("line", line)
		}
		next[from] = a
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var out []Advance
	for at := ""; ; {
		a, ok := next[at]
		if !ok {
			break
		}
		if len(out) >= len(next) {
			return nil, apierr.New("line_fork", "line %s does not chain", line).With("line", line)
		}
		out = append(out, a)
		at = a.Hash
	}
	return out, nil
}

func lineHead(ctx context.Context, q querier, line string) (LineHead, error) {
	advances, err := lineAdvances(ctx, q, line)
	if err != nil {
		return LineHead{}, err
	}
	if len(advances) == 0 {
		r, err := repository(ctx, q)
		if err != nil {
			return LineHead{}, err
		}
		return LineHead{Tree: r.Tree}, nil
	}
	head := advances[len(advances)-1]
	tree, err := acceptedTree(ctx, q, head.LineAdvance)
	if err != nil {
		return LineHead{}, err
	}
	return LineHead{Advance: &head.Hash, Tree: tree}, nil
}

// acceptedTree is the tree a committed advance accepted: its manifest's,
// or its foreign change's.
func acceptedTree(ctx context.Context, q querier, a schema.LineAdvance) (string, error) {
	if a.ForeignChange != "" {
		var raw string
		err := q.QueryRowContext(ctx, `SELECT payload FROM objects WHERE hash=? AND kind='foreign_change'`, a.ForeignChange).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return "", apierr.New("unknown_entity", "no foreign change %s", a.ForeignChange)
		}
		if err != nil {
			return "", err
		}
		var f schema.ForeignChange
		return f.Tree, schema.Decode(2, schema.KindForeignChange, []byte(raw), &f)
	}
	m, err := manifestIn(ctx, q, a.Manifest)
	return m.Tree, err
}

// advanceTree is acceptedTree for an advance that may be in the group
// being validated.
func (v *validator) advanceTree(a schema.LineAdvance) (string, error) {
	if a.ForeignChange != "" {
		var f schema.ForeignChange
		err := v.object(a.ForeignChange, schema.KindForeignChange, &f)
		return f.Tree, err
	}
	m, err := manifestIn(v.ctx, v.q, a.Manifest)
	return m.Tree, err
}

func (v *validator) checkRepository(p *schema.Repository, signer string) error {
	if isSession(signer) {
		return unauthorized("only root creates the repository record")
	}
	if _, err := repository(v.ctx, v.q); err == nil {
		return apierr.New("entity_exists", "the repository record already exists")
	} else if !apierr.HasCode(err, "not_native") {
		return err
	}
	if v.count(schema.KindRepository, func(envelope.Object) bool { return true }) > 1 {
		return apierr.New("entity_exists", "the repository record is created twice in one operation")
	}
	if (p.CasePolicy == "sensitive") != v.sensitive {
		return apierr.New("invalid_repository", "case policy %s differs from the store's", p.CasePolicy)
	}
	return nil
}

// checkLineAdvance admits an advance from the line's current head only, so
// advances under the RecordStore lock form a chain (P2-AC-023).
func (v *validator) checkLineAdvance(hash string, p *schema.LineAdvance, signer string) error {
	if p.Actor != signer {
		return unauthorized("advance actor %s is not its signer %s", p.Actor, signer)
	}
	if isSession(signer) {
		c, err := v.sessionCapability(signer)
		if err != nil {
			return err
		}
		if !slices.Contains(c.MayAdvance, p.Line) {
			return unauthorized("session %s may not advance line %s", signer, p.Line)
		}
	}
	head, ok := v.lines[p.Line]
	if !ok {
		var err error
		if head, err = lineHead(v.ctx, v.q, p.Line); err != nil {
			return err
		}
	}
	if (p.Previous == nil) != (head.Advance == nil) || (p.Previous != nil && *p.Previous != *head.Advance) {
		want := "null"
		if head.Advance != nil {
			want = *head.Advance
		}
		return apierr.New("stale_line", "line %s has moved; its head is %s", p.Line, want).With("line", p.Line).With("head", want)
	}
	tree, err := v.advanced(p, signer, head)
	if err != nil {
		return err
	}
	if v.lines == nil {
		v.lines = map[string]LineHead{}
	}
	v.lines[p.Line] = LineHead{Advance: &hash, Tree: tree}
	return nil
}

// advanced checks what an advance accepts against the line's head and
// returns the tree it moves the line to: a selected manifest based on the
// head, or, for root only, a foreign change on top of it.
func (v *validator) advanced(p *schema.LineAdvance, signer string, head LineHead) (string, error) {
	if p.ForeignChange != "" {
		if isSession(signer) {
			return "", unauthorized("only root accepts a foreign change")
		}
		var f schema.ForeignChange
		if err := v.object(p.ForeignChange, schema.KindForeignChange, &f); err != nil {
			return "", err
		}
		if f.Line != p.Line {
			return "", apierr.New("invalid_advance", "foreign change %s is for line %s, not %s", p.ForeignChange, f.Line, p.Line)
		}
		if f.BaseTree != head.Tree {
			return "", apierr.New("stale_base", "foreign change %s is based on %s, not the head of line %s", p.ForeignChange, f.BaseTree, p.Line).With("line", p.Line)
		}
		return f.Tree, nil
	}
	m, err := manifestIn(v.ctx, v.q, p.Manifest)
	if err != nil {
		return "", err
	}
	if m.BaseTree != head.Tree {
		return "", apierr.New("stale_base", "manifest %s is based on %s, not the head of line %s", p.Manifest, m.BaseTree, p.Line).With("line", p.Line)
	}
	var s schema.SelectionDecision
	if err := v.object(p.Selection, schema.KindSelectionDecision, &s); err != nil {
		return "", err
	}
	if s.Manifest != p.Manifest {
		return "", apierr.New("invalid_advance", "selection %s does not select manifest %s", p.Selection, p.Manifest)
	}
	return m.Tree, nil
}

// workspaceLine checks that a workspace forked from a line advance is based
// on the tree that advance accepted.
func (v *validator) workspaceLine(p *schema.Workspace) error {
	if p.BaseLineAdvance == nil {
		return nil
	}
	var a schema.LineAdvance
	if err := v.object(*p.BaseLineAdvance, schema.KindLineAdvance, &a); err != nil {
		return err
	}
	tree, err := v.advanceTree(a)
	if err != nil {
		return err
	}
	if tree != p.BaseTree {
		return apierr.New("binding_mismatch", "line advance %s accepted %s, not base %s", *p.BaseLineAdvance, tree, p.BaseTree)
	}
	return nil
}

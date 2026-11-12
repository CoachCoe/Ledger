package index

import (
	"context"
	"encoding/json/v2"
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

// lineHead follows line's advances from its start. Each advance names its
// predecessor and the validator admits one successor per head, so two
// advances from one head arrive only by import and leave the line forked.
func lineHead(ctx context.Context, q querier, line string) (LineHead, error) {
	rows, err := q.QueryContext(ctx, `SELECT hash, payload FROM objects WHERE kind = 'line_advance' AND json_extract(payload, '$.line') = ?`, line)
	if err != nil {
		return LineHead{}, err
	}
	next := map[string]string{}
	manifests := map[string]string{}
	for rows.Next() {
		var hash, payload string
		if err := rows.Scan(&hash, &payload); err != nil {
			rows.Close()
			return LineHead{}, err
		}
		var a schema.LineAdvance
		if err := json.Unmarshal([]byte(payload), &a); err != nil {
			rows.Close()
			return LineHead{}, err
		}
		from := ""
		if a.Previous != nil {
			from = *a.Previous
		}
		if other, ok := next[from]; ok {
			rows.Close()
			return LineHead{}, apierr.New("line_fork", "line %s has two advances from one head: %s and %s", line, min(hash, other), max(hash, other)).With("line", line)
		}
		next[from] = hash
		manifests[hash] = a.Manifest
	}
	if err := rows.Close(); err != nil {
		return LineHead{}, err
	}
	head, at := "", ""
	for steps := 0; ; steps++ {
		h, ok := next[at]
		if !ok {
			break
		}
		if steps > len(next) {
			return LineHead{}, apierr.New("line_fork", "line %s does not chain", line).With("line", line)
		}
		head, at = h, h
	}
	if head == "" {
		r, err := repository(ctx, q)
		if err != nil {
			return LineHead{}, err
		}
		return LineHead{Tree: r.Tree}, nil
	}
	m, err := manifestIn(ctx, q, manifests[head])
	if err != nil {
		return LineHead{}, err
	}
	return LineHead{Advance: &head, Tree: m.Tree}, nil
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
	m, err := manifestIn(v.ctx, v.q, p.Manifest)
	if err != nil {
		return err
	}
	if m.BaseTree != head.Tree {
		return apierr.New("stale_base", "manifest %s is based on %s, not the head of line %s", p.Manifest, m.BaseTree, p.Line).With("line", p.Line)
	}
	var s schema.SelectionDecision
	if err := v.object(p.Selection, schema.KindSelectionDecision, &s); err != nil {
		return err
	}
	if s.Manifest != p.Manifest {
		return apierr.New("invalid_advance", "selection %s does not select manifest %s", p.Selection, p.Manifest)
	}
	if v.lines == nil {
		v.lines = map[string]LineHead{}
	}
	v.lines[p.Line] = LineHead{Advance: &hash, Tree: m.Tree}
	return nil
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
	m, err := manifestIn(v.ctx, v.q, a.Manifest)
	if err != nil {
		return err
	}
	if m.Tree != p.BaseTree {
		return apierr.New("binding_mismatch", "line advance %s accepted %s, not base %s", *p.BaseLineAdvance, m.Tree, p.BaseTree)
	}
	return nil
}

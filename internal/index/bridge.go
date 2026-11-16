package index

import (
	"context"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// Mapping is a committed bridge mapping and its object hash.
type Mapping struct {
	Hash string
	schema.BridgeMapping
}

// MappingsForCommit returns the mappings that name Git commit sha, in
// hash order.
func (x *Index) MappingsForCommit(ctx context.Context, sha string) ([]Mapping, error) {
	return x.mappings(ctx, `json_extract(payload, '$.git_commit') = ?`, sha)
}

// MappingsForTree returns the mappings that name native tree id, in hash
// order.
func (x *Index) MappingsForTree(ctx context.Context, tree string) ([]Mapping, error) {
	return x.mappings(ctx, `json_extract(payload, '$.native_tree') = ?`, tree)
}

func (x *Index) mappings(ctx context.Context, where string, arg string) ([]Mapping, error) {
	rows, err := x.conn().QueryContext(ctx, `SELECT hash, version, payload FROM objects WHERE kind = 'bridge_mapping' AND `+where+` ORDER BY hash`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Mapping
	for rows.Next() {
		var m Mapping
		var version int
		var payload string
		if err := rows.Scan(&m.Hash, &version, &payload); err != nil {
			return nil, err
		}
		if err := schema.Decode(version, schema.KindBridgeMapping, []byte(payload), &m.BridgeMapping); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// checkMapping admits root-signed mappings only: the bridge acts for the
// repository owner, never for a session (ADR 0028). An import mapping
// names no Ledger record; an export mapping names the advance or the
// manifest it was written for, or neither for the root commit of a
// repository's initial tree.
func (v *validator) checkMapping(p *schema.BridgeMapping, signer string) error {
	if isSession(signer) {
		return unauthorized("only root signs bridge mappings")
	}
	if p.Direction == "import" {
		if p.LineAdvance != nil || p.Manifest != nil {
			return apierr.New("invalid_mapping", "an import mapping names no line advance or manifest")
		}
		return nil
	}
	if p.LineAdvance != nil && p.Manifest != nil {
		return apierr.New("invalid_mapping", "an export mapping names at most one of a line advance and a manifest")
	}
	if p.LineAdvance == nil && p.Manifest == nil {
		r, err := repository(v.ctx, v.q)
		if err != nil {
			return err
		}
		if r.Tree != p.NativeTree {
			return apierr.New("invalid_mapping", "an export mapping without an advance or manifest must be the repository's initial tree %s", r.Tree)
		}
		return nil
	}
	if p.LineAdvance != nil {
		var a schema.LineAdvance
		if err := v.object(*p.LineAdvance, schema.KindLineAdvance, &a); err != nil {
			return err
		}
		m, err := manifestIn(v.ctx, v.q, a.Manifest)
		if err != nil {
			return err
		}
		if m.Tree != p.NativeTree {
			return apierr.New("invalid_mapping", "line advance %s accepted tree %s, not %s", *p.LineAdvance, m.Tree, p.NativeTree)
		}
		return nil
	}
	m, err := manifestIn(v.ctx, v.q, *p.Manifest)
	if err != nil {
		return err
	}
	if m.Tree != p.NativeTree {
		return apierr.New("invalid_mapping", "manifest %s names tree %s, not %s", *p.Manifest, m.Tree, p.NativeTree)
	}
	return nil
}

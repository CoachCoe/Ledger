package index

import (
	"context"
	"encoding/json/jsontext"

	"github.com/CoachCoe/Ledger/internal/schema"
)

// HistoricalRecord carries portable record identity, never a runtime row id.
type HistoricalRecord struct {
	Hash    string         `json:"hash"`
	Kind    schema.Kind    `json:"kind"`
	Payload jsontext.Value `json:"payload"`
}

// History reads one consistent, hash-ordered snapshot of committed query facts.
// objects contains accepted receipt groups only; pending files are excluded.
func (x *Index) History(ctx context.Context) ([]HistoricalRecord, error) {
	rows, err := x.conn().QueryContext(ctx, `SELECT hash,kind,payload FROM objects WHERE kind IN ('intent','change_manifest','attempt_record','selection_decision') ORDER BY hash`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HistoricalRecord{}
	for rows.Next() {
		var r HistoricalRecord
		var raw string
		if err := rows.Scan(&r.Hash, &r.Kind, &raw); err != nil {
			return nil, err
		}
		r.Payload = []byte(raw)
		out = append(out, r)
	}
	return out, rows.Err()
}

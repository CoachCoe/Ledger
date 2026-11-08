package index

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/coord"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/symbols"
)

func (x *Index) SaveSymbols(ctx context.Context, wid, tree, at string, summary symbols.Summary) error {
	raw, err := canon.Marshal(summary)
	if err != nil {
		return err
	}
	tx, err := x.conn().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO symbol_observations VALUES(?,?,?,?)`, wid, tree, string(raw), at); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM dependency_edges WHERE workspace_id=?`, wid); err != nil {
		return err
	}
	for _, edge := range summary.Edges {
		raw, err := canon.Marshal(edge)
		if err != nil {
			return err
		}
		if err := schema.ValidateRuntime("dependency_edge", raw); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO dependency_edges VALUES(?,?)`, wid, string(raw)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (x *Index) Symbols(ctx context.Context, wid string) (symbols.Summary, string, error) {
	var raw, tree string
	err := x.conn().QueryRowContext(ctx, `SELECT doc,tree FROM symbol_observations WHERE workspace_id=?`, wid).Scan(&raw, &tree)
	if errors.Is(err, sql.ErrNoRows) {
		return symbols.Summary{}, "", nil
	}
	if err != nil {
		return symbols.Summary{}, "", err
	}
	var s symbols.Summary
	err = json.Unmarshal([]byte(raw), &s)
	return s, tree, err
}
func (x *Index) PutLease(ctx context.Context, l coord.Lease) error {
	raw, err := canon.Marshal(l)
	if err != nil {
		return err
	}
	if err := schema.ValidateRuntime("lease", raw); err != nil {
		return err
	}
	if err := x.notificationFault("lease_before"); err != nil {
		return err
	}
	_, err = x.conn().ExecContext(ctx, `INSERT OR IGNORE INTO leases VALUES(?,?,?,?)`, l.ID, l.Holder, string(raw), l.ExpiresAt)
	if err != nil {
		return err
	}
	var existing string
	if err := x.conn().QueryRowContext(ctx, `SELECT doc FROM leases WHERE id=?`, l.ID).Scan(&existing); err != nil {
		return err
	}
	if existing != string(raw) {
		return apierr.New("operation_conflict", "lease retry has different input")
	}
	return x.notificationFault("lease_after")
}
func (x *Index) Leases(ctx context.Context) ([]coord.Lease, error) {
	rows, err := x.conn().QueryContext(ctx, `SELECT doc FROM leases ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []coord.Lease{}
	for rows.Next() {
		var raw string
		var l coord.Lease
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
func (x *Index) DeleteLease(ctx context.Context, id string) error {
	if err := x.notificationFault("lease_delete_before"); err != nil {
		return err
	}
	_, err := x.conn().ExecContext(ctx, `DELETE FROM leases WHERE id=?`, id)
	if err != nil {
		return err
	}
	return x.notificationFault("lease_delete_after")
}

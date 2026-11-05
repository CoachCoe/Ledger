// Package index is Ledger's SQLite index: a rebuildable view of committed
// record groups, the state derived from them, and runtime-only tables.
// Nothing in it is authoritative; the record files are (ADR 0009).
package index

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"

	_ "modernc.org/sqlite"
)

const schemaVersion = 1

// Durable tables are rebuilt from records by reindex; runtime tables hold
// local state that is never exported and may be discarded.
var (
	durableTables = []string{"objects", "members", "operations", "creations", "events", "bindings", "subjects"}
	runtimeTables = []string{"workspace_paths", "write_sets", "notifications", "leases", "dependency_edges"}
)

const ddl = `
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS objects (
	hash TEXT PRIMARY KEY, kind TEXT NOT NULL, signer TEXT NOT NULL, payload TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS members (
	receipt TEXT NOT NULL, hash TEXT NOT NULL, PRIMARY KEY (receipt, hash));
CREATE INDEX IF NOT EXISTS members_hash ON members (hash);
CREATE TABLE IF NOT EXISTS operations (
	operation_id TEXT NOT NULL, receipt TEXT NOT NULL, operation TEXT NOT NULL,
	request_digest TEXT NOT NULL, result TEXT NOT NULL,
	PRIMARY KEY (operation_id, receipt));
CREATE UNIQUE INDEX IF NOT EXISTS operations_receipt ON operations (receipt);
CREATE TABLE IF NOT EXISTS creations (
	entity_id TEXT NOT NULL, hash TEXT NOT NULL, kind TEXT NOT NULL, signer TEXT NOT NULL,
	payload TEXT NOT NULL, PRIMARY KEY (entity_id, hash));
CREATE TABLE IF NOT EXISTS events (
	hash TEXT PRIMARY KEY, subject_id TEXT NOT NULL, creation TEXT NOT NULL,
	signer TEXT NOT NULL, payload TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS events_subject ON events (subject_id);
CREATE TABLE IF NOT EXISTS bindings (
	hash TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, previous TEXT NOT NULL,
	session_id TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS bindings_workspace ON bindings (workspace_id);
CREATE TABLE IF NOT EXISTS subjects (
	id TEXT PRIMARY KEY, kind TEXT NOT NULL, creation TEXT NOT NULL, state TEXT NOT NULL,
	head TEXT NOT NULL, resume TEXT NOT NULL, owner TEXT NOT NULL, binding_head TEXT NOT NULL,
	conflict TEXT NOT NULL, detail TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS workspace_paths (
	workspace_id TEXT PRIMARY KEY, path TEXT NOT NULL, observed_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS write_sets (
	workspace_id TEXT NOT NULL, path TEXT NOT NULL, status TEXT NOT NULL,
	PRIMARY KEY (workspace_id, path));
CREATE TABLE IF NOT EXISTS notifications (
	id TEXT PRIMARY KEY, recipient TEXT NOT NULL, doc TEXT NOT NULL,
	created_at TEXT NOT NULL, acknowledged_at TEXT);
CREATE INDEX IF NOT EXISTS notifications_recipient ON notifications (recipient);
CREATE TABLE IF NOT EXISTS leases (
	id TEXT PRIMARY KEY, holder TEXT NOT NULL, doc TEXT NOT NULL, expires_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS dependency_edges (
	workspace_id TEXT NOT NULL, doc TEXT NOT NULL);
`

// Index is an open index database.
type Index struct {
	db   *sql.DB
	path string
	file os.FileInfo
}

// Open opens or creates the index at path and applies migrations.
func Open(path string) (*Index, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String())
	if err != nil {
		return nil, err
	}
	x := &Index{db: db, path: path}
	if err := x.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	x.file, err = os.Stat(path)
	if err != nil {
		db.Close()
		return nil, err
	}
	return x, nil
}

func (x *Index) migrate() error {
	if _, err := x.db.Exec(ddl); err != nil {
		return fmt.Errorf("index: migrate: %w", err)
	}
	var v int
	err := x.db.QueryRow(`SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'schema_version'`).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = x.db.Exec(`INSERT INTO meta (key, value) VALUES ('schema_version', ?)`, fmt.Sprint(schemaVersion))
		return err
	case err != nil:
		return err
	case v != schemaVersion:
		return fmt.Errorf("index: schema version %d is not supported by this build", v)
	}
	return nil
}

// Close closes the database.
func (x *Index) Close() error { return x.db.Close() }

// RuntimeRows counts rows in the runtime tables, which reindex discards.
func (x *Index) RuntimeRows() (map[string]int, error) {
	out := map[string]int{}
	for _, t := range runtimeTables {
		var n int
		if err := x.db.QueryRow(`SELECT COUNT(*) FROM ` + t).Scan(&n); err != nil {
			return nil, err
		}
		out[t] = n
	}
	return out, nil
}

// Digest returns a hash over every durable table in a fixed order, so two
// indexes built from the same records compare equal.
func (x *Index) Digest() (string, error) {
	h := sha256.New()
	for _, t := range durableTables {
		if err := dumpTable(x.db, t, h); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func dumpTable(db *sql.DB, table string, w io.Writer) error {
	rows, err := db.QueryContext(context.Background(), `SELECT * FROM `+table+` ORDER BY 1, 2`) //nolint:gosec // table is from durableTables
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "%s\x00%d\n", table, len(cols))
	vals := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		for _, v := range vals {
			fmt.Fprintf(w, "%t\x00%q\x00", v.Valid, v.String)
		}
		fmt.Fprintln(w)
	}
	return rows.Err()
}

// Reset clears derived state in-place. Replacing the database inode would
// strand other open processes on an obsolete copy despite the store lock.
func (x *Index) Reset(ctx context.Context) error {
	if err := x.Refresh(); err != nil {
		return err
	}
	tx, err := x.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range append(append([]string{}, durableTables...), runtimeTables...) {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil { //nolint:gosec // table is from the fixed internal table lists
			return err
		}
	}
	return tx.Commit()
}

// Refresh reconnects after an external deletion/replacement of index.db.
// Call it under the RecordStore lock before writing or rebuilding.
func (x *Index) Refresh() error {
	fi, err := os.Stat(x.path)
	if err == nil && os.SameFile(fi, x.file) {
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := x.db.Close(); err != nil {
		return err
	}
	fresh, err := Open(x.path)
	if err != nil {
		return err
	}
	*x = *fresh
	return nil
}

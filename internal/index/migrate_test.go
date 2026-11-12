package index

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// An index built before schema_version 2 gains the version column, and its
// rows, all version 1, read as such.
func TestMigrateAddsVersionColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE objects (hash TEXT PRIMARY KEY, kind TEXT NOT NULL, signer TEXT NOT NULL, payload TEXT NOT NULL)`,
		`CREATE TABLE creations (entity_id TEXT NOT NULL, hash TEXT NOT NULL, kind TEXT NOT NULL, signer TEXT NOT NULL,
			payload TEXT NOT NULL, PRIMARY KEY (entity_id, hash))`,
		`INSERT INTO objects VALUES ('h', 'intent', 's', '{}')`,
		`INSERT INTO creations VALUES ('int_x', 'h', 'intent', 's', '{}')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	for range 2 {
		x, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		cr, err := x.Creation(t.Context(), "int_x")
		if err != nil || cr.Version != 1 {
			t.Fatalf("creation %+v, %v", cr, err)
		}
		var v int
		if err := x.conn().QueryRow(`SELECT version FROM objects WHERE hash='h'`).Scan(&v); err != nil || v != 1 {
			t.Fatalf("object version %d, %v", v, err)
		}
		x.Close()
	}
}

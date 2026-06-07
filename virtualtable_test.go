package postlite

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// A virtual table whose module is not compiled into postlite's build — most
// commonly an FTS5 full-text index in a user's SQLite file — appears in
// sqlite_master as an ordinary type='table' row. The pg_catalog and
// information_schema views walk sqlite_master and call pragma table-valued
// functions (pragma_table_info / pragma_index_list / pragma_foreign_key_list) on
// every row; touching such a table forces SQLite to load its module, which fails
// with "no such module: fts5" and aborts the whole introspection statement. That
// broke NocoDB's columnList during meta sync. Virtual tables — and the internal
// "shadow" tables a module creates alongside them (e.g. <name>_data, <name>_idx) —
// must therefore be excluded from every catalog walk so a Postgres client never
// sees, queries, or trips over them.
//
// newServerWithFTS5 seeds the normal schema and then injects an fts5 virtual table
// plus an fts5-style shadow table directly into sqlite_master via writable_schema.
// This reproduces a file written by a build that had fts5 and later opened by
// postlite, which does not — without needing the fts5 module to be present here.
func newServerWithFTS5(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	sdb, err := sql.Open("postlite-sqlite3", filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	for _, stmt := range []string{
		seedSchema,
		`PRAGMA writable_schema=ON`,
		`INSERT INTO sqlite_master(type,name,tbl_name,rootpage,sql)
		 VALUES('table','ftsdocs','ftsdocs',0,'CREATE VIRTUAL TABLE ftsdocs USING fts5(body)')`,
		`INSERT INTO sqlite_master(type,name,tbl_name,rootpage,sql)
		 VALUES('table','ftsdocs_data','ftsdocs_data',2,'CREATE TABLE ftsdocs_data(id INTEGER PRIMARY KEY, block BLOB)')`,
		`PRAGMA writable_schema=OFF`,
	} {
		if _, err := sdb.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	if err := sdb.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	s := NewServer()
	s.Addr = "127.0.0.1:0"
	s.DataDir = dir
	if err := s.Open(); err != nil {
		t.Fatalf("server open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s.ln.Addr().String()
}

// The introspection surface must skip the fts5 virtual table and its shadow table:
// each of these queries must succeed AND return only the real tables (users,
// products), never ftsdocs/ftsdocs_data.
func TestVirtualTable_ExcludedFromIntrospection(t *testing.T) {
	addr := newServerWithFTS5(t)
	c := dial(t, addr, "test.db")

	cases := []struct {
		name string
		sql  string
	}{
		{"information_schema.tables", `SELECT table_name FROM information_schema.tables WHERE table_type = 'BASE TABLE' ORDER BY table_name`},
		{"information_schema.columns", `SELECT DISTINCT table_name FROM information_schema.columns ORDER BY table_name`},
		{"pg_class", `SELECT relname FROM pg_catalog.pg_class WHERE relkind = 'r' ORDER BY relname`},
	}
	for _, tc := range cases {
		r := c.simpleQuery(tc.sql)
		if r.err != nil {
			t.Errorf("%s: unexpected error: %s", tc.name, r.err.Message)
			continue
		}
		got := flatten(r.rows)
		if !equal(got, []string{"products", "users"}) {
			t.Errorf("%s = %v, want [products users] (fts5 table/shadow must be excluded)", tc.name, got)
		}
	}
}

// NocoDB's columnList is the query that actually crashed against a database with an
// fts5 table. Run the real query shape (nocoColumnListQuery) — which walks
// pg_constraint / pg_attribute / information_schema.columns — against the
// fts5-injected database. Because the catalog views exclude virtual + shadow tables,
// the query never touches the fts5 table; it must succeed and report the real
// products columns with the primary key flagged.
func TestVirtualTable_ColumnListSucceeds(t *testing.T) {
	addr := newServerWithFTS5(t)
	c := dial(t, addr, "test.db")

	r := c.extendedQuery(nocoColumnListQuery, "test.db", "public", "products")
	if r.err != nil {
		t.Fatalf("columnList: unexpected error: %s", r.err.Message)
	}
	if len(r.rows) == 0 {
		t.Fatalf("columnList returned no columns for products")
	}
	var pkSeen bool
	for _, row := range r.rows {
		if row[0] != "products" { // tn
			t.Errorf("columnList returned a row for %q, want only products", row[0])
		}
		if row[1] == "id" && row[3] == "p" { // cn == id, ck == 'p'
			pkSeen = true
		}
	}
	if !pkSeen {
		t.Errorf("columnList did not flag products.id as the primary key: %v", r.rows)
	}
}

// The foreign-key relationList query must likewise run cleanly with an fts5 table
// present and report the real FK (products.owner_id -> users.id) only.
func TestVirtualTable_RelationListSucceeds(t *testing.T) {
	addr := newServerWithFTS5(t)
	c := dial(t, addr, "test.db")

	r := c.extendedQuery(nocoRelationListQuery, "public")
	if r.err != nil {
		t.Fatalf("relationList: unexpected error: %s", r.err.Message)
	}
	if len(r.rows) != 1 {
		t.Fatalf("relationList rows = %d, want 1: %v", len(r.rows), r.rows)
	}
	// columns: ts, cstn, tn, cn, foreign_table_schema, rtn, rcn, ur, dr
	if row := r.rows[0]; row[2] != "products" || row[3] != "owner_id" || row[5] != "users" || row[6] != "id" {
		t.Errorf("relationList = %v, want products.owner_id -> users.id", row)
	}
}

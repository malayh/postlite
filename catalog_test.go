package postlite

import "testing"

// Phase 6: the pg_catalog views must reflect the live user schema. These drive the
// server at the wire level and assert the catalog rows for the seed schema
// (users, products with a NOT NULL column, a PK on each, an FK products.owner_id
// -> users.id, and an index on products.title).

// pg_type must be populated (empty pg_type is why GUI/driver type resolution fails).
func TestCatalog_PgTypePopulated(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	r := c.simpleQuery("SELECT typname FROM pg_catalog.pg_type WHERE oid = 23")
	if r.err != nil {
		t.Fatalf("pg_type query: %s", r.err.Message)
	}
	if len(r.rows) != 1 || r.rows[0][0] != "int4" {
		t.Errorf("pg_type oid 23 = %v, want [[int4]]", r.rows)
	}

	r = c.simpleQuery("SELECT count(*) FROM pg_catalog.pg_type")
	if r.err != nil || len(r.rows) != 1 || r.rows[0][0] == "0" {
		t.Errorf("pg_type should be non-empty, got %v (err=%v)", r.rows, r.err)
	}
}

// pg_database must list the database the session connected to, so clients that
// check for a database's existence find it. NocoDB's createDatabaseIfNotExists
// runs exactly this shape; an empty pg_database made it fall through to
// "CREATE DATABASE", which SQLite cannot parse.
func TestCatalog_PgDatabaseListsConnectedDB(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	r := c.simpleQuery("SELECT datname FROM pg_catalog.pg_database WHERE datistemplate = false AND datname = 'test.db'")
	if r.err != nil {
		t.Fatalf("pg_database query: %s", r.err.Message)
	}
	if len(r.rows) != 1 || r.rows[0][0] != "test.db" {
		t.Errorf("pg_database datname = %v, want [[test.db]]", r.rows)
	}
}

// CREATE DATABASE / DROP DATABASE are accepted as no-ops: a postlite database is a
// SQLite file, so the operation has no SQLite equivalent and must not error.
func TestCatalog_CreateDropDatabaseNoop(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	if r := c.simpleQuery(`CREATE DATABASE "kalki" ENCODING 'UTF8'`); r.err != nil {
		t.Fatalf("CREATE DATABASE should be a no-op, got error: %s", r.err.Message)
	}
	if r := c.simpleQuery("DROP DATABASE kalki"); r.err != nil {
		t.Fatalf("DROP DATABASE should be a no-op, got error: %s", r.err.Message)
	}
	// The connection must still be usable afterward.
	if r := c.simpleQuery("SELECT 1"); r.err != nil {
		t.Fatalf("connection broken after no-op DDL: %s", r.err.Message)
	}
}

func TestCatalog_PgClassListsTables(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	r := c.simpleQuery("SELECT relname FROM pg_catalog.pg_class WHERE relkind = 'r' ORDER BY relname")
	if r.err != nil {
		t.Fatalf("pg_class query: %s", r.err.Message)
	}
	got := flatten(r.rows)
	if !equal(got, []string{"products", "users"}) {
		t.Errorf("pg_class tables = %v, want [products users]", got)
	}
}

func TestCatalog_PgClassRelnatts(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	// users has 3 columns (id, name, email).
	r := c.simpleQuery("SELECT relnatts FROM pg_catalog.pg_class WHERE relname = 'users'")
	if r.err != nil {
		t.Fatalf("relnatts query: %s", r.err.Message)
	}
	if len(r.rows) != 1 || r.rows[0][0] != "3" {
		t.Errorf("users relnatts = %v, want [[3]]", r.rows)
	}
}

func TestCatalog_PgAttributeColumns(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	r := c.simpleQuery(`
		SELECT a.attname, a.attnum, a.attnotnull
		FROM pg_catalog.pg_attribute a
		WHERE a.attrelname = 'users'
		ORDER BY a.attnum`)
	if r.err != nil {
		t.Fatalf("pg_attribute query: %s", r.err.Message)
	}
	if len(r.rows) != 3 {
		t.Fatalf("users has %d attributes, want 3: %v", len(r.rows), r.rows)
	}
	// id, name (NOT NULL), email
	if r.rows[0][0] != "id" || r.rows[1][0] != "name" || r.rows[2][0] != "email" {
		t.Errorf("attnames = %v, want id,name,email", flattenCol(r.rows, 0))
	}
	// id is the PK -> not null; name is declared NOT NULL; email is nullable.
	if r.rows[0][2] != "t" {
		t.Errorf("id attnotnull = %q, want t (primary key)", r.rows[0][2])
	}
	if r.rows[1][2] != "t" {
		t.Errorf("name attnotnull = %q, want t (declared NOT NULL)", r.rows[1][2])
	}
	if r.rows[2][2] != "f" {
		t.Errorf("email attnotnull = %q, want f (nullable)", r.rows[2][2])
	}
}

func TestCatalog_PgAttributeTypeOID(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	// products.price is REAL -> float8 (701); title is TEXT -> text (25).
	r := c.simpleQuery(`
		SELECT attname, atttypid FROM pg_catalog.pg_attribute
		WHERE attrelname = 'products' AND attname IN ('price','title') ORDER BY attname`)
	if r.err != nil {
		t.Fatalf("query: %s", r.err.Message)
	}
	want := map[string]string{"price": "701", "title": "25"}
	for _, row := range r.rows {
		if want[row[0]] != row[1] {
			t.Errorf("%s atttypid = %s, want %s", row[0], row[1], want[row[0]])
		}
	}
}

func TestCatalog_PgConstraintPKandFK(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	// Primary keys: users_pkey and products_pkey.
	r := c.simpleQuery("SELECT conname FROM pg_catalog.pg_constraint WHERE contype = 'p' ORDER BY conname")
	if r.err != nil {
		t.Fatalf("pk query: %s", r.err.Message)
	}
	if !equal(flatten(r.rows), []string{"products_pkey", "users_pkey"}) {
		t.Errorf("pk constraints = %v, want [products_pkey users_pkey]", flatten(r.rows))
	}

	// Foreign key: products.owner_id -> users.
	r = c.simpleQuery("SELECT conrelname, confrelname FROM pg_catalog.pg_constraint WHERE contype = 'f'")
	if r.err != nil {
		t.Fatalf("fk query: %s", r.err.Message)
	}
	if len(r.rows) != 1 || r.rows[0][0] != "products" || r.rows[0][1] != "users" {
		t.Errorf("fk = %v, want [[products users]]", r.rows)
	}
}

// indexedSchema adds an explicit index and a UNIQUE column so the index/unique
// catalog paths have something to reflect (the shared seedSchema has neither).
const indexedSchema = `
CREATE TABLE items (
	id   INTEGER PRIMARY KEY,
	sku  TEXT UNIQUE,
	name TEXT
);
CREATE INDEX idx_items_name ON items(name);
`

func TestCatalog_PgIndex(t *testing.T) {
	_, addr := newTestServer(t, indexedSchema)
	c := dial(t, addr, "test.db")

	// The explicit index on items(name) must be present, joined back to its table.
	r := c.simpleQuery(`
		SELECT i.indexrelname FROM pg_catalog.pg_index i
		JOIN pg_catalog.pg_class c ON c.oid = i.indrelid
		WHERE c.relname = 'items'`)
	if r.err != nil {
		t.Fatalf("pg_index query: %s", r.err.Message)
	}
	found := false
	for _, row := range r.rows {
		if row[0] == "idx_items_name" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected index idx_items_name in pg_index, got %v", flatten(r.rows))
	}
}

// A UNIQUE column produces a unique constraint in pg_constraint.
func TestCatalog_PgConstraintUnique(t *testing.T) {
	_, addr := newTestServer(t, indexedSchema)
	c := dial(t, addr, "test.db")

	r := c.simpleQuery("SELECT count(*) FROM pg_catalog.pg_constraint WHERE contype = 'u' AND conrelname = 'items'")
	if r.err != nil {
		t.Fatalf("unique constraint query: %s", r.err.Message)
	}
	if len(r.rows) != 1 || r.rows[0][0] == "0" {
		t.Errorf("expected a UNIQUE constraint on items, got %v", r.rows)
	}
}

func TestCatalog_PgSettings(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	r := c.simpleQuery("SELECT setting FROM pg_catalog.pg_settings WHERE name = 'server_version'")
	if r.err != nil {
		t.Fatalf("pg_settings query: %s", r.err.Message)
	}
	if len(r.rows) != 1 || r.rows[0][0] != ServerVersion {
		t.Errorf("server_version setting = %v, want [[%s]]", r.rows, ServerVersion)
	}
}

// --- helpers ---

func flatten(rows [][]string) []string { return flattenCol(rows, 0) }

func flattenCol(rows [][]string, col int) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		if col < len(r) {
			out[i] = r[col]
		}
	}
	return out
}

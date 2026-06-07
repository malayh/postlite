package postlite

import (
	"context"
	"testing"
)

// Phase 9 (Tier 3): schema management. A client must be able to CREATE/ALTER/DROP
// using PostgreSQL syntax and types and have it take effect in SQLite, with the
// changes immediately visible through information_schema. Unsupported operations
// must surface as a clean error without dropping the connection. Driven through
// pgx so the whole protocol path (simple + extended, RETURNING) is exercised.

func TestDDL_CreateTableWithPgTypes(t *testing.T) {
	_, addr := newTestServer(t, "")
	conn := pgxConnect(t, addr, "test.db")
	ctx := context.Background()

	_, err := conn.Exec(ctx, `
		CREATE TABLE accounts (
			id        serial PRIMARY KEY,
			email     character varying(255),
			balance   double precision,
			is_active boolean,
			created   timestamp with time zone,
			metadata  jsonb
		)`)
	if err != nil {
		t.Fatalf("create table with pg types: %v", err)
	}

	// The new table and its columns must be visible through information_schema with
	// SQLite-mapped PostgreSQL data types.
	rows, err := conn.Query(ctx, `
		SELECT column_name, data_type FROM information_schema.columns
		WHERE table_name = 'accounts' ORDER BY ordinal_position`)
	if err != nil {
		t.Fatalf("describe accounts: %v", err)
	}
	defer rows.Close()

	got := map[string]string{}
	for rows.Next() {
		var name, dtype string
		if err := rows.Scan(&name, &dtype); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[name] = dtype
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	want := map[string]string{
		"id":        "bigint",
		"balance":   "double precision",
		"is_active": "boolean",
		"created":   "timestamp without time zone",
		"metadata":  "text", // jsonb -> TEXT affinity
	}
	for col, w := range want {
		if got[col] != w {
			t.Errorf("accounts.%s data_type = %q, want %q", col, got[col], w)
		}
	}
	if _, ok := got["email"]; !ok {
		t.Errorf("email column missing from information_schema; got %v", got)
	}
}

// serial must behave like an autoincrementing identity column, and INSERT ...
// RETURNING must hand back the generated key.
func TestDDL_SerialAndReturning(t *testing.T) {
	_, addr := newTestServer(t, "")
	conn := pgxConnect(t, addr, "test.db")
	ctx := context.Background()

	if _, err := conn.Exec(ctx, `CREATE TABLE notes (id serial PRIMARY KEY, body text)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	var id1, id2 int64
	if err := conn.QueryRow(ctx, `INSERT INTO notes (body) VALUES ($1) RETURNING id`, "first").Scan(&id1); err != nil {
		t.Fatalf("insert returning 1: %v", err)
	}
	if err := conn.QueryRow(ctx, `INSERT INTO notes (body) VALUES ($1) RETURNING id`, "second").Scan(&id2); err != nil {
		t.Fatalf("insert returning 2: %v", err)
	}
	if id1 != 1 || id2 != 2 {
		t.Errorf("serial ids = (%d, %d), want (1, 2)", id1, id2)
	}
}

func TestDDL_AlterAddColumn(t *testing.T) {
	_, addr := newTestServer(t, "")
	conn := pgxConnect(t, addr, "test.db")
	ctx := context.Background()

	if _, err := conn.Exec(ctx, `CREATE TABLE widgets (id serial PRIMARY KEY, name text)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := conn.Exec(ctx, `ALTER TABLE widgets ADD COLUMN price double precision`); err != nil {
		t.Fatalf("alter add column: %v", err)
	}

	var dtype string
	err := conn.QueryRow(ctx, `
		SELECT data_type FROM information_schema.columns
		WHERE table_name = 'widgets' AND column_name = 'price'`).Scan(&dtype)
	if err != nil {
		t.Fatalf("describe new column: %v", err)
	}
	if dtype != "double precision" {
		t.Errorf("widgets.price data_type = %q, want double precision", dtype)
	}
}

func TestDDL_DropTable(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	conn := pgxConnect(t, addr, "test.db")
	ctx := context.Background()

	if _, err := conn.Exec(ctx, `DROP TABLE products`); err != nil {
		t.Fatalf("drop table: %v", err)
	}

	// The dropped table must disappear from information_schema. (We check for the
	// row's absence rather than count(*), which is an untyped aggregate.)
	rows, err := conn.Query(ctx, `
		SELECT table_name FROM information_schema.tables
		WHERE table_name = 'products' AND table_type = 'BASE TABLE'`)
	if err != nil {
		t.Fatalf("list after drop: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Errorf("products still present in information_schema after drop")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	// users must remain.
	var name string
	if err := conn.QueryRow(ctx, `SELECT name FROM users WHERE id = 1`).Scan(&name); err != nil {
		t.Fatalf("users gone after dropping products: %v", err)
	}
}

// CREATE SEQUENCE has no SQLite equivalent; it is accepted as a no-op rather than
// erroring, so migration scripts that create sequences do not blow up.
func TestDDL_CreateSequenceNoop(t *testing.T) {
	_, addr := newTestServer(t, "")
	conn := pgxConnect(t, addr, "test.db")
	ctx := context.Background()

	if _, err := conn.Exec(ctx, `CREATE SEQUENCE my_seq START 1`); err != nil {
		t.Fatalf("create sequence should be a no-op, got: %v", err)
	}
}

// An unsupported DDL operation must produce a clean error and leave the connection
// fully usable.
func TestDDL_UnsupportedIsCleanError(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	conn := pgxConnect(t, addr, "test.db")
	ctx := context.Background()

	// SQLite cannot change a column's type.
	if _, err := conn.Exec(ctx, `ALTER TABLE users ALTER COLUMN name TYPE integer`); err == nil {
		t.Fatal("expected an error for ALTER COLUMN TYPE")
	}

	// The connection must still work afterward.
	var name string
	if err := conn.QueryRow(ctx, `SELECT name FROM users WHERE id = 1`).Scan(&name); err != nil {
		t.Fatalf("connection unusable after failed DDL: %v", err)
	}
	if name != "alice" {
		t.Errorf("name = %q, want alice", name)
	}
}

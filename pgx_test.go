package postlite

import (
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/jackc/pgx/v5"
)

// These tests drive the server with pgx — a strict, real-world Postgres driver
// whose protocol state machine closely mirrors node-postgres (the driver NocoDB
// uses). If pgx is happy, a generic Postgres client should feel at home.

func pgxConnect(t *testing.T, addr, database string) *pgx.Conn {
	t.Helper()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	dsn := fmt.Sprintf("postgres://sqlite3:pw@%s:%s/%s?sslmode=disable", host, port, database)
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgx connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// typedSchema exercises the four type families NocoDB relies on most.
const typedSchema = `
CREATE TABLE typed (
	i BIGINT,
	r DOUBLE PRECISION,
	b BOOLEAN,
	t TEXT
);
INSERT INTO typed (i, r, b, t) VALUES (42, 3.5, 1, 'hello');
`

// The headline Phase 4 win: a strict driver scans result columns into native Go
// types (int64/float64/bool/string), which only works when RowDescription carries
// real type OIDs and values are encoded in Postgres's text format.
func TestPgx_ScansTypedValues(t *testing.T) {
	_, addr := newTestServer(t, typedSchema)
	conn := pgxConnect(t, addr, "test.db")

	var (
		i int64
		r float64
		b bool
		s string
	)
	if err := conn.QueryRow(context.Background(),
		"SELECT i, r, b, t FROM typed").Scan(&i, &r, &b, &s); err != nil {
		t.Fatalf("scan typed row: %v", err)
	}
	if i != 42 || r != 3.5 || b != true || s != "hello" {
		t.Errorf("got (%d, %v, %v, %q), want (42, 3.5, true, hello)", i, r, b, s)
	}
}

func TestPgx_ConnectAndParameterizedQuery(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	conn := pgxConnect(t, addr, "test.db")

	var name string
	err := conn.QueryRow(context.Background(),
		"SELECT name FROM users WHERE id = $1", 1).Scan(&name)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if name != "alice" {
		t.Errorf("name = %q, want alice", name)
	}
}

func TestPgx_MultipleRows(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	conn := pgxConnect(t, addr, "test.db")

	rows, err := conn.Query(context.Background(), "SELECT name FROM users ORDER BY id")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if !equal(got, []string{"alice", "bob"}) {
		t.Errorf("names = %v, want [alice bob]", got)
	}
}

// pgx reads the affected-row count from the CommandComplete tag; it must be
// correct for writes (a hard-coded "SELECT 1" would make every write report 1).
func TestPgx_CommandTagRowsAffected(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	conn := pgxConnect(t, addr, "test.db")
	ctx := context.Background()

	// No-arg Exec uses the simple protocol.
	tag, err := conn.Exec(ctx, "INSERT INTO products (title) VALUES ('x'),('y'),('z')")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if tag.RowsAffected() != 3 {
		t.Errorf("insert RowsAffected = %d, want 3", tag.RowsAffected())
	}

	// Parameterized Exec uses the extended protocol.
	tag, err = conn.Exec(ctx, "INSERT INTO products (title) VALUES ($1)", "p")
	if err != nil {
		t.Fatalf("param insert: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Errorf("param insert RowsAffected = %d, want 1", tag.RowsAffected())
	}

	tag, err = conn.Exec(ctx, "UPDATE products SET price = 2.0 WHERE title IN ('x','y')")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if tag.RowsAffected() != 2 {
		t.Errorf("update RowsAffected = %d, want 2", tag.RowsAffected())
	}

	tag, err = conn.Exec(ctx, "DELETE FROM products")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if tag.RowsAffected() != 4 {
		t.Errorf("delete RowsAffected = %d, want 4", tag.RowsAffected())
	}
}

// A real driver's transaction must be atomic: Rollback discards every write made
// in the block. This only works if BEGIN, the writes, and ROLLBACK all run on the
// same underlying SQLite connection (the single-connection refactor).
func TestPgx_TransactionRollback(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	conn := pgxConnect(t, addr, "test.db")
	ctx := context.Background()

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO products (title) VALUES ('rollme')"); err != nil {
		t.Fatalf("insert in tx: %v", err)
	}
	// Visible inside the transaction. (Counts are scanned as text because all
	// columns are still advertised as TEXT until Phase 4 assigns real type OIDs.)
	var inTx string
	if err := tx.QueryRow(ctx, "SELECT COUNT(*) FROM products WHERE title = 'rollme'").Scan(&inTx); err != nil {
		t.Fatalf("count in tx: %v", err)
	}
	if inTx != "1" {
		t.Fatalf("inside tx count = %q, want 1", inTx)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// Gone after rollback, on the same connection.
	var after string
	if err := conn.QueryRow(ctx, "SELECT COUNT(*) FROM products WHERE title = 'rollme'").Scan(&after); err != nil {
		t.Fatalf("count after rollback: %v", err)
	}
	if after != "0" {
		t.Errorf("after rollback count = %q, want 0", after)
	}
}

func TestPgx_TransactionCommit(t *testing.T) {
	s, addr := newTestServer(t, seedSchema)
	conn := pgxConnect(t, addr, "test.db")
	ctx := context.Background()

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO products (title) VALUES ('keepme')"); err != nil {
		t.Fatalf("insert in tx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Durable: visible on a separate, direct handle to the file.
	db := openFileDB(t, s)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM products WHERE title = 'keepme'`).Scan(&n); err != nil {
		t.Fatalf("count on file: %v", err)
	}
	if n != 1 {
		t.Errorf("after commit %d rows on disk, want 1", n)
	}
}

// pgx surfaces an error inside a transaction, then issues ROLLBACK; the
// connection must be fully usable afterward.
func TestPgx_FailedTransactionRecovers(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	conn := pgxConnect(t, addr, "test.db")
	ctx := context.Background()

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO does_not_exist VALUES (1)"); err == nil {
		t.Fatal("expected an error for a missing table")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	var name string
	if err := conn.QueryRow(ctx, "SELECT name FROM users WHERE id = 2").Scan(&name); err != nil {
		t.Fatalf("connection unusable after aborted tx: %v", err)
	}
	if name != "bob" {
		t.Errorf("name = %q, want bob", name)
	}
}

// A failed query must surface as an error and leave the connection usable — the
// classic symptom of a broken proxy is the driver's connection dying here.
func TestPgx_ErrorThenRecovers(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	conn := pgxConnect(t, addr, "test.db")
	ctx := context.Background()

	if _, err := conn.Exec(ctx, "SELECT * FROM does_not_exist"); err == nil {
		t.Fatal("expected an error for a missing table")
	}

	var name string
	if err := conn.QueryRow(ctx, "SELECT name FROM users WHERE id = 2").Scan(&name); err != nil {
		t.Fatalf("connection unusable after error: %v", err)
	}
	if name != "bob" {
		t.Errorf("name = %q, want bob", name)
	}
}

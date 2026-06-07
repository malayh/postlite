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

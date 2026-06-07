package postlite

import (
	"strings"
	"testing"
)

// Phase 1 (Tier 0): the wire protocol must be complete enough for strict client
// libraries — emit ParseComplete/BindComplete/ParameterDescription, describe
// results without side effects, recover from errors without dropping the
// connection, and advertise a believable server version.

func TestProtocol_ExtendedEmitsCompletionMessages(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	r := c.extendedQuery("SELECT name FROM users WHERE id = $1", "1")
	if r.err != nil {
		t.Fatalf("unexpected error: %s", r.err.Message)
	}
	for _, want := range []string{"ParseComplete", "BindComplete", "RowDescription", "CommandComplete"} {
		if !r.has(want) {
			t.Errorf("extended query missing %s; got %v", want, r.seq)
		}
	}
	if len(r.rows) != 1 || r.rows[0][0] != "alice" {
		t.Errorf("rows = %v, want [[alice]]", r.rows)
	}
}

// A write described via the extended protocol must yield NoData (no columns) and
// must not be executed twice (Describe + Execute share one execution).
func TestProtocol_ExtendedWriteDescribesAsNoData(t *testing.T) {
	s, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	r := c.extendedQuery("INSERT INTO products (title) VALUES ($1)", "widget")
	if r.err != nil {
		t.Fatalf("unexpected error: %s", r.err.Message)
	}
	if !r.has("NoData") {
		t.Errorf("describe of a write should yield NoData; got %v", r.seq)
	}
	if r.has("RowDescription") {
		t.Errorf("write must not produce RowDescription; got %v", r.seq)
	}

	// Exactly one row should have been inserted (no double execution).
	db := openFileDB(t, s)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM products WHERE title = 'widget'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("inserted %d rows, want exactly 1 (describe must not re-execute)", n)
	}
}

func TestProtocol_SimpleErrorKeepsConnectionAlive(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	r := c.simpleQuery("SELECT * FROM does_not_exist")
	if r.err == nil {
		t.Fatal("expected an error for a missing table")
	}
	if r.err.Code == "" {
		t.Error("ErrorResponse is missing a SQLSTATE code")
	}
	if r.txStatus != 'I' {
		t.Errorf("expected ReadyForQuery after error, txStatus = %q", r.txStatus)
	}

	// The connection must still be usable.
	r2 := c.simpleQuery("SELECT name FROM users WHERE id = 1")
	if r2.err != nil || len(r2.rows) != 1 || r2.rows[0][0] != "alice" {
		t.Errorf("connection unusable after error: err=%v rows=%v", r2.err, r2.rows)
	}
}

func TestProtocol_ExtendedErrorRecoversOnSync(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	r := c.extendedQuery("SELECT * FROM does_not_exist WHERE id = $1", "1")
	if r.err == nil {
		t.Fatal("expected an error for a missing table")
	}
	if r.txStatus != 'I' {
		t.Errorf("expected ReadyForQuery (via Sync) after error, txStatus = %q", r.txStatus)
	}

	// A subsequent extended query on the same connection must still work.
	r2 := c.extendedQuery("SELECT name FROM users WHERE id = $1", "2")
	if r2.err != nil || len(r2.rows) != 1 || r2.rows[0][0] != "bob" {
		t.Errorf("connection unusable after extended error: err=%v rows=%v", r2.err, r2.rows)
	}
}

func TestProtocol_EmptyQuery(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	r := c.simpleQuery("")
	if r.err != nil {
		t.Fatalf("unexpected error: %s", r.err.Message)
	}
	if !r.has("EmptyQueryResponse") {
		t.Errorf("empty query should yield EmptyQueryResponse; got %v", r.seq)
	}
}

// Command tags must report the real verb and affected/returned row counts, not a
// hard-coded "SELECT 1".
func TestProtocol_CommandTags(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	cases := []struct{ query, want string }{
		{"INSERT INTO products (title) VALUES ('a')", "INSERT 0 1"},
		{"INSERT INTO products (title) VALUES ('b'),('c')", "INSERT 0 2"},
		{"UPDATE products SET price = 1.0", "UPDATE 3"},
		{"SELECT id FROM products", "SELECT 3"},
		{"DELETE FROM products", "DELETE 3"},
		{"CREATE TABLE t (a INTEGER)", "CREATE TABLE"},
	}
	for _, tc := range cases {
		r := c.simpleQuery(tc.query)
		if r.err != nil {
			t.Errorf("%s: error %s", tc.query, r.err.Message)
			continue
		}
		if r.commandTag != tc.want {
			t.Errorf("%s: tag = %q, want %q", tc.query, r.commandTag, tc.want)
		}
	}
}

func TestProtocol_VersionLooksLikePostgres(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	r := c.simpleQuery("SELECT version()")
	if r.err != nil {
		t.Fatalf("version() error: %s", r.err.Message)
	}
	if len(r.rows) != 1 || !strings.HasPrefix(r.rows[0][0], "PostgreSQL 14") {
		t.Errorf("version() = %v, want a 'PostgreSQL 14...' string", r.rows)
	}
}

func TestProtocol_StartupReportsServerParameters(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	if got := c.startup.params["server_version"]; !strings.HasPrefix(got, "14") {
		t.Errorf("server_version = %q, want 14.x", got)
	}
	if got := c.startup.params["standard_conforming_strings"]; got != "on" {
		t.Errorf("standard_conforming_strings = %q, want on", got)
	}
	if !c.startup.has("BackendKeyData") {
		t.Errorf("startup missing BackendKeyData; got %v", c.startup.seq)
	}
}

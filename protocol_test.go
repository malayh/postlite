package postlite

import (
	"strings"
	"testing"

	"github.com/jackc/pgtype"
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

// A transaction opened over the simple protocol must report its status in
// ReadyForQuery ('T' inside the block, 'I' once ended) and ROLLBACK must undo the
// work — which only holds if every statement shares one pinned connection.
func TestProtocol_TransactionStatusAndRollback(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	if r := c.simpleQuery("BEGIN"); r.err != nil || r.txStatus != 'T' {
		t.Fatalf("BEGIN: err=%v txStatus=%q, want 'T'", r.err, r.txStatus)
	}
	if r := c.simpleQuery("INSERT INTO products (title) VALUES ('rollme')"); r.err != nil || r.txStatus != 'T' {
		t.Fatalf("INSERT in tx: err=%v txStatus=%q, want 'T'", r.err, r.txStatus)
	}
	if r := c.simpleQuery("ROLLBACK"); r.err != nil || r.txStatus != 'I' {
		t.Fatalf("ROLLBACK: err=%v txStatus=%q, want 'I'", r.err, r.txStatus)
	}

	// The inserted row must be gone.
	r := c.simpleQuery("SELECT COUNT(*) FROM products WHERE title = 'rollme'")
	if r.err != nil {
		t.Fatalf("count: %s", r.err.Message)
	}
	if len(r.rows) != 1 || r.rows[0][0] != "0" {
		t.Errorf("after rollback count = %v, want 0", r.rows)
	}
}

// COMMIT must persist; a fresh connection (and the on-disk file) must see it.
func TestProtocol_TransactionCommitPersists(t *testing.T) {
	s, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	c.simpleQuery("BEGIN")
	c.simpleQuery("INSERT INTO products (title) VALUES ('keepme')")
	if r := c.simpleQuery("COMMIT"); r.err != nil || r.txStatus != 'I' {
		t.Fatalf("COMMIT: err=%v txStatus=%q, want 'I'", r.err, r.txStatus)
	}

	db := openFileDB(t, s)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM products WHERE title = 'keepme'`).Scan(&n); err != nil {
		t.Fatalf("count on file: %v", err)
	}
	if n != 1 {
		t.Errorf("after commit %d rows on disk, want 1", n)
	}
}

// An error inside a transaction must put it in the aborted ('E') state, where
// every statement but COMMIT/ROLLBACK is rejected with SQLSTATE 25P02, and
// ROLLBACK restores a fully usable connection.
func TestProtocol_FailedTransactionAbortsUntilRollback(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	c.simpleQuery("BEGIN")
	if r := c.simpleQuery("SELECT * FROM does_not_exist"); r.err == nil || r.txStatus != 'E' {
		t.Fatalf("error in tx: err=%v txStatus=%q, want an error and 'E'", r.err, r.txStatus)
	}

	r := c.simpleQuery("SELECT 1")
	if r.err == nil {
		t.Error("a statement in an aborted transaction should be rejected")
	} else if r.err.Code != "25P02" {
		t.Errorf("aborted-tx SQLSTATE = %q, want 25P02", r.err.Code)
	}
	if r.txStatus != 'E' {
		t.Errorf("still aborted, txStatus = %q, want 'E'", r.txStatus)
	}

	if r := c.simpleQuery("ROLLBACK"); r.err != nil || r.txStatus != 'I' {
		t.Fatalf("ROLLBACK: err=%v txStatus=%q, want 'I'", r.err, r.txStatus)
	}

	// Fully recovered.
	r = c.simpleQuery("SELECT name FROM users WHERE id = 1")
	if r.err != nil || len(r.rows) != 1 || r.rows[0][0] != "alice" {
		t.Errorf("connection unusable after recovery: err=%v rows=%v", r.err, r.rows)
	}
}

// RowDescription must advertise a real type OID per column (derived from the
// SQLite declared type), not a blanket TEXT OID — that is what lets strict
// drivers scan into int64/float64/bool. Computed columns with no declared type
// fall back to text.
func TestProtocol_RowDescriptionOIDs(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	// products(id INTEGER, title TEXT, price REAL, ...); COUNT(*) is computed.
	r := c.simpleQuery("SELECT id, title, price, COUNT(*) FROM products")
	if r.err != nil {
		t.Fatalf("query: %s", r.err.Message)
	}
	want := []uint32{pgtype.Int8OID, pgtype.TextOID, pgtype.Float8OID, pgtype.TextOID}
	if len(r.fields) != len(want) {
		t.Fatalf("got %d fields, want %d", len(r.fields), len(want))
	}
	for i, f := range r.fields {
		if f.DataTypeOID != want[i] {
			t.Errorf("field %d (%s) OID = %d, want %d", i, f.Name, f.DataTypeOID, want[i])
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
	// The column must be named "version" exactly as PostgreSQL labels it: strict
	// clients (Knex/node-postgres, used by NocoDB) run `select version()` and read
	// the result by row.version. SQLite would otherwise label it "version()" and
	// the client reads undefined, rejecting the connection.
	if got := r.colNames(); len(got) != 1 || got[0] != "version" {
		t.Errorf("version() column names = %v, want [version]", got)
	}
}

// SHOW <setting> must return a single column named after the setting (PostgreSQL's
// behavior), because NocoDB's PgClient runs "SHOW server_version" and reads the
// result by row.server_version.
func TestProtocol_ShowServerVersionColumnName(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	r := c.simpleQuery("SHOW server_version")
	if r.err != nil {
		t.Fatalf("SHOW server_version error: %s", r.err.Message)
	}
	if got := r.colNames(); len(got) != 1 || got[0] != "server_version" {
		t.Errorf("SHOW server_version column names = %v, want [server_version]", got)
	}
	if len(r.rows) != 1 || r.rows[0][0] != ServerVersion {
		t.Errorf("SHOW server_version = %v, want [[%s]]", r.rows, ServerVersion)
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

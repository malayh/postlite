package postlite

import (
	"testing"
)

// These tests pin the behavior postlite ALREADY has and that we must not break.
// They are the regression safety net for the larger protocol/catalog work.
//
// They intentionally assert *data* outcomes (and the handshake), not the exact
// wire-message sequence, because some of the current sequence is wrong and will
// change in later phases (e.g. missing ParseComplete, bogus "SELECT 1" tags).
// Behavior that is intended to change is asserted in the phase that changes it.

func TestCharacterization_HandshakeCompletes(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	r := c.startup
	if !r.has("AuthenticationOk") {
		t.Errorf("handshake missing AuthenticationOk; got %v", r.seq)
	}
	if !r.has("ParameterStatus") {
		t.Errorf("handshake missing ParameterStatus; got %v", r.seq)
	}
	if r.txStatus != 'I' {
		t.Errorf("ReadyForQuery TxStatus = %q, want 'I'", r.txStatus)
	}
}

func TestCharacterization_SimpleSelect(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	r := c.simpleQuery("SELECT id, name, email FROM users ORDER BY id")
	if r.err != nil {
		t.Fatalf("unexpected error: %s", r.err.Message)
	}
	if got, want := r.colNames(), []string{"id", "name", "email"}; !equal(got, want) {
		t.Errorf("columns = %v, want %v", got, want)
	}
	if len(r.rows) != 2 {
		t.Fatalf("got %d rows, want 2: %v", len(r.rows), r.rows)
	}
	if r.rows[0][1] != "alice" || r.rows[1][1] != "bob" {
		t.Errorf("names = %q,%q want alice,bob", r.rows[0][1], r.rows[1][1])
	}
}

func TestCharacterization_SelectWhere(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	r := c.simpleQuery("SELECT name FROM users WHERE id = 1")
	if r.err != nil {
		t.Fatalf("unexpected error: %s", r.err.Message)
	}
	if len(r.rows) != 1 || r.rows[0][0] != "alice" {
		t.Errorf("rows = %v, want [[alice]]", r.rows)
	}
}

func TestCharacterization_InsertUpdatePersist(t *testing.T) {
	s, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	if r := c.simpleQuery("INSERT INTO products (title, price) VALUES ('widget', 9.99)"); r.err != nil {
		t.Fatalf("insert error: %s", r.err.Message)
	}
	if r := c.simpleQuery("UPDATE products SET price = 1.0 WHERE title = 'widget'"); r.err != nil {
		t.Fatalf("update error: %s", r.err.Message)
	}

	// Visible over the wire.
	r := c.simpleQuery("SELECT title, price FROM products")
	if len(r.rows) != 1 || r.rows[0][0] != "widget" || r.rows[0][1] != "1" {
		t.Errorf("over wire rows = %v, want [[widget 1]]", r.rows)
	}

	// And actually persisted to the file.
	db := openFileDB(t, s)
	var title string
	var price float64
	if err := db.QueryRow("SELECT title, price FROM products").Scan(&title, &price); err != nil {
		t.Fatalf("file scan: %v", err)
	}
	if title != "widget" || price != 1.0 {
		t.Errorf("persisted = (%q, %v), want (widget, 1)", title, price)
	}
}

func TestCharacterization_ExtendedBindParam(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	r := c.extendedQuery("SELECT name FROM users WHERE id = $1", "2")
	if r.err != nil {
		t.Fatalf("unexpected error: %s", r.err.Message)
	}
	if len(r.rows) != 1 || r.rows[0][0] != "bob" {
		t.Errorf("rows = %v, want [[bob]]", r.rows)
	}
}

// The seeded pg_catalog virtual tables that already have data must keep returning it.
func TestCharacterization_SeededCatalogs(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	cases := []struct {
		query string
		want  int
	}{
		{"SELECT nspname FROM pg_catalog.pg_namespace", 4},
		{"SELECT description FROM pg_catalog.pg_description", 3},
		{"SELECT rngtypid FROM pg_catalog.pg_range", 6},
	}
	for _, tc := range cases {
		r := c.simpleQuery(tc.query)
		if r.err != nil {
			t.Errorf("%s: error %s", tc.query, r.err.Message)
			continue
		}
		if len(r.rows) != tc.want {
			t.Errorf("%s: got %d rows, want %d", tc.query, len(r.rows), tc.want)
		}
	}
}

// --- small test helpers ---

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

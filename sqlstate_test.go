package postlite

import "testing"

// Strict Postgres drivers (node-postgres, used by NocoDB) branch on the five-char
// SQLSTATE and only handle the specific integrity-violation subclasses. PostgreSQL
// never returns the bare class code 23000 for a constraint failure — it returns
// 23502/23503/23505/23514 — so postlite must translate SQLite's extended result
// code into the matching subclass. (A bare 23000 made NocoDB abort an UPDATE with
// "23000 is not handled on database pg".)
func TestSQLState_ConstraintSubclasses(t *testing.T) {
	const seed = `
		CREATE TABLE things (
			id     INTEGER PRIMARY KEY,
			code   TEXT UNIQUE,
			qty    INTEGER NOT NULL,
			status TEXT CHECK (status IN ('a','b','c'))
		);
		INSERT INTO things (id, code, qty, status) VALUES (1, 'x', 5, 'a');`
	_, addr := newTestServer(t, seed)
	c := dial(t, addr, "test.db")

	cases := []struct {
		name string
		sql  string
		code string
	}{
		{"not_null", `INSERT INTO things (id, code, qty) VALUES (2, 'y', NULL)`, "23502"},
		{"unique", `INSERT INTO things (id, code, qty) VALUES (3, 'x', 1)`, "23505"},
		{"primary_key", `INSERT INTO things (id, code, qty) VALUES (1, 'z', 1)`, "23505"},
		{"check", `INSERT INTO things (id, qty, status) VALUES (4, 1, 'zzz')`, "23514"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := c.simpleQuery(tc.sql)
			if r.err == nil {
				t.Fatalf("%s: expected a constraint error, got none", tc.sql)
			}
			if r.err.Code != tc.code {
				t.Errorf("%s: SQLSTATE = %q, want %q (msg: %s)", tc.sql, r.err.Code, tc.code, r.err.Message)
			}
			// The connection must remain usable after a handled error.
			if r.txStatus != 'I' {
				t.Errorf("%s: txStatus = %q, want 'I'", tc.sql, r.txStatus)
			}
		})
	}

	// Sanity: a normal insert still works after all the rejected ones.
	if r := c.simpleQuery(`INSERT INTO things (id, code, qty, status) VALUES (9, 'ok', 1, 'b')`); r.err != nil {
		t.Fatalf("valid insert after errors: %s", r.err.Message)
	}
}

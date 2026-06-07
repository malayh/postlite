package postlite

import (
	"context"
	"testing"
)

// Database GUIs/ORMs (NocoDB among them) render timestamp columns with
// "TO_CHAR(<col> AT TIME ZONE current_setting('timezone') AT TIME ZONE 'UTC', fmt)".
// SQLite has neither TO_CHAR nor AT TIME ZONE; postlite drops the (tz-naive) zone
// conversions and rewrites TO_CHAR to strftime. This must format a stored timestamp
// and, critically, propagate NULL for an empty timestamp (strftime returns NULL),
// so the grid query both succeeds and reports null cells as null.
func TestDateTime_ToCharFormatting(t *testing.T) {
	const seed = `
		CREATE TABLE events (id INTEGER PRIMARY KEY, created_at TIMESTAMP);
		INSERT INTO events (id, created_at) VALUES (1, '2026-06-07 12:34:56'), (2, NULL);`
	_, addr := newTestServer(t, seed)
	conn := pgxConnect(t, addr, "test.db")
	ctx := context.Background()

	const q = `SELECT TO_CHAR(("created_at" AT TIME ZONE CURRENT_SETTING('timezone') AT TIME ZONE 'UTC'),
	                          'YYYY-MM-DD HH24:MI:SSTZH:TZM') AS ts
	           FROM events ORDER BY id`

	rows, err := conn.Query(ctx, q)
	if err != nil {
		t.Fatalf("to_char query: %v", err)
	}
	defer rows.Close()

	var got []*string
	for rows.Next() {
		var s *string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("rows = %d, want 2", len(got))
	}
	if got[0] == nil || *got[0] != "2026-06-07 12:34:56+00:00" {
		t.Errorf("row 1 = %v, want 2026-06-07 12:34:56+00:00", deref(got[0]))
	}
	if got[1] != nil {
		t.Errorf("row 2 = %q, want NULL (null timestamp must stay null)", *got[1])
	}
}

// current_setting() reports the run-time parameter (same source as SHOW), which
// clients read for session/timezone introspection.
func TestDateTime_CurrentSetting(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	r := c.simpleQuery("SELECT current_setting('timezone') AS tz")
	if r.err != nil {
		t.Fatalf("current_setting: %s", r.err.Message)
	}
	if len(r.rows) != 1 || r.rows[0][0] != "UTC" {
		t.Errorf("current_setting('timezone') = %v, want UTC", r.rows)
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

package postlite

import "testing"

func TestCommandTag(t *testing.T) {
	cases := []struct {
		query              string
		affected, returned int64
		want               string
	}{
		{"INSERT INTO t VALUES (1)", 1, 0, "INSERT 0 1"},
		{"insert into t values (1),(2)", 2, 0, "INSERT 0 2"},
		{"UPDATE t SET a = 1", 3, 0, "UPDATE 3"},
		{"DELETE FROM t", 2, 0, "DELETE 2"},
		{"SELECT * FROM t", 0, 5, "SELECT 5"},
		{"  select 1", 0, 1, "SELECT 1"},
		{"VALUES (1),(2)", 0, 2, "SELECT 2"},
		{"CREATE TABLE t (a)", 0, 0, "CREATE TABLE"},
		{"DROP TABLE t", 0, 0, "DROP TABLE"},
		{"ALTER TABLE t ADD COLUMN b", 0, 0, "ALTER TABLE"},
		{"CREATE INDEX idx ON t (a)", 0, 0, "CREATE INDEX"},
		{"BEGIN", 0, 0, "BEGIN"},
		{"COMMIT", 0, 0, "COMMIT"},
		{"ROLLBACK", 0, 0, "ROLLBACK"},
		{"SET search_path = public", 0, 0, "SET"},
		{"INSERT INTO t VALUES (1) RETURNING id", 0, 1, "INSERT 0 1"},
	}
	for _, tc := range cases {
		if got := string(commandTag(tc.query, tc.affected, tc.returned)); got != tc.want {
			t.Errorf("commandTag(%q, %d, %d) = %q, want %q", tc.query, tc.affected, tc.returned, got, tc.want)
		}
	}
}

func TestIsRowReturning(t *testing.T) {
	yes := []string{
		"SELECT 1", "  select * from t", "WITH x AS (SELECT 1) SELECT * FROM x",
		"VALUES (1)", "INSERT INTO t VALUES (1) RETURNING id", "PRAGMA table_info('t')",
	}
	no := []string{
		"INSERT INTO t VALUES (1)", "UPDATE t SET a = 1", "DELETE FROM t",
		"CREATE TABLE t (a)", "BEGIN", "SET x = 1",
	}
	for _, q := range yes {
		if !isRowReturning(q) {
			t.Errorf("isRowReturning(%q) = false, want true", q)
		}
	}
	for _, q := range no {
		if isRowReturning(q) {
			t.Errorf("isRowReturning(%q) = true, want false", q)
		}
	}
}

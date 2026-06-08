package postlite

import (
	"context"
	"reflect"
	"testing"
)

// parseEnums must reconstruct an enum only from a single-column, all-string-literal
// IN-list CHECK — and must leave compound predicates, numeric lists, and other
// constraints alone.
func TestParseEnums(t *testing.T) {
	cases := []struct {
		name string
		ddl  string
		want []enumType
	}{
		{
			name: "simple",
			ddl:  `CREATE TABLE t (status TEXT CHECK (status IN ('a', 'b', 'c')))`,
			want: []enumType{{table: "t", column: "status", typname: "t_status", labels: []string{"a", "b", "c"}}},
		},
		{
			name: "named constraint, quoted column",
			ddl:  `CREATE TABLE "t" (x INT, CONSTRAINT ck CHECK ("kind" IN ('insight', 'fact')))`,
			want: []enumType{{table: "t", column: "kind", typname: "t_kind", labels: []string{"insight", "fact"}}},
		},
		{
			name: "two enums in one table",
			ddl: `CREATE TABLE t (
				status TEXT CHECK (status IN ('open','closed')),
				CONSTRAINT c2 CHECK (src IN ('auto','manual')))`,
			want: []enumType{
				{table: "t", column: "status", typname: "t_status", labels: []string{"open", "closed"}},
				{table: "t", column: "src", typname: "t_src", labels: []string{"auto", "manual"}},
			},
		},
		{
			name: "escaped quote in value",
			ddl:  `CREATE TABLE t (m TEXT CHECK (m IN ('it''s', 'no')))`,
			want: []enumType{{table: "t", column: "m", typname: "t_m", labels: []string{"it's", "no"}}},
		},
		{
			name: "compound predicate is not an enum",
			ddl:  `CREATE TABLE t (a INT, b INT, CHECK (a IS NULL OR b IS NULL))`,
			want: nil,
		},
		{
			name: "membership ANDed with another term is not an enum",
			ddl:  `CREATE TABLE t (s TEXT, CHECK (s IN ('a','b') AND s <> ''))`,
			want: nil,
		},
		{
			name: "numeric list is not a string enum",
			ddl:  `CREATE TABLE t (p INT CHECK (p IN (1, 2, 3)))`,
			want: nil,
		},
		{
			name: "no check at all",
			ddl:  `CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseEnums(tableName(tc.ddl), tc.ddl)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseEnums:\n got  %+v\n want %+v", got, tc.want)
			}
		})
	}
}

// tableName pulls the table name out of a CREATE TABLE used by the test cases above,
// mirroring what sqlite_master would report (unquoted).
func tableName(ddl string) string {
	// every fixture above declares table "t".
	return "t"
}

// End to end: a SQLite VARCHAR + CHECK(col IN (...)) column must present to a
// PostgreSQL client as a USER-DEFINED enum type with its labels in pg_enum, while
// columns with other (or no) CHECK constraints stay their ordinary scalar type. The
// query is NocoDB's real columnList, so this proves the single-select recognition
// path end to end.
func TestEnum_ColumnListReportsSingleSelect(t *testing.T) {
	const seed = `
		CREATE TABLE tasks (
			id     INTEGER PRIMARY KEY,
			title  TEXT NOT NULL,
			status VARCHAR(11) CHECK (status IN ('todo', 'in_progress', 'done')),
			note   TEXT CHECK (note IS NULL OR length(note) > 0)
		);`
	_, addr := newTestServer(t, seed)
	conn := pgxConnect(t, addr, "test.db")
	ctx := context.Background()

	rows, err := conn.Query(ctx, nocoColumnListQuery, "test.db", "public", "tasks")
	if err != nil {
		t.Fatalf("columnList: %v", err)
	}
	defer rows.Close()

	type col struct {
		dataType string
		enum     *string
	}
	got := map[string]col{}
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			t.Fatalf("values: %v", err)
		}
		// columns: tn(0) cn(1) dt(2) ck(3) au(4) ge(5) csn(6) enum_values(7)
		cn, _ := vals[1].(string)
		dt, _ := vals[2].(string)
		c := col{dataType: dt}
		if vals[7] != nil {
			s := vals[7].(string)
			c.enum = &s
		}
		got[cn] = c
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	if c := got["status"]; c.dataType != "USER-DEFINED" || c.enum == nil || *c.enum != "todo,in_progress,done" {
		t.Errorf("status = %+v, want USER-DEFINED with enum_values 'todo,in_progress,done' (enum=%s)", c, deref(c.enum))
	}
	if c := got["title"]; c.dataType != "text" || c.enum != nil {
		t.Errorf("title = %+v, want plain text with no enum_values", c)
	}
	// A compound CHECK must NOT be reconstructed as an enum.
	if c := got["note"]; c.dataType == "USER-DEFINED" || c.enum != nil {
		t.Errorf("note = %+v, want plain text (compound CHECK is not an enum)", c)
	}
}

// information_schema.columns must place the synthetic enum type in the public schema
// (udt_schema) with the synthetic name (udt_name) so the enum_values join resolves;
// non-enum columns keep pg_catalog / their base udt_name. pg_enum carries the labels.
func TestEnum_InformationSchemaAndPgEnum(t *testing.T) {
	const seed = `CREATE TABLE m (
		id INTEGER PRIMARY KEY,
		kind VARCHAR(8) CHECK (kind IN ('insight','fact','goal')),
		body TEXT NOT NULL);`
	_, addr := newTestServer(t, seed)
	c := dial(t, addr, "test.db")

	r := c.simpleQuery(`SELECT data_type, udt_schema, udt_name FROM information_schema_columns
		WHERE table_name = 'm' AND column_name = 'kind'`)
	if r.err != nil {
		t.Fatalf("columns(kind): %s", r.err.Message)
	}
	if len(r.rows) != 1 || r.rows[0][0] != "USER-DEFINED" || r.rows[0][1] != "public" || r.rows[0][2] != "m_kind" {
		t.Errorf("kind column meta = %v, want [USER-DEFINED public m_kind]", r.rows)
	}

	r = c.simpleQuery(`SELECT data_type, udt_schema FROM information_schema_columns
		WHERE table_name = 'm' AND column_name = 'body'`)
	if r.err != nil {
		t.Fatalf("columns(body): %s", r.err.Message)
	}
	if len(r.rows) != 1 || r.rows[0][0] != "text" || r.rows[0][1] != "pg_catalog" {
		t.Errorf("body column meta = %v, want [text pg_catalog]", r.rows)
	}

	r = c.simpleQuery(`SELECT enumlabel FROM pg_enum e JOIN pg_type t ON t.oid = e.enumtypid
		WHERE t.typname = 'm_kind' ORDER BY e.enumsortorder`)
	if r.err != nil {
		t.Fatalf("pg_enum: %s", r.err.Message)
	}
	got := []string{}
	for _, row := range r.rows {
		got = append(got, row[0])
	}
	want := []string{"insight", "fact", "goal"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("enum labels = %v, want %v", got, want)
	}
}

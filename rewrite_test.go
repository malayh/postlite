package postlite

import (
	"strings"
	"testing"
)

// Phase 5: the rewriter is the single chokepoint that makes PostgreSQL-flavored
// SQL run on SQLite. These pin the specific shapes real client libraries emit.

func TestRewrite(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"set", "SET search_path = public", "SELECT 'SET'"},
		{"set-trailing-ws", "  SET TIME ZONE 'UTC'", "SELECT 'SET'"},
		{"reset", "RESET ALL", "SELECT 'SET'"},
		{"pg_catalog-strip", "SELECT * FROM pg_catalog.pg_class", "SELECT * FROM pg_class"},
		{"pg_catalog-strip-upper", "SELECT * FROM PG_CATALOG.pg_class", "SELECT * FROM pg_class"},
		{"information_schema", "SELECT * FROM information_schema.tables", "SELECT * FROM information_schema_tables"},
		{"information_schema-upper", "SELECT * FROM INFORMATION_SCHEMA.views", "SELECT * FROM information_schema_views"},
		{"cast-regclass", "SELECT 'users'::regclass", "SELECT 'users'"},
		{"cast-text", "SELECT 'x'::text", "SELECT 'x'"},
		{"cast-oid", "SELECT relname FROM pg_class WHERE oid = '5'::oid", "SELECT relname FROM pg_class WHERE oid = '5'"},
		{"cast-varchar-paren", "SELECT a::varchar(10) FROM t", "SELECT a FROM t"},
		{"cast-array", "SELECT '{1,2}'::int[]", "SELECT '{1,2}'"},
		{"any-array", "WHERE relkind = ANY(ARRAY['r','v'])", "WHERE relkind IN ('r','v')"},
		{"any-brace", "WHERE relkind = ANY('{r,v}')", "WHERE relkind IN ('r', 'v')"},
		{"bare-current_schema", "SELECT current_schema", "SELECT current_schema()"},
		{"show", "SHOW search_path", `SELECT show('search_path') AS "search_path"`},
		{"create-database", `CREATE DATABASE "kalki" ENCODING 'UTF8'`, `SELECT 'CREATE DATABASE'`},
		{"drop-database", "DROP DATABASE kalki", `SELECT 'DROP DATABASE'`},
	}
	for _, tc := range cases {
		if got := rewrite(tc.in); got != tc.want {
			t.Errorf("%s: rewrite(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// pgColumnName must rename a bare function/aggregate-call label to the function
// name (PostgreSQL's column naming) while leaving aliases and plain columns alone.
func TestPgColumnName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"version()", "version"},
		{"VERSION()", "version"},
		{"now()", "now"},
		{"count(*)", "count"},
		{"max(a)", "max"},
		{"coalesce(max(a), 0)", "coalesce"},
		{"current_database()", "current_database"},
		// Not bare function calls: left unchanged.
		{"id", "id"},
		{"server_version", "server_version"},
		{"a + b", "a + b"},
		{"f(x) + 1", "f(x) + 1"},
		{"'literal'", "'literal'"},
	}
	for _, tc := range cases {
		if got := pgColumnName(tc.in); got != tc.want {
			t.Errorf("pgColumnName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The relationList foreign-key query (LATERAL UNNEST ... WITH ORDINALITY) has no
// SQLite equivalent token-for-token; rewrite swaps the whole statement for a
// pragma_foreign_key_list query, keeping the single $1 schema placeholder.
func TestRewriteFKRelationList(t *testing.T) {
	in := `SELECT pc.conname FROM pg_constraint pc
		LEFT JOIN LATERAL UNNEST(pc.conkey)  WITH ORDINALITY AS u(attnum, attposition) ON TRUE
		LEFT JOIN LATERAL UNNEST(pc.confkey) WITH ORDINALITY AS f_u(attnum, attposition) ON TRUE
		WHERE pc.contype = 'f' AND sch.nspname = $1`
	got := rewrite(in)
	if strings.Contains(got, "UNNEST") {
		t.Errorf("UNNEST not rewritten: %q", got)
	}
	if !strings.Contains(got, "pragma_foreign_key_list") {
		t.Errorf("expected pragma_foreign_key_list, got %q", got)
	}
	if strings.Count(got, "$1") != 1 {
		t.Errorf("rewritten query must keep exactly one $1 (bind-param count), got %q", got)
	}
}

// NocoDB's columnList query also contains UNNEST(pc.conkey) (in a PK sub-select)
// but takes eight bind parameters. It must NOT be mistaken for relationList (one
// param); rewrite swaps it for the information_schema_columns equivalent that keeps
// all eight $-placeholders.
func TestRewriteColumnList(t *testing.T) {
	in := `select c.table_name as tn, pk1.constraint_name as pk_constraint_name1
		from information_schema.columns c
		LEFT JOIN LATERAL UNNEST(pc.conkey) WITH ORDINALITY AS u(attnum, attposition) ON TRUE
		where c.table_catalog=$6 and c.table_schema=$7 and c.table_name=$8`
	got := rewrite(in)
	if strings.Contains(got, "UNNEST") {
		t.Errorf("UNNEST not rewritten: %q", got)
	}
	for _, n := range []string{"$1", "$2", "$3", "$4", "$5", "$6", "$7", "$8"} {
		if !strings.Contains(got, n) {
			t.Errorf("rewritten columnList must keep %s (8 bind params), got %q", n, got)
		}
	}
	if strings.Contains(got, "$9") {
		t.Errorf("rewritten columnList must not introduce a 9th param: %q", got)
	}
}

func TestRewriteOperator(t *testing.T) {
	got := rewrite("SELECT 1 WHERE c.relname OPERATOR(pg_catalog.~) '^(users)$'")
	if strings.Contains(got, "OPERATOR") {
		t.Errorf("OPERATOR(...) not unwrapped: %q", got)
	}
	if !strings.Contains(got, "REGEXP") {
		t.Errorf("~ should map to REGEXP: %q", got)
	}
}

func TestTranslateDDL(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		mustContain []string
		mustAbsent  []string
	}{
		{"serial-pk", "CREATE TABLE t (id serial PRIMARY KEY)", []string{"INTEGER", "PRIMARY KEY"}, []string{"serial"}},
		{"bigserial", "CREATE TABLE t (id bigserial)", []string{"INTEGER"}, []string{"bigserial", "serial"}},
		{"character-varying", "CREATE TABLE t (a character varying(20))", []string{"VARCHAR(20)"}, []string{"character varying"}},
		{"double-precision", "CREATE TABLE t (a double precision)", []string{"REAL"}, []string{"double precision"}},
		{"boolean", "CREATE TABLE t (a boolean)", []string{"BOOLEAN"}, nil},
		{"timestamptz", "CREATE TABLE t (a timestamp with time zone)", []string{"TIMESTAMP"}, []string{"time zone"}},
		{"bytea", "CREATE TABLE t (a bytea)", []string{"BLOB"}, []string{"bytea"}},
		{"jsonb", "CREATE TABLE t (a jsonb)", []string{"TEXT"}, []string{"jsonb"}},
		{"uuid", "CREATE TABLE t (a uuid)", []string{"TEXT"}, []string{"uuid"}},
		{"nextval-default", "CREATE TABLE t (id integer DEFAULT nextval('s'))", []string{"INTEGER"}, []string{"nextval"}},
		{"create-sequence", "CREATE SEQUENCE s START 1", []string{"SELECT 'CREATE SEQUENCE'"}, []string{"START"}},
		{"drop-sequence", "DROP SEQUENCE s", []string{"SELECT 'DROP SEQUENCE'"}, nil},
		{"plain-dml-untouched", "INSERT INTO t (serial_no) VALUES (1)", []string{"INSERT INTO t (serial_no) VALUES (1)"}, nil},
	}
	for _, tc := range cases {
		got := translateDDL(tc.in)
		for _, sub := range tc.mustContain {
			if !strings.Contains(got, sub) {
				t.Errorf("%s: translateDDL(%q) = %q, want to contain %q", tc.name, tc.in, got, sub)
			}
		}
		for _, sub := range tc.mustAbsent {
			if strings.Contains(got, sub) {
				t.Errorf("%s: translateDDL(%q) = %q, should not contain %q", tc.name, tc.in, got, sub)
			}
		}
	}
}

func TestFormatType(t *testing.T) {
	cases := []struct {
		args []interface{}
		want string
	}{
		{[]interface{}{int64(23)}, "integer"},
		{[]interface{}{int64(20)}, "bigint"},
		{[]interface{}{int64(25)}, "text"},
		{[]interface{}{int64(16)}, "boolean"},
		{[]interface{}{int64(1043), int64(259)}, "character varying(255)"},
		{[]interface{}{int64(1700), int64(655366)}, "numeric(10,2)"},
		{[]interface{}{int64(99999)}, "-"},
		{[]interface{}{nil}, "-"},
	}
	for _, tc := range cases {
		if got := formatType(tc.args...); got != tc.want {
			t.Errorf("formatType(%v) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

func TestShow(t *testing.T) {
	cases := []struct{ name, want string }{
		{"server_version", ServerVersion},
		{"search_path", `"$user", public`},
		{"client_encoding", "UTF8"},
		{"nonexistent_setting", ""},
	}
	for _, tc := range cases {
		if got := show(tc.name); got != tc.want {
			t.Errorf("show(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestDeclTypeHelpers(t *testing.T) {
	cases := []struct {
		decl, pgType, udt string
	}{
		{"INTEGER", "bigint", "int8"},
		{"BIGINT", "bigint", "int8"},
		{"TEXT", "text", "text"},
		{"REAL", "double precision", "float8"},
		{"BOOLEAN", "boolean", "bool"},
		{"NUMERIC", "numeric", "numeric"},
		{"BLOB", "bytea", "bytea"},
		{"", "text", "text"},
	}
	for _, tc := range cases {
		if got := sqliteDeclToPGType(tc.decl); got != tc.pgType {
			t.Errorf("sqliteDeclToPGType(%q) = %q, want %q", tc.decl, got, tc.pgType)
		}
		if got := sqliteDeclToUDT(tc.decl); got != tc.udt {
			t.Errorf("sqliteDeclToUDT(%q) = %q, want %q", tc.decl, got, tc.udt)
		}
	}
}

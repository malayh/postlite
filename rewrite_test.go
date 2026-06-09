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
		{"cast-regclass-literal", "SELECT 'users'::regclass", "SELECT (SELECT oid FROM pg_class WHERE relname = 'users')"},
		{"cast-regclass-ident", "SELECT t.oid::regclass::text", "SELECT (SELECT relname FROM pg_class WHERE oid = t.oid)"},
		{"cast-text", "SELECT 'x'::text", "SELECT 'x'"},
		{"cast-oid", "SELECT relname FROM pg_class WHERE oid = '5'::oid", "SELECT relname FROM pg_class WHERE oid = '5'"},
		{"cast-varchar-paren", "SELECT a::varchar(10) FROM t", "SELECT a FROM t"},
		{"cast-array", "SELECT '{1,2}'::int[]", "SELECT '{1,2}'"},
		{"any-array", "WHERE relkind = ANY(ARRAY['r','v'])", "WHERE relkind IN ('r','v')"},
		{"any-brace", "WHERE relkind = ANY('{r,v}')", "WHERE relkind IN ('r', 'v')"},
		{"ilike", "SELECT 1 WHERE a ILIKE 'x'", "SELECT 1 WHERE a LIKE 'x'"},
		{"ilike-cast", `WHERE "title"::text ILIKE '%d%'`, `WHERE "title" LIKE '%d%'`},
		{"not-ilike", "WHERE a NOT ILIKE 'x'", "WHERE a NOT LIKE 'x'"},
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

// UNNEST(arr) [WITH ORDINALITY] AS t(v, ord) — used by client foreign-key/column
// introspection — has no SQLite keyword, so it is rewritten to json_each over the
// array (stored as JSON), with the value column -> t.value and the ordinality
// column -> (t.key + 1). The bind-parameter count is preserved (no $-params are
// added or dropped), which the extended protocol relies on.
func TestRewriteUnnest(t *testing.T) {
	in := `SELECT u.attnum, f.attposition
		FROM pg_constraint pc
		LEFT JOIN LATERAL UNNEST(pc.conkey)  WITH ORDINALITY AS u(attnum, attposition) ON TRUE
		LEFT JOIN LATERAL UNNEST(pc.confkey) WITH ORDINALITY AS f(attnum, attposition) ON f.attposition = u.attposition
		WHERE pc.contype = 'f' AND sch.nspname = $1`
	got := rewrite(in)
	for _, bad := range []string{"UNNEST", "LATERAL", "ORDINALITY"} {
		if strings.Contains(got, bad) {
			t.Errorf("%q not rewritten: %q", bad, got)
		}
	}
	for _, want := range []string{
		"json_each(COALESCE(pc.conkey,'[]')) u",
		"json_each(COALESCE(pc.confkey,'[]')) f",
		"u.value",     // u.attnum -> u.value
		"(u.key + 1)", // u.attposition -> (u.key + 1)
		"(f.key + 1)", // f.attposition -> (f.key + 1), in both SELECT and ON
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in %q", want, got)
		}
	}
	if strings.Count(got, "$1") != 1 {
		t.Errorf("bind-param count must be preserved (one $1), got %q", got)
	}
}

// string_agg/CONCAT/regclass have SQLite equivalents the rewriter substitutes so the
// original introspection SQL runs unchanged.
func TestRewriteFunctions(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"string_agg", "SELECT string_agg(word, ',') FROM t", "SELECT group_concat(word, ',') FROM t"},
		{"concat", "SELECT CONCAT('a', b, 'c')", "SELECT (COALESCE('a','') || COALESCE(b,'') || COALESCE('c',''))"},
		{"concat-nested", "SELECT CONCAT('x', CONCAT(a, b))", "SELECT (COALESCE('x','') || COALESCE((COALESCE(a,'') || COALESCE(b,'')),''))"},
		{"regclass-oid", "WHERE oid = 'users'::regclass", "WHERE oid = (SELECT oid FROM pg_class WHERE relname = 'users')"},
		{"regclass-name", "SELECT pc.conrelid::regclass::text", "SELECT (SELECT relname FROM pg_class WHERE oid = pc.conrelid)"},
		{"keywords", "select string_agg(word, ',') from pg_catalog.pg_get_keywords()", "select group_concat(word, ',') from pg_get_keywords"},
	}
	for _, tc := range cases {
		if got := rewrite(tc.in); got != tc.want {
			t.Errorf("%s: rewrite(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// Timestamp display: "<ts> AT TIME ZONE <zone>" is dropped (postlite is tz-naive)
// and "TO_CHAR(ts, fmt)" becomes strftime, with the PostgreSQL format translated.
func TestRewriteTimestamp(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			"to_char-at-time-zone",
			`TO_CHAR(("t"."created_at" AT TIME ZONE CURRENT_SETTING('timezone') AT TIME ZONE 'UTC'), 'YYYY-MM-DD HH24:MI:SSTZH:TZM')`,
			`strftime('%Y-%m-%d %H:%M:%S+00:00', ("t"."created_at"))`,
		},
		{
			"at-time-zone-literal-only",
			`SELECT ts AT TIME ZONE 'UTC' FROM t`,
			`SELECT ts FROM t`,
		},
		{
			"to_char-plain",
			`SELECT TO_CHAR(d, 'YYYY-MM-DD')`,
			`SELECT strftime('%Y-%m-%d', d)`,
		},
	}
	for _, tc := range cases {
		if got := rewrite(tc.in); got != tc.want {
			t.Errorf("%s: rewrite(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
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

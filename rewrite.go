package postlite

import (
	"regexp"
	"strings"
)

// rewrite is the single chokepoint that adapts PostgreSQL SQL into something
// SQLite can execute. BOTH the simple and the extended protocol paths call it, so
// a client gets identical translation regardless of how it submits a statement.
//
// The transformations are deliberately textual and conservative: they target the
// specific shapes real client libraries (node-postgres/Knex, pgx, psql) emit when
// they introspect the catalog or configure a session, and leave ordinary DML
// untouched.
func rewrite(q string) string {
	trimmed := strings.TrimSpace(q)

	// psql pulls the keyword list with a catalog function we do not implement;
	// hand back an empty result set instead of erroring.
	if strings.Contains(q, `select string_agg(word, ',') from pg_catalog.pg_get_keywords()`) {
		return `SELECT '' AS "string_agg" WHERE 1 = 2`
	}

	// SET / RESET configure session GUCs that PostgreSQL tracks but SQLite has no
	// notion of. Accept and ignore them so session setup does not fail.
	switch leadingKeyword(trimmed) {
	case "SET", "RESET":
		return `SELECT 'SET'`
	}

	// DDL translation (Tier 3): serial/sequences/PostgreSQL type names -> SQLite.
	q = translateDDL(q)

	// Unwrap "OPERATOR(pg_catalog.~)"-style operator qualifications into a bare
	// SQLite operator.
	q = operatorRegex.ReplaceAllStringFunc(q, unwrapOperator)

	// "x = ANY(ARRAY[...])" and "x = ANY('{...}')" -> "x IN (...)".
	q = anyArrayRegex.ReplaceAllString(q, "IN ($1)")
	q = anyBraceRegex.ReplaceAllStringFunc(q, rewriteAnyBrace)

	// Strip the pg_catalog. schema qualifier: the catalog objects (attached
	// virtual tables and temp views) and the registered catalog functions are all
	// reachable unqualified.
	q = pgCatalogRegex.ReplaceAllString(q, "")

	// information_schema.<x> is implemented as a flat temp view named
	// information_schema_<x> (a view cannot live in an attached :memory: schema and
	// still reference the user's tables).
	q = informationSchemaRegex.ReplaceAllString(q, "information_schema_")

	// Remove ::type casts; SQLite has no :: cast syntax. The underlying value is
	// already the right shape for our text-format results.
	q = castRegex.ReplaceAllString(q, "")

	// Turn bare system-information identifiers (current_schema, current_user, ...)
	// into the function calls we register.
	q = systemFunctionRegex.ReplaceAllString(q, "$1()$2")

	// "SHOW name" -> "SELECT show('name')".
	q = showRegex.ReplaceAllString(q, "SELECT show('$1')")

	return q
}

// rewrite is the per-connection entry point: it applies the shared textual
// rewrites and then substitutes connection-specific values that the global pass
// cannot know — namely current_database(), which must report the name this client
// connected with (and which information_schema exposes in its *_catalog columns).
func (c *Conn) rewrite(q string) string {
	q = rewrite(q)
	if c.database != "" {
		q = currentDatabaseRegex.ReplaceAllString(q, sqlStringLiteral(c.database))
	}
	return q
}

var currentDatabaseRegex = regexp.MustCompile(`(?i)\bcurrent_database\s*\(\s*\)`)

var (
	// Bare system-information identifiers that PostgreSQL also exposes as
	// zero-argument functions. We rewrite the bare form to a call so the registered
	// function supplies a value.
	systemFunctionRegex = regexp.MustCompile(`\b(current_catalog|current_schema|current_user|session_user|user)\b([^\(]|$)`)

	// ::type casts, including quoted ("char"), schema-qualified, parameterized
	// (varchar(10)) and array ([]) forms.
	castRegex = regexp.MustCompile(`::\s*(?:"[^"]*"|\w+(?:\.\w+)?)(?:\s*\([^)]*\))?(?:\s*\[\])*`)

	// pg_catalog. schema qualifier.
	pgCatalogRegex = regexp.MustCompile(`\bpg_catalog\.`)

	// information_schema. schema qualifier.
	informationSchemaRegex = regexp.MustCompile(`\binformation_schema\.`)

	// SHOW name.
	showRegex = regexp.MustCompile(`(?i)^\s*SHOW\s+(\w+)`)

	// OPERATOR( [pg_catalog.] <op> ).
	operatorRegex = regexp.MustCompile(`(?i)OPERATOR\s*\(\s*(?:pg_catalog\.)?\s*([^)\s]+)\s*\)`)

	// = ANY(ARRAY[ ... ]).
	anyArrayRegex = regexp.MustCompile(`(?i)=\s*ANY\s*\(\s*ARRAY\[([^\]]*)\]\s*\)`)

	// = ANY('{ ... }'[::type]).
	anyBraceRegex = regexp.MustCompile(`(?i)=\s*ANY\s*\(\s*'\{([^}]*)\}'\s*(?:::\s*[\w".\[\] ]+)?\)`)
)

// unwrapOperator turns a matched OPERATOR(pg_catalog.<op>) into a SQLite operator.
// Pattern-matching operators map onto LIKE/REGEXP; anything else collapses to the
// bare operator.
func unwrapOperator(match string) string {
	sub := operatorRegex.FindStringSubmatch(match)
	if sub == nil {
		return match
	}
	switch sub[1] {
	case "~~", "~~*":
		return " LIKE "
	case "!~~", "!~~*":
		return " NOT LIKE "
	case "~", "~*":
		return " REGEXP "
	case "!~", "!~*":
		return " NOT REGEXP "
	default:
		return " " + sub[1] + " "
	}
}

// rewriteAnyBrace turns "= ANY('{a,b,c}')" into "IN ('a', 'b', 'c')". PostgreSQL
// array element syntax may quote elements with double quotes; we normalize them to
// SQLite string literals.
func rewriteAnyBrace(match string) string {
	sub := anyBraceRegex.FindStringSubmatch(match)
	if sub == nil {
		return match
	}
	inner := strings.TrimSpace(sub[1])
	if inner == "" {
		return "IN (NULL)"
	}
	parts := strings.Split(inner, ",")
	for i, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, `"`)
		parts[i] = "'" + strings.ReplaceAll(p, "'", "''") + "'"
	}
	return "IN (" + strings.Join(parts, ", ") + ")"
}

// --- DDL translation (Tier 3) ---

// translateDDL rewrites PostgreSQL-specific DDL into SQLite-compatible DDL. It
// only touches CREATE/ALTER/DROP statements, so it never disturbs DML. Unsupported
// operations are left untouched and surface as a clean SQLite error (the
// connection survives).
func translateDDL(q string) string {
	switch leadingKeyword(q) {
	case "CREATE":
		if createSequenceRegex.MatchString(q) {
			return `SELECT 'CREATE SEQUENCE'`
		}
	case "ALTER":
		if alterSequenceRegex.MatchString(q) {
			return `SELECT 'ALTER SEQUENCE'`
		}
	case "DROP":
		if dropSequenceRegex.MatchString(q) {
			return `SELECT 'DROP SEQUENCE'`
		}
		return q
	default:
		return q
	}

	// serial / bigserial / smallserial -> INTEGER. Combined with PRIMARY KEY this
	// becomes an alias for the rowid, i.e. autoincrementing, in SQLite.
	q = serialRegex.ReplaceAllString(q, "INTEGER")

	// A column DEFAULT nextval('seq') has no SQLite equivalent; drop it and rely on
	// the INTEGER PRIMARY KEY rowid for identity columns.
	q = nextvalDefaultRegex.ReplaceAllString(q, "")

	// Map PostgreSQL scalar type names onto SQLite-friendly declared types. Order
	// matters: longer/more-specific names are replaced before their substrings.
	for _, m := range ddlTypeMappings {
		q = m.re.ReplaceAllString(q, m.repl)
	}
	return q
}

var (
	createSequenceRegex = regexp.MustCompile(`(?i)^\s*CREATE\s+(?:TEMP(?:ORARY)?\s+)?SEQUENCE\b`)
	alterSequenceRegex  = regexp.MustCompile(`(?i)^\s*ALTER\s+SEQUENCE\b`)
	dropSequenceRegex   = regexp.MustCompile(`(?i)^\s*DROP\s+SEQUENCE\b`)

	serialRegex = regexp.MustCompile(`(?i)\b(?:big|small)?serial\b`)

	// DEFAULT nextval('...'[::regclass]) on a column definition.
	nextvalDefaultRegex = regexp.MustCompile(`(?i)\s+DEFAULT\s+nextval\([^)]*\)`)
)

type ddlTypeMapping struct {
	re   *regexp.Regexp
	repl string
}

// ddlTypeMappings translate PostgreSQL scalar type names to SQLite declared types
// whose affinity (and our OID inference) matches the intended PostgreSQL type.
var ddlTypeMappings = []ddlTypeMapping{
	{regexp.MustCompile(`(?i)\btimestamp\s+with(?:out)?\s+time\s+zone\b`), "TIMESTAMP"},
	{regexp.MustCompile(`(?i)\btimestamptz\b`), "TIMESTAMP"},
	{regexp.MustCompile(`(?i)\btime\s+with(?:out)?\s+time\s+zone\b`), "TIME"},
	{regexp.MustCompile(`(?i)\btimetz\b`), "TIME"},
	{regexp.MustCompile(`(?i)\bcharacter\s+varying\s*(\([^)]*\))?`), "VARCHAR$1"},
	{regexp.MustCompile(`(?i)\bcharacter\s*(\([^)]*\))`), "CHAR$1"},
	{regexp.MustCompile(`(?i)\bbpchar\b`), "TEXT"},
	{regexp.MustCompile(`(?i)\bdouble\s+precision\b`), "REAL"},
	{regexp.MustCompile(`(?i)\bbytea\b`), "BLOB"},
	{regexp.MustCompile(`(?i)\bboolean\b`), "BOOLEAN"},
	{regexp.MustCompile(`(?i)\bbigint\b`), "INTEGER"},
	{regexp.MustCompile(`(?i)\bsmallint\b`), "INTEGER"},
	{regexp.MustCompile(`(?i)\bint8\b`), "INTEGER"},
	{regexp.MustCompile(`(?i)\bint4\b`), "INTEGER"},
	{regexp.MustCompile(`(?i)\bint2\b`), "INTEGER"},
	{regexp.MustCompile(`(?i)\binteger\b`), "INTEGER"},
	{regexp.MustCompile(`(?i)\bjsonb?\b`), "TEXT"},
	{regexp.MustCompile(`(?i)\buuid\b`), "TEXT"},
	{regexp.MustCompile(`(?i)\b(?:inet|cidr|macaddr)\b`), "TEXT"},
}

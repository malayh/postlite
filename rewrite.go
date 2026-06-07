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

	// NocoDB's columnList is a large per-table column-metadata query whose PK
	// sub-select uses "LATERAL UNNEST(pc.conkey) WITH ORDINALITY" (no SQLite
	// equivalent) and takes eight bind parameters. Recognize it (the pk_constraint_
	// name1 alias is unique to it) and answer it from information_schema_columns plus
	// pragma-derived primary-key / unique-column joins, keeping all eight $-params.
	if strings.Contains(q, "pk_constraint_name1") {
		return columnListQuery
	}

	// NocoDB's relationList enumerates foreign keys with a PostgreSQL-specific
	// "LEFT JOIN LATERAL UNNEST(pc.conkey/confkey) WITH ORDINALITY" join that SQLite
	// cannot parse. It is distinguished from columnList by unnesting confkey (the
	// referenced-column side) too. Answer it from pragma_foreign_key_list, which
	// already yields one row per foreign-key column (its seq column is the composite-
	// key position the WITH ORDINALITY join reconstructs), producing the same result
	// columns (ts, cstn, tn, cn, foreign_table_schema, rtn, rcn, ur, dr).
	if strings.Contains(q, "UNNEST(pc.confkey)") {
		return fkRelationListQuery
	}

	// SET / RESET configure session GUCs that PostgreSQL tracks but SQLite has no
	// notion of. Accept and ignore them so session setup does not fail.
	switch leadingKeyword(trimmed) {
	case "SET", "RESET":
		return `SELECT 'SET'`
	}

	// CREATE/DROP DATABASE have no SQLite equivalent. A postlite "database" is just a
	// SQLite file (the single mounted file, or one created on first connect), so it
	// effectively always exists; accept these as no-ops rather than erroring. NocoDB
	// issues CREATE DATABASE from createDatabaseIfNotExists when adding a source.
	if m := databaseDDLRegex.FindStringSubmatch(trimmed); m != nil {
		return "SELECT '" + strings.ToUpper(m[1]) + " DATABASE'"
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

	// "SHOW name" -> "SELECT show('name') AS "name"". The alias matters: PostgreSQL
	// names the result column after the setting, and strict clients read it by that
	// name (NocoDB's PgClient runs "SHOW server_version" and reads row.server_version).
	q = showRegex.ReplaceAllStringFunc(q, func(m string) string {
		name := strings.ToLower(showRegex.FindStringSubmatch(m)[1])
		return `SELECT show('` + name + `') AS "` + name + `"`
	})

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

	// pg_catalog. schema qualifier. Case-insensitive: clients (NocoDB) emit mixed
	// case like PG_CATALOG./INFORMATION_SCHEMA.; SQLite resolves the remaining
	// identifier case-insensitively to the lower-cased catalog object.
	pgCatalogRegex = regexp.MustCompile(`(?i)\bpg_catalog\.`)

	// information_schema. schema qualifier (case-insensitive, see above).
	informationSchemaRegex = regexp.MustCompile(`(?i)\binformation_schema\.`)

	// SHOW name.
	showRegex = regexp.MustCompile(`(?i)^\s*SHOW\s+(\w+)`)

	// CREATE/DROP DATABASE (a no-op in postlite's one-file-per-database model).
	databaseDDLRegex = regexp.MustCompile(`(?i)^\s*(CREATE|DROP)\s+DATABASE\b`)

	// OPERATOR( [pg_catalog.] <op> ).
	operatorRegex = regexp.MustCompile(`(?i)OPERATOR\s*\(\s*(?:pg_catalog\.)?\s*([^)\s]+)\s*\)`)

	// = ANY(ARRAY[ ... ]).
	anyArrayRegex = regexp.MustCompile(`(?i)=\s*ANY\s*\(\s*ARRAY\[([^\]]*)\]\s*\)`)

	// = ANY('{ ... }'[::type]).
	anyBraceRegex = regexp.MustCompile(`(?i)=\s*ANY\s*\(\s*'\{([^}]*)\}'\s*(?:::\s*[\w".\[\] ]+)?\)`)
)

// fkRelationListQuery is the SQLite equivalent of NocoDB's relationList
// foreign-key query (see rewrite). It keeps a single $1 placeholder for the schema
// filter so the bound-parameter count still matches, and maps SQLite's on_update /
// on_delete action text onto PostgreSQL's single-character confupdtype/confdeltype
// codes. postlite reports every object in the "public" schema (relnamespace 2200).
const fkRelationListQuery = `SELECT
	'public' AS ts,
	m.name || '_' || fk.id || '_fkey' AS cstn,
	m.name AS tn,
	fk."from" AS cn,
	'public' AS foreign_table_schema,
	fk."table" AS rtn,
	fk."to" AS rcn,
	CASE fk.on_update WHEN 'CASCADE' THEN 'c' WHEN 'SET NULL' THEN 'n' WHEN 'SET DEFAULT' THEN 'd' WHEN 'RESTRICT' THEN 'r' ELSE 'a' END AS ur,
	CASE fk.on_delete WHEN 'CASCADE' THEN 'c' WHEN 'SET NULL' THEN 'n' WHEN 'SET DEFAULT' THEN 'd' WHEN 'RESTRICT' THEN 'r' ELSE 'a' END AS dr
FROM main.sqlite_master m
JOIN pragma_foreign_key_list(m.name) fk
WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%' AND 'public' = $1
ORDER BY tn`

// columnListQuery is the SQLite equivalent of NocoDB's relationList-free
// columnList query (see rewrite). It returns the same result columns NocoDB reads
// per column (tn, cn, dt, au, ck, clen, np, ns, dp, cop, nrqd, cdf,
// generation_expression, character_octet_length, csn, pk_ordinal_position,
// pk_constraint_name, pk_ordinal_position1, pk_constraint_name1, udt_name,
// udt_schema, ii, is_unique, enum_values, udt_typtype), drawing columns from
// information_schema_columns and primary-key / unique membership from pragma joins.
// It keeps all eight $-placeholders, referenced in order so SQLite's positional
// bind maps the eight values correctly; only the schema ($7) and table ($8) filter,
// the rest are NULL-safe no-ops. postlite has no NocoDB autoincrement triggers,
// enums, generated columns or character sets, so au/enum_values/generation_
// expression/csn are NULL and udt_typtype is the base-type marker 'b'.
const columnListQuery = `SELECT
	c.table_name AS tn,
	c.column_name AS cn,
	c.data_type AS dt,
	NULL AS au,
	pk.constraint_type AS ck,
	c.character_maximum_length AS clen,
	c.numeric_precision AS np,
	c.numeric_scale AS ns,
	c.datetime_precision AS dp,
	c.ordinal_position AS cop,
	c.is_nullable AS nrqd,
	c.column_default AS cdf,
	NULL AS generation_expression,
	c.character_octet_length AS character_octet_length,
	NULL AS csn,
	pk.ordinal_position AS pk_ordinal_position,
	pk.constraint_name AS pk_constraint_name,
	pk.ordinal_position AS pk_ordinal_position1,
	pk.constraint_name AS pk_constraint_name1,
	c.udt_name AS udt_name,
	c.udt_schema AS udt_schema,
	c.is_identity AS ii,
	CASE WHEN uq.column_name IS NOT NULL THEN 1 ELSE NULL END AS is_unique,
	NULL AS enum_values,
	'b' AS udt_typtype
FROM information_schema_columns c
LEFT JOIN (
	SELECT m.name AS table_name, ti.name AS column_name,
	       'p' AS constraint_type, ti.pk AS ordinal_position,
	       m.name || '_pkey' AS constraint_name
	FROM main.sqlite_master m
	JOIN pragma_table_info(m.name) ti
	WHERE m.type = 'table' AND ti.pk > 0
) pk ON pk.table_name = c.table_name AND pk.column_name = c.column_name
LEFT JOIN (
	SELECT m.name AS table_name, ii2.name AS column_name
	FROM main.sqlite_master m
	JOIN pragma_index_list(m.name) il
	JOIN pragma_index_info(il.name) ii2
	WHERE m.type = 'table' AND il."unique" = 1 AND il.origin <> 'pk'
	      AND (SELECT count(*) FROM pragma_index_info(il.name)) = 1
	GROUP BY m.name, ii2.name
) uq ON uq.table_name = c.table_name AND uq.column_name = c.column_name
WHERE $1 IS $1 AND $2 IS $2 AND $3 IS $3 AND $4 IS $4 AND $5 IS $5 AND $6 IS $6
      AND c.table_schema = $7 AND c.table_name = $8
ORDER BY c.table_name, c.ordinal_position`

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

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

	// SET / RESET configure session GUCs that PostgreSQL tracks but SQLite has no
	// notion of. Accept and ignore them so session setup does not fail.
	switch leadingKeyword(trimmed) {
	case "SET", "RESET":
		return `SELECT 'SET'`
	}

	// CREATE/DROP DATABASE have no SQLite equivalent. A postlite "database" is just a
	// SQLite file (the single mounted file, or one created on first connect), so it
	// effectively always exists; accept these as no-ops rather than erroring.
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

	// PostgreSQL functions/constructs with no identical SQLite spelling. These are
	// general translations (not tied to any one client): they let real introspection
	// queries — node-postgres/Knex, pgx, psql, DBeaver — run against the catalog as
	// written, instead of being recognized and swapped out wholesale.
	//
	//   string_agg(x, d)  -> group_concat(x, d)            (same aggregate)
	//   CONCAT(a, b, ...) -> (COALESCE(a,'') || ...)        (NULL-as-empty semantics)
	//   UNNEST(arr) [WITH ORDINALITY] AS t(v[, ord])
	//                     -> json_each(COALESCE(arr,'[]')) t (see rewriteUnnest)
	q = stringAggRegex.ReplaceAllString(q, "group_concat(")
	q = rewriteConcat(q)
	q = rewriteUnnest(q)

	// "<oid>::regclass[::text]" -> the relation's name; "'name'::regclass" -> its oid.
	// This must run before the generic cast strip below, which would otherwise drop
	// "::regclass" and leave a bare oid where a relation name is expected.
	q = regclassOidRegex.ReplaceAllString(q, "(SELECT oid FROM pg_class WHERE relname = '$1')")
	q = regclassNameRegex.ReplaceAllString(q, "(SELECT relname FROM pg_class WHERE oid = $1)")

	// Strip the pg_catalog. schema qualifier: the catalog objects (attached
	// virtual tables and temp views) and the registered catalog functions are all
	// reachable unqualified.
	q = pgCatalogRegex.ReplaceAllString(q, "")

	// information_schema.<x> is implemented as a flat temp view named
	// information_schema_<x> (a view cannot live in an attached :memory: schema and
	// still reference the user's tables).
	q = informationSchemaRegex.ReplaceAllString(q, "information_schema_")

	// pg_get_keywords() is a no-argument set-returning function in PostgreSQL; postlite
	// exposes the keyword list as a view, so drop the call parentheses.
	q = keywordsCallRegex.ReplaceAllString(q, "pg_get_keywords")

	// Remove remaining ::type casts; SQLite has no :: cast syntax. The underlying value
	// is already the right shape for our text-format results.
	q = castRegex.ReplaceAllString(q, "")

	// Turn bare system-information identifiers (current_schema, current_user, ...)
	// into the function calls we register.
	q = systemFunctionRegex.ReplaceAllString(q, "$1()$2")

	// "SHOW name" -> "SELECT show('name') AS "name"". The alias matters: PostgreSQL
	// names the result column after the setting, and strict clients read it by that
	// name (e.g. node-postgres runs "SHOW server_version" and reads row.server_version).
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

	// string_agg( -> group_concat( (the call's leading token only; args are unchanged).
	stringAggRegex = regexp.MustCompile(`(?i)\bstring_agg\s*\(`)

	// CONCAT( — start of a PostgreSQL CONCAT() call (rewriteConcat finds its matching
	// close paren). The negative-lookbehind-free \b avoids matching concat_ws.
	concatRegex = regexp.MustCompile(`(?i)\bconcat\s*\(`)

	// UNNEST(arr) [WITH ORDINALITY] [AS] alias(valcol[, ordcol]) — a lateral array
	// expansion. Groups: 1=array expr, 2=alias, 3=value column, 4=ordinality column.
	unnestRegex = regexp.MustCompile(`(?is)(?:LATERAL\s+)?UNNEST\s*\(\s*([^()]*?)\s*\)\s*(?:WITH\s+ORDINALITY\s+)?(?:AS\s+)?(\w+)\s*\(\s*(\w+)\s*(?:,\s*(\w+)\s*)?\)`)

	// 'name'::regclass -> the relation's oid.
	regclassOidRegex = regexp.MustCompile(`(?i)'([^']*)'\s*::\s*regclass\b`)
	// <ident>::regclass[::text|::varchar|::name] -> the relation's name.
	regclassNameRegex = regexp.MustCompile(`(?i)\b(\w+(?:\.\w+)?)\s*::\s*regclass(?:\s*::\s*(?:text|varchar|name))?`)

	// pg_get_keywords() call parentheses (postlite exposes it as a view).
	keywordsCallRegex = regexp.MustCompile(`(?i)\bpg_get_keywords\s*\(\s*\)`)
)

// rewriteConcat rewrites PostgreSQL CONCAT(a, b, ...) into the SQLite expression
// (COALESCE(a,”) || COALESCE(b,”) || ...). PostgreSQL's CONCAT treats NULL as the
// empty string, which the COALESCE wrappers reproduce (bare || would yield NULL).
// Nested CONCAT calls are handled by repeated passes; quoted strings are respected.
func rewriteConcat(q string) string {
	for {
		loc := concatRegex.FindStringIndex(q)
		if loc == nil {
			return q
		}
		open := loc[1] - 1 // index of '('
		end := matchParen(q, open)
		if end < 0 {
			return q // unbalanced parentheses; leave untouched
		}
		args := splitArgs(q[open+1 : end])
		var b strings.Builder
		b.WriteString("(")
		for i, a := range args {
			if i > 0 {
				b.WriteString(" || ")
			}
			b.WriteString("COALESCE(")
			b.WriteString(strings.TrimSpace(a))
			b.WriteString(",'')")
		}
		b.WriteString(")")
		q = q[:loc[0]] + b.String() + q[end+1:]
	}
}

// rewriteUnnest converts PostgreSQL "UNNEST(arr) [WITH ORDINALITY] [AS] t(v[, ord])"
// into SQLite "json_each(COALESCE(arr,'[]')) t", then rewrites the alias's columns:
// the value column becomes t.value and the ordinality column becomes (t.key + 1)
// (json_each's key is 0-based). Array-valued catalog columns (e.g. pg_constraint.
// conkey/confkey) are stored as JSON arrays so json_each expands them; UNNEST(NULL)
// yields no rows in both engines.
func rewriteUnnest(q string) string {
	type rename struct{ alias, valCol, ordCol string }
	var renames []rename
	q = unnestRegex.ReplaceAllStringFunc(q, func(match string) string {
		m := unnestRegex.FindStringSubmatch(match)
		expr, alias, valCol, ordCol := m[1], m[2], m[3], m[4]
		renames = append(renames, rename{alias, valCol, ordCol})
		return "json_each(COALESCE(" + expr + ",'[]')) " + alias
	})
	for _, r := range renames {
		q = aliasColRegex(r.alias, r.valCol).ReplaceAllString(q, r.alias+".value")
		if r.ordCol != "" {
			q = aliasColRegex(r.alias, r.ordCol).ReplaceAllString(q, "("+r.alias+".key + 1)")
		}
	}
	return q
}

// aliasColRegex matches a qualified column reference "<alias>.<col>" on word
// boundaries so an alias is not matched inside a longer identifier.
func aliasColRegex(alias, col string) *regexp.Regexp {
	return regexp.MustCompile(`\b` + regexp.QuoteMeta(alias) + `\.` + regexp.QuoteMeta(col) + `\b`)
}

// matchParen returns the index of the ')' that matches the '(' at index open, or -1
// if unbalanced. Parentheses inside single-quoted string literals are ignored.
func matchParen(s string, open int) int {
	depth, inStr := 0, false
	for i := open; i < len(s); i++ {
		c := s[i]
		if inStr {
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' { // doubled '' escape
					i++
					continue
				}
				inStr = false
			}
			continue
		}
		switch c {
		case '\'':
			inStr = true
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return i
			}
		}
	}
	return -1
}

// splitArgs splits a function argument list on top-level commas, respecting nested
// parentheses and single-quoted string literals.
func splitArgs(s string) []string {
	var args []string
	depth, inStr, start := 0, false, 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inStr = false
			}
			continue
		}
		switch c {
		case '\'':
			inStr = true
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				args = append(args, s[start:i])
				start = i + 1
			}
		}
	}
	return append(args, s[start:])
}

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

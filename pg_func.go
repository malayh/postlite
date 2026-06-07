package postlite

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgtype"
)

// This file implements the pg_catalog functions that client libraries and GUIs
// call while introspecting a database. They are registered on every SQLite
// connection (see the driver ConnectHook in server.go). None of them are
// NocoDB-specific; they return what a real PostgreSQL server would for the kind of
// metadata queries node-postgres/Knex, pgx and psql issue.

// toInt64 coerces a value SQLite passes to a registered function into an int64.
func toInt64(v interface{}) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case string:
		i, _ := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		return i
	default:
		return 0
	}
}

// typeOIDName maps a PostgreSQL type OID to its canonical SQL name, used by
// format_type.
var typeOIDName = map[uint32]string{
	pgtype.BoolOID:        "boolean",
	pgtype.ByteaOID:       "bytea",
	pgtype.QCharOID:       `"char"`,
	pgtype.NameOID:        "name",
	pgtype.Int8OID:        "bigint",
	pgtype.Int2OID:        "smallint",
	pgtype.Int4OID:        "integer",
	pgtype.TextOID:        "text",
	pgtype.OIDOID:         "oid",
	pgtype.Float4OID:      "real",
	pgtype.Float8OID:      "double precision",
	pgtype.BPCharOID:      "character",
	pgtype.VarcharOID:     "character varying",
	pgtype.DateOID:        "date",
	pgtype.TimeOID:        "time without time zone",
	pgtype.TimestampOID:   "timestamp without time zone",
	pgtype.TimestamptzOID: "timestamp with time zone",
	pgtype.NumericOID:     "numeric",
	pgtype.JSONOID:        "json",
	pgtype.JSONBOID:       "jsonb",
	pgtype.UUIDOID:        "uuid",
}

// formatType implements format_type(type_oid, typemod). It returns the canonical
// type name, applying the type modifier for the length/precision-carrying types.
func formatType(args ...interface{}) string {
	if len(args) == 0 || args[0] == nil {
		return "-"
	}
	oid := uint32(toInt64(args[0]))
	name, ok := typeOIDName[oid]
	if !ok {
		return "-"
	}

	typmod := int64(-1)
	if len(args) > 1 && args[1] != nil {
		typmod = toInt64(args[1])
	}
	if typmod < 0 {
		return name
	}

	switch oid {
	case pgtype.VarcharOID, pgtype.BPCharOID:
		if typmod >= 4 {
			return fmt.Sprintf("%s(%d)", name, typmod-4)
		}
	case pgtype.NumericOID:
		if typmod >= 4 {
			m := typmod - 4
			return fmt.Sprintf("numeric(%d,%d)", (m>>16)&0xffff, m&0xffff)
		}
	}
	return name
}

// show returns the value of a session setting for "SHOW <name>" (rewritten to
// "SELECT show('<name>')"). Unknown settings return an empty string, which is what
// PostgreSQL does for a handful of compatibility GUCs.
func show(name string) string {
	switch strings.ToLower(name) {
	case "server_version":
		return ServerVersion
	case "server_encoding", "client_encoding":
		return "UTF8"
	case "standard_conforming_strings", "integer_datetimes":
		return "on"
	case "transaction_isolation", "default_transaction_isolation":
		return "read committed"
	case "transaction_read_only":
		return "off"
	case "search_path":
		return `"$user", public`
	case "timezone":
		return "UTC"
	case "datestyle":
		return "ISO, MDY"
	case "max_identifier_length":
		return "63"
	case "application_name":
		return ""
	default:
		return ""
	}
}

// currentDatabase implements current_database(). postlite serves one logical
// database per connection; we report a stable name.
func currentDatabase() string { return "postlite" }

// currentSetting implements current_setting(name[, missing_ok]). It returns the
// run-time parameter value (the same source as SHOW), so e.g.
// current_setting('timezone') is 'UTC'. An unknown setting yields an empty string.
func currentSetting(args ...interface{}) string {
	if len(args) == 0 || args[0] == nil {
		return ""
	}
	name, _ := args[0].(string)
	return show(name)
}

// pgGetExpr implements pg_get_expr(expr, relid[, pretty]); we do not store node
// trees, so the decompiled expression is empty.
func pgGetExpr(args ...interface{}) string { return "" }

// pgGetConstraintdef implements pg_get_constraintdef(oid[, pretty]).
func pgGetConstraintdef(args ...interface{}) string { return "" }

// pgGetIndexdef implements pg_get_indexdef(index_oid[, column_no, pretty]).
func pgGetIndexdef(args ...interface{}) string { return "" }

// pgGetFunctionIdentityArguments implements the same-named function.
func pgGetFunctionIdentityArguments(args ...interface{}) string { return "" }

// pgGetUserbyid implements pg_get_userbyid(oid); postlite has a single role.
func pgGetUserbyid(args ...interface{}) string { return "sqlite3" }

// pgTableIsVisible implements pg_table_is_visible(oid). Every object lives in the
// single visible schema, so it is always true.
func pgTableIsVisible(args ...interface{}) int64 { return 1 }

// pgFunctionIsVisible / pgTypeIsVisible mirror pg_table_is_visible.
func pgFunctionIsVisible(args ...interface{}) int64 { return 1 }
func pgTypeIsVisible(args ...interface{}) int64     { return 1 }

// objDescription implements obj_description(oid[, catalog]); no comments are
// stored, so it returns an empty string. (go-sqlite3 registered functions cannot
// return a typeless NULL, and an empty string is harmless for the display/filter
// uses these have.)
func objDescription(args ...interface{}) string   { return "" }
func colDescription(args ...interface{}) string   { return "" }
func shobjDescription(args ...interface{}) string { return "" }

// has*Privilege functions report that the single role may do anything.
func hasTablePrivilege(args ...interface{}) int64    { return 1 }
func hasSchemaPrivilege(args ...interface{}) int64   { return 1 }
func hasColumnPrivilege(args ...interface{}) int64   { return 1 }
func hasDatabasePrivilege(args ...interface{}) int64 { return 1 }
func hasAnyColumnPrivilege(args ...interface{}) int64 {
	return 1
}

// pgEncodingToChar implements pg_encoding_to_char(int); we only speak UTF8.
func pgEncodingToChar(args ...interface{}) string { return "UTF8" }

// pgBackendPid implements pg_backend_pid().
func pgBackendPid() int64 { return 0 }

// pgGetServerVersionNum reports the numeric server version (14.0 -> 140000).
func pgGetServerVersionNum() int64 { return 140000 }

// regexpMatch backs SQLite's REGEXP operator, which our OPERATOR(pg_catalog.~)
// rewrite produces. It implements a forgiving match: a literal substring test
// with PostgreSQL's common anchors honored.
func regexpMatch(pattern, s string) bool {
	return matchPattern(pattern, s)
}

func matchPattern(pattern, s string) bool {
	// Handle the simple anchored/unanchored substring patterns psql emits for
	// object-name filters (e.g. '^(users)$'); fall back to substring containment.
	p := pattern
	anchoredStart := strings.HasPrefix(p, "^")
	anchoredEnd := strings.HasSuffix(p, "$")
	p = strings.TrimPrefix(p, "^")
	p = strings.TrimSuffix(p, "$")
	p = strings.TrimPrefix(p, "(")
	p = strings.TrimSuffix(p, ")")
	switch {
	case anchoredStart && anchoredEnd:
		return s == p
	case anchoredStart:
		return strings.HasPrefix(s, p)
	case anchoredEnd:
		return strings.HasSuffix(s, p)
	default:
		return strings.Contains(s, p)
	}
}

// --- declared-type helpers used by the catalog views ---

// sqliteDeclToPGType maps a SQLite declared type to a PostgreSQL data_type name as
// it appears in information_schema.columns.
func sqliteDeclToPGType(decl string) string {
	switch oidForType(decl) {
	case pgtype.BoolOID:
		return "boolean"
	case pgtype.Int8OID:
		return "bigint"
	case pgtype.Float8OID:
		return "double precision"
	case pgtype.NumericOID:
		return "numeric"
	case pgtype.ByteaOID:
		return "bytea"
	case pgtype.DateOID:
		return "date"
	case pgtype.TimeOID:
		return "time without time zone"
	case pgtype.TimestampOID:
		return "timestamp without time zone"
	default:
		return "text"
	}
}

// sqliteDeclToUDT maps a SQLite declared type to the PostgreSQL udt_name (the
// internal type name, e.g. int8/float8/bool).
func sqliteDeclToUDT(decl string) string {
	switch oidForType(decl) {
	case pgtype.BoolOID:
		return "bool"
	case pgtype.Int8OID:
		return "int8"
	case pgtype.Float8OID:
		return "float8"
	case pgtype.NumericOID:
		return "numeric"
	case pgtype.ByteaOID:
		return "bytea"
	case pgtype.DateOID:
		return "date"
	case pgtype.TimeOID:
		return "time"
	case pgtype.TimestampOID:
		return "timestamp"
	default:
		return "text"
	}
}

// sqliteDeclToOID maps a SQLite declared type to its PostgreSQL type OID, for the
// pg_attribute.atttypid / pg_type joins.
func sqliteDeclToOID(decl string) int64 { return int64(oidForType(decl)) }

// catalogFuncs is the set of pg_catalog / session functions registered on every
// SQLite connection (the version/current_* functions are registered separately
// because they predate this map). Names beginning with "__" are internal helpers
// the catalog views call.
var catalogFuncs = map[string]interface{}{
	"current_database":                   currentDatabase,
	"current_setting":                    currentSetting,
	"pg_get_expr":                        pgGetExpr,
	"pg_get_constraintdef":               pgGetConstraintdef,
	"pg_get_indexdef":                    pgGetIndexdef,
	"pg_get_function_identity_arguments": pgGetFunctionIdentityArguments,
	"pg_get_userbyid":                    pgGetUserbyid,
	"pg_table_is_visible":                pgTableIsVisible,
	"pg_function_is_visible":             pgFunctionIsVisible,
	"pg_type_is_visible":                 pgTypeIsVisible,
	"obj_description":                    objDescription,
	"col_description":                    colDescription,
	"shobj_description":                  shobjDescription,
	"has_table_privilege":                hasTablePrivilege,
	"has_schema_privilege":               hasSchemaPrivilege,
	"has_column_privilege":               hasColumnPrivilege,
	"has_database_privilege":             hasDatabasePrivilege,
	"has_any_column_privilege":           hasAnyColumnPrivilege,
	"pg_encoding_to_char":                pgEncodingToChar,
	"pg_backend_pid":                     pgBackendPid,
	"pg_get_server_version_num":          pgGetServerVersionNum,
	"regexp":                             regexpMatch,
	"__pg_type_name":                     sqliteDeclToPGType,
	"__pg_udt_name":                      sqliteDeclToUDT,
	"__pg_type_oid":                      sqliteDeclToOID,
}

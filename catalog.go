package postlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// This file holds the catalog data and the SQL that builds the parts of the
// PostgreSQL catalog derived from the live user schema.
//
// Two mechanisms are used, picked per object:
//   - Static, curated catalogs that do NOT depend on the user's tables (pg_type,
//     pg_settings, ...) are Go virtual tables in the attached pg_catalog schema;
//     this file fills in their row data.
//   - Catalogs that reflect the user's tables (pg_class, pg_attribute, pg_index,
//     pg_constraint, and all of information_schema) are TEMP VIEWS over
//     sqlite_master + pragma table-valued functions. A view cannot live in an
//     attached :memory: schema and still reference main's tables, so they live in
//     temp; the rewriter strips the pg_catalog./information_schema. qualifiers so
//     clients reach them by their unqualified / flattened names.

// catalogNameToken is replaced in the catalog view SQL with the SQL string
// literal of the connection's database name. information_schema exposes the
// database name in its *_catalog columns, and client libraries (Knex/NocoDB) filter
// on it, so it must equal the name the client actually connected with rather than a
// constant.
const catalogNameToken = "$$CATALOG$$"

// notVirtualToken is replaced in catalog view SQL with notVirtualPredicate. The
// derived catalogs walk main.sqlite_master and call pragma table-valued functions
// (pragma_table_info / pragma_index_list / pragma_foreign_key_list) on every row;
// a virtual table (e.g. an FTS5 table) appears in sqlite_master as type='table',
// and touching it with a pragma forces SQLite to load its module. postlite's build
// does not include fts5/rtree/etc., so that load fails with "no such module: fts5"
// and aborts the whole introspection query. PostgreSQL has no such tables anyway,
// so exclude them — and their internal shadow tables — from every catalog walk.
const notVirtualToken = "$$NOTVT$$"

// notVirtualPredicate excludes virtual tables and their shadow tables from a
// sqlite_master walk aliased "m". It has three parts:
//   - the table itself is classified virtual or shadow by pragma_table_list (which,
//     unlike the pragma_* functions above, reads cached schema metadata and so is
//     safe to call even when the module is missing);
//   - shadow tables whose owning module is absent are NOT flagged "shadow" by
//     SQLite (shadow marking runs through the module's xShadowName callback), so we
//     also drop any table whose name is "<virtualtable>_<suffix>" — a virtual table
//     is always classified "virtual" from its CREATE syntax, no module required.
//
// Every term references only "m", so SQLite evaluates them at the outer-table loop
// level, before the pragma TVF is invoked for that row — the excluded table is
// never touched.
const notVirtualPredicate = `AND m.name NOT IN (SELECT name FROM pragma_table_list WHERE schema = 'main' AND type IN ('virtual','shadow'))
		AND NOT EXISTS (SELECT 1 FROM pragma_table_list v WHERE v.schema = 'main' AND v.type = 'virtual' AND m.name LIKE v.name || '\_%' ESCAPE '\')`

// createCatalogViews creates the derived catalog temp views on the session's
// pinned connection. Temp objects are per-connection, so each session gets its own
// always-live view of its database, with the catalog name templated in.
func createCatalogViews(ctx context.Context, conn *sql.Conn, dbName string) error {
	lit := sqlStringLiteral(dbName)
	for _, stmt := range catalogViews {
		stmt = strings.ReplaceAll(stmt, catalogNameToken, lit)
		stmt = strings.ReplaceAll(stmt, notVirtualToken, notVirtualPredicate)
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("catalog view: %w\n%s", err, stmt)
		}
	}
	return nil
}

// sqlStringLiteral renders s as a single-quoted SQL string literal with embedded
// quotes doubled.
func sqlStringLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// catalogViews is the ordered list of CREATE TEMP VIEW statements. pg_catalog
// views come first; information_schema views are appended in
// catalog_information_schema.go's init so they can be maintained separately.
var catalogViews = []string{
	// pg_class: one row per table/view/index. oid is the sqlite_master rowid, which
	// is stable for the life of the connection.
	`CREATE TEMP VIEW pg_class AS
		SELECT
			m.rowid AS oid,
			m.name AS relname,
			2200 AS relnamespace,
			0 AS reltype,
			0 AS reloftype,
			10 AS relowner,
			0 AS relam,
			m.rowid AS relfilenode,
			0 AS reltablespace,
			0 AS relpages,
			-1 AS reltuples,
			0 AS relallvisible,
			0 AS reltoastrelid,
			(SELECT CASE WHEN count(*) > 0 THEN 1 ELSE 0 END FROM pragma_index_list(m.name)) AS relhasindex,
			0 AS relisshared,
			'p' AS relpersistence,
			CASE m.type WHEN 'table' THEN 'r' WHEN 'view' THEN 'v' WHEN 'index' THEN 'i' ELSE 'r' END AS relkind,
			(SELECT count(*) FROM pragma_table_info(m.name)) AS relnatts,
			0 AS relchecks,
			0 AS relhasrules,
			0 AS relhastriggers,
			0 AS relhassubclass,
			0 AS relrowsecurity,
			0 AS relforcerowsecurity,
			1 AS relispopulated,
			'd' AS relreplident,
			0 AS relispartition,
			0 AS relfrozenxid,
			0 AS relminmxid,
			'' AS relacl,
			'' AS reloptions
		FROM main.sqlite_master m
		WHERE m.type IN ('table','view','index') AND m.name NOT LIKE 'sqlite_%' $$NOTVT$$`,

	// pg_attribute: one row per column of a table/view (attnum is 1-based, like pg).
	`CREATE TEMP VIEW pg_attribute AS
		SELECT
			m.rowid AS attrelid,
			m.name AS attrelname,
			p.name AS attname,
			__pg_type_oid(p.type) AS atttypid,
			0 AS attstattarget,
			-1 AS attlen,
			p.cid + 1 AS attnum,
			0 AS attndims,
			-1 AS attcacheoff,
			-1 AS atttypmod,
			CASE WHEN p."notnull" = 1 OR p.pk > 0 THEN 1 ELSE 0 END AS attnotnull,
			CASE WHEN p.dflt_value IS NOT NULL THEN 1 ELSE 0 END AS atthasdef,
			'' AS attidentity,
			'' AS attgenerated,
			0 AS attisdropped,
			1 AS attislocal,
			0 AS attinhcount,
			0 AS attcollation,
			'' AS attacl,
			'' AS attoptions
		FROM main.sqlite_master m
		JOIN pragma_table_info(m.name) p
		WHERE m.type IN ('table','view') AND m.name NOT LIKE 'sqlite_%' $$NOTVT$$`,

	// pg_index: one row per index (including the auto indexes SQLite creates for PK
	// and UNIQUE). indexrelid/indrelid are synthesized but stable.
	`CREATE TEMP VIEW pg_index AS
		SELECT
			m.rowid * 100000 + il.seq AS indexrelid,
			m.rowid AS indrelid,
			il.name AS indexrelname,
			(SELECT count(*) FROM pragma_index_info(il.name)) AS indnatts,
			CASE WHEN il."unique" = 1 THEN 1 ELSE 0 END AS indisunique,
			CASE WHEN il.origin = 'pk' THEN 1 ELSE 0 END AS indisprimary,
			0 AS indisexclusion,
			CASE WHEN il."unique" = 1 THEN 1 ELSE 0 END AS indimmediate,
			0 AS indisclustered,
			1 AS indisvalid,
			1 AS indisready,
			1 AS indislive
		FROM main.sqlite_master m
		JOIN pragma_index_list(m.name) il
		WHERE m.type = 'table' $$NOTVT$$`,

	// pg_constraint: primary keys, foreign keys and unique constraints, unioned.
	`CREATE TEMP VIEW pg_constraint AS
		SELECT
			m.rowid * 1000000 + 1 AS oid,
			m.name || '_pkey' AS conname,
			2200 AS connamespace,
			'p' AS contype,
			0 AS condeferrable,
			0 AS condeferred,
			1 AS convalidated,
			m.rowid AS conrelid,
			0 AS contypid,
			m.name AS conrelname,
			NULL AS confrelname,
			0 AS confrelid,
			'' AS confupdtype,
			'' AS confdeltype,
			'' AS confmatchtype,
			(SELECT json_group_array(attnum) FROM (SELECT ti.cid + 1 AS attnum FROM pragma_table_info(m.name) ti WHERE ti.pk > 0 ORDER BY ti.pk)) AS conkey,
			NULL AS confkey
		FROM main.sqlite_master m
		JOIN pragma_table_info(m.name) p ON p.pk > 0
		WHERE m.type = 'table' $$NOTVT$$
		GROUP BY m.rowid, m.name
		UNION ALL
		SELECT
			m.rowid * 1000000 + 1000 + fk.id AS oid,
			m.name || '_' || fk.id || '_fkey' AS conname,
			2200, 'f', 0, 0, 1,
			m.rowid AS conrelid,
			0,
			m.name AS conrelname,
			fk."table" AS confrelname,
			(SELECT rm.rowid FROM main.sqlite_master rm WHERE rm.name = fk."table") AS confrelid,
			CASE fk.on_update WHEN 'CASCADE' THEN 'c' WHEN 'SET NULL' THEN 'n' WHEN 'SET DEFAULT' THEN 'd' WHEN 'RESTRICT' THEN 'r' ELSE 'a' END,
			CASE fk.on_delete WHEN 'CASCADE' THEN 'c' WHEN 'SET NULL' THEN 'n' WHEN 'SET DEFAULT' THEN 'd' WHEN 'RESTRICT' THEN 'r' ELSE 'a' END,
			'f',
			(SELECT json_group_array(attnum) FROM (SELECT (SELECT t2.cid + 1 FROM pragma_table_info(m.name) t2 WHERE t2.name = fk2."from") AS attnum FROM pragma_foreign_key_list(m.name) fk2 WHERE fk2.id = fk.id ORDER BY fk2.seq)),
			(SELECT json_group_array(attnum) FROM (SELECT (SELECT t3.cid + 1 FROM pragma_table_info(fk."table") t3 WHERE t3.name = fk2."to") AS attnum FROM pragma_foreign_key_list(m.name) fk2 WHERE fk2.id = fk.id ORDER BY fk2.seq))
		FROM main.sqlite_master m
		JOIN pragma_foreign_key_list(m.name) fk
		WHERE m.type = 'table' $$NOTVT$$ AND fk.seq = 0
		UNION ALL
		SELECT
			m.rowid * 1000000 + 2000 + il.seq AS oid,
			il.name AS conname,
			2200, 'u', 0, 0, 1,
			m.rowid AS conrelid,
			0,
			m.name AS conrelname,
			NULL, 0, '', '', '',
			(SELECT json_group_array(attnum) FROM (SELECT (SELECT ti.cid + 1 FROM pragma_table_info(m.name) ti WHERE ti.name = ii.name) AS attnum FROM pragma_index_info(il.name) ii ORDER BY ii.seqno)),
			NULL
		FROM main.sqlite_master m
		JOIN pragma_index_list(m.name) il
		WHERE m.type = 'table' $$NOTVT$$ AND il."unique" = 1 AND il.origin = 'u'`,

	// pg_proc: postlite has no user-defined SQL functions to enumerate. The view is
	// empty but typed so introspection queries that join against it do not error.
	`CREATE TEMP VIEW pg_proc AS
		SELECT 0 AS oid, '' AS proname, 11 AS pronamespace, 10 AS proowner,
		       0 AS prolang, 0 AS prorettype, '' AS proargtypes, 0 AS pronargs
		WHERE 0`,

	// pg_database: one row for the database this session connected to. postlite
	// serves one SQLite file per connection, so the only database that "exists" from
	// a session's point of view is its own. Clients list databases and check for a
	// database's existence here (e.g. NocoDB's createDatabaseIfNotExists runs
	// "SELECT datname FROM pg_database WHERE datistemplate=false AND datname=$1").
	`CREATE TEMP VIEW pg_database AS
		SELECT
			1 AS oid,
			` + catalogNameToken + ` AS datname,
			10 AS datdba,
			6 AS encoding,
			'C' AS datcollate,
			'C' AS datctype,
			0 AS datistemplate,
			1 AS datallowconn,
			-1 AS datconnlimit,
			0 AS datlastsysoid,
			0 AS datfrozenxid,
			0 AS datminmxid,
			0 AS dattablespace,
			NULL AS datacl`,

	// pg_enum: postlite has no enum types. The view is empty but typed so the enum
	// look-ups client introspection performs (e.g. the enum_values sub-select in
	// NocoDB's columnList) resolve to NULL instead of erroring on a missing relation.
	`CREATE TEMP VIEW pg_enum AS
		SELECT 0 AS oid, 0 AS enumtypid, 0 AS enumsortorder, '' AS enumlabel WHERE 0`,

	// pg_get_keywords: PostgreSQL exposes the SQL keyword list as a set-returning
	// function pg_get_keywords(); psql reads it for tab-completion. SQLite has no such
	// function, so we provide the data as a view (the rewriter drops the call
	// parentheses). The list is representative rather than exhaustive — it only drives
	// client-side completion, never query results.
	`CREATE TEMP VIEW pg_get_keywords AS
		SELECT word, 'U' AS catcode, 'unreserved' AS catdesc FROM (
			SELECT 'abort' AS word UNION ALL SELECT 'add' UNION ALL SELECT 'all' UNION ALL
			SELECT 'alter' UNION ALL SELECT 'analyze' UNION ALL SELECT 'and' UNION ALL
			SELECT 'as' UNION ALL SELECT 'asc' UNION ALL SELECT 'begin' UNION ALL
			SELECT 'between' UNION ALL SELECT 'by' UNION ALL SELECT 'case' UNION ALL
			SELECT 'cast' UNION ALL SELECT 'check' UNION ALL SELECT 'collate' UNION ALL
			SELECT 'column' UNION ALL SELECT 'commit' UNION ALL SELECT 'constraint' UNION ALL
			SELECT 'create' UNION ALL SELECT 'cross' UNION ALL SELECT 'default' UNION ALL
			SELECT 'delete' UNION ALL SELECT 'desc' UNION ALL SELECT 'distinct' UNION ALL
			SELECT 'drop' UNION ALL SELECT 'else' UNION ALL SELECT 'end' UNION ALL
			SELECT 'except' UNION ALL SELECT 'exists' UNION ALL SELECT 'foreign' UNION ALL
			SELECT 'from' UNION ALL SELECT 'full' UNION ALL SELECT 'group' UNION ALL
			SELECT 'having' UNION ALL SELECT 'in' UNION ALL SELECT 'index' UNION ALL
			SELECT 'inner' UNION ALL SELECT 'insert' UNION ALL SELECT 'intersect' UNION ALL
			SELECT 'into' UNION ALL SELECT 'is' UNION ALL SELECT 'join' UNION ALL
			SELECT 'left' UNION ALL SELECT 'like' UNION ALL SELECT 'limit' UNION ALL
			SELECT 'not' UNION ALL SELECT 'null' UNION ALL SELECT 'on' UNION ALL
			SELECT 'or' UNION ALL SELECT 'order' UNION ALL SELECT 'outer' UNION ALL
			SELECT 'primary' UNION ALL SELECT 'references' UNION ALL SELECT 'right' UNION ALL
			SELECT 'rollback' UNION ALL SELECT 'select' UNION ALL SELECT 'set' UNION ALL
			SELECT 'table' UNION ALL SELECT 'then' UNION ALL SELECT 'union' UNION ALL
			SELECT 'unique' UNION ALL SELECT 'update' UNION ALL SELECT 'using' UNION ALL
			SELECT 'values' UNION ALL SELECT 'when' UNION ALL SELECT 'where' UNION ALL
			SELECT 'with'
		)`,
}

// --- static catalog data (filled into the Go virtual tables) ---

func scalarType(oid int, name, category string, length int) pgType {
	byval := 0
	if length > 0 && length <= 8 {
		byval = 1
	}
	return pgType{
		oid:          oid,
		typname:      name,
		typnamespace: 11, // pg_catalog
		typowner:     10,
		typlen:       length,
		typbyval:     byval,
		typtype:      "b", // base type
		typcategory:  category,
		typisdefined: 1,
		typdelim:     ",",
		typalign:     "i",
		typstorage:   "p",
	}
}

// pgTypes are the standard PostgreSQL scalar types client libraries expect to find
// when they resolve a type OID to a name (or look one up by name). The OIDs are the
// real PostgreSQL OIDs so they line up with what RowDescription advertises.
var pgTypes = []pgType{
	scalarType(16, "bool", "B", 1),
	scalarType(17, "bytea", "U", -1),
	scalarType(18, "char", "Z", 1),
	scalarType(19, "name", "S", 64),
	scalarType(20, "int8", "N", 8),
	scalarType(21, "int2", "N", 2),
	scalarType(23, "int4", "N", 4),
	scalarType(25, "text", "S", -1),
	scalarType(26, "oid", "N", 4),
	scalarType(700, "float4", "N", 4),
	scalarType(701, "float8", "N", 8),
	scalarType(1042, "bpchar", "S", -1),
	scalarType(1043, "varchar", "S", -1),
	scalarType(1082, "date", "D", 4),
	scalarType(1083, "time", "D", 8),
	scalarType(1114, "timestamp", "D", 8),
	scalarType(1184, "timestamptz", "D", 8),
	scalarType(1700, "numeric", "N", -1),
	scalarType(114, "json", "U", -1),
	scalarType(3802, "jsonb", "U", -1),
	scalarType(2950, "uuid", "U", 16),
}

func setting(name, val, vartype string) pgSetting {
	return pgSetting{
		name:      name,
		setting:   val,
		category:  "postlite",
		context:   "user",
		vartype:   vartype,
		source:    "default",
		boot_val:  val,
		reset_val: val,
	}
}

// pgSettings are the run-time parameters clients most commonly read from
// pg_settings / SHOW. They mirror the ParameterStatus values sent at startup.
var pgSettings = []pgSetting{
	setting("server_version", ServerVersion, "string"),
	setting("server_version_num", "140000", "integer"),
	setting("server_encoding", "UTF8", "string"),
	setting("client_encoding", "UTF8", "string"),
	setting("DateStyle", "ISO, MDY", "string"),
	setting("IntervalStyle", "postgres", "string"),
	setting("TimeZone", "UTC", "string"),
	setting("integer_datetimes", "on", "string"),
	setting("standard_conforming_strings", "on", "string"),
	setting("search_path", `"$user", public`, "string"),
	setting("max_connections", "100", "integer"),
	setting("max_identifier_length", "63", "integer"),
	setting("block_size", "8192", "integer"),
}

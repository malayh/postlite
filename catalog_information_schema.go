package postlite

// information_schema is the SQL-standard introspection surface, and the one
// node-postgres/Knex (and thus NocoDB) lean on most heavily: listing tables,
// describing columns with their types and nullability, and discovering primary and
// foreign keys.
//
// Each view is a TEMP VIEW over sqlite_master + pragma table-valued functions, with
// $$CATALOG$$ replaced by the connection's database name (see createCatalogViews).
// The rewriter flattens "information_schema.<x>" to "information_schema_<x>", and
// "table_schema = current_schema()" resolves to 'public', which is the single
// schema every object is reported in.
//
// These statements are appended to catalogViews so createCatalogViews builds them
// alongside the pg_catalog views.
func init() {
	catalogViews = append(catalogViews, informationSchemaViews...)
}

var informationSchemaViews = []string{
	`CREATE TEMP VIEW information_schema_tables AS
		SELECT
			$$CATALOG$$ AS table_catalog,
			'public' AS table_schema,
			m.name AS table_name,
			CASE m.type WHEN 'view' THEN 'VIEW' ELSE 'BASE TABLE' END AS table_type,
			NULL AS self_referencing_column_name,
			NULL AS reference_generation,
			NULL AS user_defined_type_catalog,
			NULL AS user_defined_type_schema,
			NULL AS user_defined_type_name,
			'YES' AS is_insertable_into,
			'NO' AS is_typed,
			NULL AS commit_action
		FROM main.sqlite_master m
		WHERE m.type IN ('table','view') AND m.name NOT LIKE 'sqlite_%' $$NOTVT$$`,

	`CREATE TEMP VIEW information_schema_columns AS
		SELECT
			$$CATALOG$$ AS table_catalog,
			'public' AS table_schema,
			m.name AS table_name,
			p.name AS column_name,
			p.cid + 1 AS ordinal_position,
			p.dflt_value AS column_default,
			CASE WHEN p."notnull" = 1 OR p.pk > 0 THEN 'NO' ELSE 'YES' END AS is_nullable,
			CASE WHEN ec.typname IS NOT NULL THEN 'USER-DEFINED' ELSE __pg_type_name(p.type) END AS data_type,
			NULL AS character_maximum_length,
			NULL AS character_octet_length,
			NULL AS numeric_precision,
			NULL AS numeric_precision_radix,
			NULL AS numeric_scale,
			NULL AS datetime_precision,
			NULL AS character_set_catalog,
			NULL AS character_set_schema,
			NULL AS character_set_name,
			NULL AS collation_catalog,
			NULL AS collation_schema,
			NULL AS collation_name,
			$$CATALOG$$ AS udt_catalog,
			CASE WHEN ec.typname IS NOT NULL THEN 'public' ELSE 'pg_catalog' END AS udt_schema,
			COALESCE(ec.typname, __pg_udt_name(p.type)) AS udt_name,
			'NO' AS is_identity,
			NULL AS identity_generation,
			'NEVER' AS is_generated,
			NULL AS generation_expression,
			'YES' AS is_updatable
		FROM main.sqlite_master m
		JOIN pragma_table_info(m.name) p
		LEFT JOIN __pg_enum_col ec ON ec.table_name = m.name AND ec.column_name = p.name COLLATE NOCASE
		WHERE m.type IN ('table','view') AND m.name NOT LIKE 'sqlite_%' $$NOTVT$$`,

	`CREATE TEMP VIEW information_schema_key_column_usage AS
		SELECT
			$$CATALOG$$ AS constraint_catalog,
			'public' AS constraint_schema,
			m.name || '_pkey' AS constraint_name,
			$$CATALOG$$ AS table_catalog,
			'public' AS table_schema,
			m.name AS table_name,
			p.name AS column_name,
			p.pk AS ordinal_position,
			NULL AS position_in_unique_constraint
		FROM main.sqlite_master m
		JOIN pragma_table_info(m.name) p
		WHERE m.type = 'table' $$NOTVT$$ AND p.pk > 0
		UNION ALL
		SELECT
			$$CATALOG$$,
			'public',
			m.name || '_' || fk.id || '_fkey',
			$$CATALOG$$,
			'public',
			m.name,
			fk."from",
			fk.seq + 1,
			fk.seq + 1
		FROM main.sqlite_master m
		JOIN pragma_foreign_key_list(m.name) fk
		WHERE m.type = 'table' $$NOTVT$$`,

	`CREATE TEMP VIEW information_schema_table_constraints AS
		SELECT
			$$CATALOG$$ AS constraint_catalog,
			'public' AS constraint_schema,
			m.name || '_pkey' AS constraint_name,
			$$CATALOG$$ AS table_catalog,
			'public' AS table_schema,
			m.name AS table_name,
			'PRIMARY KEY' AS constraint_type,
			'NO' AS is_deferrable,
			'NO' AS initially_deferred
		FROM main.sqlite_master m
		JOIN pragma_table_info(m.name) p ON p.pk > 0
		WHERE m.type = 'table' $$NOTVT$$
		GROUP BY m.name
		UNION ALL
		SELECT
			$$CATALOG$$, 'public', m.name || '_' || fk.id || '_fkey',
			$$CATALOG$$, 'public', m.name, 'FOREIGN KEY', 'NO', 'NO'
		FROM main.sqlite_master m
		JOIN pragma_foreign_key_list(m.name) fk
		WHERE m.type = 'table' $$NOTVT$$ AND fk.seq = 0
		UNION ALL
		SELECT
			$$CATALOG$$, 'public', il.name,
			$$CATALOG$$, 'public', m.name, 'UNIQUE', 'NO', 'NO'
		FROM main.sqlite_master m
		JOIN pragma_index_list(m.name) il
		WHERE m.type = 'table' $$NOTVT$$ AND il."unique" = 1 AND il.origin = 'u'`,

	`CREATE TEMP VIEW information_schema_referential_constraints AS
		SELECT
			$$CATALOG$$ AS constraint_catalog,
			'public' AS constraint_schema,
			m.name || '_' || fk.id || '_fkey' AS constraint_name,
			$$CATALOG$$ AS unique_constraint_catalog,
			'public' AS unique_constraint_schema,
			fk."table" || '_pkey' AS unique_constraint_name,
			'NONE' AS match_option,
			COALESCE(fk.on_update, 'NO ACTION') AS update_rule,
			COALESCE(fk.on_delete, 'NO ACTION') AS delete_rule
		FROM main.sqlite_master m
		JOIN pragma_foreign_key_list(m.name) fk
		WHERE m.type = 'table' $$NOTVT$$ AND fk.seq = 0`,

	`CREATE TEMP VIEW information_schema_constraint_column_usage AS
		SELECT
			$$CATALOG$$ AS table_catalog,
			'public' AS table_schema,
			fk."table" AS table_name,
			COALESCE(fk."to", (SELECT ti.name FROM pragma_table_info(fk."table") ti WHERE ti.pk = 1)) AS column_name,
			$$CATALOG$$ AS constraint_catalog,
			'public' AS constraint_schema,
			m.name || '_' || fk.id || '_fkey' AS constraint_name
		FROM main.sqlite_master m
		JOIN pragma_foreign_key_list(m.name) fk
		WHERE m.type = 'table' $$NOTVT$$
		UNION ALL
		SELECT
			$$CATALOG$$, 'public', m.name, p.name,
			$$CATALOG$$, 'public', m.name || '_pkey'
		FROM main.sqlite_master m
		JOIN pragma_table_info(m.name) p
		WHERE m.type = 'table' $$NOTVT$$ AND p.pk > 0`,

	`CREATE TEMP VIEW information_schema_views AS
		SELECT
			$$CATALOG$$ AS table_catalog,
			'public' AS table_schema,
			m.name AS table_name,
			m.sql AS view_definition,
			'NONE' AS check_option,
			'NO' AS is_updatable,
			'NO' AS is_insertable_into,
			'NO' AS is_trigger_updatable,
			'NO' AS is_trigger_deletable,
			'NO' AS is_trigger_insertable_into
		FROM main.sqlite_master m
		WHERE m.type = 'view'`,

	`CREATE TEMP VIEW information_schema_schemata AS
		SELECT $$CATALOG$$ AS catalog_name, 'public' AS schema_name, 'sqlite3' AS schema_owner,
		       NULL AS default_character_set_catalog, NULL AS default_character_set_schema,
		       NULL AS default_character_set_name, NULL AS sql_path
		UNION ALL SELECT $$CATALOG$$, 'pg_catalog', 'sqlite3', NULL, NULL, NULL, NULL
		UNION ALL SELECT $$CATALOG$$, 'information_schema', 'sqlite3', NULL, NULL, NULL, NULL`,

	`CREATE TEMP VIEW information_schema_sequences AS
		SELECT $$CATALOG$$ AS sequence_catalog, 'public' AS sequence_schema, '' AS sequence_name,
		       'bigint' AS data_type, 64 AS numeric_precision, 2 AS numeric_precision_radix, 0 AS numeric_scale,
		       '1' AS start_value, '1' AS minimum_value, '9223372036854775807' AS maximum_value,
		       '1' AS increment, 'NO' AS cycle_option
		WHERE 0`,

	`CREATE TEMP VIEW information_schema_triggers AS
		SELECT
			$$CATALOG$$ AS trigger_catalog,
			'public' AS trigger_schema,
			m.name AS trigger_name,
			'' AS event_manipulation,
			$$CATALOG$$ AS event_object_catalog,
			'public' AS event_object_schema,
			m.tbl_name AS event_object_table,
			1 AS action_order,
			NULL AS action_condition,
			m.sql AS action_statement,
			'ROW' AS action_orientation,
			'AFTER' AS action_timing
		FROM main.sqlite_master m
		WHERE m.type = 'trigger'`,
}

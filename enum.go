package postlite

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
)

// SQLite has no native enum type. ORMs (SQLAlchemy, Django, Rails, ...) that target
// both PostgreSQL and SQLite emulate an enum on SQLite as a text column with a
// CHECK (col IN ('a','b',...)) constraint — on PostgreSQL the same model emits a
// native CREATE TYPE ... AS ENUM. postlite reconstructs that intent: each such
// single-column, all-string-literal IN-list CHECK is surfaced as a synthetic
// PostgreSQL enum type, so strict clients/GUIs (e.g. NocoDB) that key single-select
// columns off pg_enum / a USER-DEFINED data_type recognize them as choice columns.
//
// This is a heuristic that goes slightly beyond a literal proxy (real PostgreSQL
// would not invent an enum from a CHECK constraint), but it is fully general and
// matches what the ORM would have produced against a real PostgreSQL server. CHECK
// constraints that are not a plain membership test (e.g. "a IS NULL OR b IS NULL")
// are left untouched.

// enumOIDBase is the first synthetic OID handed to a reconstructed enum type. It
// sits well above the static base-type OIDs (<= 3802) so a reconstructed enum can
// never collide with a real pg_type row on oid.
const enumOIDBase = 90000

// enumType is a synthetic PostgreSQL enum reconstructed from a single SQLite
// CHECK (col IN (...)) constraint. oid is assigned per connection.
type enumType struct {
	oid     int
	typname string // synthetic type name, e.g. "tasks_status"
	table   string
	column  string
	labels  []string
}

// enumCatalogTables are the per-connection temp tables the catalog views read the
// reconstructed enums from. They are populated by setupEnumCatalog before the
// catalog views are created.
var enumCatalogTables = []string{
	`CREATE TEMP TABLE __pg_enum_type (oid INTEGER, typname TEXT, typnamespace INTEGER)`,
	// The composite PRIMARY KEY (WITHOUT ROWID) stores labels physically in
	// (enumtypid, enumsortorder) order, so the correlated enumtypid scan clients'
	// label string_agg/group_concat performs visits them in sort order — this SQLite
	// (3.38) has no ordered aggregates to lean on instead.
	`CREATE TEMP TABLE __pg_enum_label (enumtypid INTEGER, enumsortorder INTEGER, enumlabel TEXT, PRIMARY KEY (enumtypid, enumsortorder)) WITHOUT ROWID`,
	`CREATE TEMP TABLE __pg_enum_col (table_name TEXT, column_name TEXT, typoid INTEGER, typname TEXT)`,
}

// setupEnumCatalog creates the enum temp tables and populates them by parsing every
// user table's CREATE statement for IN-list CHECK constraints. It runs before the
// catalog views are created (the views reference these tables).
func setupEnumCatalog(ctx context.Context, conn *sql.Conn) error {
	for _, ddl := range enumCatalogTables {
		if _, err := conn.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("enum temp table: %w", err)
		}
	}

	// Read every table's CREATE SQL up front and close the cursor before issuing
	// INSERTs: a single *sql.Conn cannot run statements while a query is streaming.
	// Reading sqlite_master.sql is cached metadata, so it is safe even for virtual
	// tables whose module is absent (their CREATE VIRTUAL TABLE text simply has no
	// IN-list CHECK to find).
	rows, err := conn.QueryContext(ctx, `SELECT name, sql FROM main.sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND sql IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("read schema for enums: %w", err)
	}
	type table struct{ name, sql string }
	var tables []table
	for rows.Next() {
		var t table
		if err := rows.Scan(&t.name, &t.sql); err != nil {
			rows.Close()
			return fmt.Errorf("scan schema row: %w", err)
		}
		tables = append(tables, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read schema rows: %w", err)
	}

	oid := enumOIDBase
	for _, t := range tables {
		for _, e := range parseEnums(t.name, t.sql) {
			e.oid = oid
			oid++
			if _, err := conn.ExecContext(ctx,
				`INSERT INTO __pg_enum_type (oid, typname, typnamespace) VALUES (?, ?, 2200)`,
				e.oid, e.typname); err != nil {
				return fmt.Errorf("insert enum type: %w", err)
			}
			if _, err := conn.ExecContext(ctx,
				`INSERT INTO __pg_enum_col (table_name, column_name, typoid, typname) VALUES (?, ?, ?, ?)`,
				e.table, e.column, e.oid, e.typname); err != nil {
				return fmt.Errorf("insert enum col: %w", err)
			}
			for i, label := range e.labels {
				if _, err := conn.ExecContext(ctx,
					`INSERT INTO __pg_enum_label (enumtypid, enumsortorder, enumlabel) VALUES (?, ?, ?)`,
					e.oid, i+1, label); err != nil {
					return fmt.Errorf("insert enum label: %w", err)
				}
			}
		}
	}
	return nil
}

// checkKwRegex finds the start of a CHECK constraint's parenthesized body.
var checkKwRegex = regexp.MustCompile(`(?is)\bCHECK\s*\(`)

// inHeadRegex matches the head of a membership test — an identifier (bare, "quoted",
// [bracketed], or `backticked`) followed by IN and the opening parenthesis of the
// value list. It anchors at the start of the (already paren-stripped) CHECK body so
// compound predicates like "a IS NULL OR b IN (...)" are not mistaken for enums.
var inHeadRegex = regexp.MustCompile("(?is)^\\s*(?:\"([^\"]+)\"|\\[([^\\]]+)\\]|`([^`]+)`|([A-Za-z_]\\w*))\\s+IN\\s*\\(")

// parseEnums extracts the reconstructable enums from one table's CREATE SQL. A
// column with more than one IN-list CHECK keeps only the first (a degenerate case).
func parseEnums(table, ddl string) []enumType {
	var out []enumType
	seen := map[string]bool{}
	for idx := 0; idx < len(ddl); {
		loc := checkKwRegex.FindStringIndex(ddl[idx:])
		if loc == nil {
			break
		}
		open := idx + loc[1] - 1 // index of '(' immediately after CHECK
		close := matchParen(ddl, open)
		if close < 0 {
			break
		}
		body := ddl[open+1 : close]
		idx = close + 1

		col, labels, ok := parseInList(body)
		if !ok {
			continue
		}
		key := strings.ToLower(col)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, enumType{
			table:   table,
			column:  col,
			typname: strings.ToLower(table + "_" + col),
			labels:  labels,
		})
	}
	return out
}

// parseInList reports whether a CHECK body is exactly a "<col> IN (<string literals>)"
// membership test and, if so, returns the column name (unquoted) and the literal
// values (unescaped). Anything else — a compound predicate, a numeric list, a
// subquery — is rejected so only genuine string enums are reconstructed.
func parseInList(body string) (string, []string, bool) {
	loc := inHeadRegex.FindStringSubmatchIndex(body)
	if loc == nil {
		return "", nil, false
	}
	col := ""
	for g := 1; g <= 4; g++ { // first non-empty identifier capture group
		if loc[2*g] >= 0 {
			col = body[loc[2*g]:loc[2*g+1]]
			break
		}
	}
	open := loc[1] - 1 // the '(' the head regex ended on
	close := matchParen(body, open)
	if close < 0 {
		return "", nil, false
	}
	if strings.TrimSpace(body[close+1:]) != "" {
		return "", nil, false // trailing predicate -> not a pure membership test
	}

	var labels []string
	for _, a := range splitArgs(body[open+1 : close]) {
		a = strings.TrimSpace(a)
		if len(a) < 2 || a[0] != '\'' || a[len(a)-1] != '\'' {
			return "", nil, false // a non-string-literal element -> not a string enum
		}
		labels = append(labels, strings.ReplaceAll(a[1:len(a)-1], "''", "'"))
	}
	if len(labels) == 0 {
		return "", nil, false
	}
	return col, labels, true
}

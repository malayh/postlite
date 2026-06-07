package postlite

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Phase 8: the "feels like a real PostgreSQL" gate. These tests drive the server
// with pgx (a strict driver whose state machine mirrors node-postgres, which
// NocoDB uses) and perform the full sequence a database GUI / ORM does on connect:
// enumerate tables, describe each table's columns and types, and discover primary
// and foreign keys — all through information_schema, with parameters bound over the
// extended protocol. Then it exercises a realistic data round-trip. Nothing here is
// NocoDB-specific; it is the generic introspection any Postgres client performs.

type columnMeta struct {
	name     string
	dataType string
	nullable string
}

// introspectColumns runs the parameterized columnInfo query an ORM issues.
func introspectColumns(t *testing.T, conn *pgx.Conn, table string) []columnMeta {
	t.Helper()
	ctx := context.Background()
	rows, err := conn.Query(ctx, `
		SELECT column_name, data_type, is_nullable
		FROM information_schema.columns
		WHERE table_name = $1 AND table_schema = current_schema()
		ORDER BY ordinal_position`, table)
	if err != nil {
		t.Fatalf("columns query for %s: %v", table, err)
	}
	defer rows.Close()

	var cols []columnMeta
	for rows.Next() {
		var c columnMeta
		if err := rows.Scan(&c.name, &c.dataType, &c.nullable); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("columns rows: %v", err)
	}
	return cols
}

// TestE2E_NocoDBRelationList runs NocoDB's exact foreign-key introspection query,
// which uses PostgreSQL-only "LEFT JOIN LATERAL UNNEST(...) WITH ORDINALITY".
// postlite recognizes the shape and answers it from pragma_foreign_key_list, so
// the seed FK products.owner_id -> users.id comes back with the columns NocoDB
// reads (tn, cn, rtn, rcn, ur, dr).
func TestE2E_NocoDBRelationList(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	conn := pgxConnect(t, addr, "test.db")
	ctx := context.Background()

	const q = `SELECT
          sch.nspname    AS ts,
          pc.conname     AS cstn,
          tbl.relname    AS tn,
          col.attname    AS cn,
          f_sch.nspname  AS foreign_table_schema,
          f_tbl.relname  AS rtn,
          f_col.attname  AS rcn,
          pc.confupdtype AS ur,
          pc.confdeltype AS dr
        FROM pg_constraint pc
          LEFT JOIN LATERAL UNNEST(pc.conkey)  WITH ORDINALITY AS u(attnum, attposition)   ON TRUE
          LEFT JOIN LATERAL UNNEST(pc.confkey) WITH ORDINALITY AS f_u(attnum, attposition) ON f_u.attposition = u.attposition
          JOIN pg_class tbl ON tbl.oid = pc.conrelid
          JOIN pg_namespace sch ON sch.oid = tbl.relnamespace
          LEFT JOIN pg_attribute col ON (col.attrelid = tbl.oid AND col.attnum = u.attnum)
          LEFT JOIN pg_class f_tbl ON f_tbl.oid = pc.confrelid
          LEFT JOIN pg_namespace f_sch ON f_sch.oid = f_tbl.relnamespace
          LEFT JOIN pg_attribute f_col ON (f_col.attrelid = f_tbl.oid AND f_col.attnum = f_u.attnum)
        WHERE pc.contype = 'f' AND sch.nspname = $1 AND f_sch.nspname = sch.nspname
        ORDER BY tn`

	rows, err := conn.Query(ctx, q, "public")
	if err != nil {
		t.Fatalf("relationList query: %v", err)
	}
	defer rows.Close()

	type rel struct{ ts, cstn, tn, cn, fts, rtn, rcn, ur, dr string }
	var got []rel
	for rows.Next() {
		var r rel
		if err := rows.Scan(&r.ts, &r.cstn, &r.tn, &r.cn, &r.fts, &r.rtn, &r.rcn, &r.ur, &r.dr); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("relationList rows = %d, want 1 (%+v)", len(got), got)
	}
	if r := got[0]; r.tn != "products" || r.cn != "owner_id" || r.rtn != "users" || r.rcn != "id" {
		t.Errorf("FK = %s.%s -> %s.%s, want products.owner_id -> users.id", r.tn, r.cn, r.rtn, r.rcn)
	}
	if r := got[0]; r.ts != "public" || r.fts != "public" {
		t.Errorf("schemas = %s / %s, want public / public", r.ts, r.fts)
	}
}

// TestE2E_NocoDBColumnList drives NocoDB's columnList shape (detected by the
// pk_constraint_name1 alias) with its eight bind parameters. postlite swaps the
// whole statement for its information_schema_columns equivalent; this verifies the
// replacement executes over the extended protocol and reports the products columns
// with the primary key (id) flagged via ck = 'p'.
func TestE2E_NocoDBColumnList(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	conn := pgxConnect(t, addr, "test.db")
	ctx := context.Background()

	const q = `select c.table_name as tn, pk1.constraint_name as pk_constraint_name1
		from information_schema.columns c
		where c.table_catalog=$1 and c.table_schema=$2 and c.table_name=$3
		  and $4=$4 and $5=$5 and $6=$6 and $7=$7 and $8=$8`

	// Args map to the rewritten query's $1..$8; only $7 (schema) and $8 (table)
	// filter. Pass NocoDB's real semantics: catalog, schema, schema, table, table,
	// catalog, schema, table.
	rows, err := conn.Query(ctx, q, "test.db", "public", "public", "products", "products", "test.db", "public", "products")
	if err != nil {
		t.Fatalf("columnList query: %v", err)
	}
	defer rows.Close()

	ckByCol := map[string]any{}
	var order []string
	for rows.Next() {
		v, err := rows.Values()
		if err != nil {
			t.Fatalf("values: %v", err)
		}
		// Column order is fixed by columnListQuery: tn=0, cn=1, ..., ck=4.
		cn, _ := v[1].(string)
		order = append(order, cn)
		ckByCol[cn] = v[4]
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	want := []string{"id", "title", "price", "owner_id"}
	if len(order) != len(want) {
		t.Fatalf("columns = %v, want %v", order, want)
	}
	for i, c := range want {
		if order[i] != c {
			t.Errorf("column %d = %q, want %q", i, order[i], c)
		}
	}
	if ck, _ := ckByCol["id"].(string); ck != "p" {
		t.Errorf("id ck = %v, want \"p\" (primary key)", ckByCol["id"])
	}
	if ckByCol["title"] != nil {
		t.Errorf("title ck = %v, want nil (not a primary key)", ckByCol["title"])
	}
}

func TestE2E_GuiIntrospection(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	conn := pgxConnect(t, addr, "test.db")
	ctx := context.Background()

	// 1. Enumerate the user tables, exactly as a GUI's schema browser would.
	rows, err := conn.Query(ctx, `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'
		ORDER BY table_name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table: %v", err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	if !equal(tables, []string{"products", "users"}) {
		t.Fatalf("tables = %v, want [products users]", tables)
	}

	// 2. Describe each table's columns with their types.
	userCols := introspectColumns(t, conn, "users")
	if len(userCols) != 3 {
		t.Fatalf("users columns = %d, want 3: %+v", len(userCols), userCols)
	}
	if userCols[0].name != "id" || userCols[0].dataType != "bigint" || userCols[0].nullable != "NO" {
		t.Errorf("users.id meta = %+v, want {id bigint NO}", userCols[0])
	}
	if userCols[2].name != "email" || userCols[2].nullable != "YES" {
		t.Errorf("users.email meta = %+v, want nullable email", userCols[2])
	}

	prodCols := introspectColumns(t, conn, "products")
	types := map[string]string{}
	for _, c := range prodCols {
		types[c.name] = c.dataType
	}
	if types["price"] != "double precision" || types["title"] != "text" || types["owner_id"] != "bigint" {
		t.Errorf("products column types = %v", types)
	}

	// 3. Discover the primary key of users.
	var pkCol string
	err = conn.QueryRow(ctx, `
		SELECT kcu.column_name
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON kcu.constraint_name = tc.constraint_name
		WHERE tc.constraint_type = 'PRIMARY KEY' AND tc.table_name = $1`, "users").Scan(&pkCol)
	if err != nil {
		t.Fatalf("pk query: %v", err)
	}
	if pkCol != "id" {
		t.Errorf("users PK = %q, want id", pkCol)
	}

	// 4. Discover the foreign key products.owner_id -> users.id.
	var fkTable, fkCol, refTable, refCol string
	err = conn.QueryRow(ctx, `
		SELECT kcu.table_name, kcu.column_name, ccu.table_name, ccu.column_name
		FROM information_schema.referential_constraints rc
		JOIN information_schema.key_column_usage kcu ON kcu.constraint_name = rc.constraint_name
		JOIN information_schema.constraint_column_usage ccu ON ccu.constraint_name = rc.constraint_name
		WHERE kcu.table_name = $1`, "products").Scan(&fkTable, &fkCol, &refTable, &refCol)
	if err != nil {
		t.Fatalf("fk query: %v", err)
	}
	if fkTable != "products" || fkCol != "owner_id" || refTable != "users" || refCol != "id" {
		t.Errorf("fk = %s.%s -> %s.%s, want products.owner_id -> users.id", fkTable, fkCol, refTable, refCol)
	}
}

// After introspecting, a client reads and writes data and expects native Go types
// back — the proof that catalog metadata and result encoding agree.
func TestE2E_ReadWriteRoundTrip(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	conn := pgxConnect(t, addr, "test.db")
	ctx := context.Background()

	// Insert a product and read it back into native types.
	if _, err := conn.Exec(ctx,
		"INSERT INTO products (title, price, owner_id) VALUES ($1, $2, $3)",
		"gadget", 19.95, 1); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var (
		id      int64
		title   string
		price   float64
		ownerID int64
	)
	err := conn.QueryRow(ctx,
		"SELECT id, title, price, owner_id FROM products WHERE title = $1", "gadget").
		Scan(&id, &title, &price, &ownerID)
	if err != nil {
		t.Fatalf("select back: %v", err)
	}
	if title != "gadget" || price != 19.95 || ownerID != 1 {
		t.Errorf("round-trip = (%d, %q, %v, %d), want (_, gadget, 19.95, 1)", id, title, price, ownerID)
	}

	// A join across the discovered foreign key must work end to end.
	var owner string
	err = conn.QueryRow(ctx, `
		SELECT u.name FROM products p JOIN users u ON u.id = p.owner_id
		WHERE p.title = $1`, "gadget").Scan(&owner)
	if err != nil {
		t.Fatalf("fk join: %v", err)
	}
	if owner != "alice" {
		t.Errorf("owner = %q, want alice", owner)
	}
}

// pg_catalog (pg_class/pg_attribute) must also be navigable by a strict driver —
// some tools introspect via the catalog rather than information_schema.
func TestE2E_PgCatalogIntrospection(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	conn := pgxConnect(t, addr, "test.db")
	ctx := context.Background()

	rows, err := conn.Query(ctx, `
		SELECT a.attname, a.attnotnull
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid
		WHERE c.relname = $1 AND c.relkind = 'r'
		ORDER BY a.attnum`, "users")
	if err != nil {
		t.Fatalf("pg_catalog introspection: %v", err)
	}
	defer rows.Close()

	type attr struct {
		name    string
		notNull bool
	}
	var attrs []attr
	for rows.Next() {
		var a attr
		// attnotnull is advertised as a real bool OID, so it scans into a Go bool.
		if err := rows.Scan(&a.name, &a.notNull); err != nil {
			t.Fatalf("scan attr: %v", err)
		}
		attrs = append(attrs, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("attr rows: %v", err)
	}
	if len(attrs) != 3 {
		t.Fatalf("users attrs = %d, want 3: %+v", len(attrs), attrs)
	}
	if attrs[0].name != "id" || !attrs[0].notNull {
		t.Errorf("attr[0] = %+v, want {id true}", attrs[0])
	}
	if attrs[2].name != "email" || attrs[2].notNull {
		t.Errorf("attr[2] = %+v, want {email false}", attrs[2])
	}
}

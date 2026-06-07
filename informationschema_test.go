package postlite

import "testing"

// Phase 7: information_schema is the introspection surface node-postgres/Knex (and
// thus NocoDB) rely on. These tests issue the same shapes those libraries do —
// schema-qualified names, current_schema() / current_database() predicates, and
// the multi-view joins used to discover primary and foreign keys — against the
// seed schema (users, products, products.owner_id -> users.id).

// information_schema must be reachable case-insensitively: NocoDB's viewList issues
// "SELECT * FROM INFORMATION_SCHEMA.views" (uppercase schema), which must resolve to
// the information_schema_views temp view instead of erroring "no such table".
func TestInfoSchema_ViewsUppercase(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	if r := c.simpleQuery("CREATE VIEW active_users AS SELECT id, name FROM users"); r.err != nil {
		t.Fatalf("create view: %s", r.err.Message)
	}

	r := c.simpleQuery("SELECT * FROM INFORMATION_SCHEMA.views WHERE table_schema = 'public'")
	if r.err != nil {
		t.Fatalf("INFORMATION_SCHEMA.views query: %s", r.err.Message)
	}
	found := false
	for _, row := range r.rows {
		if row[2] == "active_users" { // table_name is the 3rd column of the view
			found = true
		}
	}
	if !found {
		t.Errorf("INFORMATION_SCHEMA.views did not list the created view; rows=%v", r.rows)
	}
}

func TestInfoSchema_Tables(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	r := c.simpleQuery(`
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'
		ORDER BY table_name`)
	if r.err != nil {
		t.Fatalf("tables query: %s", r.err.Message)
	}
	if !equal(flatten(r.rows), []string{"products", "users"}) {
		t.Errorf("tables = %v, want [products users]", flatten(r.rows))
	}
}

// Mirrors Knex's columnInfo(): filters by table_catalog (the connection's database
// name, sent as a value) and table_schema = current_schema().
func TestInfoSchema_ColumnsKnexShape(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	r := c.simpleQuery(`
		SELECT column_name, data_type, is_nullable, ordinal_position
		FROM information_schema.columns
		WHERE table_name = 'users'
		  AND table_catalog = 'test.db'
		  AND table_schema = current_schema()
		ORDER BY ordinal_position`)
	if r.err != nil {
		t.Fatalf("columns query: %s", r.err.Message)
	}
	if len(r.rows) != 3 {
		t.Fatalf("users columns = %d, want 3: %v", len(r.rows), r.rows)
	}
	// id bigint NOT NULL (PK), name text NOT NULL, email text NULL.
	want := [][]string{
		{"id", "bigint", "NO", "1"},
		{"name", "text", "NO", "2"},
		{"email", "text", "YES", "3"},
	}
	for i, w := range want {
		if !equal(r.rows[i], w) {
			t.Errorf("column %d = %v, want %v", i, r.rows[i], w)
		}
	}
}

func TestInfoSchema_ColumnTypes(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	r := c.simpleQuery(`
		SELECT column_name, data_type, udt_name FROM information_schema.columns
		WHERE table_name = 'products' ORDER BY ordinal_position`)
	if r.err != nil {
		t.Fatalf("query: %s", r.err.Message)
	}
	got := map[string][2]string{}
	for _, row := range r.rows {
		got[row[0]] = [2]string{row[1], row[2]}
	}
	cases := map[string][2]string{
		"id":       {"bigint", "int8"},
		"title":    {"text", "text"},
		"price":    {"double precision", "float8"},
		"owner_id": {"bigint", "int8"},
	}
	for col, want := range cases {
		if got[col] != want {
			t.Errorf("%s = %v, want %v", col, got[col], want)
		}
	}
}

func TestInfoSchema_PrimaryKey(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	r := c.simpleQuery(`
		SELECT kcu.column_name
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON kcu.constraint_name = tc.constraint_name
		WHERE tc.constraint_type = 'PRIMARY KEY' AND tc.table_name = 'users'`)
	if r.err != nil {
		t.Fatalf("pk query: %s", r.err.Message)
	}
	if len(r.rows) != 1 || r.rows[0][0] != "id" {
		t.Errorf("users PK columns = %v, want [[id]]", r.rows)
	}
}

// The canonical foreign-key discovery join used by Knex/NocoDB.
func TestInfoSchema_ForeignKey(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	r := c.simpleQuery(`
		SELECT kcu.table_name, kcu.column_name,
		       ccu.table_name AS ref_table, ccu.column_name AS ref_column,
		       rc.update_rule, rc.delete_rule
		FROM information_schema.referential_constraints rc
		JOIN information_schema.key_column_usage kcu
		  ON kcu.constraint_name = rc.constraint_name
		JOIN information_schema.constraint_column_usage ccu
		  ON ccu.constraint_name = rc.constraint_name
		WHERE kcu.table_name = 'products'`)
	if r.err != nil {
		t.Fatalf("fk query: %s", r.err.Message)
	}
	if len(r.rows) != 1 {
		t.Fatalf("fk rows = %d, want 1: %v", len(r.rows), r.rows)
	}
	row := r.rows[0]
	if row[0] != "products" || row[1] != "owner_id" || row[2] != "users" || row[3] != "id" {
		t.Errorf("fk = %v, want [products owner_id users id ...]", row)
	}
}

func TestInfoSchema_TableConstraintsTypes(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	// products has a PRIMARY KEY and a FOREIGN KEY.
	r := c.simpleQuery(`
		SELECT constraint_type FROM information_schema.table_constraints
		WHERE table_name = 'products' ORDER BY constraint_type`)
	if r.err != nil {
		t.Fatalf("query: %s", r.err.Message)
	}
	if !equal(flatten(r.rows), []string{"FOREIGN KEY", "PRIMARY KEY"}) {
		t.Errorf("products constraints = %v, want [FOREIGN KEY PRIMARY KEY]", flatten(r.rows))
	}
}

func TestInfoSchema_Schemata(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	r := c.simpleQuery(`SELECT schema_name FROM information_schema.schemata ORDER BY schema_name`)
	if r.err != nil {
		t.Fatalf("schemata query: %s", r.err.Message)
	}
	if !equal(flatten(r.rows), []string{"information_schema", "pg_catalog", "public"}) {
		t.Errorf("schemata = %v", flatten(r.rows))
	}
}

// current_database() must report the name the client connected with (templated
// into the catalog views and substituted into queries), not a constant.
func TestInfoSchema_CurrentDatabase(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)
	c := dial(t, addr, "test.db")

	r := c.simpleQuery(`SELECT current_database()`)
	if r.err != nil {
		t.Fatalf("current_database query: %s", r.err.Message)
	}
	if len(r.rows) != 1 || r.rows[0][0] != "test.db" {
		t.Errorf("current_database() = %v, want test.db", r.rows)
	}

	// And it lines up with table_catalog so "table_catalog = current_database()"
	// (a common predicate) selects rows.
	r = c.simpleQuery(`
		SELECT count(*) FROM information_schema.tables
		WHERE table_catalog = current_database() AND table_type = 'BASE TABLE'`)
	if r.err != nil {
		t.Fatalf("catalog match query: %s", r.err.Message)
	}
	if len(r.rows) != 1 || r.rows[0][0] != "2" {
		t.Errorf("tables in current_database = %v, want [[2]]", r.rows)
	}
}

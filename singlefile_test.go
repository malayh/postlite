package postlite

import (
	"context"
	"path/filepath"
	"testing"
)

// Single-file mode (the "mount one .db file" Docker workflow): a server configured
// with DatabasePath serves that exact file to every connection, regardless of the
// database name the client requests.
func TestSingleFile_ServesMountedFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mydata.db")
	seedTestDB(t, dbPath, seedSchema)

	s := NewServer()
	s.Addr = "127.0.0.1:0"
	s.DatabasePath = dbPath
	if err := s.Open(); err != nil {
		t.Fatalf("server open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	addr := s.ln.Addr().String()

	// Connect using an arbitrary database name; it must still resolve to the one
	// mounted file and see its data.
	conn := pgxConnect(t, addr, "literally_anything")
	ctx := context.Background()

	var name string
	if err := conn.QueryRow(ctx, "SELECT name FROM users WHERE id = 1").Scan(&name); err != nil {
		t.Fatalf("query mounted file: %v", err)
	}
	if name != "alice" {
		t.Errorf("name = %q, want alice", name)
	}

	// current_database() reflects the name the client connected with, so catalog
	// filters like "table_catalog = current_database()" still line up.
	var db string
	if err := conn.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
		t.Fatalf("current_database: %v", err)
	}
	if db != "literally_anything" {
		t.Errorf("current_database() = %q, want literally_anything", db)
	}
}

// Single-file mode plus authentication is the full Docker configuration.
func TestSingleFile_WithAuth(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mydata.db")
	seedTestDB(t, dbPath, seedSchema)

	s := NewServer()
	s.Addr = "127.0.0.1:0"
	s.DatabasePath = dbPath
	s.Username = "admin"
	s.Password = "p@ss"
	if err := s.Open(); err != nil {
		t.Fatalf("server open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	addr := s.ln.Addr().String()

	if _, err := tryPgxConnectAuth(t, addr, "app", "admin", "wrong"); err == nil {
		t.Fatal("expected rejection with wrong password")
	}

	conn, err := tryPgxConnectAuth(t, addr, "app", "admin", "p@ss")
	if err != nil {
		t.Fatalf("connect with correct credentials: %v", err)
	}
	var name string
	if err := conn.QueryRow(context.Background(), "SELECT name FROM users WHERE id = 2").Scan(&name); err != nil {
		t.Fatalf("query: %v", err)
	}
	if name != "bob" {
		t.Errorf("name = %q, want bob", name)
	}
}

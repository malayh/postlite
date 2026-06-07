package postlite

import (
	"database/sql"
	"net"
	"path/filepath"
	"testing"

	"github.com/jackc/pgproto3/v2"
)

// seedSchema is a small, representative schema (incl. a NOT NULL column and a
// foreign key) used by most tests.
const seedSchema = `
CREATE TABLE users (
	id    INTEGER PRIMARY KEY,
	name  TEXT NOT NULL,
	email TEXT
);
INSERT INTO users (name, email) VALUES ('alice', 'alice@example.com'), ('bob', 'bob@example.com');
CREATE TABLE products (
	id       INTEGER PRIMARY KEY,
	title    TEXT,
	price    REAL,
	owner_id INTEGER REFERENCES users(id)
);
`

// newTestServer creates a temp data directory containing a seeded SQLite database
// named "test.db", starts a Server on an ephemeral port, and returns the server
// and the address clients should dial. The server is closed via t.Cleanup.
func newTestServer(t *testing.T, seedSQL string) (*Server, string) {
	t.Helper()

	dir := t.TempDir()
	seedTestDB(t, filepath.Join(dir, "test.db"), seedSQL)

	s := NewServer()
	s.Addr = "127.0.0.1:0"
	s.DataDir = dir
	if err := s.Open(); err != nil {
		t.Fatalf("server open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	return s, s.ln.Addr().String()
}

// newAuthServer is like newTestServer but requires MD5 password authentication
// with the given username/password.
func newAuthServer(t *testing.T, seedSQL, username, password string) (*Server, string) {
	t.Helper()

	dir := t.TempDir()
	seedTestDB(t, filepath.Join(dir, "test.db"), seedSQL)

	s := NewServer()
	s.Addr = "127.0.0.1:0"
	s.DataDir = dir
	s.Username = username
	s.Password = password
	if err := s.Open(); err != nil {
		t.Fatalf("server open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	return s, s.ln.Addr().String()
}

// seedTestDB creates the SQLite file at path and runs seedSQL against it (or, when
// seedSQL is empty, touches the file so it exists), giving tests a deterministic
// starting point.
func seedTestDB(t *testing.T, path, seedSQL string) {
	t.Helper()

	sdb, err := sql.Open("postlite-sqlite3", path)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	if seedSQL != "" {
		if _, err := sdb.Exec(seedSQL); err != nil {
			t.Fatalf("seed db: %v", err)
		}
	} else {
		// Touch the database so the file exists.
		if _, err := sdb.Exec(`CREATE TABLE IF NOT EXISTS _postlite_touch (x)`); err != nil {
			t.Fatalf("touch db: %v", err)
		}
		if _, err := sdb.Exec(`DROP TABLE IF EXISTS _postlite_touch`); err != nil {
			t.Fatalf("touch db: %v", err)
		}
	}
	if err := sdb.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}
}

// openFileDB opens the on-disk database directly (bypassing the wire protocol),
// for asserting that writes made over the protocol actually persisted.
func openFileDB(t *testing.T, s *Server) *sql.DB {
	t.Helper()
	db, err := sql.Open("postlite-sqlite3", filepath.Join(s.DataDir, "test.db"))
	if err != nil {
		t.Fatalf("open file db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// client is a minimal Postgres frontend used to drive the server at the wire
// level. It lets tests assert the exact backend message sequence.
type client struct {
	t       *testing.T
	conn    net.Conn
	fe      *pgproto3.Frontend
	startup queryResult // messages received during the startup handshake
}

// dial connects, performs the startup handshake for the given database, and
// drains the handshake through the first ReadyForQuery.
func dial(t *testing.T, addr, database string) *client {
	t.Helper()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c := &client{
		t:    t,
		conn: conn,
		fe:   pgproto3.NewFrontend(pgproto3.NewChunkReader(conn), conn),
	}
	t.Cleanup(func() { _ = conn.Close() })

	if err := c.fe.Send(&pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": "sqlite3", "database": database},
	}); err != nil {
		t.Fatalf("send startup: %v", err)
	}
	c.startup = c.readResult()
	return c
}

// send writes a single frontend message, failing the test on error.
func (c *client) send(msg pgproto3.FrontendMessage) {
	c.t.Helper()
	if err := c.fe.Send(msg); err != nil {
		c.t.Fatalf("send %T: %v", msg, err)
	}
}

// queryResult is a decoded summary of the backend messages for one query.
type queryResult struct {
	seq        []string          // ordered backend message names
	params     map[string]string // ParameterStatus values (mainly during startup)
	fields     []pgproto3.FieldDescription
	rows       [][]string
	commandTag string
	err        *pgproto3.ErrorResponse
	txStatus   byte
}

// readResult reads backend messages until ReadyForQuery, decoding them into an
// owned queryResult. Values are copied as they arrive because pgproto3's Frontend
// reuses message buffers between Receive calls — keeping the raw messages and
// decoding later would alias every row to the last one read.
func (c *client) readResult() queryResult {
	c.t.Helper()
	var r queryResult
	for {
		m, err := c.fe.Receive()
		if err != nil {
			c.t.Fatalf("receive (after %v): %v", r.seq, err)
		}
		r.seq = append(r.seq, messageName(m))
		switch m := m.(type) {
		case *pgproto3.RowDescription:
			r.fields = copyFields(m.Fields)
		case *pgproto3.DataRow:
			row := make([]string, len(m.Values))
			for i, v := range m.Values {
				if v != nil {
					row[i] = string(v) // string() copies the bytes
				}
			}
			r.rows = append(r.rows, row)
		case *pgproto3.CommandComplete:
			r.commandTag = string(m.CommandTag)
		case *pgproto3.ParameterStatus:
			if r.params == nil {
				r.params = map[string]string{}
			}
			r.params[m.Name] = m.Value
		case *pgproto3.ErrorResponse:
			e := *m
			r.err = &e
		case *pgproto3.ReadyForQuery:
			r.txStatus = m.TxStatus
			return r
		}
	}
}

func copyFields(fields []pgproto3.FieldDescription) []pgproto3.FieldDescription {
	out := make([]pgproto3.FieldDescription, len(fields))
	for i, f := range fields {
		f.Name = append([]byte(nil), f.Name...)
		out[i] = f
	}
	return out
}

// has reports whether a backend message with the given name was received.
func (r queryResult) has(name string) bool {
	for _, n := range r.seq {
		if n == name {
			return true
		}
	}
	return false
}

func messageName(m pgproto3.BackendMessage) string {
	switch m.(type) {
	case *pgproto3.ParseComplete:
		return "ParseComplete"
	case *pgproto3.BindComplete:
		return "BindComplete"
	case *pgproto3.ParameterDescription:
		return "ParameterDescription"
	case *pgproto3.NoData:
		return "NoData"
	case *pgproto3.RowDescription:
		return "RowDescription"
	case *pgproto3.DataRow:
		return "DataRow"
	case *pgproto3.CommandComplete:
		return "CommandComplete"
	case *pgproto3.EmptyQueryResponse:
		return "EmptyQueryResponse"
	case *pgproto3.ErrorResponse:
		return "ErrorResponse"
	case *pgproto3.ReadyForQuery:
		return "ReadyForQuery"
	case *pgproto3.AuthenticationOk:
		return "AuthenticationOk"
	case *pgproto3.ParameterStatus:
		return "ParameterStatus"
	case *pgproto3.BackendKeyData:
		return "BackendKeyData"
	default:
		return "?"
	}
}

func (r queryResult) colNames() []string {
	names := make([]string, len(r.fields))
	for i, f := range r.fields {
		names[i] = string(f.Name)
	}
	return names
}

// simpleQuery runs a query via the simple query protocol.
func (c *client) simpleQuery(sql string) queryResult {
	c.t.Helper()
	c.send(&pgproto3.Query{String: sql})
	return c.readResult()
}

// extendedQuery runs a query via the extended protocol (Parse/Bind/Describe/
// Execute/Sync), binding the given text parameters to $1, $2, ...
func (c *client) extendedQuery(sql string, params ...string) queryResult {
	c.t.Helper()
	bind := make([][]byte, len(params))
	for i, p := range params {
		bind[i] = []byte(p)
	}
	c.send(&pgproto3.Parse{Query: sql})
	c.send(&pgproto3.Bind{Parameters: bind})
	c.send(&pgproto3.Describe{ObjectType: 'P'})
	c.send(&pgproto3.Execute{})
	c.send(&pgproto3.Sync{})
	return c.readResult()
}

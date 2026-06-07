package postlite

import (
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/jackc/pgproto3/v2"
	"github.com/jackc/pgx/v5"
)

// Authentication: when a password is configured, clients must complete the MD5
// password exchange; when it is not, every client is accepted (the original
// behavior, which the rest of the suite relies on).

// tryPgxConnectAuth attempts a pgx connection with explicit credentials and
// returns the error (if any) instead of failing the test, so negative cases can
// assert that a connection is refused.
func tryPgxConnectAuth(t *testing.T, addr, database, user, password string) (*pgx.Conn, error) {
	t.Helper()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable", user, password, host, port, database)
	conn, err := pgx.Connect(context.Background(), dsn)
	if err == nil {
		t.Cleanup(func() { _ = conn.Close(context.Background()) })
	}
	return conn, err
}

// A strict, real driver (pgx, whose handshake mirrors node-postgres/NocoDB) must
// authenticate with the right password and then run queries normally.
func TestAuth_PgxCorrectPassword(t *testing.T) {
	_, addr := newAuthServer(t, seedSchema, "postgres", "s3cret")

	conn, err := tryPgxConnectAuth(t, addr, "test.db", "postgres", "s3cret")
	if err != nil {
		t.Fatalf("connect with correct password: %v", err)
	}
	var name string
	if err := conn.QueryRow(context.Background(), "SELECT name FROM users WHERE id = 1").Scan(&name); err != nil {
		t.Fatalf("query after auth: %v", err)
	}
	if name != "alice" {
		t.Errorf("name = %q, want alice", name)
	}
}

func TestAuth_PgxWrongPasswordRejected(t *testing.T) {
	_, addr := newAuthServer(t, seedSchema, "postgres", "s3cret")

	if _, err := tryPgxConnectAuth(t, addr, "test.db", "postgres", "wrong"); err == nil {
		t.Fatal("expected connection with wrong password to be rejected")
	}
}

func TestAuth_PgxWrongUserRejected(t *testing.T) {
	_, addr := newAuthServer(t, seedSchema, "postgres", "s3cret")

	if _, err := tryPgxConnectAuth(t, addr, "test.db", "intruder", "s3cret"); err == nil {
		t.Fatal("expected connection with wrong username to be rejected")
	}
}

// With no password configured, any credentials connect (the default, trust-all
// behavior the existing tests depend on).
func TestAuth_NoPasswordAcceptsAnyone(t *testing.T) {
	_, addr := newTestServer(t, seedSchema)

	conn, err := tryPgxConnectAuth(t, addr, "test.db", "whoever", "whatever")
	if err != nil {
		t.Fatalf("connect to unauthenticated server: %v", err)
	}
	var name string
	if err := conn.QueryRow(context.Background(), "SELECT name FROM users WHERE id = 2").Scan(&name); err != nil {
		t.Fatalf("query: %v", err)
	}
	if name != "bob" {
		t.Errorf("name = %q, want bob", name)
	}
}

// dialMD5 performs the wire-level MD5 handshake by hand so we can assert the exact
// backend message flow. It returns the frontend positioned just after the startup
// handshake's ReadyForQuery, plus the result of that handshake.
func dialMD5(t *testing.T, addr, database, user, password string) (*pgproto3.Frontend, queryResult) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	fe := pgproto3.NewFrontend(pgproto3.NewChunkReader(conn), conn)
	if err := fe.Send(&pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": user, "database": database},
	}); err != nil {
		t.Fatalf("send startup: %v", err)
	}

	// The first message must be the MD5 challenge.
	msg, err := fe.Receive()
	if err != nil {
		t.Fatalf("receive auth request: %v", err)
	}
	md5req, ok := msg.(*pgproto3.AuthenticationMD5Password)
	if !ok {
		t.Fatalf("first message = %T, want *AuthenticationMD5Password", msg)
	}

	if err := fe.Send(&pgproto3.PasswordMessage{Password: md5Password(user, password, md5req.Salt)}); err != nil {
		t.Fatalf("send password: %v", err)
	}

	return fe, readFrontendResult(t, fe)
}

// readFrontendResult drains backend messages until ReadyForQuery or an
// ErrorResponse-terminated stream, recording what arrived. (The shared client
// helper assumes no auth challenge, so the auth tests use this directly.)
func readFrontendResult(t *testing.T, fe *pgproto3.Frontend) queryResult {
	t.Helper()
	var r queryResult
	for {
		m, err := fe.Receive()
		if err != nil {
			// A rejected client gets a FATAL ErrorResponse and the socket closes; a
			// receive error after we have already captured that error is expected.
			if r.err != nil {
				return r
			}
			t.Fatalf("receive (after %v): %v", r.seq, err)
		}
		r.seq = append(r.seq, messageName(m))
		switch m := m.(type) {
		case *pgproto3.ErrorResponse:
			e := *m
			r.err = &e
		case *pgproto3.ReadyForQuery:
			r.txStatus = m.TxStatus
			return r
		}
	}
}

// The happy-path MD5 handshake must yield AuthenticationOk and a ReadyForQuery.
func TestAuth_MD5HandshakeSucceeds(t *testing.T) {
	_, addr := newAuthServer(t, seedSchema, "postgres", "hunter2")

	_, r := dialMD5(t, addr, "test.db", "postgres", "hunter2")
	if r.err != nil {
		t.Fatalf("handshake failed: %s", r.err.Message)
	}
	if !r.has("AuthenticationOk") {
		t.Errorf("missing AuthenticationOk; sequence = %v", r.seq)
	}
	if r.txStatus != 'I' {
		t.Errorf("txStatus = %q, want I", r.txStatus)
	}
}

// A bad MD5 response must produce a FATAL ErrorResponse with SQLSTATE 28P01 and no
// AuthenticationOk.
func TestAuth_MD5HandshakeBadPassword(t *testing.T) {
	_, addr := newAuthServer(t, seedSchema, "postgres", "hunter2")

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	fe := pgproto3.NewFrontend(pgproto3.NewChunkReader(conn), conn)
	if err := fe.Send(&pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": "postgres", "database": "test.db"},
	}); err != nil {
		t.Fatalf("send startup: %v", err)
	}
	msg, err := fe.Receive()
	if err != nil {
		t.Fatalf("receive auth request: %v", err)
	}
	if _, ok := msg.(*pgproto3.AuthenticationMD5Password); !ok {
		t.Fatalf("first message = %T, want *AuthenticationMD5Password", msg)
	}
	// Send a deliberately wrong response.
	if err := fe.Send(&pgproto3.PasswordMessage{Password: "md5deadbeef"}); err != nil {
		t.Fatalf("send password: %v", err)
	}

	r := readFrontendResult(t, fe)
	if r.err == nil {
		t.Fatalf("expected an ErrorResponse; sequence = %v", r.seq)
	}
	if r.err.Code != "28P01" {
		t.Errorf("error code = %q, want 28P01", r.err.Code)
	}
	if r.has("AuthenticationOk") {
		t.Errorf("must not send AuthenticationOk on failure; sequence = %v", r.seq)
	}
}

// md5Password must match the canonical PostgreSQL scheme for a known salt, so that
// any standard client computes the same value. The expectation is a fixed vector
// for user "postgres", password "secret", salt {1,2,3,4}.
func TestMD5Password_KnownVector(t *testing.T) {
	salt := [4]byte{0x01, 0x02, 0x03, 0x04}
	got := md5Password("postgres", "secret", salt)
	const want = "md5bb41a296aab6baccb36ff243a562abff"
	if got != want {
		t.Errorf("md5Password = %q, want %q", got, want)
	}
}

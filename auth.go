package postlite

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgproto3/v2"
)

// errAuthFailed ends a session whose client failed authentication. The client has
// already been told via a FATAL ErrorResponse; this just stops the server from
// proceeding into the normal message loop.
var errAuthFailed = errors.New("authentication failed")

// authenticate runs the password handshake at the start of a session. When the
// server is configured without a password it is a no-op and every client is
// accepted (preserving the original trust-everyone behavior). When a password is
// set, the client must complete an MD5 password exchange with the configured
// username and password, mirroring how a real PostgreSQL server behaves; this is
// supported out of the box by psql, pgx, node-postgres (NocoDB), JDBC, and the
// other mainstream drivers.
//
// On success it returns nil and the caller continues the startup sequence (which
// sends AuthenticationOk). On failure it has already written a FATAL
// ErrorResponse and returns errAuthFailed so the session is torn down.
func (s *Server) authenticate(c *Conn, startup *pgproto3.StartupMessage) error {
	if s.Password == "" {
		return nil // authentication disabled
	}

	user := getParameter(startup.Parameters, "user")

	salt, err := randSalt()
	if err != nil {
		_ = writeMessages(c, &pgproto3.ErrorResponse{Severity: "FATAL", Code: "XX000", Message: "could not generate authentication salt"})
		return errAuthFailed
	}

	// Challenge the client for an MD5-hashed password.
	if err := writeMessages(c, &pgproto3.AuthenticationMD5Password{Salt: salt}); err != nil {
		return err
	}

	// Multiple frontend messages share the 'p' type byte; tell the backend that the
	// next one is a password response so it decodes correctly.
	if err := c.backend.SetAuthType(pgproto3.AuthTypeMD5Password); err != nil {
		return err
	}

	resp, err := c.backend.Receive()
	if err != nil {
		return fmt.Errorf("receive password message: %w", err)
	}
	pw, ok := resp.(*pgproto3.PasswordMessage)
	if !ok {
		_ = writeMessages(c, &pgproto3.ErrorResponse{Severity: "FATAL", Code: "08P01", Message: "expected a password message"})
		return errAuthFailed
	}

	expected := md5Password(user, s.Password, salt)
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(s.Username)) == 1
	pwOK := subtle.ConstantTimeCompare([]byte(expected), []byte(pw.Password)) == 1
	if !userOK || !pwOK {
		_ = writeMessages(c, &pgproto3.ErrorResponse{
			Severity: "FATAL",
			Code:     "28P01", // invalid_password
			Message:  fmt.Sprintf("password authentication failed for user %q", user),
		})
		return errAuthFailed
	}
	return nil
}

// md5Password computes the response a client sends for AuthenticationMD5Password:
// "md5" + md5( md5(password + username) + salt ), each md5 rendered as lowercase
// hex. This is the exact PostgreSQL MD5 authentication scheme, so any standard
// driver computes the same value.
func md5Password(username, password string, salt [4]byte) string {
	inner := md5.Sum([]byte(password + username))
	innerHex := hex.EncodeToString(inner[:])

	outer := md5.New()
	outer.Write([]byte(innerHex))
	outer.Write(salt[:])
	return "md5" + hex.EncodeToString(outer.Sum(nil))
}

// randSalt returns four random bytes used as the MD5 challenge salt.
func randSalt() ([4]byte, error) {
	var b [4]byte
	_, err := rand.Read(b[:])
	return b, err
}

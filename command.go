package postlite

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgproto3/v2"
	"github.com/mattn/go-sqlite3"
)

var paramRegex = regexp.MustCompile(`\$(\d+)`)

// countParams returns the number of distinct positional parameters ($1, $2, ...)
// referenced by a query, i.e. the highest placeholder index. Postgres clients use
// this (via ParameterDescription) to know how many parameters to bind.
func countParams(query string) int {
	max := 0
	for _, m := range paramRegex.FindAllStringSubmatch(query, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && n > max {
			max = n
		}
	}
	return max
}

var returningRegex = regexp.MustCompile(`(?i)\breturning\b`)

// leadingKeyword returns the uppercased first keyword of a statement (ignoring a
// leading open-paren, e.g. "(SELECT ...)").
func leadingKeyword(query string) string {
	for _, f := range strings.Fields(query) {
		f = strings.TrimLeft(f, "(")
		if f != "" {
			return strings.ToUpper(f)
		}
	}
	return ""
}

// isRowReturning reports whether a statement produces a result set (and so should
// be run with Query and described with a RowDescription). Writes with a RETURNING
// clause also return rows.
func isRowReturning(query string) bool {
	switch leadingKeyword(query) {
	case "SELECT", "VALUES", "WITH", "SHOW", "EXPLAIN", "TABLE", "PRAGMA":
		return true
	}
	return returningRegex.MatchString(query)
}

// commandTag builds the CommandComplete tag for a statement, e.g. "SELECT 3",
// "INSERT 0 1", "UPDATE 2", "DELETE 1", "CREATE TABLE", "BEGIN". rowsAffected is
// used for writes, rowsReturned for result sets (and for RETURNING writes).
func commandTag(query string, rowsAffected, rowsReturned int64) []byte {
	kw := leadingKeyword(query)
	writeCount := rowsAffected
	if isRowReturning(query) {
		writeCount = rowsReturned
	}
	switch kw {
	case "INSERT":
		// The first number is the (always-zero) legacy OID field.
		return []byte(fmt.Sprintf("INSERT 0 %d", writeCount))
	case "UPDATE", "DELETE":
		return []byte(fmt.Sprintf("%s %d", kw, writeCount))
	case "SELECT", "VALUES", "WITH", "SHOW", "TABLE":
		return []byte(fmt.Sprintf("SELECT %d", rowsReturned))
	case "CREATE", "DROP", "ALTER", "TRUNCATE", "GRANT", "REVOKE", "COMMENT", "REINDEX":
		return []byte(objectTag(query, kw))
	case "BEGIN", "START":
		return []byte("BEGIN")
	case "COMMIT", "END":
		return []byte("COMMIT")
	case "ROLLBACK":
		return []byte("ROLLBACK")
	case "SAVEPOINT", "RELEASE", "SET", "RESET", "PRAGMA", "VACUUM", "ANALYZE", "EXPLAIN":
		return []byte(kw)
	case "":
		return []byte("")
	default:
		return []byte(kw)
	}
}

// objectTag produces a two-word tag like "CREATE TABLE" / "DROP INDEX" when the
// object kind is recognized, otherwise just the verb.
func objectTag(query, kw string) string {
	fields := strings.Fields(query)
	if len(fields) >= 2 {
		switch obj := strings.ToUpper(fields[1]); obj {
		case "TABLE", "INDEX", "VIEW", "TRIGGER", "DATABASE", "SCHEMA", "SEQUENCE":
			return kw + " " + obj
		}
	}
	return kw
}

// Transaction status codes reported in ReadyForQuery.
const (
	txIdle       byte = 'I' // not in a transaction block
	txInProgress byte = 'T' // in a transaction block
	txFailed     byte = 'E' // in a failed (aborted) transaction block
)

// errInFailedTransaction is returned for any statement (other than COMMIT or
// ROLLBACK) issued while the transaction is in the aborted state, mirroring
// PostgreSQL, which ignores commands until the transaction block ends.
var errInFailedTransaction = errors.New("current transaction is aborted, commands ignored until end of transaction block")

// Transaction-control keyword classifiers. PostgreSQL also spells these START
// TRANSACTION / END / ABORT; we recognize the synonyms so the session's
// transaction state is tracked regardless of which the client uses.
func isTxBegin(kw string) bool    { return kw == "BEGIN" || kw == "START" }
func isTxCommit(kw string) bool   { return kw == "COMMIT" || kw == "END" }
func isTxRollback(kw string) bool { return kw == "ROLLBACK" || kw == "ABORT" }

// sqlState maps an error to a best-effort five-character SQLSTATE code so clients
// can branch on err.Code. The mapping is deliberately coarse; SQLite does not
// expose Postgres error codes.
func sqlState(err error) string {
	if errors.Is(err, errInFailedTransaction) {
		return "25P02" // in_failed_sql_transaction
	}
	var serr sqlite3.Error
	if errors.As(err, &serr) {
		// PostgreSQL never returns the bare class code 23000 for a constraint
		// violation; it returns a specific subclass (23502/23503/23505/23514), and
		// strict drivers (node-postgres/NocoDB) only handle those subclasses —
		// "23000 is not handled on database pg". SQLite's *extended* result code
		// distinguishes which constraint failed, so map it to the matching subclass.
		switch serr.ExtendedCode {
		case sqlite3.ErrConstraintNotNull:
			return "23502" // not_null_violation
		case sqlite3.ErrConstraintForeignKey:
			return "23503" // foreign_key_violation
		case sqlite3.ErrConstraintUnique, sqlite3.ErrConstraintPrimaryKey:
			return "23505" // unique_violation
		case sqlite3.ErrConstraintCheck:
			return "23514" // check_violation
		}
		switch serr.Code {
		case sqlite3.ErrConstraint:
			return "23514" // unclassified constraint: report a handled subclass
		case sqlite3.ErrReadonly:
			return "25006" // read_only_sql_transaction
		case sqlite3.ErrError:
			return "42601" // syntax_error (SQLite's generic "SQL logic" error)
		}
	}
	return "XX000" // internal_error
}

// writeError sends an ErrorResponse describing err. Used by the extended protocol,
// where the trailing ReadyForQuery is deferred until the client's Sync.
func writeError(w io.Writer, err error) error {
	return writeMessages(w, &pgproto3.ErrorResponse{
		Severity: "ERROR",
		Code:     sqlState(err),
		Message:  err.Error(),
	})
}

// writeErrorReady sends an ErrorResponse followed by ReadyForQuery. Used by the
// simple query protocol, which is self-contained and recovers immediately rather
// than waiting for a Sync. Crucially the connection is NOT closed, so a failed
// query no longer terminates the session.
func writeErrorReady(w io.Writer, txStatus byte, err error) error {
	return writeMessages(w,
		&pgproto3.ErrorResponse{Severity: "ERROR", Code: sqlState(err), Message: err.Error()},
		&pgproto3.ReadyForQuery{TxStatus: txStatus},
	)
}

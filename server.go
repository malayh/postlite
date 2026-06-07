package postlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/jackc/pgproto3/v2"
	"github.com/mattn/go-sqlite3"
	"golang.org/x/sync/errgroup"
)

// Postgres settings. ServerVersion is reported to clients via the server_version
// parameter and version() function; many drivers (and Knex/NocoDB) parse it to
// select a SQL dialect, so it must look like a real, recent PostgreSQL version.
const (
	ServerVersion = "14.0"
)

func init() {
	sql.Register("postlite-sqlite3", &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			if err := conn.RegisterFunc("current_catalog", currentCatalog, true); err != nil {
				return fmt.Errorf("cannot register current_catalog() function")
			}
			if err := conn.RegisterFunc("current_schema", currentSchema, true); err != nil {
				return fmt.Errorf("cannot register current_schema() function")
			}
			if err := conn.RegisterFunc("current_user", currentUser, true); err != nil {
				return fmt.Errorf("cannot register current_schema() function")
			}
			if err := conn.RegisterFunc("session_user", sessionUser, true); err != nil {
				return fmt.Errorf("cannot register session_user() function")
			}
			if err := conn.RegisterFunc("user", user, true); err != nil {
				return fmt.Errorf("cannot register user() function")
			}
			if err := conn.RegisterFunc("show", show, true); err != nil {
				return fmt.Errorf("cannot register show() function")
			}
			if err := conn.RegisterFunc("format_type", formatType, true); err != nil {
				return fmt.Errorf("cannot register format_type() function")
			}
			if err := conn.RegisterFunc("version", version, true); err != nil {
				return fmt.Errorf("cannot register version() function")
			}

			// Catalog/introspection functions that client libraries and GUIs call.
			for name, fn := range catalogFuncs {
				if err := conn.RegisterFunc(name, fn, true); err != nil {
					return fmt.Errorf("cannot register %s() function: %w", name, err)
				}
			}

			if err := conn.CreateModule("pg_namespace_module", &pgNamespaceModule{}); err != nil {
				return fmt.Errorf("cannot register pg_namespace module")
			}
			if err := conn.CreateModule("pg_description_module", &pgDescriptionModule{}); err != nil {
				return fmt.Errorf("cannot register pg_description module")
			}
			if err := conn.CreateModule("pg_database_module", &pgDatabaseModule{}); err != nil {
				return fmt.Errorf("cannot register pg_database module")
			}
			if err := conn.CreateModule("pg_settings_module", &pgSettingsModule{}); err != nil {
				return fmt.Errorf("cannot register pg_settings module")
			}
			if err := conn.CreateModule("pg_type_module", &pgTypeModule{}); err != nil {
				return fmt.Errorf("cannot register pg_type module")
			}
			if err := conn.CreateModule("pg_class_module", &pgClassModule{}); err != nil {
				return fmt.Errorf("cannot register pg_class module")
			}
			if err := conn.CreateModule("pg_range_module", &pgRangeModule{}); err != nil {
				return fmt.Errorf("cannot register pg_range module")
			}
			return nil
		},
	})
}

func currentCatalog() string { return "public" }
func currentSchema() string  { return "public" }

func currentUser() string { return "sqlite3" }
func sessionUser() string { return "sqlite3" }
func user() string        { return "sqlite3" }

func version() string {
	return "PostgreSQL " + ServerVersion + " (postlite) on x86_64-pc-linux-gnu"
}

type Server struct {
	mu    sync.Mutex
	ln    net.Listener
	conns map[*Conn]struct{}

	g      errgroup.Group
	ctx    context.Context
	cancel func()

	// Bind address to listen to Postgres wire protocol.
	Addr string

	// Directory that holds SQLite databases.
	DataDir string
}

func NewServer() *Server {
	s := &Server{
		conns: make(map[*Conn]struct{}),
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s
}

func (s *Server) Open() (err error) {
	// Ensure data directory exists.
	if _, err := os.Stat(s.DataDir); err != nil {
		return err
	}

	s.ln, err = net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}

	s.g.Go(func() error {
		if err := s.serve(); s.ctx.Err() != nil {
			return err // return error unless context canceled
		}
		return nil
	})
	return nil
}

func (s *Server) Close() (err error) {
	if s.ln != nil {
		if e := s.ln.Close(); err == nil {
			err = e
		}
	}
	s.cancel()

	// Track and close all open connections.
	if e := s.CloseClientConnections(); err == nil {
		err = e
	}

	if err := s.g.Wait(); err != nil {
		return err
	}
	return err
}

// CloseClientConnections disconnects all Postgres connections.
func (s *Server) CloseClientConnections() (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for conn := range s.conns {
		if e := conn.Close(); err == nil {
			err = e
		}
	}

	s.conns = make(map[*Conn]struct{})

	return err
}

// CloseClientConnection disconnects a Postgres connections.
func (s *Server) CloseClientConnection(conn *Conn) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.conns, conn)
	return conn.Close()
}

func (s *Server) serve() error {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return err
		}
		conn := newConn(c)

		// Track live connections.
		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()

		log.Println("connection accepted: ", conn.RemoteAddr())

		s.g.Go(func() error {
			defer s.CloseClientConnection(conn)

			if err := s.serveConn(s.ctx, conn); err != nil && s.ctx.Err() == nil {
				log.Printf("connection error, closing: %s", err)
				return nil
			}

			log.Printf("connection closed: %s", conn.RemoteAddr())
			return nil
		})
	}
}

func (s *Server) serveConn(ctx context.Context, c *Conn) error {
	if err := s.serveConnStartup(ctx, c); err != nil {
		return fmt.Errorf("startup: %w", err)
	}

	for {
		msg, err := c.backend.Receive()
		if err != nil {
			return fmt.Errorf("receive message: %w", err)
		}

		log.Printf("[recv] %#v", msg)

		// After an error in the extended protocol the backend discards all
		// messages until the next Sync, then reports ReadyForQuery.
		if c.skipUntilSync {
			if _, ok := msg.(*pgproto3.Sync); !ok {
				continue
			}
		}

		switch msg := msg.(type) {
		case *pgproto3.Query:
			if err := s.handleQueryMessage(ctx, c, msg); err != nil {
				return fmt.Errorf("query message: %w", err)
			}

		case *pgproto3.Parse:
			if err := s.handleParseMessage(ctx, c, msg); err != nil {
				return fmt.Errorf("parse message: %w", err)
			}

		case *pgproto3.Bind:
			if err := s.handleBindMessage(ctx, c, msg); err != nil {
				return fmt.Errorf("bind message: %w", err)
			}

		case *pgproto3.Describe:
			if err := s.handleDescribeMessage(ctx, c, msg); err != nil {
				return fmt.Errorf("describe message: %w", err)
			}

		case *pgproto3.Execute:
			if err := s.handleExecuteMessage(ctx, c, msg); err != nil {
				return fmt.Errorf("execute message: %w", err)
			}

		case *pgproto3.Close:
			if err := s.handleCloseMessage(ctx, c, msg); err != nil {
				return fmt.Errorf("close message: %w", err)
			}

		case *pgproto3.Sync:
			c.skipUntilSync = false
			if err := writeMessages(c, &pgproto3.ReadyForQuery{TxStatus: c.txStatus}); err != nil {
				return fmt.Errorf("sync ready: %w", err)
			}

		case *pgproto3.Flush: // we never buffer responses; nothing to flush
			continue

		case *pgproto3.Terminate:
			return nil // exit

		default:
			return fmt.Errorf("unexpected message type: %#v", msg)
		}
	}
}

func (s *Server) serveConnStartup(ctx context.Context, c *Conn) error {
	msg, err := c.backend.ReceiveStartupMessage()
	if err != nil {
		return fmt.Errorf("receive startup message: %w", err)
	}

	switch msg := msg.(type) {
	case *pgproto3.StartupMessage:
		if err := s.handleStartupMessage(ctx, c, msg); err != nil {
			return fmt.Errorf("startup message: %w", err)
		}
		return nil
	case *pgproto3.SSLRequest:
		if err := s.handleSSLRequestMessage(ctx, c, msg); err != nil {
			return fmt.Errorf("ssl request message: %w", err)
		}
		return nil
	case *pgproto3.CancelRequest:
		// We cannot cancel an in-flight query; acknowledge by closing cleanly.
		log.Printf("received cancel request: %#v", msg)
		return nil
	default:
		return fmt.Errorf("unexpected startup message: %#v", msg)
	}
}

func (s *Server) handleStartupMessage(ctx context.Context, c *Conn, msg *pgproto3.StartupMessage) (err error) {
	log.Printf("received startup message: %#v", msg)

	// Validate
	name := getParameter(msg.Parameters, "database")
	if name == "" {
		return writeMessages(c, &pgproto3.ErrorResponse{Message: "database required"})
	} else if strings.Contains(name, "..") {
		return writeMessages(c, &pgproto3.ErrorResponse{Message: "invalid database name"})
	}

	// Open SQL database & attach to the connection.
	if c.db, err = sql.Open("postlite-sqlite3", filepath.Join(s.DataDir, name)); err != nil {
		return err
	}

	// Pin a single underlying SQLite connection to this session. Every query,
	// prepared statement, transaction, attached catalog, and registered function
	// then shares one connection, so BEGIN/COMMIT/ROLLBACK and the in-memory
	// pg_catalog behave consistently. A *sql.DB is a pool that could route
	// statements to different connections, which would break transactions and
	// hide the attached catalog from later queries.
	if c.conn, err = c.db.Conn(ctx); err != nil {
		return fmt.Errorf("pin connection: %w", err)
	}

	// Attach an in-memory database for pg_catalog.
	if _, err := c.conn.ExecContext(ctx, `ATTACH ':memory:' AS pg_catalog`); err != nil {
		return fmt.Errorf("attach pg_catalog: %w", err)
	}

	// Register virtual tables to imitate postgres.
	if _, err := c.conn.ExecContext(ctx, "CREATE VIRTUAL TABLE IF NOT EXISTS pg_catalog.pg_namespace USING pg_namespace_module (oid, nspname, nspowner, nspacl)"); err != nil {
		return fmt.Errorf("create pg_namespace: %w", err)
	}
	if _, err := c.conn.ExecContext(ctx, "CREATE VIRTUAL TABLE IF NOT EXISTS pg_catalog.pg_description USING pg_description_module (objoid, classoid, objsubid, description)"); err != nil {
		return fmt.Errorf("create pg_description: %w", err)
	}
	if _, err := c.conn.ExecContext(ctx, "CREATE VIRTUAL TABLE IF NOT EXISTS pg_catalog.pg_database USING pg_database_module (oid, datname, datdba, encoding, datcollate, datctype, datistemplate, datallowconn, datconnlimit, datlastsysoid, datfrozenxid, datminmxid, dattablespace, datacl)"); err != nil {
		return fmt.Errorf("create pg_database: %w", err)
	}
	if _, err := c.conn.ExecContext(ctx, "CREATE VIRTUAL TABLE IF NOT EXISTS pg_catalog.pg_settings USING pg_settings_module (name, setting, unit, category, short_desc, extra_desc, context, vartype, source, min_val, max_val, enumvals, boot_val, reset_val, sourcefile, sourceline, pending_restart)"); err != nil {
		return fmt.Errorf("create pg_settings: %w", err)
	}
	if _, err := c.conn.ExecContext(ctx, "CREATE VIRTUAL TABLE IF NOT EXISTS pg_catalog.pg_type USING pg_type_module (oid, typname, typnamespace, typowner, typlen, typbyval, typtype, typcategory, typispreferred, typisdefined, typdelim, typrelid, typelem, typarray, typinput, typoutput, typreceive, typsend, typmodin, typmodout, typanalyze, typalign, typstorage, typnotnull, typbasetype, typtypmod, typndims, typcollation, typdefaultbin, typdefault, typacl)"); err != nil {
		return fmt.Errorf("create pg_type: %w", err)
	}
	if _, err := c.conn.ExecContext(ctx, "CREATE VIRTUAL TABLE IF NOT EXISTS pg_catalog.pg_range USING pg_range_module (rngtypid, rngsubtype, rngmultitypid, rngcollation, rngsubopc, rngcanonical, rngsubdiff)"); err != nil {
		return fmt.Errorf("create pg_range: %w", err)
	}

	// Create the catalog objects derived from the live user schema (pg_class,
	// pg_attribute, ... and information_schema) as temp views over sqlite_master +
	// pragma functions. These reflect the user's tables, so unlike the static
	// virtual tables above they cannot be precomputed.
	c.database = name
	if err := createCatalogViews(ctx, c.conn, name); err != nil {
		return fmt.Errorf("create catalog views: %w", err)
	}

	// Report the parameter set a real PostgreSQL server sends at startup. Drivers
	// read several of these (e.g. standard_conforming_strings governs string
	// escaping; server_version selects the SQL dialect). BackendKeyData provides
	// the keys a client would use to issue a CancelRequest.
	return writeMessages(c,
		&pgproto3.AuthenticationOk{},
		&pgproto3.ParameterStatus{Name: "server_version", Value: ServerVersion},
		&pgproto3.ParameterStatus{Name: "server_encoding", Value: "UTF8"},
		&pgproto3.ParameterStatus{Name: "client_encoding", Value: "UTF8"},
		&pgproto3.ParameterStatus{Name: "DateStyle", Value: "ISO, MDY"},
		&pgproto3.ParameterStatus{Name: "IntervalStyle", Value: "postgres"},
		&pgproto3.ParameterStatus{Name: "TimeZone", Value: "UTC"},
		&pgproto3.ParameterStatus{Name: "integer_datetimes", Value: "on"},
		&pgproto3.ParameterStatus{Name: "standard_conforming_strings", Value: "on"},
		&pgproto3.ParameterStatus{Name: "application_name", Value: getParameter(msg.Parameters, "application_name")},
		&pgproto3.BackendKeyData{ProcessID: randUint32(), SecretKey: randUint32()},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	)
}

// randUint32 returns a random uint32 for BackendKeyData. It falls back to a fixed
// value if the system RNG is unavailable (the keys are only used for query
// cancellation, which this server does not implement).
func randUint32() uint32 {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 1
	}
	return binary.BigEndian.Uint32(b[:])
}

func (s *Server) handleSSLRequestMessage(ctx context.Context, c *Conn, msg *pgproto3.SSLRequest) error {
	log.Printf("received ssl request message: %#v", msg)
	if _, err := c.Write([]byte("N")); err != nil {
		return err
	}
	return s.serveConnStartup(ctx, c)
}

func (s *Server) handleQueryMessage(ctx context.Context, c *Conn, msg *pgproto3.Query) error {
	log.Printf("received query: %q", msg.String)

	// Rewrite system-information queries so they're tolerable by SQLite. This is
	// applied to the simple query protocol too (not just Parse), so e.g. SET works
	// regardless of which protocol the client uses.
	query := c.rewrite(msg.String)
	if strings.TrimSpace(query) == "" {
		return writeMessages(c,
			&pgproto3.EmptyQueryResponse{},
			&pgproto3.ReadyForQuery{TxStatus: c.txStatus},
		)
	}

	// In a failed transaction block PostgreSQL ignores every command until the
	// block is ended; only COMMIT/ROLLBACK are honored (both discard the work).
	kw := leadingKeyword(query)
	if c.txStatus == txFailed {
		if isTxCommit(kw) || isTxRollback(kw) {
			c.rollbackFailed(ctx)
			return writeMessages(c,
				&pgproto3.CommandComplete{CommandTag: []byte("ROLLBACK")},
				&pgproto3.ReadyForQuery{TxStatus: txIdle},
			)
		}
		return writeErrorReady(c, txFailed, errInFailedTransaction)
	}

	// Statements that return no result set are run with Exec so we can report the
	// number of affected rows; result sets are streamed with Query.
	if !isRowReturning(query) {
		return s.execSimpleQuery(ctx, c, query)
	}
	return s.streamSimpleQuery(ctx, c, query)
}

// execSimpleQuery runs a non-row-returning statement (INSERT/UPDATE/DELETE/DDL/
// SET/...) and reports CommandComplete with the affected-row count.
func (s *Server) execSimpleQuery(ctx context.Context, c *Conn, query string) error {
	res, err := c.conn.ExecContext(ctx, query)
	if err != nil {
		return s.failSimple(c, err)
	}
	affected, _ := res.RowsAffected()
	c.noteCompleted(leadingKeyword(query))
	return writeMessages(c,
		&pgproto3.CommandComplete{CommandTag: commandTag(query, affected, 0)},
		&pgproto3.ReadyForQuery{TxStatus: c.txStatus},
	)
}

// streamSimpleQuery runs a result-returning statement and streams its rows.
func (s *Server) streamSimpleQuery(ctx context.Context, c *Conn, query string) error {
	rows, err := c.conn.QueryContext(ctx, query)
	if err != nil {
		return s.failSimple(c, err)
	}
	defer rows.Close()

	cols, err := rows.ColumnTypes()
	if err != nil {
		return s.failSimple(c, err)
	}
	oids := columnOIDs(cols)

	var buf []byte
	if len(cols) > 0 {
		buf = toRowDescription(cols, oids).Encode(buf)
	}

	var n int64
	for rows.Next() {
		row, err := scanRow(rows, oids)
		if err != nil {
			return s.failSimple(c, err)
		}
		buf = row.Encode(buf)
		n++
	}
	if err := rows.Err(); err != nil {
		return s.failSimple(c, err)
	}

	buf = (&pgproto3.CommandComplete{CommandTag: commandTag(query, 0, n)}).Encode(buf)
	buf = (&pgproto3.ReadyForQuery{TxStatus: c.txStatus}).Encode(buf)
	_, err = c.Write(buf)
	return err
}

// failSimple reports a query-level error during the simple query protocol. An
// error inside a transaction block moves it to the failed (aborted) state. The
// connection is never closed; the client recovers immediately.
func (s *Server) failSimple(c *Conn, err error) error {
	if c.txStatus == txInProgress {
		c.txStatus = txFailed
	}
	return writeErrorReady(c, c.txStatus, err)
}

// noteCompleted advances the transaction state after a statement with the given
// leading keyword completed successfully.
func (c *Conn) noteCompleted(kw string) {
	switch {
	case isTxBegin(kw):
		c.txStatus = txInProgress
	case isTxCommit(kw), isTxRollback(kw):
		c.txStatus = txIdle
	}
}

// rollbackFailed ends an aborted transaction block by rolling back the still-open
// SQLite transaction and returning the session to idle. Most statement errors
// leave SQLite's transaction open, but a few roll it back implicitly; a "no
// transaction is active" error here is therefore expected and ignored.
func (c *Conn) rollbackFailed(ctx context.Context) {
	_, _ = c.conn.ExecContext(ctx, "ROLLBACK")
	c.txStatus = txIdle
}

func toRowDescription(cols []*sql.ColumnType, oids []uint32) *pgproto3.RowDescription {
	var desc pgproto3.RowDescription
	for i, col := range cols {
		oid := oids[i]
		desc.Fields = append(desc.Fields, pgproto3.FieldDescription{
			Name:                 []byte(col.Name()),
			TableOID:             0,
			TableAttributeNumber: 0,
			DataTypeOID:          oid,
			DataTypeSize:         typeSize(oid),
			TypeModifier:         -1,
			Format:               0,
		})
	}
	return &desc
}

func scanRow(rows *sql.Rows, oids []uint32) (*pgproto3.DataRow, error) {
	refs := make([]interface{}, len(oids))
	values := make([]interface{}, len(oids))
	for i := range refs {
		refs[i] = &values[i]
	}

	// Scan from SQLite database.
	if err := rows.Scan(refs...); err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}

	// Encode each value into the Postgres text format for its column's type.
	row := pgproto3.DataRow{Values: make([][]byte, len(values))}
	for i := range values {
		row.Values[i] = encodeText(oids[i], values[i])
	}
	return &row, nil
}

// failExtended reports a query-level error during the extended protocol. It sends
// an ErrorResponse and arranges for subsequent messages to be discarded until the
// client's next Sync (per the Postgres protocol); the connection stays open. An
// error inside a transaction block moves it to the failed (aborted) state, which
// is reported by the ReadyForQuery sent on Sync.
func (s *Server) failExtended(c *Conn, err error) error {
	if c.txStatus == txInProgress {
		c.txStatus = txFailed
	}
	c.skipUntilSync = true
	return writeError(c, err)
}

// handleParseMessage prepares a statement and stores it under the given name
// ("" is the unnamed statement). It responds with ParseComplete.
func (s *Server) handleParseMessage(ctx context.Context, c *Conn, msg *pgproto3.Parse) error {
	query := c.rewrite(msg.Query)
	if msg.Query != query {
		log.Printf("query rewrite: %s", query)
	}

	stmt, err := c.conn.PrepareContext(ctx, query)
	if err != nil {
		return s.failExtended(c, fmt.Errorf("prepare: %w", err))
	}

	if old, ok := c.stmts[msg.Name]; ok && old.stmt != nil {
		old.stmt.Close()
	}
	c.stmts[msg.Name] = &preparedStatement{
		name:    msg.Name,
		query:   query,
		stmt:    stmt,
		nparams: countParams(query),
	}
	return writeMessages(c, &pgproto3.ParseComplete{})
}

// handleBindMessage binds parameter values to a prepared statement, producing a
// portal. It responds with BindComplete.
func (s *Server) handleBindMessage(ctx context.Context, c *Conn, msg *pgproto3.Bind) error {
	ps, ok := c.stmts[msg.PreparedStatement]
	if !ok {
		return s.failExtended(c, fmt.Errorf("prepared statement %q does not exist", msg.PreparedStatement))
	}

	// Parameters arrive as text (we advertise parameter type OID 0, so clients do
	// not binary-encode them). A nil value represents SQL NULL.
	params := make([]interface{}, len(msg.Parameters))
	for i, p := range msg.Parameters {
		if p != nil {
			params[i] = string(p)
		}
	}

	if old, ok := c.portals[msg.DestinationPortal]; ok {
		old.close()
	}
	c.portals[msg.DestinationPortal] = &boundPortal{name: msg.DestinationPortal, ps: ps, params: params}
	return writeMessages(c, &pgproto3.BindComplete{})
}

// handleDescribeMessage describes a prepared statement ('S') or portal ('P'),
// reporting the parameters and/or result columns the client should expect.
func (s *Server) handleDescribeMessage(ctx context.Context, c *Conn, msg *pgproto3.Describe) error {
	switch msg.ObjectType {
	case 'S':
		ps, ok := c.stmts[msg.Name]
		if !ok {
			return s.failExtended(c, fmt.Errorf("prepared statement %q does not exist", msg.Name))
		}
		return s.describeStatement(ctx, c, ps)
	case 'P':
		p, ok := c.portals[msg.Name]
		if !ok {
			return s.failExtended(c, fmt.Errorf("portal %q does not exist", msg.Name))
		}
		// In a failed transaction, describing a portal must not execute it: only
		// COMMIT/ROLLBACK are allowed, and they produce no rows.
		if c.txStatus == txFailed {
			kw := leadingKeyword(p.ps.query)
			if isTxCommit(kw) || isTxRollback(kw) {
				return writeMessages(c, &pgproto3.NoData{})
			}
			return s.failExtended(c, errInFailedTransaction)
		}
		if err := p.execute(ctx); err != nil {
			return s.failExtended(c, err)
		}
		return writeMessages(c, rowDescriptionOrNoData(p.cols, p.oids))
	default:
		return s.failExtended(c, fmt.Errorf("invalid Describe object type %q", msg.ObjectType))
	}
}

// describeStatement reports a prepared statement's parameters and result columns
// WITHOUT executing it (no side effects): it binds NULLs and reads the column
// metadata without ever stepping the statement.
func (s *Server) describeStatement(ctx context.Context, c *Conn, ps *preparedStatement) error {
	nullArgs := make([]interface{}, ps.nparams)
	rows, err := ps.stmt.QueryContext(ctx, nullArgs...)
	if err != nil {
		return s.failExtended(c, err)
	}
	cols, err := rows.ColumnTypes()
	rows.Close()
	if err != nil {
		return s.failExtended(c, err)
	}

	// Report parameter types as unspecified (OID 0); clients then send parameters
	// as text, which is all this server consumes.
	paramOIDs := make([]uint32, ps.nparams)
	return writeMessages(c, &pgproto3.ParameterDescription{ParameterOIDs: paramOIDs}, rowDescriptionOrNoData(cols, columnOIDs(cols)))
}

// handleExecuteMessage runs a bound portal and streams its rows, then reports
// CommandComplete. RowDescription, if wanted, was sent in response to Describe.
func (s *Server) handleExecuteMessage(ctx context.Context, c *Conn, msg *pgproto3.Execute) error {
	p, ok := c.portals[msg.Portal]
	if !ok {
		return s.failExtended(c, fmt.Errorf("portal %q does not exist", msg.Portal))
	}

	// In a failed transaction block only COMMIT/ROLLBACK are honored; both end
	// the block by discarding its work.
	kw := leadingKeyword(p.ps.query)
	if c.txStatus == txFailed {
		if isTxCommit(kw) || isTxRollback(kw) {
			c.rollbackFailed(ctx)
			return writeMessages(c, &pgproto3.CommandComplete{CommandTag: []byte("ROLLBACK")})
		}
		return s.failExtended(c, errInFailedTransaction)
	}

	if err := p.execute(ctx); err != nil {
		return s.failExtended(c, err)
	}

	var buf []byte
	var streamed int64
	if p.rows != nil {
		for p.rows.Next() {
			row, err := scanRow(p.rows, p.oids)
			if err != nil {
				p.close()
				return s.failExtended(c, err)
			}
			buf = row.Encode(buf)
			streamed++
		}
		if err := p.rows.Err(); err != nil {
			p.close()
			return s.failExtended(c, err)
		}
		p.close()
	}

	var affected int64
	if p.result != nil {
		affected, _ = p.result.RowsAffected()
	}
	c.noteCompleted(kw)
	buf = (&pgproto3.CommandComplete{CommandTag: commandTag(p.ps.query, affected, streamed)}).Encode(buf)
	_, err := c.Write(buf)
	return err
}

// handleCloseMessage closes a prepared statement or portal and responds with
// CloseComplete.
func (s *Server) handleCloseMessage(ctx context.Context, c *Conn, msg *pgproto3.Close) error {
	switch msg.ObjectType {
	case 'S':
		if ps, ok := c.stmts[msg.Name]; ok {
			if ps.stmt != nil {
				ps.stmt.Close()
			}
			delete(c.stmts, msg.Name)
		}
	case 'P':
		if p, ok := c.portals[msg.Name]; ok {
			p.close()
			delete(c.portals, msg.Name)
		}
	}
	return writeMessages(c, &pgproto3.CloseComplete{})
}

// rowDescriptionOrNoData returns a RowDescription when the statement produces
// columns, otherwise NoData.
func rowDescriptionOrNoData(cols []*sql.ColumnType, oids []uint32) pgproto3.Message {
	if len(cols) == 0 {
		return &pgproto3.NoData{}
	}
	return toRowDescription(cols, oids)
}

type Conn struct {
	net.Conn
	backend *pgproto3.Backend
	db      *sql.DB   // sqlite database handle (a connection pool)
	conn    *sql.Conn // the single underlying connection pinned to this session

	database string // the database name the client connected with
	txStatus byte   // 'I' idle, 'T' in a transaction, 'E' in a failed transaction

	// Extended-protocol session state.
	stmts         map[string]*preparedStatement // by name ("" = unnamed)
	portals       map[string]*boundPortal       // by name ("" = unnamed)
	skipUntilSync bool                          // discarding messages after an error
}

// preparedStatement is a parsed (and rewritten) statement created by Parse.
type preparedStatement struct {
	name    string
	query   string
	stmt    *sql.Stmt
	nparams int
}

// boundPortal is a prepared statement with bound parameter values, created by
// Bind. It is executed at most once; Describe and Execute share that single
// execution so statements with side effects (e.g. INSERT) run exactly once.
type boundPortal struct {
	name   string
	ps     *preparedStatement
	params []interface{}

	executed bool
	rows     *sql.Rows
	cols     []*sql.ColumnType
	oids     []uint32
	result   sql.Result
	execErr  error
}

// execute runs the portal's statement once. Result-returning statements are run
// with Query (rows are not stepped here, so side effects are deferred to Execute);
// non-row statements are run with Exec so the affected-row count is available.
func (p *boundPortal) execute(ctx context.Context) error {
	if p.executed {
		return p.execErr
	}
	p.executed = true

	if !isRowReturning(p.ps.query) {
		res, err := p.ps.stmt.ExecContext(ctx, p.params...)
		if err != nil {
			p.execErr = err
			return err
		}
		p.result = res
		return nil
	}

	rows, err := p.ps.stmt.QueryContext(ctx, p.params...)
	if err != nil {
		p.execErr = err
		return err
	}
	cols, err := rows.ColumnTypes()
	if err != nil {
		rows.Close()
		p.execErr = err
		return err
	}
	p.rows, p.cols, p.oids = rows, cols, columnOIDs(cols)
	return nil
}

func (p *boundPortal) close() {
	if p.rows != nil {
		p.rows.Close()
		p.rows = nil
	}
}

func newConn(conn net.Conn) *Conn {
	return &Conn{
		Conn:     conn,
		backend:  pgproto3.NewBackend(pgproto3.NewChunkReader(conn), conn),
		txStatus: txIdle,
		stmts:    make(map[string]*preparedStatement),
		portals:  make(map[string]*boundPortal),
	}
}

func (c *Conn) Close() (err error) {
	for _, p := range c.portals {
		p.close()
	}
	for _, ps := range c.stmts {
		if ps.stmt != nil {
			ps.stmt.Close()
		}
	}

	// Release the pinned connection back to the pool before closing the pool.
	if c.conn != nil {
		if e := c.conn.Close(); err == nil {
			err = e
		}
	}

	if c.db != nil {
		if e := c.db.Close(); err == nil {
			err = e
		}
	}

	if e := c.Conn.Close(); err == nil {
		err = e
	}
	return err
}

func getParameter(m map[string]string, k string) string {
	if m == nil {
		return ""
	}
	return m[k]
}

// writeMessages writes all messages to a single buffer before sending.
func writeMessages(w io.Writer, msgs ...pgproto3.Message) error {
	var buf []byte
	for _, msg := range msgs {
		buf = msg.Encode(buf)
	}
	_, err := w.Write(buf)
	return err
}

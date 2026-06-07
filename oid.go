package postlite

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgtype"
)

// This file maps SQLite column types to PostgreSQL type OIDs and encodes scanned
// values into the text wire format PostgreSQL uses. Result columns are still sent
// in the text format, but advertising real OIDs lets strict drivers (pgx, the
// node-postgres NocoDB uses) scan into int64/bool/float64/time.Time instead of
// rejecting everything as untyped text.

// columnOIDs returns the PostgreSQL type OID for each result column.
func columnOIDs(cols []*sql.ColumnType) []uint32 {
	oids := make([]uint32, len(cols))
	for i, col := range cols {
		oids[i] = oidForColumn(col)
	}
	return oids
}

// oidForColumn picks a PostgreSQL type OID for a result column from its declared
// SQLite type. Computed columns (COUNT(*), expressions) have no declared type and
// fall back to text, which every driver can consume.
//
// The catalog views expose boolean columns (attnotnull, indisprimary, ...) as 0/1
// expressions, which SQLite cannot tag with a declared type. So that strict
// drivers and psql see real PostgreSQL booleans ('t'/'f') rather than the string
// "0" (which is truthy in some languages), we map those well-known catalog column
// names to bool when the column has no declared type of its own.
func oidForColumn(col *sql.ColumnType) uint32 {
	decl := col.DatabaseTypeName()
	if decl == "" {
		if oid, ok := catalogColumnOID[col.Name()]; ok {
			return oid
		}
	}
	return oidForType(decl)
}

// catalogColumnOID maps catalog/computed column names to a PostgreSQL OID, used
// only when SQLite reports no declared type for the column. These are the boolean
// columns of the pg_catalog views, which must encode as 't'/'f'.
var catalogColumnOID = map[string]uint32{
	// pg_class
	"relhasindex":         pgtype.BoolOID,
	"relisshared":         pgtype.BoolOID,
	"relhasrules":         pgtype.BoolOID,
	"relhastriggers":      pgtype.BoolOID,
	"relhassubclass":      pgtype.BoolOID,
	"relrowsecurity":      pgtype.BoolOID,
	"relforcerowsecurity": pgtype.BoolOID,
	"relispopulated":      pgtype.BoolOID,
	"relispartition":      pgtype.BoolOID,
	// pg_attribute
	"attnotnull":   pgtype.BoolOID,
	"atthasdef":    pgtype.BoolOID,
	"attisdropped": pgtype.BoolOID,
	"attislocal":   pgtype.BoolOID,
	// pg_index
	"indisunique":    pgtype.BoolOID,
	"indisprimary":   pgtype.BoolOID,
	"indisexclusion": pgtype.BoolOID,
	"indimmediate":   pgtype.BoolOID,
	"indisclustered": pgtype.BoolOID,
	"indisvalid":     pgtype.BoolOID,
	"indisready":     pgtype.BoolOID,
	"indislive":      pgtype.BoolOID,
	// pg_constraint
	"condeferrable": pgtype.BoolOID,
	"condeferred":   pgtype.BoolOID,
	"convalidated":  pgtype.BoolOID,
}

// oidForType maps a SQLite declared type name to a PostgreSQL type OID. It follows
// SQLite's affinity rules (substring matching) with a few extra spellings — BOOL,
// the date/time family — recognized first so they get a more faithful OID than the
// affinity rules alone would give.
func oidForType(declType string) uint32 {
	t := strings.ToUpper(strings.TrimSpace(declType))
	switch {
	case t == "":
		return pgtype.TextOID
	case strings.Contains(t, "BOOL"):
		return pgtype.BoolOID
	case strings.Contains(t, "INT"):
		// SQLite INTEGER is 64-bit, so bigint (int8) is the faithful match.
		return pgtype.Int8OID
	case strings.Contains(t, "CHAR"), strings.Contains(t, "CLOB"), strings.Contains(t, "TEXT"):
		return pgtype.TextOID
	case strings.Contains(t, "BLOB"):
		return pgtype.ByteaOID
	case strings.Contains(t, "REAL"), strings.Contains(t, "FLOA"), strings.Contains(t, "DOUB"):
		return pgtype.Float8OID
	case strings.Contains(t, "NUMERIC"), strings.Contains(t, "DECIMAL"), strings.Contains(t, "DEC"):
		return pgtype.NumericOID
	case strings.Contains(t, "TIMESTAMP"), strings.Contains(t, "DATETIME"):
		return pgtype.TimestampOID
	case strings.Contains(t, "DATE"):
		return pgtype.DateOID
	case strings.Contains(t, "TIME"):
		return pgtype.TimeOID
	default:
		// SQLite would give this NUMERIC affinity; text is the safe wire choice.
		return pgtype.TextOID
	}
}

// typeSize returns the fixed wire size for a type OID, or -1 for variable-length
// types, matching the value PostgreSQL reports in RowDescription.
func typeSize(oid uint32) int16 {
	switch oid {
	case pgtype.BoolOID:
		return 1
	case pgtype.Int8OID, pgtype.Float8OID, pgtype.TimeOID, pgtype.TimestampOID, pgtype.TimestamptzOID:
		return 8
	case pgtype.Int4OID, pgtype.Float4OID, pgtype.DateOID:
		return 4
	case pgtype.Int2OID:
		return 2
	default:
		return -1
	}
}

// encodeText renders a value scanned from SQLite into the PostgreSQL text wire
// format for the given column OID. A nil value is SQL NULL (encoded as a nil
// slice, which the wire protocol sends as a -1 length).
func encodeText(oid uint32, v interface{}) []byte {
	switch val := v.(type) {
	case nil:
		return nil
	case bool:
		return boolText(val)
	case []byte:
		if oid == pgtype.ByteaOID {
			return encodeBytea(val)
		}
		return val // TEXT stored as bytes
	case time.Time:
		return []byte(encodeTime(oid, val))
	case int64:
		// A column declared BOOLEAN is stored by SQLite as 0/1; render it as t/f.
		if oid == pgtype.BoolOID {
			return boolText(val != 0)
		}
		return []byte(strconv.FormatInt(val, 10))
	case float64:
		return []byte(strconv.FormatFloat(val, 'g', -1, 64))
	case string:
		return []byte(val)
	default:
		return []byte(fmt.Sprint(val))
	}
}

func boolText(b bool) []byte {
	if b {
		return []byte("t")
	}
	return []byte("f")
}

// encodeBytea renders bytes in PostgreSQL's hex bytea format, e.g. \x0a1b.
func encodeBytea(b []byte) []byte {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 2+len(b)*2)
	out[0], out[1] = '\\', 'x'
	for i, c := range b {
		out[2+i*2] = hexdigits[c>>4]
		out[3+i*2] = hexdigits[c&0x0f]
	}
	return out
}

// encodeTime formats a time.Time the way PostgreSQL renders the corresponding
// date/time type in text.
func encodeTime(oid uint32, t time.Time) string {
	switch oid {
	case pgtype.DateOID:
		return t.Format("2006-01-02")
	case pgtype.TimeOID:
		return t.Format("15:04:05.999999")
	case pgtype.TimestamptzOID:
		return t.Format("2006-01-02 15:04:05.999999-07")
	default: // timestamp without time zone
		return t.Format("2006-01-02 15:04:05.999999")
	}
}

package postlite

import (
	"testing"
	"time"

	"github.com/jackc/pgtype"
)

func TestOIDForType(t *testing.T) {
	cases := []struct {
		decl string
		oid  uint32
	}{
		{"INTEGER", pgtype.Int8OID},
		{"INT", pgtype.Int8OID},
		{"BIGINT", pgtype.Int8OID},
		{"TEXT", pgtype.TextOID},
		{"VARCHAR(255)", pgtype.TextOID},
		{"CHARACTER(10)", pgtype.TextOID},
		{"CLOB", pgtype.TextOID},
		{"BLOB", pgtype.ByteaOID},
		{"REAL", pgtype.Float8OID},
		{"DOUBLE PRECISION", pgtype.Float8OID},
		{"FLOAT", pgtype.Float8OID},
		{"NUMERIC", pgtype.NumericOID},
		{"DECIMAL(10,2)", pgtype.NumericOID},
		{"BOOLEAN", pgtype.BoolOID},
		{"BOOL", pgtype.BoolOID},
		{"DATE", pgtype.DateOID},
		{"DATETIME", pgtype.TimestampOID},
		{"TIMESTAMP", pgtype.TimestampOID},
		{"TIME", pgtype.TimeOID},
		{"", pgtype.TextOID}, // computed column / unknown
		{"WHATSIT", pgtype.TextOID},
	}
	for _, tc := range cases {
		if got := oidForType(tc.decl); got != tc.oid {
			t.Errorf("oidForType(%q) = %d, want %d", tc.decl, got, tc.oid)
		}
	}
}

func TestEncodeText(t *testing.T) {
	cases := []struct {
		name string
		oid  uint32
		v    interface{}
		want string
		null bool
	}{
		{"null", pgtype.TextOID, nil, "", true},
		{"int", pgtype.Int8OID, int64(42), "42", false},
		{"bool-from-int-true", pgtype.BoolOID, int64(1), "t", false},
		{"bool-from-int-false", pgtype.BoolOID, int64(0), "f", false},
		{"bool-true", pgtype.BoolOID, true, "t", false},
		{"bool-false", pgtype.BoolOID, false, "f", false},
		{"float", pgtype.Float8OID, float64(9.99), "9.99", false},
		{"float-whole", pgtype.Float8OID, float64(1), "1", false},
		{"text", pgtype.TextOID, "hello", "hello", false},
		{"bytea", pgtype.ByteaOID, []byte{0x0a, 0x1b, 0xff}, `\x0a1bff`, false},
		{"text-bytes", pgtype.TextOID, []byte("raw"), "raw", false},
	}
	for _, tc := range cases {
		got := encodeText(tc.oid, tc.v)
		if tc.null {
			if got != nil {
				t.Errorf("%s: encodeText = %q, want nil (NULL)", tc.name, got)
			}
			continue
		}
		if string(got) != tc.want {
			t.Errorf("%s: encodeText = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestEncodeTime(t *testing.T) {
	ts := time.Date(2026, 6, 7, 13, 4, 5, 0, time.UTC)
	cases := []struct {
		oid  uint32
		want string
	}{
		{pgtype.DateOID, "2026-06-07"},
		{pgtype.TimeOID, "13:04:05"},
		{pgtype.TimestampOID, "2026-06-07 13:04:05"},
		{pgtype.TimestamptzOID, "2026-06-07 13:04:05+00"},
	}
	for _, tc := range cases {
		if got := encodeTime(tc.oid, ts); got != tc.want {
			t.Errorf("encodeTime(%d) = %q, want %q", tc.oid, got, tc.want)
		}
	}
}

package format

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The byte vector comes from Python's uuid module (uuid.UUID(str).bytes), independently of this package:
// RFC 9562 byte order is the order of the hex digits in the canonical string, left to right, with no swapping.
func TestParseUUID_ByteOrder(t *testing.T) {
	t.Parallel()

	const s = "f47ac10b-58cc-4372-a567-0e02b2c3d479"

	want := UUID{
		0xf4, 0x7a, 0xc1, 0x0b, 0x58, 0xcc, 0x43, 0x72,
		0xa5, 0x67, 0x0e, 0x02, 0xb2, 0xc3, 0xd4, 0x79,
	}

	got, err := ParseUUID(s)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, s, got.String())
}

func TestParseUUID_CaseInsensitive(t *testing.T) {
	t.Parallel()

	lower, err := ParseUUID("f47ac10b-58cc-4372-a567-0e02b2c3d479")
	require.NoError(t, err)

	upper, err := ParseUUID("F47AC10B-58CC-4372-A567-0E02B2C3D479")
	require.NoError(t, err)

	require.Equal(t, lower, upper)
	// String always renders lowercase, regardless of the input case.
	require.Equal(t, "f47ac10b-58cc-4372-a567-0e02b2c3d479", upper.String())
}

func TestParseUUID_Invalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
	}{
		{name: "empty", in: ""},
		{name: "too short", in: "f47ac10b-58cc-4372-a567-0e02b2c3d47"},
		{name: "too long", in: "f47ac10b-58cc-4372-a567-0e02b2c3d4790"},
		{name: "missing hyphens", in: "f47ac10b58cc4372a5670e02b2c3d479"},
		{name: "hyphen in wrong place", in: "f47ac10b5-8cc-4372-a567-0e02b2c3d479"},
		{name: "non-hex digit", in: "g47ac10b-58cc-4372-a567-0e02b2c3d479"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := ParseUUID(tt.in)
			require.Error(t, err)
		})
	}
}

func TestUUID_IsZero(t *testing.T) {
	t.Parallel()

	var zero UUID
	require.True(t, zero.IsZero())

	nonZero, err := ParseUUID("f47ac10b-58cc-4372-a567-0e02b2c3d479")
	require.NoError(t, err)
	require.False(t, nonZero.IsZero())
}

func TestZeroUUID_String(t *testing.T) {
	t.Parallel()

	var zero UUID
	require.Equal(t, "00000000-0000-0000-0000-000000000000", zero.String())
}

// The timestamp bytes come from an independent computation of the big-endian 48-bit millisecond field, not from NewUUIDv7 itself.
func TestNewUUIDv7_Timestamp(t *testing.T) {
	t.Parallel()

	const unixMilli = 1_700_000_000_000

	rnd := [10]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0}

	got := NewUUIDv7(unixMilli, rnd)

	wantTS := []byte{0x01, 0x8b, 0xcf, 0xe5, 0x68, 0x00}
	require.Equal(t, wantTS, got[0:6])
}

// RFC 9562 §5.7 fixes the version to 0111 in the high nibble of byte 6
// and the variant to 10 in the top two bits of byte 8.
func TestNewUUIDv7_VersionAndVariant(t *testing.T) {
	t.Parallel()

	rnd := [10]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}

	got := NewUUIDv7(1_700_000_000_000, rnd)

	require.Equal(t, byte(0x7), got[6]>>4, "version nibble")
	require.Equal(t, byte(0x2), got[8]>>6, "variant bits")
}

// NewUUIDv7 must not clobber the random bits it does not own: the low nibble of byte 6
// and the low 6 bits of byte 8 come straight from rand.
func TestNewUUIDv7_PreservesRandomBits(t *testing.T) {
	t.Parallel()

	rnd := [10]byte{0x0A, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99}

	got := NewUUIDv7(1_700_000_000_000, rnd)

	require.Equal(t, byte(0x0A)&0x0F, got[6]&0x0F, "low nibble of byte 6 is rand[0]'s low nibble")
	require.Equal(t, rnd[1], got[7])
	require.Equal(t, rnd[2]&0x3F, got[8]&0x3F, "low 6 bits of byte 8 are rand[2]'s low 6 bits")
	require.Equal(t, rnd[3:], got[9:16])
}

// UUIDv7 values sort lexicographically by creation time:
// a later millisecond timestamp always produces a larger UUID, regardless of the random bits.
func TestNewUUIDv7_MonotonicOrdering(t *testing.T) {
	t.Parallel()

	early := NewUUIDv7(1_700_000_000_000, [10]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
	late := NewUUIDv7(1_700_000_000_001, [10]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0})

	require.Less(t, early.String(), late.String())
	require.Negative(t, bytes.Compare(early[:], late[:]))
}

func TestGenerateUUIDv7(t *testing.T) {
	t.Parallel()

	fixedNow := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

	u, err := GenerateUUIDv7(func() time.Time { return fixedNow }, rand.Reader)
	require.NoError(t, err)
	require.False(t, u.IsZero())
	require.Equal(t, byte(0x7), u[6]>>4)
	require.Equal(t, byte(0x2), u[8]>>6)

	wantMilli := fixedNow.UnixMilli()
	gotMilli := int64(u[0])<<40 | int64(u[1])<<32 | int64(u[2])<<24 | int64(u[3])<<16 | int64(u[4])<<8 | int64(u[5])
	require.Equal(t, wantMilli, gotMilli)
}

func TestGenerateUUIDv7_ReadError(t *testing.T) {
	t.Parallel()

	_, err := GenerateUUIDv7(time.Now, errReader{})
	require.Error(t, err)
}

type errReader struct{}

var errReaderFailed = errors.New("errReader: forced read failure")

func (errReader) Read([]byte) (int, error) { return 0, errReaderFailed }

package format

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"
)

// uuidStringLen is the length of a canonical UUID string: 32 hex digits and 4 hyphens.
const uuidStringLen = 36

// errInvalidUUID reports that a string is not a canonical UUID.
var errInvalidUUID = errors.New("invalid UUID string")

// UUID is a 16-byte UUID in RFC 9562 byte order: the order of the hex digits in the canonical string, left to right.
//
// The tracepack format specification §2 requires this byte order for pack_id, capture_id and every other UUID field;
// an implementation whose native UUID type uses another byte order must convert.
// The zero value is the nil UUID.
type UUID [16]byte

var _ fmt.Stringer = UUID{}

// ParseUUID parses the canonical 8-4-4-4-12 string form of a UUID, such as "f47ac10b-58cc-4372-a567-0e02b2c3d479".
// Hex digits are case-insensitive; String always renders them lowercase.
//
// Returns:
//   - UUID: the parsed value; the zero UUID on error.
//   - error: non-nil if s is not exactly 36 characters in the canonical form.
func ParseUUID(s string) (UUID, error) {
	var u UUID

	if len(s) != uuidStringLen || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return u, fmt.Errorf("%q: %w", s, errInvalidUUID)
	}

	// Decode each hyphen-separated group directly into its byte range,
	// skipping the hyphens themselves.
	groups := [5][2]int{{0, 8}, {9, 13}, {14, 18}, {19, 23}, {24, 36}}
	dst := u[:]

	for _, g := range groups {
		n, err := hex.Decode(dst, []byte(s[g[0]:g[1]]))
		if err != nil {
			return UUID{}, fmt.Errorf("%q: %w", s, errInvalidUUID)
		}

		dst = dst[n:]
	}

	return u, nil
}

// NewUUIDv7 builds a UUIDv7 from a Unix millisecond timestamp and 10 bytes of randomness,
// setting the version and variant bits per RFC 9562 §5.7.
//
// unixMilli occupies the 48-bit timestamp field in big-endian order;
// it must be non-negative and fit in 48 bits (up to the year 10889), or the high bits are silently truncated.
// rnd fills bytes 6-15;
// NewUUIDv7 overwrites only the version nibble of rnd[0] and the variant bits of rnd[2], leaving the rest of the randomness untouched.
// Because the timestamp occupies the leading bytes, UUIDv7 values compare and sort in creation order.
//
// GenerateUUIDv7 is the usual caller;
// call NewUUIDv7 directly only to control the timestamp or randomness source explicitly, such as in a test.
func NewUUIDv7(unixMilli int64, rnd [10]byte) UUID {
	var u UUID

	// unixMilli is documented as non-negative and 48-bit; the conversion is exact under that precondition.
	ts := uint64(unixMilli)
	u[0] = byte(ts >> 40)
	u[1] = byte(ts >> 32)
	u[2] = byte(ts >> 24)
	u[3] = byte(ts >> 16)
	u[4] = byte(ts >> 8)
	u[5] = byte(ts)

	copy(u[6:16], rnd[:])
	u[6] = (u[6] & 0x0F) | 0x70 // version 0111
	u[8] = (u[8] & 0x3F) | 0x80 // variant 10

	return u
}

// GenerateUUIDv7 returns a new UUIDv7 using the current time from now and 10 bytes of randomness read from rnd.
//
// Returns:
//   - UUID: the generated value; the zero UUID on error.
//   - error: non-nil only if reading from rnd fails.
func GenerateUUIDv7(now func() time.Time, rnd io.Reader) (UUID, error) {
	var buf [10]byte

	if _, err := io.ReadFull(rnd, buf[:]); err != nil {
		return UUID{}, fmt.Errorf("read random bytes for UUIDv7: %w", err)
	}

	return NewUUIDv7(now().UnixMilli(), buf), nil
}

// String returns the canonical 8-4-4-4-12 lowercase hex form of u.
func (u UUID) String() string {
	var buf [uuidStringLen]byte

	hex.Encode(buf[0:8], u[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], u[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], u[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], u[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], u[10:16])

	return string(buf[:])
}

// IsZero reports whether u is the nil UUID (every byte zero).
func (u UUID) IsZero() bool {
	return u == UUID{}
}

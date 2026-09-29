package format

import (
	"errors"
	"fmt"
)

// ErrBadMagic reports that a structure's magic bytes do not match the tracepack format specification.
var ErrBadMagic = errors.New("bad magic")

// ErrCRC reports that a structure's CRC does not match its covered bytes.
var ErrCRC = errors.New("CRC mismatch")

// ErrLimit reports that a decoded value exceeds a portable encoding limit of the tracepack format specification §2,
// such as the 2^63-1 ceiling on a u64 offset, length, count or seq, or the 2^31-1 ceiling on a u32 length.
var ErrLimit = errors.New("value exceeds format limit")

// ErrSchema reports an unknown format major version or an unknown pack metadata schema_version.
var ErrSchema = errors.New("unknown format or schema version")

// ErrShort reports that a buffer is shorter than the structure being decoded from it.
var ErrShort = errors.New("buffer shorter than structure")

// ErrCorrupt reports a structural violation that is not a bad magic, a CRC mismatch or a short buffer,
// such as a record_header_len below 44 or a record_count of zero.
var ErrCorrupt = errors.New("corrupt structure")

// OffsetError reports a decode failure at a specific field of a fixed-layout structure.
//
// Structure names the layout being decoded, such as "file header" or "block envelope".
// Field names the field that failed validation,
// and Offset is that field's byte offset from the start of the structure.
// Err is the underlying sentinel error; Unwrap returns it so callers can match it with errors.Is.
type OffsetError struct {
	Structure string
	Field     string
	Offset    int
	Err       error
}

var _ error = (*OffsetError)(nil)

// Error implements the error interface.
func (e *OffsetError) Error() string {
	return fmt.Sprintf("%s: %s at offset %d: %v", e.Structure, e.Field, e.Offset, e.Err)
}

// Unwrap returns the underlying sentinel error, so errors.Is and errors.As see through OffsetError.
func (e *OffsetError) Unwrap() error {
	return e.Err
}

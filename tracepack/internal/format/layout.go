package format

import (
	"encoding/binary"
	"fmt"
)

// crcLen is the width of every CRC field in the fixed layouts.
const crcLen = 4

// u64Field names one u64 field of a decoded layout for the limit check of the tracepack format specification §2.
type u64Field struct {
	name  string
	off   int
	value uint64
}

// len32Field names one u32 length field of a decoded layout for the limit check of the tracepack format specification §2.
type len32Field struct {
	name  string
	off   int
	value uint32
}

// checkShort reports ErrShort if b holds fewer than n bytes.
//
// The error names the pseudo-field "length" at offset len(b), where the buffer ends;
// every Unmarshal function of this package reports a short buffer this way.
func checkShort(structure string, b []byte, n int) error {
	if len(b) < n {
		return &OffsetError{
			Structure: structure,
			Field:     "length",
			Offset:    len(b),
			Err:       fmt.Errorf("need %d bytes, have %d: %w", n, len(b), ErrShort),
		}
	}

	return nil
}

// checkMagic reports ErrBadMagic unless b[off:] starts with magic.
// The caller has already checked that b is long enough.
func checkMagic(structure string, b []byte, off int, magic string) error {
	if string(b[off:off+len(magic)]) != magic {
		return &OffsetError{Structure: structure, Field: "magic", Offset: off, Err: ErrBadMagic}
	}

	return nil
}

// checkCRC reports ErrCRC unless the u32 stored at b[crcOff:] equals the CRC of b[:crcOff].
// Every CRC field of the fixed layouts directly follows the bytes it covers, starting at offset 0.
//
// Returns the stored CRC value, so the caller decodes it as a field.
func checkCRC(structure, field string, b []byte, crcOff int) (uint32, error) {
	stored := binary.LittleEndian.Uint32(b[crcOff : crcOff+crcLen])
	computed := CRC(b[:crcOff])

	if stored != computed {
		return 0, &OffsetError{
			Structure: structure,
			Field:     field,
			Offset:    crcOff,
			Err:       fmt.Errorf("stored 0x%08X, computed 0x%08X over bytes 0-%d: %w", stored, computed, crcOff-1, ErrCRC),
		}
	}

	return stored, nil
}

// checkVersion reports ErrSchema unless got equals want.
func checkVersion(structure, field string, off int, got, want uint16) error {
	if got != want {
		return &OffsetError{
			Structure: structure,
			Field:     field,
			Offset:    off,
			Err:       fmt.Errorf("version %d, want %d: %w", got, want, ErrSchema),
		}
	}

	return nil
}

// checkU64Fields reports ErrLimit for the first field that exceeds MaxU64.
func checkU64Fields(structure string, fields []u64Field) error {
	for _, f := range fields {
		if err := checkU64(f.value, f.name); err != nil {
			return &OffsetError{Structure: structure, Field: f.name, Offset: f.off, Err: err}
		}
	}

	return nil
}

// checkLen32Fields reports ErrLimit for the first field that exceeds MaxLen32.
func checkLen32Fields(structure string, fields []len32Field) error {
	for _, f := range fields {
		if err := checkLen32(f.value, f.name); err != nil {
			return &OffsetError{Structure: structure, Field: f.name, Offset: f.off, Err: err}
		}
	}

	return nil
}

// checkRecordHeaderLen reports ErrCorrupt if n is below RecordHeaderLen,
// the smallest record_header_len the tracepack format specification §6 and §7.1 allow.
func checkRecordHeaderLen(structure string, off, n int) error {
	if n < RecordHeaderLen {
		return &OffsetError{
			Structure: structure,
			Field:     "record_header_len",
			Offset:    off,
			Err:       fmt.Errorf("record_header_len %d below %d: %w", n, RecordHeaderLen, ErrCorrupt),
		}
	}

	return nil
}

package format

import "fmt"

// MaxU64 is the largest value the tracepack format specification §2 allows for a u64 offset, length, count or seq: 2^63-1,
// so a language without an unsigned 64-bit type can hold every valid value in a signed 64-bit one.
const MaxU64 uint64 = 1<<63 - 1

// MaxLen32 is the largest value the tracepack format specification §2 allows for body_len, uncompressed_len and payload_len:
// 2^31-1, the largest array length several runtimes support.
const MaxLen32 uint32 = 1<<31 - 1

// checkU64 reports ErrLimit if v exceeds MaxU64.
//
// field names the value in the error, for example "seq" or "first_seq".
func checkU64(v uint64, field string) error {
	if v > MaxU64 {
		return fmt.Errorf("%s: value %d exceeds the %d limit: %w", field, v, MaxU64, ErrLimit)
	}

	return nil
}

// checkLen32 reports ErrLimit if v exceeds MaxLen32.
//
// field names the value in the error, for example "body_len" or "uncompressed_len".
func checkLen32(v uint32, field string) error {
	if v > MaxLen32 {
		return fmt.Errorf("%s: value %d exceeds the %d limit: %w", field, v, MaxLen32, ErrLimit)
	}

	return nil
}

package tracepack

import "errors"

// Sentinel errors that name a family of faults:
// a sentinel of the family also matches its parent under errors.Is,
// so a caller that handles the whole family tests the parent alone.
var (
	// ErrCorrupt reports bytes that are not a valid tracepack encoding:
	// a pack structure, a block, the pack metadata, a TLV body or a trailer.
	// ErrChecksum and ErrInvalidFooter also match it.
	// UnmarshalPackMeta, UnmarshalPackMetaMinor, UnmarshalTransportEvent, UnmarshalAnnotation and DecodeDescriptor
	// wrap it for bytes they do not decode, Open for pack metadata whose checksum holds,
	// beside a *FieldError naming the field at fault when an entry is.
	// No encode wraps it, and no writing call: a record whose payload does not decode is refused as ErrInvalidRecord.
	//
	// It is not reported for an object that is not a pack (ErrNotTracepack),
	// a format this package does not read (ErrUnsupportedFormat),
	// a structure over a reader budget (ErrReadLimit),
	// or a pack that is merely not finalized (ErrNotFinalized).
	ErrCorrupt = errors.New("tracepack: corrupt data")
	// ErrNotFinalized reports a pack with no valid trailer:
	// one still being written, one cut short, or one whose trailer is damaged,
	// so more records may follow the ones read, or were lost.
	// It is not ErrCorrupt and has no parent.
	ErrNotFinalized = errors.New("tracepack: pack not finalized")
	// ErrInvalidArgument reports an argument or option the call cannot take:
	// a caller's mistake, which the same call with the same arguments repeats.
	// ErrInvalidQuery and ErrInvalidKey also match it.
	//
	// The writing calls (see ErrRefused) wrap it for an invalid argument or option that wraps no sentinel of its own,
	// such as a nil WriterOptions.Meta, and for a value of WriterOptions.Meta, CloseOptions.Meta, a classifier's Name
	// or an AppendEvent event that the encoding refuses, beside the *FieldError naming the field,
	// and mark that error ErrRefused.
	// A CaptureDescriptor value the encoding refuses is the *FieldError CaptureDescriptor.Validate returns, without it.
	// EncodeDescriptor wraps it for a nil PackMeta, and ParseUUID for a string that is not a canonical UUID;
	// neither is a writing call, so neither marks it.
	ErrInvalidArgument = errors.New("tracepack: invalid argument")
)

// sentinelError is a sentinel error that also matches its parent sentinel:
// its text is its own, and Unwrap returns the parent, so errors.Is on any error wrapping it reaches the parent.
// Each is one pointer, so it stays a comparable value.
type sentinelError struct {
	text   string
	parent error
}

// newChildSentinel returns a sentinel error of text that also matches parent.
func newChildSentinel(text string, parent error) error {
	return &sentinelError{text: text, parent: parent}
}

// Error returns the sentinel's own text, without its parent's.
func (e *sentinelError) Error() string { return e.text }

// Unwrap returns the parent sentinel.
func (e *sentinelError) Unwrap() error { return e.parent }

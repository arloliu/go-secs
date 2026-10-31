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
	// Open also wraps it, beside io.ErrUnexpectedEOF, for pack metadata that extends past the end of the object.
	// The reads wrap it, beside the cause, in PackHeader.FooterErr for a damaged trailer (with ErrNotFinalized)
	// and in Defect.Err for a block that fails a check or disagrees with the index, and for a forward walk that stops at one
	// or disagrees with a valid trailer, or ends at a valid trailer whose footer CRC fails (Defect.Err lists them by reason);
	// so do Verify's VerifyReport.Failed and VerifyReport.Disagreements,
	// MergeIterate's and the transaction lookups' defects and FooterErrs, ExportJSONL's Result,
	// and Merge's error for an unusable input, beside ErrMergeInput.
	// A ReadAt error is never reported as ErrCorrupt, io.ErrUnexpectedEOF for a short read included.
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
	// PackHeader.FooterErr wraps it for every pack with no valid trailer:
	// alone when the trailer is absent, and beside ErrChecksum or ErrCorrupt when a trailer is present but damaged,
	// so a caller tests it before ErrCorrupt.
	// Defect.Err wraps it at the end of the forward walk of a pack with no valid trailer (IncompleteReasonTruncated);
	// a pack whose trailer is valid but whose footer CRC fails is not finalized either (I-5),
	// but it was finished, so its PackHeader.FooterErr wraps ErrChecksum and the end of its walk ErrCorrupt, without ErrNotFinalized.
	// Merge wraps it, beside ErrMergeInput, for an input with no valid trailer.
	// It is not ErrCorrupt and has no parent.
	ErrNotFinalized = errors.New("tracepack: pack not finalized")
	// ErrInvalidArgument reports an argument or option the call cannot take:
	// a caller's mistake, which the same call with the same arguments repeats.
	// ErrInvalidQuery and ErrInvalidKey also match it.
	//
	// The sentinel beside it, such as ErrRequiredTag, ErrFieldValue, ErrMetadataCommitment or ErrScopeBreach, names the fault on every path,
	// and the call that took the value as its argument or option adds ErrInvalidArgument:
	// NewWriter, WriteClose and NewSegmentWriter wrap it for every error of checking or encoding their options,
	// one with no sentinel of its own, such as a nil WriterOptions.Meta, included,
	// a *FieldError of PackMeta.Validate or of CaptureDescriptor.Validate, a classifier's Name or pack metadata the encoding refuses,
	// a period outside one UTC hour, and a seq_start above 2^63-1;
	// SegmentWriter.AppendEvent wraps it for an event the encoding refuses.
	// Being writing calls, they also mark the error ErrRefused (see ErrRefused).
	// Merge wraps it, unmarked, for an option it cannot take, a WriterTag the pack metadata cannot hold beside the *FieldError naming writer,
	// and an opts.ReplacementSetID or opts.PackID that an input already uses.
	// Open and Verify wrap it for a negative field of ReaderOptions;
	// NewDirSink for a prefix outside the form it states,
	// and a DirSink segment for a Write, Sync or Commit after its Commit or Abort.
	// A standalone codec, such as CaptureDescriptor.Validate, PackMeta.Validate, a MarshalBinary or an Unmarshal function,
	// returns the field sentinel without it,
	// except EncodeDescriptor for a nil PackMeta and ParseUUID for a string that is not a canonical UUID.
	// A nil reader from a PackSource wraps ErrInvalidQuery, which matches it.
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

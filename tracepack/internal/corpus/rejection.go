package corpus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// Bootstrap rejection codes, one per reason of the tracepack format specification §13
// (the tracepack corpus specification §5.3).
const (
	RejectShortObject        = "short-object"
	RejectBadMagic           = "bad-magic"
	RejectHeaderCRC          = "header-crc"
	RejectUnsupportedVersion = "unsupported-version"
	RejectMetadataPastObject = "metadata-past-object"
	RejectMetadataCRC        = "metadata-crc"
	RejectMetadataEntryList  = "metadata-entry-list"
	RejectMetadataRequired   = "metadata-required-tag"
	RejectReplacementSet     = "replacement-set"
)

// ErrNotRejection reports an error of Open that is not a rejection of the pack at bootstrap:
// a reader limit, a cancellation, an I/O error, or an error no rejection code names.
var ErrNotRejection = errors.New("corpus: not a bootstrap rejection")

// Rejection is rejection.json (the tracepack corpus specification §5.3).
type Rejection struct {
	Rejection string `json:"rejection"`
}

// RejectionCode returns the code of the bootstrap rejection err,
// an error tracepack.Open returned for a pack held in memory,
// classified by the sentinels and typed errors err wraps, never by its text:
//   - ErrNotTracepack: bad-magic with format.ErrBadMagic, else short-object for a size below the file header;
//   - ErrUnsupportedFormat: unsupported-version;
//   - ErrChecksum: header-crc with format.ErrCRC, which only the file header's check wraps, else metadata-crc;
//   - io.ErrUnexpectedEOF alone: metadata-past-object, since a read of an in-memory pack is never short;
//   - a *FieldError wrapping ErrFieldValue for replacement_set_size or replacement_set_index: replacement-set;
//   - ErrRequiredTag, or a *tlv.EntryError that is itself the missing top-level tag: metadata-required-tag;
//   - any other *tlv.EntryError, a nested missing tag included,
//     which the enclosing entry's error wraps: metadata-entry-list.
//
// Returns:
//   - string: the code.
//   - error: an error wrapping ErrNotRejection and err
//     for a reader limit (ErrReadLimit), a cancellation, or an error no code names.
func RejectionCode(err error) (string, error) {
	var (
		fe *tracepack.FieldError
		ee *tlv.EntryError
	)
	switch {
	case errors.Is(err, tracepack.ErrReadLimit), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "", fmt.Errorf("%w: %w", ErrNotRejection, err)
	case errors.Is(err, tracepack.ErrNotTracepack) && errors.Is(err, format.ErrBadMagic):
		return RejectBadMagic, nil
	case errors.Is(err, tracepack.ErrNotTracepack) && (errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, format.ErrShort)):
		return RejectShortObject, nil
	case errors.Is(err, tracepack.ErrUnsupportedFormat):
		return RejectUnsupportedVersion, nil
	case errors.Is(err, tracepack.ErrChecksum) && errors.Is(err, format.ErrCRC):
		return RejectHeaderCRC, nil
	case errors.Is(err, tracepack.ErrChecksum):
		return RejectMetadataCRC, nil
	case errors.Is(err, io.ErrUnexpectedEOF):
		return RejectMetadataPastObject, nil
	case errors.As(err, &fe) && errors.Is(fe, tracepack.ErrFieldValue) &&
		(fe.Field == "replacement_set_size" || fe.Field == "replacement_set_index"):
		return RejectReplacementSet, nil
	case errors.Is(err, tracepack.ErrRequiredTag):
		return RejectMetadataRequired, nil
	case errors.As(err, &ee):
		// errors.As finds the outermost entry error.
		// Only a missing tag's error has no offset, and a missing nested tag's is wrapped by its enclosing entry's,
		// which has the entry's offset, so an outermost error without an offset is a missing top-level tag.
		if ee.Offset == -1 && errors.Is(ee, tlv.ErrMissing) {
			return RejectMetadataRequired, nil
		}

		return RejectMetadataEntryList, nil
	default:
		return "", fmt.Errorf("%w: %w", ErrNotRejection, err)
	}
}

// RejectionOf opens pack and returns the code of its bootstrap rejection.
//
// Returns:
//   - string: the code.
//   - error: an error when pack opens, or the error of RejectionCode.
func RejectionOf(ctx context.Context, pack []byte) (string, error) {
	r, err := tracepack.Open(ctx, bytes.NewReader(pack), int64(len(pack)), tracepack.ReaderOptions{})
	if err == nil && r != nil {
		return "", fmt.Errorf("%w: the pack opens", ErrNotRejection)
	}

	return RejectionCode(err)
}

// Marshal returns rejection.json in the canonical form, after checking that it holds a code of the schema.
func (r *Rejection) Marshal() ([]byte, error) {
	switch r.Rejection {
	case RejectShortObject, RejectBadMagic, RejectHeaderCRC, RejectUnsupportedVersion, RejectMetadataPastObject,
		RejectMetadataCRC, RejectMetadataEntryList, RejectMetadataRequired, RejectReplacementSet:
		return Marshal(r)
	default:
		return nil, fmt.Errorf("corpus: rejection code %q is not defined", r.Rejection)
	}
}

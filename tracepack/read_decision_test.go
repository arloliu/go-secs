package tracepack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// readDecision is what a caller of a reading call does with an error, the first match deciding.
// The table applies to the error a call returns, to PackHeader.FooterErr and the Err of a FooterErrs element,
// and to the Err of a Defect or a TxGap that carries a sentinel.
type readDecision string

const (
	// decideReadCtx: the call's ctx is done; stop.
	decideReadCtx readDecision = "ctx"
	// decideReadSource: the source's ReadAt failed or did not return; retry by the source's policy.
	decideReadSource readDecision = "source"
	// decideReadFixCall: an argument or option the call cannot take; fix the call.
	decideReadFixCall readDecision = "fix the call"
	// decideReadNotPrimary: the key names no primary; look up the primary instead.
	decideReadNotPrimary readDecision = "not a primary"
	// decideReadNotPack: the object is not a pack; skip it.
	decideReadNotPack readDecision = "not a pack"
	// decideReadUpgrade: a format major this module does not read; skip it and alert.
	decideReadUpgrade readDecision = "unsupported format"
	// decideReadBudget: a structure over a reader budget; raise the budget or skip.
	decideReadBudget readDecision = "budget"
	// decideReadNotFinalized: the pack may still be written; retry later.
	decideReadNotFinalized readDecision = "not finalized"
	// decideReadCorrupt: the bytes are damaged; report and skip.
	decideReadCorrupt readDecision = "corrupt"
	// decideReadTransient: a short read or another error; it may be transient.
	decideReadTransient readDecision = "transient"
)

// decideRead returns the decision a caller of a reading call takes for err, in the documented order.
func decideRead(err error) readDecision {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return decideReadCtx
	case errors.Is(err, ErrSourceRead):
		return decideReadSource
	case errors.Is(err, ErrInvalidArgument):
		return decideReadFixCall
	case errors.Is(err, ErrNotPrimary):
		return decideReadNotPrimary
	case errors.Is(err, ErrNotTracepack):
		return decideReadNotPack
	case errors.Is(err, ErrUnsupportedFormat):
		return decideReadUpgrade
	case errors.Is(err, ErrReadLimit):
		return decideReadBudget
	case errors.Is(err, ErrNotFinalized):
		return decideReadNotFinalized
	case errors.Is(err, ErrCorrupt):
		return decideReadCorrupt
	default:
		return decideReadTransient
	}
}

// TestReadDecision drives each row of the reading calls' decision table through a public call,
// including the errors that match more than one row, where the first decides:
// a damaged trailer is not finalized before it is corrupt,
// and pack metadata cut short by the object is corrupt, while the same io.ErrUnexpectedEOF from a caller's short read is not.
func TestReadDecision(t *testing.T) {
	t.Parallel()

	file, clean := readFaultPack(t)
	l := layoutOf(t, file)
	trailerAt := uint64(l.size - format.TrailerLen)
	block := clean.Blocks()[1]
	open := func(file []byte, opts ReaderOptions) func(t *testing.T) error {
		return func(t *testing.T) error {
			_, err := Open(t.Context(), bytes.NewReader(file), int64(len(file)), opts)
			return err
		}
	}
	footerErr := func(file []byte) func(t *testing.T) error {
		return func(t *testing.T) error {
			return mustOpen(t, file, ReaderOptions{}).Header().FooterErr
		}
	}
	// truncatedDefect returns the Err of the IncompleteReasonTruncated defect Iterate reports for file.
	truncatedDefect := func(file []byte) func(t *testing.T) error {
		return func(t *testing.T) error {
			res, err := mustOpen(t, file, ReaderOptions{}).Iterate(t.Context(), Query{}, func(*Item) error { return nil })
			require.NoError(t, err)
			for _, d := range res.Incomplete {
				if d.Reason == IncompleteReasonTruncated {
					return d.Err
				}
			}
			require.Failf(t, "no truncated defect", "%v", res.Incomplete)

			return nil
		}
	}
	findAt := func(file []byte, seq uint64) func(t *testing.T) error {
		return func(t *testing.T) error {
			_, err := FindTransaction(t.Context(), txSource(t, file), txKeyAt(seq), TxOptions{MaxHours: 1})
			return err
		}
	}
	lookupPack := txPack(t, seg0, nil, txBlock(txRecord(12, nil), txNote(13, nil)))

	tests := []struct {
		name string
		err  func(t *testing.T) error
		want readDecision
		// also holds the sentinels of later rows the error also matches, which the first row overrides.
		also []error
	}{
		{name: "Open: ctx cancelled", want: decideReadCtx, err: func(t *testing.T) error {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err := Open(ctx, bytes.NewReader(file), int64(len(file)), ReaderOptions{})

			return err
		}},
		{name: "Open: a negative budget", want: decideReadFixCall, err: open(file, ReaderOptions{MaxFooterLen: -1})},
		{name: "MergeIterate: a nil fn", want: decideReadFixCall, also: []error{ErrInvalidQuery}, err: func(t *testing.T) error {
			_, err := MergeIterate(t.Context(), []*Reader{clean}, Query{}, MergeIterateOptions{}, nil)
			return err
		}},
		{name: "FindTransaction: a key naming no primary", want: decideReadNotPrimary, err: findAt(lookupPack, 13)},
		{name: "Open: not a pack", want: decideReadNotPack, err: open(bytes.Repeat([]byte{0x5A}, len(file)), ReaderOptions{})},
		{
			name: "Open: a newer format major", want: decideReadUpgrade,
			err: open(patchHeader(t, file, func(h *format.FileHeader) { h.FormatMajor = format.FormatMajor + 1 }), ReaderOptions{}),
		},
		{name: "Open: pack metadata over its budget", want: decideReadBudget, err: open(file, ReaderOptions{MaxPackMetadataLen: 1})},
		{name: "Iterate: a block over its budget", want: decideReadBudget, err: func(t *testing.T) error {
			r := mustOpen(t, file, ReaderOptions{MaxBlockLen: int64(block.UncompressedLen) - 1})
			res, err := r.Iterate(t.Context(), Query{}, func(*Item) error { return nil })
			require.NoError(t, err)
			require.NotEmpty(t, res.Incomplete)

			return res.Incomplete[0].Err
		}},
		{name: "(*Reader).Verify: a block over its budget", want: decideReadBudget, err: func(t *testing.T) error {
			_, err := mustOpen(t, file, ReaderOptions{MaxBlockLen: int64(block.UncompressedLen) - 1}).Verify(t.Context())
			return err
		}},
		{name: "FooterErr: no trailer", want: decideReadNotFinalized, err: footerErr(file[:l.tr.FooterOffset])},
		{
			name: "FooterErr: a trailer CRC mismatch", want: decideReadNotFinalized, also: []error{ErrChecksum},
			err: footerErr(flipByte(file, trailerAt+30)),
		},
		{name: "Defect: the end of a pack not finalized", want: decideReadNotFinalized, err: truncatedDefect(file[:block.Offset])},
		{name: "Open: a file header CRC mismatch", want: decideReadCorrupt, also: []error{ErrChecksum}, err: open(flipByte(file, 40), ReaderOptions{})},
		{
			name: "Open: pack metadata past the end of the object", want: decideReadCorrupt, also: []error{io.ErrUnexpectedEOF},
			err: open(patchHeader(t, file, func(h *format.FileHeader) { h.PackMetadataLen = uint32(l.size) }), ReaderOptions{}),
		},
		{name: "FooterErr: a footer that fails validation", want: decideReadCorrupt, also: []error{ErrInvalidFooter}, err: footerErr(invalidFooterFile(t, file))},
		{name: "Defect: a block body CRC mismatch", want: decideReadCorrupt, also: []error{ErrChecksum}, err: func(t *testing.T) error {
			res, err := mustOpen(t, flipByte(file, block.Offset+format.EnvelopeLen+3), ReaderOptions{}).
				Iterate(t.Context(), Query{}, func(*Item) error { return nil })
			require.NoError(t, err)
			require.Len(t, res.Incomplete, 1)

			return res.Incomplete[0].Err
		}},
		{
			name: "Defect: the end of the walk of a valid trailer whose footer CRC fails", want: decideReadCorrupt,
			err: truncatedDefect(flipByte(file, l.tr.FooterOffset+5)),
		},
		{name: "Open: a caller's ReadAt error", want: decideReadSource, also: []error{errInjected}, err: func(t *testing.T) error {
			g := newGatedReader(file)
			g.fail = func(int64, int) bool { return true }
			_, err := Open(t.Context(), g, int64(len(file)), ReaderOptions{})

			return err
		}},
		{name: "(*Reader).Verify: a caller's ReadAt error on a block", want: decideReadSource, also: []error{errInjected}, err: func(t *testing.T) error {
			g := newGatedReader(file)
			g.fail = func(off int64, _ int) bool { return off == int64(block.Offset) }
			r, err := Open(t.Context(), g, int64(len(file)), uncachedWindows())
			require.NoError(t, err)
			_, err = r.Verify(t.Context())

			return err
		}},
		{
			name: "Open: a ReadAt returning ctx's error", want: decideReadCtx, also: []error{ErrSourceRead},
			err: func(t *testing.T) error {
				g := newGatedReader(file)
				g.fail = func(int64, int) bool { return true }
				g.failErr = context.Canceled
				_, err := Open(t.Context(), g, int64(len(file)), ReaderOptions{})

				return err
			},
		},
		{
			// The source's own error keeps its chain, a module sentinel in it included; the source row decides first.
			name: "Iterate: a ReadAt error that wraps ErrCorrupt itself", want: decideReadSource, also: []error{ErrCorrupt},
			err: func(t *testing.T) error {
				g := newGatedReader(file)
				g.fail = func(off int64, _ int) bool { return off == int64(block.Offset) }
				g.failErr = fmt.Errorf("the source's own check: %w", ErrCorrupt)
				r, err := Open(t.Context(), g, int64(len(file)), uncachedWindows())
				require.NoError(t, err)
				_, err = r.Iterate(t.Context(), Query{}, func(*Item) error { return nil })

				return err
			},
		},
		{name: "Open: a caller's short read", want: decideReadTransient, also: []error{io.ErrUnexpectedEOF}, err: func(t *testing.T) error {
			_, err := Open(t.Context(), &shortReaderAt{data: file}, int64(len(file)), ReaderOptions{})
			return err
		}},
		{name: "FindTransaction: a caller's ReadAt error on a block", want: decideReadSource, also: []error{errInjected}, err: func(t *testing.T) error {
			g := newGatedReader(file)
			g.fail = func(off int64, _ int) bool { return off == int64(block.Offset) }
			r, err := Open(t.Context(), g, int64(len(file)), uncachedWindows())
			require.NoError(t, err)
			src, err := NewReaderSource([]*Reader{r})
			require.NoError(t, err)
			key := TxKeyOf(UUID(clean.hdr.CaptureID), block.FirstSeq+1, time.Unix(0, block.TSMin))
			_, err = FindTransaction(t.Context(), src, key, TxOptions{MaxHours: 1})

			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.err(t)
			require.Error(t, err)
			assert.Equal(t, tt.want, decideRead(err), "%v", err)
			for _, s := range tt.also {
				assert.ErrorIs(t, err, s, "the first row decides, the others still match")
			}
		})
	}
}

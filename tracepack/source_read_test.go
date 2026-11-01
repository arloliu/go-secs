package tracepack

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixedReaderAt returns n and err from every ReadAt, copying its data first.
type fixedReaderAt struct {
	data []byte
	n    int
	err  error
}

func (f fixedReaderAt) ReadAt(p []byte, _ int64) (int, error) {
	copy(p, f.data)

	return f.n, f.err
}

// errNoSource is a PackSource's own error.
var errNoSource = errors.New("no source for the hour")

// failingSource is a PackSource whose Readers fails.
type failingSource struct{}

func (failingSource) Readers(context.Context, int64) ([]*Reader, error) { return nil, errNoSource }

// TestReadAtMarksSourceErrors requires readAt to mark exactly the errors a ReadAt returned other than io.EOF,
// leaving the text unchanged, and to leave a short read unmarked.
func TestReadAtMarksSourceErrors(t *testing.T) {
	t.Parallel()

	data := []byte("0123456789")
	tests := []struct {
		name   string
		ra     fixedReaderAt
		marked bool
		cause  error
		text   string
	}{
		{name: "an error", ra: fixedReaderAt{err: errInjected}, marked: true, cause: errInjected, text: "tracepack: read 10 bytes at offset 0: injected read failure"},
		{
			name: "an error with a full buffer", ra: fixedReaderAt{data: data, n: 10, err: errInjected}, marked: true, cause: errInjected,
			text: "tracepack: read 10 bytes at offset 0: injected read failure",
		},
		{
			name: "io.ErrUnexpectedEOF returned", ra: fixedReaderAt{data: data, n: 4, err: io.ErrUnexpectedEOF}, marked: true, cause: io.ErrUnexpectedEOF,
			text: "tracepack: read 10 bytes at offset 0: unexpected EOF",
		},
		{
			name: "a short read with io.EOF", ra: fixedReaderAt{data: data, n: 4, err: io.EOF}, cause: io.ErrUnexpectedEOF,
			text: "tracepack: read 10 bytes at offset 0: unexpected EOF",
		},
		{
			name: "a short read without an error", ra: fixedReaderAt{data: data, n: 4}, cause: io.ErrUnexpectedEOF,
			text: "tracepack: read 10 bytes at offset 0: unexpected EOF",
		},
		{name: "a full buffer with io.EOF", ra: fixedReaderAt{data: data, n: 10, err: io.EOF}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := readAt(tt.ra, make([]byte, 10), 0)
			if tt.cause == nil {
				require.NoError(t, err)

				return
			}
			require.ErrorIs(t, err, tt.cause)
			assert.Equal(t, tt.marked, errors.Is(err, ErrSourceRead))
			assert.EqualError(t, err, tt.text)
		})
	}
}

// TestSourceReadUnmarked requires the errors that are no read of the source to stay unmarked:
// the module's own ctx check, a callback's or a writer's error, a PackSource's own error, and an object too small to be a pack.
func TestSourceReadUnmarked(t *testing.T) {
	t.Parallel()

	file, clean := readFaultPack(t)
	errCallback := errors.New("callback")
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	tests := []struct {
		name string
		err  func(t *testing.T) error
		want error
	}{
		{name: "Open: ctx done", want: context.Canceled, err: func(t *testing.T) error {
			_, err := Open(cancelled, bytes.NewReader(file), int64(len(file)), ReaderOptions{})
			return err
		}},
		{name: "Iterate: the callback's error", want: errCallback, err: func(t *testing.T) error {
			_, err := clean.Iterate(t.Context(), Query{}, func(*Item) error { return errCallback })
			return err
		}},
		{name: "ExportJSONL: the writer's error", want: errInjected, err: func(t *testing.T) error {
			_, err := ExportJSONL(t.Context(), clean, &failingWriter{err: errInjected})
			return err
		}},
		{name: "FindTransaction: the PackSource's error", want: errNoSource, err: func(t *testing.T) error {
			_, err := FindTransaction(t.Context(), failingSource{}, txKeyAt(1), TxOptions{MaxHours: 1})
			return err
		}},
		{name: "Open: an object smaller than a file header", want: ErrNotTracepack, err: func(t *testing.T) error {
			_, err := Open(t.Context(), bytes.NewReader(file[:10]), 10, ReaderOptions{})
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.err(t)
			require.ErrorIs(t, err, tt.want)
			require.NotErrorIs(t, err, ErrSourceRead)
		})
	}
}

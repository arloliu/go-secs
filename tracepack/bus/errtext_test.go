package bus

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
)

// requireNamespaceOnce requires each line of err's text to name errNamespace at most once,
// and a text of one line to name it exactly once.
func requireNamespaceOnce(t *testing.T, err error) {
	t.Helper()

	require.Error(t, err)
	lines := strings.Split(err.Error(), "\n")
	for i, line := range lines {
		n := strings.Count(line, errNamespace)
		if len(lines) == 1 {
			require.Equal(t, 1, n, "the namespace once: %q", line)
		} else {
			require.LessOrEqual(t, n, 1, "line %d names the namespace at most once: %q", i, line)
		}
	}
}

// TestWrapfText pins wrapf: fmt's rendering with every later namespace on a line removed, and fmt's causes unchanged.
func TestWrapfText(t *testing.T) {
	t.Parallel()

	cause := errors.New("tracepack: a: tracepack: b\nplain: tracepack: c")
	err := wrapf("%w: seq %d: %w", ErrMalformed, 3, cause)
	require.EqualError(t, err, "tracepack: malformed bus message: seq 3: a: b\nplain: tracepack: c")
	require.ErrorIs(t, err, ErrMalformed)
	require.ErrorIs(t, err, cause)

	err = wrapf("tracepack: kind: %w", tracepack.ErrInvalidRecord)
	require.EqualError(t, err, "tracepack: kind: record refused by the writer")
	require.Same(t, tracepack.ErrInvalidRecord, errors.Unwrap(err))

	require.EqualError(t, wrapf("tracepack: no cause %d", 1), "tracepack: no cause 1")
}

// TestErrorTextNamespaceOnce requires the errors of this package over errors of the root package
// to name the namespace once, their causes still matched.
func TestErrorTextNamespaceOnce(t *testing.T) {
	t.Parallel()

	r := tracepack.Record{Kind: tracepack.Kind(9)}
	err := DeriveRecord(&r, nil)
	require.EqualError(t, err, "tracepack: bus record of kind unknown(9): record refused by the writer")
	require.ErrorIs(t, err, tracepack.ErrInvalidRecord)

	desc := descriptorBytes(t)[:10]
	_, err = CheckMessage(startEnvelope(t, base64.StdEncoding.EncodeToString(desc)), startBody(t))
	require.True(t, strings.HasPrefix(err.Error(), "tracepack: bus start not accepted: decode capture descriptor: corrupt data: "), err.Error())
	requireNamespaceOnce(t, err)
	require.ErrorIs(t, err, ErrNotAccepted)
	require.ErrorIs(t, err, tracepack.ErrCorrupt)
}

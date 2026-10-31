package tracepack

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// verboseError is an error whose fmt.Formatter rendering under %+v differs from its Error text.
type verboseError struct{}

// Error returns the terse text.
func (verboseError) Error() string { return "tracepack: terse" }

// Format writes the detailed text for %+v, and the terse text otherwise.
func (e verboseError) Format(f fmt.State, verb rune) {
	if verb == 'v' && f.Flag('+') {
		_, _ = io.WriteString(f, "tracepack: detailed: tracepack: disk full")

		return
	}
	_, _ = io.WriteString(f, e.Error())
}

// requireCauses requires err to unwrap exactly as want says:
// nil for no Unwrap method, an error for Unwrap() error, a []error for Unwrap() []error.
func requireCauses(t *testing.T, err error, want any) {
	t.Helper()

	switch u := err.(type) { //nolint:errorlint // inspects this link of the chain, not the chain
	case interface{ Unwrap() []error }:
		w, ok := want.([]error)
		require.True(t, ok, "unwraps to several causes, want %T", want)
		got := u.Unwrap()
		require.Len(t, got, len(w))
		for i := range w {
			require.Same(t, w[i], got[i], "cause %d", i)
		}
	case interface{ Unwrap() error }:
		w, ok := want.(error)
		require.True(t, ok, "unwraps to one cause, want %T", want)
		require.Equal(t, w, u.Unwrap())
	default:
		require.Nil(t, want, "wraps nothing")
	}
}

// TestWrapfText pins the text and the causes of wrapf:
// fmt's rendering with every later namespace on a line removed, and fmt's causes unchanged.
func TestWrapfText(t *testing.T) {
	t.Parallel()

	failure := errors.New("tracepack: write block: disk full")
	short := errors.New("tracepack: short")
	both := errors.Join(errors.New("tracepack: write: disk full"), errors.New("tracepack: abort: denied"))
	lines := errors.New("tracepack: a: tracepack: b\nplain: tracepack: c: tracepack: d\nno namespace")
	tests := []struct {
		name   string
		err    error
		text   string
		causes any
	}{
		{"a leading %w", wrapf("%w: %w", ErrWriterFailed, failure),
			"tracepack: writer failed: write block: disk full", []error{ErrWriterFailed, failure}},
		{"a sentinel over a sentinel", wrapf("%w: %w", ErrMetadataCommitment, ErrRedactionDefect),
			"tracepack: record contradicts the pack metadata: redaction defect", []error{ErrMetadataCommitment, ErrRedactionDefect}},
		{"a leading and a later indexed %w of one operand", wrapf("%[1]w: %[1]w", ErrWriterFailed),
			"tracepack: writer failed: writer failed", []error{ErrWriterFailed}},
		{"a literal %%", wrapf("tracepack: %d%% read: %w", 50, short),
			"tracepack: 50% read: short", short},
		{"flags, width and precision", wrapf("tracepack: block %04d %-4s| %6.2f %+d %x: %w", 7, "ab", 1.5, 3, []byte("hi"), short),
			"tracepack: block 0007 ab  |   1.50 +3 6869: short", short},
		{"a Formatter under %+w", wrapf("tracepack: append: %+w", verboseError{}),
			"tracepack: append: detailed: disk full", verboseError{}},
		{"a Formatter under %w", wrapf("tracepack: append: %w", verboseError{}),
			"tracepack: append: terse", verboseError{}},
		{"non-error operands only", wrapf("tracepack: pack %d: %s", 3, "tracepack: named"),
			"tracepack: pack 3: named", nil},
		{"one cause", wrapf("tracepack: close: %w", ErrNotFinalized),
			"tracepack: close: pack not finalized", ErrNotFinalized},
		{"a joined operand", wrapf("tracepack: close: %w", both),
			"tracepack: close: write: disk full\ntracepack: abort: denied", both},
		{"a multiline operand", wrapf("tracepack: x: %w", lines),
			"tracepack: x: a: b\nplain: tracepack: c: d\nno namespace", lines},
		{"a namespace after the line's start", wrapf("seq 3: %w: %w", short, short),
			"seq 3: tracepack: short: short", []error{short, short}},
		{"no namespace", wrapf("seq %d: %w", 3, io.EOF),
			"seq 3: EOF", io.EOF},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.EqualError(t, tt.err, tt.text)
			requireCauses(t, tt.err, tt.causes)
		})
	}
}

// TestJoinErrorsText pins joinErrors: errors.Join's nil filtering and order, with the per-line namespace rendering.
func TestJoinErrorsText(t *testing.T) {
	t.Parallel()

	require.NoError(t, joinErrors())
	require.NoError(t, joinErrors(nil, nil))
	require.NoError(t, joinErrors(joinErrors(nil), nil), "an all-nil nest is nil")
	require.True(t, joinErrors(nil) == nil, "an untyped nil") //nolint:errorlint // the nil interface itself

	write := errors.New("tracepack: write: tracepack: disk full")
	abort := errors.New("tracepack: abort: tracepack: denied")
	inner := joinErrors(nil, abort)
	require.EqualError(t, inner, "tracepack: abort: denied")
	requireCauses(t, inner, []error{abort})

	err := joinErrors(write, nil, inner)
	require.EqualError(t, err, "tracepack: write: disk full\ntracepack: abort: denied")
	requireCauses(t, err, []error{write, inner})
	require.ErrorIs(t, err, write)
	require.ErrorIs(t, err, abort)

	err = joinErrors(abort, write)
	require.EqualError(t, err, "tracepack: abort: denied\ntracepack: write: disk full", "order kept")
}

// TestWrapfMarkers requires the markers of a writing call's errors to keep working under wrapf and joinErrors:
// argError and a bare refusal sentinel are marked by refuse, externalError keeps it from marking,
// and a refusedError is matched and not marked again.
func TestWrapfMarkers(t *testing.T) {
	t.Parallel()

	arg := wrapf("tracepack: encode transport event: %w", argument(errors.New("tracepack: tag 0x0006: not valid UTF-8")))
	require.EqualError(t, arg, "tracepack: encode transport event: tag 0x0006: not valid UTF-8")
	require.ErrorIs(t, arg, ErrInvalidArgument)
	requireRefused(t, refuse(arg), ErrInvalidArgument)

	seq := fmt.Errorf("%w: seq 3", ErrSeqOrder)
	requireRefused(t, refuse(joinErrors(seq)), ErrSeqOrder)

	ext := wrapf("tracepack: create segment: %w", external(fmt.Errorf("sink: %w", ErrSeqOrder)))
	require.ErrorIs(t, ext, errExternal)
	require.ErrorIs(t, ext, ErrSeqOrder)
	require.Same(t, ext, refuse(ext))
	requireNotRefused(t, refuse(ext))

	mixed := joinErrors(seq, external(errInjected))
	require.Same(t, mixed, refuse(mixed), "the external error wins")
	requireNotRefused(t, refuse(mixed))

	marked := wrapf("tracepack: close: %w", refuse(seq))
	require.ErrorIs(t, marked, ErrRefused)
	require.Same(t, marked, refuse(marked), "marked once")
	require.Equal(t, 1, refusedCount(marked))

	fe := &FieldError{Field: "tool_id", Err: ErrFieldValue}
	var got *FieldError
	require.ErrorAs(t, wrapf("tracepack: new segment writer: %w", fe), &got)
	require.Same(t, fe, got)
}

// TestWrapfVet requires vet's printf check to treat wrapf as a wrapper of fmt.Errorf through the helper package it forwards to:
// it runs vet over a scratch module holding errtext.go and internal/errtext as they are
// and calls with a malformed format or a %w of no error.
func TestWrapfVet(t *testing.T) {
	t.Parallel()

	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command:", err)
	}
	root, err := os.ReadFile("errtext.go")
	require.NoError(t, err)
	helper, err := os.ReadFile(filepath.Join("internal", "errtext", "errtext.go"))
	require.NoError(t, err)
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "internal", "errtext"), 0o700))
	fixture := `package tracepack

import "errors"

var errFixture = errors.New("tracepack: fixture")

func fixture() []error {
	return []error{
		wrapf("tracepack: well formed %d: %w", 1, errFixture),
		wrapf("tracepack: unknown verb %z", 1),
		wrapf("tracepack: not an error: %w", 42),
		wrapf("tracepack: missing %d"),
		joinErrors(errFixture),
	}
}
`
	for name, data := range map[string][]byte{
		"go.mod":     []byte("module github.com/arloliu/go-secs/tracepack\n\ngo 1.26\n"),
		"errtext.go": root,
		filepath.Join("internal", "errtext", "errtext.go"): helper,
		"fixture.go": []byte(fixture),
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o600))
	}

	cmd := exec.CommandContext(t.Context(), goBin, "vet", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=", "GOTOOLCHAIN=local", "GOPROXY=off")
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "vet passed the fixture:\n%s", out)
	text := string(out)
	require.NotContains(t, text, "fixture.go:9:", "the well-formed call passes")
	require.Contains(t, text, "fixture.go:10:")
	require.Contains(t, text, "wrapf format %z has unknown verb z")
	require.Contains(t, text, "fixture.go:11:")
	require.Contains(t, text, "wrapf format %w has arg 42 of wrong type int")
	require.Contains(t, text, "fixture.go:12:")
	require.Contains(t, text, "wrapf format %d reads arg #1, but call has 0 args")
	require.NotContains(t, text, "fixture.go:13:", "a join is no format")
}

package sml

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseError_PositionAndUnwrap(t *testing.T) {
	// "S1F1\n<X>" — 'X' is not a valid item type; error must point at it.
	_, err := Parse("S1F1\n<X>.")
	require.Error(t, err)

	var pe *ParseError
	require.True(t, errors.As(err, &pe), "error must be *ParseError")
	require.Greater(t, pe.Offset, 0)
	require.Equal(t, 2, pe.Line)         // second line
	require.GreaterOrEqual(t, pe.Col, 1) // 1-based column
	require.Contains(t, pe.Error(), "line 2")
}

func TestParseError_MalformedHeader(t *testing.T) {
	// A bad stream/function token must also be a positional *ParseError
	// (covers the nextCode/parseHSMSHeader helper path).
	for _, bad := range []string{"SXF1\n<A[0] \"\">.", "S1FY\n<A[0] \"\">."} {
		_, err := Parse(bad)
		require.Error(t, err, "input %q", bad)
		var pe *ParseError
		require.True(t, errors.As(err, &pe), "input %q: error must be *ParseError", bad)
	}
}

// TestParseError_Classification verifies the error-model contract:
// syntax errors are *ParseError (errors.As); empty input yields ErrNoMessage (errors.Is).
func TestParseError_Classification(t *testing.T) {
	p := NewParser()

	// Syntax error: not a *ParseError subtype but IS classifiable via errors.As.
	_, syntaxErr := p.Parse("S1F1\n<X>.")
	require.Error(t, syntaxErr)
	var pe *ParseError
	require.True(t, errors.As(syntaxErr, &pe), "syntax error must be *ParseError via errors.As")

	// Empty input to ParseMessage must return ErrNoMessage, not *ParseError.
	_, emptyErr := p.ParseMessage("")
	require.Error(t, emptyErr)
	require.True(t, errors.Is(emptyErr, ErrNoMessage), "empty input must return ErrNoMessage via errors.Is")
	var pe2 *ParseError
	require.False(t, errors.As(emptyErr, &pe2), "empty-input error must NOT be *ParseError")

	// Same contract for ParseHeader.
	_, hdrErr := p.ParseHeader("   ")
	require.Error(t, hdrErr)
	require.True(t, errors.Is(hdrErr, ErrNoMessage), "whitespace-only input to ParseHeader must return ErrNoMessage")
}

func TestParseError_BadNumericToken(t *testing.T) {
	// An invalid integer value must be a *ParseError whose offset points at the
	// offending item (not past it) — i.e. at/after the body start and before EOF.
	src := "S1F1\n<I4[1] notanumber>.\n"
	_, err := Parse(src)
	require.Error(t, err)
	var pe *ParseError
	require.True(t, errors.As(err, &pe), "error must be *ParseError")

	// Teeth (T2-m1): the offset must point AT the offending numeric token, not merely somewhere in
	// the body. A loose [bodyStart, len(src)) bound would also pass for errfAt(p.pos) (the parser
	// position AFTER consuming the token, at/after '>'). Pinning it to the token span makes an
	// errfAt(p.pos)-vs-errfAt(start) regression fail: '>' sits at tokenStart+len(token).
	tokenStart := strings.Index(src, "notanumber")
	require.GreaterOrEqual(t, pe.Offset, tokenStart, "offset must point at the bad numeric token, not before it")
	require.Less(t, pe.Offset, tokenStart+len("notanumber"), "offset must point WITHIN the bad token (errfAt(start), not errfAt(p.pos))")
}

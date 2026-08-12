package sml

import (
	"strings"
	"testing"

	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// nestedSML builds a message whose body nests depth list levels around a single ASCII item.
func nestedSML(depth int) string {
	return "S1F1 " + strings.Repeat("<L[1] ", depth) + "<A 'x'>" + strings.Repeat(">", depth) + "."
}

// TestParse_ListDepthMatchesWireDecoder pins the parser to the wire decoder's nesting ceiling.
//
// Accepting deeper input would be wrong twice over.
// The resulting message could not be decoded from its own wire form,
// and depth in the text maps onto stack depth,
// so a large enough input aborts the process with an overflow no caller can recover from.
func TestParse_ListDepthMatchesWireDecoder(t *testing.T) {
	t.Parallel()

	t.Run("at the limit the parser and the decoder agree", func(t *testing.T) {
		t.Parallel()

		msgs, err := NewParser().Parse(nestedSML(secs2.MaxListDepth))
		require.NoError(t, err, "the parser must accept nesting up to the shared limit")
		require.Len(t, msgs, 1)

		item, err := msgs[0].Item()
		require.NoError(t, err)

		_, err = secs2.Decode(item.ToBytes())
		require.NoError(t, err, "a message the parser accepts must survive a wire round trip")
	})

	t.Run("one level past the limit is rejected", func(t *testing.T) {
		t.Parallel()

		_, err := NewParser().Parse(nestedSML(secs2.MaxListDepth + 1))
		require.Error(t, err)
		require.ErrorContains(t, err, "list nesting depth exceeds maximum allowed")

		var parseErr *ParseError
		require.ErrorAs(t, err, &parseErr, "the limit must be reported as a ParseError, with a position")
	})

	t.Run("deep input errors instead of exhausting the stack", func(t *testing.T) {
		t.Parallel()

		// Before the limit this depth aborted the process outright.
		_, err := NewParser().Parse(nestedSML(200_000))
		require.Error(t, err)
		require.ErrorContains(t, err, "list nesting depth exceeds maximum allowed")
	})

	t.Run("sibling lists do not accumulate depth", func(t *testing.T) {
		t.Parallel()

		// Many lists in sequence, none nested inside another, must stay well under the limit.
		body := strings.Repeat("<L[1] <A 'x'>> ", secs2.MaxListDepth*4)
		_, err := NewParser().Parse("S1F1 <L[0] " + body + ">.")
		require.NoError(t, err, "depth must unwind as each sibling list closes")
	})

	t.Run("a rejected parse leaves no depth behind", func(t *testing.T) {
		t.Parallel()

		// The decrement is deferred before the limit check precisely so the rejecting call unwinds too.
		// Were it registered after the check,
		// this parser would still hold the depth of the message it just refused,
		// and would wrongly reject the next one.
		parser := NewParser()

		_, err := parser.Parse(nestedSML(secs2.MaxListDepth + 1))
		require.Error(t, err)

		msgs, err := parser.Parse(nestedSML(secs2.MaxListDepth))
		require.NoError(t, err, "a rejected parse must not consume the next parse's nesting budget")
		require.Len(t, msgs, 1)
	})

	t.Run("successive messages each parse at the limit", func(t *testing.T) {
		t.Parallel()

		// Depth unwinds with the last closing list, so the second message starts from zero.
		at := nestedSML(secs2.MaxListDepth)
		msgs, err := NewParser().Parse(at + "\n" + at)
		require.NoError(t, err, "one message at the limit must not consume the next message's budget")
		require.Len(t, msgs, 2)
	})
}

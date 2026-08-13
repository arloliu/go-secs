package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWrapParagraphSentenceSplit proves the base case: a paragraph splits one sentence per line,
// and a short paragraph with no sentence boundary stays on one line.
func TestWrapParagraphSentenceSplit(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "single sentence stays on one line",
			in:   "Establishes if the equipment is on-line.",
			want: []string{"Establishes if the equipment is on-line."},
		},
		{
			name: "two short sentences split one per line",
			in:   "Equipment sends data to the host. The structure is the same as S6F3.",
			want: []string{
				"Equipment sends data to the host.",
				"The structure is the same as S6F3.",
			},
		},
		{
			name: "empty string returns itself unsplit",
			in:   "",
			want: []string{""},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, wrapParagraph(tt.in))
		})
	}
}

// TestWrapParagraphEllipsisAndAbbreviations proves splitSentences does not mistake the "..." in a Body: shorthand line, or an "i.e.,"/"e.g.," abbreviation, for a sentence boundary.
// Each of gem's generated Body: lines ends in "...  }." and several Exception paragraphs carry "i.e.,"/"e.g.,".
func TestWrapParagraphEllipsisAndAbbreviations(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "ellipsis before the closing brace is not a sentence end",
			in:   "Body: L[n]{ <svids>... }.",
			want: []string{"Body: L[n]{ <svids>... }."},
		},
		{
			name: "i.e., does not split even though its own period is followed by whitespace-adjacent punctuation",
			in:   `Acknowledge host command or error. If the command is not accepted due to one or more invalid parameters (i.e., HCACK = 3), a list of invalid parameters is returned containing the parameter name and reason for being invalid.`,
			want: []string{
				"Acknowledge host command or error.",
				"If the command is not accepted due to one or more invalid parameters (i.e., HCACK = 3), a list of invalid parameters is returned containing the parameter name",
				"and reason for being invalid.",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, wrapParagraph(tt.in))
		})
	}
}

// TestWrapParagraphLongSentenceBoundary pins the calibration cases that fixed semanticLinefeedThreshold at 90:
// a "that" boundary chosen over an earlier list-closing "and",
// a semicolon left unused because no eligible boundary exists at or after the threshold,
// and a long remainder that is never itself re-split after the first break.
func TestWrapParagraphLongSentenceBoundary(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{
			// The list-closing "and" (data collection mode, and other values) sits before the threshold and is skipped;
			// "that" at offset 99 is the chosen boundary.
			name: "relative clause boundary wins over an earlier list-closing and",
			in:   "Constants such as for calibration, servo gain, alarm limits, data collection mode, and other values that are changed infrequently can be obtained using this message.",
			want: []string{
				"Constants such as for calibration, servo gain, alarm limits, data collection mode, and other values",
				"that are changed infrequently can be obtained using this message.",
			},
		},
		{
			// The only boundary (a semicolon at offset 62) sits well before the threshold,
			// and no other boundary exists past it, so the 151-char sentence is left unsplit.
			name: "no eligible boundary at or after the threshold leaves the sentence long",
			in:   "These linked event reports default to disabled upon linking; the occurrence of an event does not cause the report to be sent until enabled (see S2F37).",
			want: []string{
				"These linked event reports default to disabled upon linking; the occurrence of an event does not cause the report to be sent until enabled (see S2F37).",
			},
		},
		{
			// One split at the colon; the 122-char remainder is not itself re-split.
			name: "the remainder after one split is never re-split",
			in:   "The body is opaque because each CEPVAL's shape is chosen at the value level and cannot be described by a static structure: a given CEPVAL may be a single scalar, a list of same-format items, or a recursively nested list of CPNAME/CEPVAL pairs.",
			want: []string{
				"The body is opaque because each CEPVAL's shape is chosen at the value level and cannot be described by a static structure:",
				"a given CEPVAL may be a single scalar, a list of same-format items, or a recursively nested list of CPNAME/CEPVAL pairs.",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, wrapParagraph(tt.in))
		})
	}
}

// TestSplitSentences exercises the sentence boundary detector directly, including the byte-scan edge cases regexp lookaround would normally express.
func TestSplitSentences(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{name: "trailing whitespace is trimmed", in: "One sentence.  ", want: []string{"One sentence."}},
		{name: "no terminal period still returns the fragment", in: "No period here", want: []string{"No period here"}},
		{
			name: "abbreviation period followed by a comma is not a boundary",
			in:   "reason (i.e., HCACK = 3) here.",
			want: []string{"reason (i.e., HCACK = 3) here."},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, splitSentences(tt.in))
		})
	}
}

// TestFindBoundariesOrder proves findBoundaries returns every clauseBoundaries occurrence sorted by position regardless of which token form matched,
// so splitLongSentence's leftmost-at-or-after-threshold scan does not depend on clauseBoundaries' declaration order.
func TestFindBoundariesOrder(t *testing.T) {
	got := findBoundaries("a; b: c and d but e so f which g that h where i")
	require.Len(t, got, 8)

	for i := 1; i < len(got); i++ {
		require.Lessf(t, got[i-1].pos, got[i].pos, "boundaries not sorted by position: %+v", got)
	}
}

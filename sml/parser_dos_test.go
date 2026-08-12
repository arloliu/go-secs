package sml

import (
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// maxAllowedAllocBytes bounds the TotalAlloc delta a single Parse call may produce
// while parsing an item whose declared size token is huge but whose payload is a few bytes.
// It sits well above what real parsing of these inputs needs,
// so only a preallocation driven by the attacker-controlled size token can trip it.
const maxAllowedAllocBytes = 8 << 20 // 8 MiB

// Each case declares a count sized so that removing the clamp requests roughly 64 MB.
// That is comfortably past maxAllowedAllocBytes, which keeps the guard mutation-sensitive,
// yet small enough that the failing run reports the assertion instead of being OOM-killed.
const (
	sizeWidth16 = "4000000"  // []secs2.Item, 16 bytes per element
	sizeWidth8  = "8000000"  // int64 / uint64 / float64
	sizeWidth1  = "64000000" // bool, byte, and the strict-mode string builder
)

// TestParse_HugeDeclaredSizeDoesNotPrealloc guards against the parser sizing an allocation from a raw size token.
// A short message must not balloon into a huge allocation on the strength of that token alone.
//
// The parser never reconciles the declared count against the payload,
// so the bound has to come from the input actually remaining.
// The assertion is allocation only, and does not lock in whether Parse succeeds or errors on such an input.
func TestParse_HugeDeclaredSizeDoesNotPrealloc(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		strict bool
	}{
		{
			name:  "list",
			input: `S1F1 <L[` + sizeWidth16 + `] <A 'x'> <A 'y'> >.`,
		},
		{
			name:  "boolean",
			input: `S1F1 <Boolean[` + sizeWidth1 + `] T F>.`,
		},
		{
			name:  "binary",
			input: `S1F1 <B[` + sizeWidth1 + `] 0x01 0x02>.`,
		},
		{
			name:  "float",
			input: `S1F1 <F4[` + sizeWidth8 + `] 1.5 2.5>.`,
		},
		{
			name:  "int",
			input: `S1F1 <I4[` + sizeWidth8 + `] 1 2 3>.`,
		},
		{
			name:  "uint",
			input: `S1F1 <U1[` + sizeWidth8 + `] 1 2 3>.`,
		},
		{
			// Strict mode grows a strings.Builder from the same token,
			// so it needs the same bound as the slice-preallocating kinds.
			name:   "ascii strict",
			input:  `S1F1 <A[` + sizeWidth1 + `] "x">.`,
			strict: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := require.New(t)

			var parser *Parser
			if tt.strict {
				parser = NewParser(WithParserStrictMode(true))
			} else {
				parser = NewParser()
			}

			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)

			_, _ = parser.Parse(tt.input) //nolint:errcheck // allocation is the only thing under test

			runtime.ReadMemStats(&after)

			delta := after.TotalAlloc - before.TotalAlloc
			req.Lessf(delta, uint64(maxAllowedAllocBytes),
				"Parse allocated %d bytes for input %q, want < %d", delta, tt.input, maxAllowedAllocBytes)
		})
	}
}

// TestParseStrict_LongNumericTokenDoesNotCopyQuadratically guards the unquoted numeric token.
// The token used to be accumulated one rune at a time into a string.
// Go strings are immutable, so that cost O(n^2) copying before the token was even validated.
// A 195 KB token reached roughly 19 GB.
//
// The token is now sliced out of the input at its delimiter, so cost is linear in the input.
// Parse is still expected to reject the token; only the allocation is asserted.
func TestParseStrict_LongNumericTokenDoesNotCopyQuadratically(t *testing.T) {
	req := require.New(t)

	const tokenLen = 200_000
	input := `S1F1 <A[1] ` + strings.Repeat("9", tokenLen) + ` >.`

	runtime.GC() //nolint:revive // a collected heap is what makes the TotalAlloc delta below meaningful
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	_, _ = NewParser(WithParserStrictMode(true)).Parse(input) //nolint:errcheck // allocation is the only thing under test

	runtime.ReadMemStats(&after)

	delta := after.TotalAlloc - before.TotalAlloc
	req.Lessf(delta, uint64(maxAllowedAllocBytes),
		"parsing a %d-byte numeric token allocated %d bytes, want < %d", tokenLen, delta, maxAllowedAllocBytes)
}

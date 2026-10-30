package redact_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/redact"
)

// TestCompileOverlap rejects, before use, a policy with encryption whose item rules for one stream and function
// can select overlapping items: identical paths, a prefix path, and * meeting a numbered step;
// disjoint paths, rules of different S/F, and overlapping rules without encryption are accepted.
func TestCompileOverlap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		a, b    redact.ItemRule
		overlap bool
	}{
		{"identical", rule(7, 3, "2"), rule(7, 3, "2"), true},
		{"identical in two domains", rule(7, 3, "2"), redact.ItemRule{Stream: 7, Function: 3, Path: "2", Domain: "other"}, true},
		{"prefix", rule(15, 13, "4"), rule(15, 13, "4/*/2"), true},
		{"root and any", rule(10, 9, ""), rule(10, 9, "1"), true},
		{"* meeting a numbered step", rule(13, 6, "4/*"), rule(13, 6, "4/2"), true},
		{"* on both sides", rule(16, 15, "2/*/4"), rule(16, 15, "*/1/4"), true},
		{"disjoint", rule(15, 13, "4/*/2"), rule(15, 13, "5"), false},
		{"disjoint below *", rule(15, 13, "4/*/2"), rule(15, 13, "4/*/1"), false},
		{"another S/F", rule(7, 3, "2"), rule(7, 6, "2"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := redact.Policy{ID: "p", DigestKeyID: digestKeyID, ItemRules: []redact.ItemRule{tt.a, tt.b}}
			_, err := redact.Compile(p)
			require.NoError(t, err, "without encryption, overlapping rules are accepted")

			p.Encryption = &redact.Encryption{KeyID: encKeyID}
			_, err = redact.Compile(p)
			if tt.overlap {
				require.ErrorIs(t, err, redact.ErrOverlappingRules)
				require.ErrorIs(t, err, redact.ErrInvalidPolicy)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestCompileDomains accepts domains of 1 and 65535 bytes of UTF-8,
// and rejects, in item and annotation rules alike, a domain of 0 or 65536 bytes or of invalid UTF-8.
func TestCompileDomains(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		domain string
		ok     bool
	}{
		{"1 byte", "d", true},
		{"65535 bytes", strings.Repeat("d", 65535), true},
		{"multi-byte UTF-8", "配方", true},
		{"empty", "", false},
		{"65536 bytes", strings.Repeat("d", 65536), false},
		{"invalid UTF-8", "d\xff", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			for _, p := range []redact.Policy{
				{ItemRules: []redact.ItemRule{{Stream: 7, Function: 3, Path: "2", Domain: tt.domain}}},
				{AnnotationRules: []redact.AnnotationRule{{Kind: tracepack.AnnotationKindUnparsedEntry, Domain: tt.domain}}},
			} {
				_, err := redact.Compile(p)
				if tt.ok {
					require.NoError(t, err)
				} else {
					require.ErrorIs(t, err, redact.ErrInvalidPolicy)
					require.ErrorIs(t, err, redact.ErrDomain)
				}
			}
		})
	}
}

// TestCompileRules rejects an item rule whose path is not a pattern or whose stream no record can carry.
func TestCompileRules(t *testing.T) {
	t.Parallel()

	for _, r := range []redact.ItemRule{
		{Stream: 7, Function: 3, Path: "2/", Domain: "d"},
		{Stream: 7, Function: 3, Path: "0", Domain: "d"},
		{Stream: 7, Function: 3, Path: "x", Domain: "d"},
		{Stream: 128, Function: 3, Path: "2", Domain: "d"},
	} {
		_, err := redact.Compile(redact.Policy{ItemRules: []redact.ItemRule{r}})
		require.ErrorIs(t, err, redact.ErrInvalidPolicy, "%+v", r)
	}
}

// TestPackPolicy gives the redaction_policy value of a policy: both encryption tags with encryption, neither without.
// The compiled policy keeps no reference to the caller's slices.
func TestPackPolicy(t *testing.T) {
	t.Parallel()

	p := baselinePolicy(true)
	c := mustCompile(t, p)
	got := c.PackPolicy()
	alg := tracepack.EncryptionAlgorithmAESSIVCMAC512
	keyID := encKeyID
	assert.Equal(t, tracepack.RedactionPolicy{
		PolicyID: "test-policy", PolicyVersion: 3, KeyID: digestKeyID, DigestAlgorithm: tracepack.DigestAlgorithmHMACSHA256,
		EncryptionAlgorithm: &alg, EncryptionKeyID: &keyID,
	}, got)
	*got.EncryptionKeyID = "changed"
	assert.Equal(t, encKeyID, *c.PackPolicy().EncryptionKeyID, "each call returns its own value")

	plain := mustCompile(t, baselinePolicy(false)).PackPolicy()
	assert.Nil(t, plain.EncryptionAlgorithm)
	assert.Nil(t, plain.EncryptionKeyID)

	// Changing the caller's policy after Compile changes nothing compiled.
	p.ItemRules[0].Path = "1"
	p.Encryption.KeyID = "other"
	r := dataRecord(1, frame(7, 3, ppbodyText("PP1", []byte("BODY"))))
	_, entries, err := c.Mask(t.Context(), captureA, r, false, &testKeys{})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, new("2"), entries[0].ItemPath)
	assert.Equal(t, encKeyID, *c.PackPolicy().EncryptionKeyID)
}

// rule returns an item rule of the S/F and path in the domain d.
func rule(stream, function uint8, path string) redact.ItemRule {
	return redact.ItemRule{Stream: stream, Function: function, Path: path, Domain: "d"}
}

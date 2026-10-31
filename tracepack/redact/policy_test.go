package redact_test

import (
	"io"
	"math"
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

// TestCompilePackPolicy refuses, under ErrInvalidPolicy, a policy whose redaction_policy the pack metadata encoding refuses:
// an ID, DigestKeyID or Encryption.KeyID that is not valid UTF-8, or a Version above 2^63-1.
// A policy Compile accepts never makes NewWriter refuse its PackPolicy, with encryption or without.
func TestCompilePackPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mutate    func(p *redact.Policy)
		encrypted bool
		ok        bool
	}{
		{"baseline", func(*redact.Policy) {}, false, true},
		{"baseline with encryption", func(*redact.Policy) {}, true, true},
		{"multi-byte UTF-8", func(p *redact.Policy) { p.ID, p.DigestKeyID = "配方", "鍵" }, true, true},
		{"empty strings", func(p *redact.Policy) { p.ID, p.DigestKeyID, p.Encryption.KeyID = "", "", "" }, true, true},
		{"Version 2^63-1", func(p *redact.Policy) { p.Version = math.MaxInt64 }, false, true},
		{"ID not UTF-8", func(p *redact.Policy) { p.ID = "p\xff" }, false, false},
		{"DigestKeyID not UTF-8", func(p *redact.Policy) { p.DigestKeyID = "k\xff" }, false, false},
		{"Encryption.KeyID not UTF-8", func(p *redact.Policy) { p.Encryption.KeyID = "e\xff" }, true, false},
		{"Version 2^63", func(p *redact.Policy) { p.Version = 1 << 63 }, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := baselinePolicy(tt.encrypted)
			tt.mutate(&p)
			c, err := redact.Compile(p)
			if !tt.ok {
				require.ErrorIs(t, err, redact.ErrInvalidPolicy)
				require.Nil(t, c)

				return
			}
			require.NoError(t, err)

			pol := c.PackPolicy()
			_, err = tracepack.NewWriter(io.Discard, tracepack.WriterOptions{Meta: policyMeta(&pol)})
			require.NoError(t, err)
		})
	}
}

// policyMeta returns the pack metadata of a pack written under pol:
// an extract for a policy without encryption, and a segment masked at write, with its one classifier, for one with encryption.
func policyMeta(pol *tracepack.RedactionPolicy) *tracepack.PackMeta {
	m := &tracepack.PackMeta{
		ToolID: "tool-01", Transport: tracepack.TransportHSMSSS, CaptureMethod: tracepack.CaptureMethodGenerator,
		Vantage: tracepack.VantageNone, Recorder: "redact-test", Writer: "redact-test", TimeSource: tracepack.TimeSourceGenerator,
		PeriodStart: 3600e9, PeriodEnd: 3660e9, LifecycleCoverage: tracepack.LifecycleCoverageNone,
		RecorderInstanceID: tracepack.UUID{0x01, 15: 0x01}, RedactionPolicy: pol,
	}
	if pol.EncryptionAlgorithm == nil {
		m.PackRole, m.ExtractFilter = tracepack.PackRoleExtract, new("S7F3")
	} else {
		m.PackRole, m.ScopeGeneration, m.Classifiers = tracepack.PackRoleSegment, new(uint64(0)), []string{"hsms"}
	}

	return m
}

// TestCompileErrorText names the namespace once when ErrInvalidPolicy wraps another sentinel of this package.
func TestCompileErrorText(t *testing.T) {
	t.Parallel()

	p := baselinePolicy(false)
	p.AnnotationRules = []redact.AnnotationRule{{Kind: tracepack.AnnotationKindUnparsedEntry}}
	_, err := redact.Compile(p)
	require.ErrorIs(t, err, redact.ErrInvalidPolicy)
	require.ErrorIs(t, err, redact.ErrDomain)
	require.NotErrorIs(t, err, tracepack.ErrInvalidArgument, "redact keeps its own sentinels")
	require.EqualError(t, err, "redact: invalid policy: annotation rule 0: a domain is not 1 to 65535 bytes of UTF-8: 0 bytes")

	p = baselinePolicy(true)
	p.ItemRules = append(p.ItemRules, p.ItemRules[0])
	_, err = redact.Compile(p)
	require.ErrorIs(t, err, redact.ErrOverlappingRules)
	require.Equal(t, 1, strings.Count(err.Error(), "redact: "), err.Error())
}

package redact_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/siv"
	"github.com/arloliu/go-secs/tracepack/redact"
)

// Identities of the test records.
var (
	captureA = tracepack.UUID{0x01, 0x92, 0x3a, 0x4b, 0x5c, 0x6d, 0x7e, 0x8f, 0x90, 0xa1, 0xb2, 0xc3, 0xd4, 0xe5, 0xf6, 0x07}
	captureB = tracepack.UUID{0x01, 0x92, 0x3a, 0x4b, 0x5c, 0x6d, 0x7e, 0x8f, 0x90, 0xa1, 0xb2, 0xc3, 0xd4, 0xe5, 0xf6, 0x08}
)

// Key ids of the test keys.
const (
	digestKeyID = "test-digest-1"
	encKeyID    = "test-enc-1"
)

// testKeys serves keys derived in the test: SHA-256 of a label, expanded to 64 bytes for an encryption key.
// It counts the requests, so a test can tell that a record nothing matched asked for no key.
type testKeys struct {
	calls int
}

var _ redact.Keys = (*testKeys)(nil)

// DigestKey returns the digest key of keyID.
func (k *testKeys) DigestKey(_ context.Context, keyID string) ([]byte, error) {
	k.calls++
	key := sha256.Sum256([]byte("redact test digest key " + keyID))

	return key[:], nil
}

// EncryptionKey returns the encryption key of capture and keyID.
func (k *testKeys) EncryptionKey(_ context.Context, capture tracepack.UUID, keyID string) ([]byte, error) {
	k.calls++

	return encryptionKey(capture, keyID), nil
}

// encryptionKey derives the 64-byte encryption key of capture and keyID.
func encryptionKey(capture tracepack.UUID, keyID string) []byte {
	a := sha256.Sum256(slices.Concat([]byte("redact test encryption key a "+keyID), capture[:]))
	b := sha256.Sum256(slices.Concat([]byte("redact test encryption key b "+keyID), capture[:]))

	return slices.Concat(a[:], b[:])
}

// digestOf is the digest the tracepack semantics specification §8 defines, computed independently of the package.
func digestOf(domain string, original []byte) []byte {
	key, _ := (&testKeys{}).DigestKey(context.Background(), digestKeyID)
	mac := hmac.New(sha256.New, key)
	mac.Write(binary.LittleEndian.AppendUint16(nil, uint16(len(domain))))
	mac.Write([]byte(domain))
	mac.Write(original)

	return mac.Sum(nil)
}

// item returns a SECS-II item of the format code code with the fewest length bytes that hold len(body).
func item(code byte, body ...byte) []byte {
	n := len(body)
	switch {
	case n < 1<<8:
		return append([]byte{code<<2 | 1, byte(n)}, body...)
	case n < 1<<16:
		return append([]byte{code<<2 | 2, byte(n >> 8), byte(n)}, body...)
	default:
		return append([]byte{code<<2 | 3, byte(n >> 16), byte(n >> 8), byte(n)}, body...)
	}
}

// ascii returns an ASCII item of s.
func ascii(s string) []byte {
	return item(0o20, []byte(s)...)
}

// list returns a SECS-II list of the elements.
func list(elements ...[]byte) []byte {
	return slices.Concat(append([][]byte{{1, byte(len(elements))}}, elements...)...)
}

// frame returns the HSMS data frame of the given stream and function, whose message text is text,
// with W, SessionID 1 and System Bytes 1.
func frame(stream, function byte, text []byte) []byte {
	f := make([]byte, 14, 14+len(text))
	binary.BigEndian.PutUint32(f, uint32(10+len(text)))
	f[5], f[6], f[7], f[13] = 1, 0x80|stream, function, 1

	return append(f, text...)
}

// dataRecord returns a data record of seq carrying payload.
func dataRecord(seq uint64, payload []byte) *tracepack.Record {
	r := &tracepack.Record{
		Seq: seq, TSUTCNs: 1_700_000_000_000_000_000 + int64(seq), Epoch: 1, Kind: tracepack.KindData,
		Dir: tracepack.DirHostToEquipment, Fidelity: tracepack.FidelityWireExact, DecodeStatus: tracepack.DecodeStatusOK,
		Payload: payload,
	}
	r.SetCapturedFieldValidity()

	return r
}

// annotationRecord returns an annotation record of seq whose payload is a.
func annotationRecord(t testing.TB, seq uint64, a *tracepack.Annotation) *tracepack.Record {
	t.Helper()

	b, err := a.MarshalBinary()
	require.NoError(t, err)

	return &tracepack.Record{
		Seq: seq, Kind: tracepack.KindAnnotation, Dir: tracepack.DirLocal, Fidelity: tracepack.FidelityNotApplicable,
		DecodeStatus: tracepack.DecodeStatusNotApplicable, Payload: b,
	}
}

// baselinePolicy is the baseline policy of the tracepack semantics specification §8 the tests use,
// under encryption when encrypted.
func baselinePolicy(encrypted bool) redact.Policy {
	p := redact.Policy{
		ID: "test-policy", Version: 3, DigestKeyID: digestKeyID,
		ItemRules: []redact.ItemRule{
			{Stream: 7, Function: 3, Path: "2", Domain: "ppbody"},
			{Stream: 7, Function: 6, Path: "2", Domain: "ppbody"},
			{Stream: 7, Function: 36, Path: "3", Domain: "ppbody"},
			{Stream: 7, Function: 23, Path: "4", Domain: "formatted-pp"},
			{Stream: 7, Function: 26, Path: "4", Domain: "formatted-pp"},
			{Stream: 13, Function: 6, Path: "4/*", Domain: "dataset"},
			{Stream: 16, Function: 11, Path: "5/3/*/2", Domain: "rcpparval"},
			{Stream: 16, Function: 15, Path: "2/*/4/3/*/2", Domain: "rcpparval"},
		},
		AnnotationRules: []redact.AnnotationRule{{Kind: tracepack.AnnotationKindUnparsedEntry, Domain: "source-text"}},
	}
	if encrypted {
		p.Encryption = &redact.Encryption{KeyID: encKeyID}
	}

	return p
}

// mustCompile compiles p.
func mustCompile(t testing.TB, p redact.Policy) *redact.Compiled {
	t.Helper()

	c, err := redact.Compile(p)
	require.NoError(t, err)

	return c
}

// ppbodyText returns the text of an S7F3 carrying the process program body.
func ppbodyText(ppid string, body []byte) []byte {
	return list(ascii(ppid), item(0o10, body...))
}

// formattedText returns the text of an S7F23 with two process commands, the second with two parameters.
func formattedText() []byte {
	return list(ascii("PP-F"), ascii("MDL"), ascii("1.0"), list(
		list(ascii("C1"), list(ascii("P=1"))),
		list(ascii("C2"), list(ascii("P=2"), item(0o54, 0, 0, 0, 9))),
	))
}

// datasetText returns the text of an S13F6 whose fourth item holds the data sets.
func datasetText(sets ...[]byte) []byte {
	data := make([][]byte, len(sets))
	for i, s := range sets {
		data[i] = item(0o10, s...)
	}

	return list(ascii("TABLE"), item(0o54, 0, 0, 0, 1), ascii("OK"), list(data...))
}

// recipeText returns the text of an S16F11 whose RCPPARVALs, at 5/3/*/2, are the localized strings given as bodies.
func recipeText(values ...[]byte) []byte {
	params := make([][]byte, len(values))
	for i, v := range values {
		params[i] = list(ascii("N"+string(rune('1'+i))), item(0o22, v...))
	}

	return list(ascii("JOB"), ascii("MF"), ascii("RCP"), ascii("X"), list(ascii("R"), ascii("V"), list(params...)))
}

// maskCase is a record a test masks, with the masks it expects.
type maskCase struct {
	name    string
	payload []byte
	// want lists the expected entries by item path and masked ranges; nil for none.
	want []wantEntry
}

// wantEntry is an expected entry: its item path (nil for a whole-text mask), domain, ranges and digest input range.
type wantEntry struct {
	path   *string
	domain string
	ranges []tracepack.MaskedRange
	// digest is the payload range whose original bytes the digest covers.
	digest [2]int
}

// itemCases are the records of the baseline rules masked by item, with their expected entries.
func itemCases() []maskCase {
	ppbody := frame(7, 3, ppbodyText("PP1", []byte("SECRET-RECIPE-01")))
	formatted := frame(7, 23, formattedText())
	dataset := frame(13, 6, datasetText([]byte{1, 2, 3}, []byte{4, 5, 6, 7}))
	recipe := frame(16, 11, recipeText([]byte{0, 1, 'v', 'a', 'l'}, []byte{0, 1}))
	zero := frame(7, 3, ppbodyText("PP0", make([]byte, 6)))

	return []maskCase{
		{"S7F3 PPBODY", ppbody, []wantEntry{{new("2"), "ppbody", []tracepack.MaskedRange{{Offset: 23, Length: 16}}, [2]int{21, 39}}}},
		{"S7F23 formatted", formatted, []wantEntry{{new("4"), "formatted-pp", []tracepack.MaskedRange{
			{Offset: 38, Length: 2}, {Offset: 44, Length: 3}, {Offset: 51, Length: 2}, {Offset: 57, Length: 3},
			{Offset: 62, Length: 4},
		}, [2]int{32, len(formatted)}}}},
		{"S13F6 data sets", dataset, []wantEntry{
			{new("4/1"), "dataset", []tracepack.MaskedRange{{Offset: 37, Length: 3}}, [2]int{35, 40}},
			{new("4/2"), "dataset", []tracepack.MaskedRange{{Offset: 42, Length: 4}}, [2]int{40, 46}},
		}},
		{"S16F11 format-22 leaf", recipe, []wantEntry{
			{new("5/3/1/2"), "rcpparval", []tracepack.MaskedRange{{Offset: 53, Length: 3}}, [2]int{49, 56}},
		}},
		{"already-zero PPBODY", zero, []wantEntry{{new("2"), "ppbody", []tracepack.MaskedRange{{Offset: 23, Length: 6}}, [2]int{21, 29}}}},
		{"S7F6 denial", frame(7, 6, list()), nil},
		{"S7F26 denial", frame(7, 26, list()), nil},
		{"S7F36 denial", frame(7, 36, list()), nil},
		{"S13F6 empty data list", frame(13, 6, datasetText()), nil},
		{"zero-length PPBODY", frame(7, 3, ppbodyText("PP1", nil)), nil},
		{"header-only S7F3", frame(7, 3, nil), nil},
		{"S6F11 same text", frame(6, 11, ppbodyText("PP1", []byte("SECRET-RECIPE-01"))), nil},
		{"short payload", frame(7, 3, nil)[:10], nil},
	}
}

// wholeTextCases are the S7F3 and S16F11 records masked whole, each with one message-text entry.
func wholeTextCases() []maskCase {
	whole := func(name string, f []byte) maskCase {
		return maskCase{name, f, []wantEntry{{nil, redact.DomainMessageText,
			[]tracepack.MaskedRange{{Offset: 14, Length: uint64(len(f) - 14)}}, [2]int{14, len(f)}}}}
	}

	return []maskCase{
		// A list of two elements whose second is a U2 of 3 bytes.
		whole("invalid text", frame(7, 3, slices.Concat([]byte{1, 2}, ascii("PP1"), []byte{0o52<<2 | 1, 3, 1, 2, 3}))),
		whole("trailing bytes", frame(7, 3, slices.Concat(ppbodyText("PP1", []byte("BODY")), []byte{0, 0}))),
		whole("unresolved path", frame(7, 3, list(ascii("PP1")))),
		whole("format-22 leaf of 1 byte", frame(16, 11, recipeText([]byte{0}))),
		whole("S13F6 fourth item not a list", frame(13, 6, list(ascii("T"), ascii("U"), ascii("V"), ascii("W")))),
		whole("S16F15 job lacking 4/3", frame(16, 15, list(ascii("ID"), list(
			list(ascii("J1"), ascii("a"), ascii("b"), list(ascii("x"), ascii("y"), list(list(ascii("N"), ascii("SECRET"))))),
			list(ascii("J2"), ascii("a"), ascii("b"), list(ascii("x"), ascii("y"))),
		)))),
	}
}

// checkMasked requires that masked is r masked as c expects: one entry per expected entry, in order,
// with the item path, domain, ranges and digest of the original bytes;
// the payload of the same length and unchanged outside the ranges;
// under encryption the AES-SIV ciphertext of the target bytes, else zeros;
// quality.redacted added and every other field kept.
func checkMasked(t *testing.T, c maskCase, r *tracepack.Record, masked tracepack.Record, entries []tracepack.RedactionEntry, encrypted bool) {
	t.Helper()

	if c.want == nil {
		require.Empty(t, entries)
		assert.Equal(t, *r, masked)

		return
	}
	require.Len(t, entries, len(c.want))
	require.Len(t, masked.Payload, len(r.Payload))
	want := *r
	want.Quality |= tracepack.QualityRedacted
	want.Payload = masked.Payload
	assert.Equal(t, want, masked)

	covered := make([]bool, len(r.Payload))
	for i, w := range c.want {
		e := entries[i]
		assert.Equal(t, r.Seq, e.Seq)
		assert.Equal(t, w.path, e.ItemPath, "entry %d", i)
		assert.Equal(t, w.domain, e.Domain, "entry %d", i)
		assert.Equal(t, w.ranges, e.MaskedRanges, "entry %d", i)
		assert.Empty(t, e.RawMaskedRanges)
		assert.Equal(t, digestOf(w.domain, r.Payload[w.digest[0]:w.digest[1]]), e.Digest, "entry %d: digest", i)

		var plain, got []byte
		for _, mr := range e.MaskedRanges {
			plain = append(plain, r.Payload[mr.Offset:mr.Offset+mr.Length]...)
			got = append(got, masked.Payload[mr.Offset:mr.Offset+mr.Length]...)
			for k := mr.Offset; k < mr.Offset+mr.Length; k++ {
				covered[k] = true
			}
		}
		if !encrypted {
			assert.False(t, e.HasSIV)
			assert.Equal(t, make([]byte, len(plain)), got, "entry %d: zeros", i)

			continue
		}
		require.True(t, e.HasSIV, "entry %d", i)
		ad := siv.AssociatedData(captureA, r.Seq, e.MaskedRanges[0].Offset)
		iv, ct, err := siv.Seal(encryptionKey(captureA, encKeyID), ad[:], plain)
		require.NoError(t, err)
		assert.Equal(t, iv[:], e.SIV, "entry %d: siv", i)
		assert.Equal(t, ct, got, "entry %d: ciphertext of the target bytes", i)
	}
	for k := range r.Payload {
		if !covered[k] {
			assert.Equal(t, r.Payload[k], masked.Payload[k], "payload byte %d outside the ranges", k)
		}
	}
}

// TestMaskItem masks the records of the baseline rules under a policy with encryption and under one without:
// ranges list the content bytes of the selected leaves, item headers and the HSMS header kept,
// digests cover the original encoded items, and a record with no target bytes yields no entry and no change.
func TestMaskItem(t *testing.T) {
	t.Parallel()

	for _, encrypted := range []bool{true, false} {
		c := mustCompile(t, baselinePolicy(encrypted))
		for _, tt := range itemCases() {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				r := dataRecord(42, tt.payload)
				orig := bytes.Clone(r.Payload)
				keys := &testKeys{}
				masked, entries, err := c.Mask(t.Context(), captureA, r, false, keys)
				require.NoError(t, err)
				assert.Equal(t, orig, r.Payload, "the caller's payload is unchanged")
				checkMasked(t, tt, r, masked, entries, encrypted)
				if tt.want == nil {
					assert.Zero(t, keys.calls, "a record nothing matched asks for no key")
				}
			})
		}
	}
}

// TestMaskWholeText masks whole a text that is not one valid item, holds trailing bytes,
// or meets a rule whose path does not resolve: one message-text entry without item path, and no other entry.
func TestMaskWholeText(t *testing.T) {
	t.Parallel()

	for _, encrypted := range []bool{true, false} {
		c := mustCompile(t, baselinePolicy(encrypted))
		for _, tt := range wholeTextCases() {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				r := dataRecord(7, tt.payload)
				masked, entries, err := c.Mask(t.Context(), captureA, r, false, &testKeys{})
				require.NoError(t, err)
				checkMasked(t, tt, r, masked, entries, encrypted)
			})
		}
	}

	t.Run("one rule of two unresolved", func(t *testing.T) {
		t.Parallel()

		p := baselinePolicy(true)
		p.ItemRules = append(p.ItemRules, redact.ItemRule{Stream: 7, Function: 3, Path: "3", Domain: "other"})
		r := dataRecord(7, frame(7, 3, ppbodyText("PP1", []byte("BODY"))))
		_, entries, err := mustCompile(t, p).Mask(t.Context(), captureA, r, false, &testKeys{})
		require.NoError(t, err)
		require.Len(t, entries, 1)
		assert.Nil(t, entries[0].ItemPath)
		assert.Equal(t, redact.DomainMessageText, entries[0].Domain)
	})
}

// TestMaskAnnotation masks an unparsed-entry annotation through its raw value and through its text value
// under a policy with encryption: zeros, an entry without siv, and the digest of the original value.
// An annotation of another kind is not masked.
func TestMaskAnnotation(t *testing.T) {
	t.Parallel()

	c := mustCompile(t, baselinePolicy(true))
	text := "line 12: SECRET=1"
	tests := []struct {
		name  string
		a     *tracepack.Annotation
		value []byte
	}{
		{"raw", &tracepack.Annotation{AnnotationKind: tracepack.AnnotationKindUnparsedEntry, Raw: []byte{0xde, 0xad, 0xbe, 0xef}}, []byte{0xde, 0xad, 0xbe, 0xef}},
		{"text", &tracepack.Annotation{AnnotationKind: tracepack.AnnotationKindUnparsedEntry, Text: &text}, []byte(text)},
		{"another kind", &tracepack.Annotation{AnnotationKind: tracepack.AnnotationKindNote, Text: &text}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := annotationRecord(t, 9, tt.a)
			masked, entries, err := c.Mask(t.Context(), captureA, r, false, &testKeys{})
			require.NoError(t, err)
			if tt.value == nil {
				assert.Empty(t, entries)
				assert.Equal(t, *r, masked)

				return
			}
			off := bytes.Index(r.Payload, tt.value)
			require.Positive(t, off)
			want := maskCase{tt.name, r.Payload, []wantEntry{{nil, "source-text",
				[]tracepack.MaskedRange{{Offset: uint64(off), Length: uint64(len(tt.value))}}, [2]int{off, off + len(tt.value)}}}}
			checkMasked(t, want, r, masked, entries, false)
			a, err := tracepack.UnmarshalAnnotation(masked.Payload)
			require.NoError(t, err, "the masked annotation stays well formed")
			assert.Equal(t, tracepack.AnnotationKindUnparsedEntry, a.AnnotationKind)
		})
	}
}

// TestMaskUnobserved leaves a record unobserved by design untouched, with no entry and no key request,
// refuses to take a record without quality.unobserved for one,
// and masks a record carrying a reserved bit 7 that is not unobserved like any other record.
func TestMaskUnobserved(t *testing.T) {
	t.Parallel()

	c := mustCompile(t, baselinePolicy(true))
	sensitive := frame(7, 3, ppbodyText("PP1", []byte("SECRET-RECIPE-01")))

	r := dataRecord(1, sensitive)
	r.Quality |= tracepack.QualityUnobserved
	keys := &testKeys{}
	masked, entries, err := c.Mask(t.Context(), captureA, r, true, keys)
	require.NoError(t, err)
	assert.Empty(t, entries)
	assert.Equal(t, *r, masked)
	assert.Zero(t, keys.calls)

	_, _, err = c.Mask(t.Context(), captureA, dataRecord(2, sensitive), true, &testKeys{})
	require.ErrorIs(t, err, redact.ErrNotUnobserved)

	// A record of a minor-0 pack, whose bit 7 is reserved.
	masked, entries, err = c.Mask(t.Context(), captureA, r, false, &testKeys{})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.True(t, masked.Quality.Has(tracepack.QualityRedacted))
	assert.True(t, masked.Quality.Has(tracepack.QualityUnobserved), "the stored bit is kept")
	assert.NotEqual(t, sensitive, masked.Payload)
}

// ctxKeys is a testKeys that records the context of each key request and runs onDigest, when set, inside DigestKey.
type ctxKeys struct {
	testKeys
	ctxs     []context.Context
	onDigest func()
}

// DigestKey records ctx, runs onDigest and returns the digest key of keyID.
func (k *ctxKeys) DigestKey(ctx context.Context, keyID string) ([]byte, error) {
	k.ctxs = append(k.ctxs, ctx)
	if k.onDigest != nil {
		k.onDigest()
	}

	return k.testKeys.DigestKey(ctx, keyID)
}

// EncryptionKey records ctx and returns the encryption key of capture and keyID.
func (k *ctxKeys) EncryptionKey(ctx context.Context, capture tracepack.UUID, keyID string) ([]byte, error) {
	k.ctxs = append(k.ctxs, ctx)

	return k.testKeys.EncryptionKey(ctx, capture, keyID)
}

// TestMaskContext checks the context before each key request and passes the caller's context to Keys:
// a done context is returned as is before any request, or before the encryption key once the digest key was served,
// and a record that needs no key is returned whatever the context.
func TestMaskContext(t *testing.T) {
	t.Parallel()

	c := mustCompile(t, baselinePolicy(true))
	sensitive := frame(7, 3, ppbodyText("PP1", []byte("SECRET-RECIPE-01")))

	t.Run("cancelled", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		keys := &ctxKeys{}
		_, entries, err := c.Mask(ctx, captureA, dataRecord(1, sensitive), false, keys)
		require.Equal(t, ctx.Err(), err)
		assert.Nil(t, entries)
		assert.Zero(t, keys.calls, "no key is requested under a done context")

		r := dataRecord(2, frame(1, 1, nil))
		masked, entries, err := c.Mask(ctx, captureA, r, false, keys)
		require.NoError(t, err, "a record that needs no key ignores the context")
		assert.Nil(t, entries)
		assert.Equal(t, *r, masked)
	})

	t.Run("cancelled after the digest key", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		keys := &ctxKeys{onDigest: cancel}
		_, _, err := c.Mask(ctx, captureA, dataRecord(1, sensitive), false, keys)
		require.Equal(t, ctx.Err(), err)
		assert.Equal(t, 1, keys.calls, "the encryption key is not requested once the context is done")
	})

	t.Run("caller's context", func(t *testing.T) {
		t.Parallel()

		type ctxKey struct{}
		ctx := context.WithValue(t.Context(), ctxKey{}, "caller")
		keys := &ctxKeys{}
		_, entries, err := c.Mask(ctx, captureA, dataRecord(1, sensitive), false, keys)
		require.NoError(t, err)
		require.Len(t, entries, 1)
		require.Len(t, keys.ctxs, 2, "a digest key and an encryption key")
		for _, got := range keys.ctxs {
			assert.Equal(t, "caller", got.Value(ctxKey{}))
		}
	})
}

// TestMaskDeterministic masks one record twice, in two calls: identical records and entries.
func TestMaskDeterministic(t *testing.T) {
	t.Parallel()

	c := mustCompile(t, baselinePolicy(true))
	for _, tt := range slices.Concat(itemCases(), wholeTextCases()) {
		r := dataRecord(5, tt.payload)
		m1, e1, err := c.Mask(t.Context(), captureA, r, false, &testKeys{})
		require.NoError(t, err)
		m2, e2, err := c.Mask(t.Context(), captureA, r, false, &testKeys{})
		require.NoError(t, err)
		assert.Equal(t, m1, m2, tt.name)
		assert.Equal(t, e1, e2, tt.name)
	}
}

// TestMaskCannotScreen returns an error, and no record, for a selected record that cannot be screened:
// a text whose lists nest deeper than the walker's limit, an annotation whose payload does not decode,
// and a record already carrying quality.redacted.
// A text nested to the limit is masked.
func TestMaskCannotScreen(t *testing.T) {
	t.Parallel()

	c := mustCompile(t, baselinePolicy(true))
	nested := func(depth int) []byte {
		b := item(0o10, 1, 2, 3)
		for range depth {
			b = list(b)
		}

		return b
	}

	// The PPBODY is element 2 of the root list; the deep list is element 1.
	_, entries, err := c.Mask(t.Context(), captureA, dataRecord(1, frame(7, 3, list(nested(63), item(0o10, 9)))), false, &testKeys{})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, new("2"), entries[0].ItemPath)

	masked, entries, err := c.Mask(t.Context(), captureA, dataRecord(1, frame(7, 3, list(nested(64), item(0o10, 9)))), false, &testKeys{})
	require.ErrorIs(t, err, redact.ErrCannotScreen)
	assert.Empty(t, entries)
	assert.Zero(t, masked)

	ann := &tracepack.Record{Seq: 2, Kind: tracepack.KindAnnotation, Payload: []byte{0, 1, 2}}
	_, _, err = c.Mask(t.Context(), captureA, ann, false, &testKeys{})
	require.ErrorIs(t, err, redact.ErrCannotScreen)

	r := dataRecord(3, frame(7, 3, ppbodyText("PP1", []byte("BODY"))))
	r.Quality |= tracepack.QualityRedacted
	_, _, err = c.Mask(t.Context(), captureA, r, false, &testKeys{})
	require.ErrorIs(t, err, redact.ErrRedacted)
}

// TestMaskNeverMasked leaves control records, transport events and records of other stream and function untouched.
func TestMaskNeverMasked(t *testing.T) {
	t.Parallel()

	c := mustCompile(t, redact.Policy{DigestKeyID: digestKeyID, Encryption: &redact.Encryption{KeyID: encKeyID},
		ItemRules: []redact.ItemRule{{Stream: 0, Function: 0, Path: "", Domain: "all"}}})
	control := dataRecord(1, frame(0, 0, nil))
	control.Kind = tracepack.KindControl
	event := &tracepack.Record{Seq: 2, Kind: tracepack.KindTransportEvent, Payload: []byte{1, 2, 3}}
	for _, r := range []*tracepack.Record{control, event} {
		masked, entries, err := c.Mask(t.Context(), captureA, r, false, &testKeys{})
		require.NoError(t, err)
		assert.Empty(t, entries)
		assert.Equal(t, *r, masked)
	}
}

// TestMaskOverlappingZeroMasks masks one item through two rules of a policy without encryption:
// two entries, each with the digest of the original bytes.
func TestMaskOverlappingZeroMasks(t *testing.T) {
	t.Parallel()

	p := baselinePolicy(false)
	p.ItemRules = append(p.ItemRules, redact.ItemRule{Stream: 7, Function: 3, Path: "", Domain: "whole-ppbody"})
	r := dataRecord(1, frame(7, 3, ppbodyText("PP1", []byte("BODY"))))
	masked, entries, err := mustCompile(t, p).Mask(t.Context(), captureA, r, false, &testKeys{})
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, new(""), entries[0].ItemPath)
	assert.Equal(t, digestOf("whole-ppbody", r.Payload[14:]), entries[0].Digest)
	assert.Equal(t, new("2"), entries[1].ItemPath)
	assert.Equal(t, digestOf("ppbody", r.Payload[21:27]), entries[1].Digest)
	assert.Equal(t, []tracepack.MaskedRange{{Offset: 18, Length: 3}, {Offset: 23, Length: 4}}, entries[0].MaskedRanges)
	assert.Equal(t, r.Payload[:14], masked.Payload[:14])
	assert.Equal(t, make([]byte, 4), masked.Payload[23:27])
}

// BenchmarkMaskPPBODY masks an S7F3 whose PPBODY is 4 KiB and 1 MiB under a policy with encryption.
func BenchmarkMaskPPBODY(b *testing.B) {
	c := mustCompile(b, baselinePolicy(true))
	for _, size := range []struct {
		name string
		n    int
	}{{"4KiB", 4 << 10}, {"1MiB", 1 << 20}} {
		b.Run(size.name, func(b *testing.B) {
			body := bytes.Repeat([]byte{0x5a}, size.n)
			r := dataRecord(1, frame(7, 3, ppbodyText("PP1", body)))
			keys := &testKeys{}
			b.SetBytes(int64(len(r.Payload)))
			b.ReportAllocs()
			for b.Loop() {
				if _, _, err := c.Mask(b.Context(), captureA, r, false, keys); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

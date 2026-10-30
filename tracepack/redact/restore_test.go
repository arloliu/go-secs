package redact_test

import (
	"bytes"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/siv"
	"github.com/arloliu/go-secs/tracepack/redact"
)

// restoreInput is the input of one Restore call, kept to check that Restore changes none of it.
type restoreInput struct {
	r      *tracepack.Record
	rr     *tracepack.RecordRedaction
	policy *tracepack.RedactionPolicy
	key    []byte
}

// maskedInput masks payload as record seq of captureA under the baseline policy with encryption
// and returns the masked record, its Marking as a reader reports a record with no defect, the policy and the key.
func maskedInput(t *testing.T, seq uint64, payload []byte) restoreInput {
	t.Helper()

	c := mustCompile(t, baselinePolicy(true))
	masked, entries, err := c.Mask(t.Context(), captureA, dataRecord(seq, payload), false, &testKeys{})
	require.NoError(t, err)
	require.NotEmpty(t, entries)
	pol := c.PackPolicy()

	return restoreInput{r: &masked, rr: &tracepack.RecordRedaction{Entries: entries}, policy: &pol, key: encryptionKey(captureA, encKeyID)}
}

// deepCopy returns a copy of in that shares no memory with it.
func (in restoreInput) deepCopy() restoreInput {
	r := *in.r
	r.Payload = bytes.Clone(in.r.Payload)
	var rr *tracepack.RecordRedaction
	if in.rr != nil {
		rr = &tracepack.RecordRedaction{WhollyMasked: in.rr.WhollyMasked, Defects: slices.Clone(in.rr.Defects)}
		for _, e := range in.rr.Entries {
			c := e
			if e.ItemPath != nil {
				c.ItemPath = new(*e.ItemPath)
			}
			c.MaskedRanges = slices.Clone(e.MaskedRanges)
			c.RawMaskedRanges = slices.Clone(e.RawMaskedRanges)
			c.Digest = slices.Clone(e.Digest)
			c.SIV = slices.Clone(e.SIV)
			rr.Entries = append(rr.Entries, c)
		}
	}
	var pol *tracepack.RedactionPolicy
	if in.policy != nil {
		p := *in.policy
		if in.policy.EncryptionAlgorithm != nil {
			a := *in.policy.EncryptionAlgorithm
			p.EncryptionAlgorithm = &a
		}
		if in.policy.EncryptionKeyID != nil {
			p.EncryptionKeyID = new(*in.policy.EncryptionKeyID)
		}
		pol = &p
	}

	return restoreInput{r: &r, rr: rr, policy: pol, key: bytes.Clone(in.key)}
}

// restore calls Restore on in and requires that it changed no input.
func (in restoreInput) restore(t *testing.T) ([]byte, error) {
	t.Helper()

	before := in.deepCopy()
	got, err := redact.Restore(captureA, in.r, in.rr, in.policy, in.key)
	assert.Equal(t, before, in, "Restore changes no input")

	return got, err
}

// TestRestore restores every masked record of the item and whole-text cases to its exact payload, all or nothing:
// one failing entry of two, an invalid range map, a wrong key, a missing siv,
// and duplicate or partially overlapping encrypted entries restore nothing and return the defect,
// while adjacent encrypted entries restore.
func TestRestore(t *testing.T) {
	t.Parallel()

	t.Run("masked cases", func(t *testing.T) {
		t.Parallel()

		for _, tt := range slices.Concat(itemCases(), wholeTextCases()) {
			if tt.want == nil {
				continue
			}
			in := maskedInput(t, 21, tt.payload)
			got, err := in.restore(t)
			require.NoError(t, err, tt.name)
			assert.Equal(t, tt.payload, got, tt.name)
			assert.NotSame(t, &in.r.Payload[0], &got[0], "a new buffer")
		}
	})

	dataset := frame(13, 6, datasetText([]byte{1, 2, 3}, []byte{4, 5, 6, 7}))
	tests := []struct {
		name   string
		change func(in *restoreInput)
		want   error
	}{
		{"second entry fails to authenticate", func(in *restoreInput) {
			in.r.Payload[in.rr.Entries[1].MaskedRanges[0].Offset] ^= 1
		}, redact.ErrAuthentication},
		{"wrong key", func(in *restoreInput) { in.key = encryptionKey(captureB, encKeyID) }, redact.ErrAuthentication},
		{"flipped siv", func(in *restoreInput) { in.rr.Entries[1].SIV[0] ^= 1 }, redact.ErrAuthentication},
		{"range past payload_len", func(in *restoreInput) {
			in.rr.Entries[1].MaskedRanges[0].Length = uint64(len(in.r.Payload))
		}, tracepack.ErrRedactionDefect},
		{"missing siv", func(in *restoreInput) {
			in.rr.Entries[1].SIV, in.rr.Entries[1].HasSIV = nil, false
		}, tracepack.ErrRedactionDefect},
		{"duplicate entry", func(in *restoreInput) {
			in.rr.Entries = []tracepack.RedactionEntry{in.rr.Entries[0], in.rr.Entries[0]}
		}, tracepack.ErrRedactionDefect},
		{"partial overlap", func(in *restoreInput) {
			in.rr.Entries[1].MaskedRanges[0] = tracepack.MaskedRange{Offset: 38, Length: 6}
		}, tracepack.ErrRedactionDefect},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			in := maskedInput(t, 22, dataset)
			require.Len(t, in.rr.Entries, 2)
			tt.change(&in)
			got, err := in.restore(t)
			require.ErrorIs(t, err, tt.want)
			assert.Nil(t, got, "nothing restored")
		})
	}

	t.Run("adjacent encrypted entries", func(t *testing.T) {
		t.Parallel()

		orig := frame(7, 3, ppbodyText("PP1", []byte("ABCDEFGHIJ")))
		in := maskedInput(t, 23, orig)
		// Split the PPBODY's range into two adjacent entries, each sealed on its own.
		masked := bytes.Clone(orig)
		entries := make([]tracepack.RedactionEntry, 0, 2)
		for _, mr := range []tracepack.MaskedRange{{Offset: 23, Length: 4}, {Offset: 27, Length: 6}} {
			ad := siv.AssociatedData(captureA, 23, mr.Offset)
			iv, ct, err := siv.Seal(in.key, ad[:], orig[mr.Offset:mr.Offset+mr.Length])
			require.NoError(t, err)
			copy(masked[mr.Offset:], ct)
			entries = append(entries, tracepack.RedactionEntry{
				Seq: 23, ItemPath: new("2"), MaskedRanges: []tracepack.MaskedRange{mr}, Domain: "ppbody",
				Digest: make([]byte, 32), SIV: iv[:], HasSIV: true,
			})
		}
		in.r.Payload = masked
		in.rr.Entries = entries
		got, err := in.restore(t)
		require.NoError(t, err)
		assert.Equal(t, orig, got)
	})

	t.Run("policy without both encryption tags", func(t *testing.T) {
		t.Parallel()

		alg := tracepack.EncryptionAlgorithmAESSIVCMAC512
		keyID := encKeyID
		for _, tt := range []struct {
			name      string
			alg       *tracepack.EncryptionAlgorithm
			keyID     *string
			viaHeader bool
		}{
			{name: "neither"}, {name: "algorithm only", alg: &alg}, {name: "key id only", keyID: &keyID},
			{name: "neither, read back", viaHeader: true},
			{name: "algorithm only, read back", alg: &alg, viaHeader: true},
			{name: "key id only, read back", keyID: &keyID, viaHeader: true},
		} {
			in := maskedInput(t, 24, dataset)
			in.policy.EncryptionAlgorithm, in.policy.EncryptionKeyID = tt.alg, tt.keyID
			if tt.viaHeader {
				in.policy = readBack(t, in.policy)
			}
			got, err := in.restore(t)
			require.ErrorIs(t, err, redact.ErrNoEncryption, tt.name)
			require.NotErrorIs(t, err, tracepack.ErrRedactionDefect, tt.name)
			assert.Nil(t, got, tt.name)
		}

		in := maskedInput(t, 24, dataset)
		in.policy = nil
		_, err := in.restore(t)
		require.ErrorIs(t, err, redact.ErrNoEncryption)
	})

	t.Run("annotation", func(t *testing.T) {
		t.Parallel()

		c := mustCompile(t, baselinePolicy(true))
		ann := annotationRecord(t, 25, &tracepack.Annotation{AnnotationKind: tracepack.AnnotationKindUnparsedEntry, Raw: []byte("raw")})
		masked, entries, err := c.Mask(t.Context(), captureA, ann, false, &testKeys{})
		require.NoError(t, err)
		pol := c.PackPolicy()
		in := restoreInput{r: &masked, rr: &tracepack.RecordRedaction{Entries: entries}, policy: &pol, key: encryptionKey(captureA, encKeyID)}
		got, err := in.restore(t)
		require.ErrorIs(t, err, redact.ErrNotRestorable, "an annotation mask keeps no original")
		assert.Nil(t, got)
	})

	t.Run("key size", func(t *testing.T) {
		t.Parallel()

		in := maskedInput(t, 26, dataset)
		in.key = in.key[:32]
		_, err := in.restore(t)
		require.ErrorIs(t, err, redact.ErrKeySize)
	})
}

// readBack returns pol as a pack metadata of format minor 1 holding it reads it back.
func readBack(t *testing.T, pol *tracepack.RedactionPolicy) *tracepack.RedactionPolicy {
	t.Helper()

	var generation uint64
	b, err := (&tracepack.PackMeta{RedactionPolicy: pol, ScopeGeneration: &generation}).MarshalBinary()
	require.NoError(t, err)
	m, err := tracepack.UnmarshalPackMetaMinor(b, 1)
	require.NoError(t, err)
	require.NotNil(t, m.RedactionPolicy)
	assert.Equal(t, pol, m.RedactionPolicy, "the policy reads back as stored")

	return m.RedactionPolicy
}

// TestRestoreUntrustedInput gives Restore Marking values the test builds, not a reader,
// with no defect listed and WhollyMasked false: Restore reruns every Marking check on its own inputs,
// so each of these restores nothing, returns an error wrapping tracepack.ErrRedactionDefect and changes no input.
// A nil Marking, a listed defect and a true WhollyMasked are refused too,
// and an encryption algorithm other than aes-siv-cmac-512 is unsupported, checked before any decryption.
func TestRestoreUntrustedInput(t *testing.T) {
	t.Parallel()

	ppbody := frame(7, 3, ppbodyText("PP1", []byte("SECRET-RECIPE-01")))
	tests := []struct {
		name   string
		change func(in *restoreInput)
	}{
		{"entry naming another seq", func(in *restoreInput) { in.rr.Entries[0].Seq++ }},
		{"record without redacted", func(in *restoreInput) { in.r.Quality &^= tracepack.QualityRedacted }},
		{"range past payload_len", func(in *restoreInput) { in.rr.Entries[0].MaskedRanges[0].Length++ }},
		{"item range inside the frame prefix", func(in *restoreInput) {
			in.rr.Entries[0].MaskedRanges = []tracepack.MaskedRange{{Offset: 13, Length: 26}}
		}},
		{"raw masked ranges", func(in *restoreInput) {
			in.rr.Entries[0].MaskedRanges, in.rr.Entries[0].RawMaskedRanges = nil, []byte{1, 2, 3}
		}},
		{"missing siv", func(in *restoreInput) { in.rr.Entries[0].SIV, in.rr.Entries[0].HasSIV = nil, false }},
		{"empty siv", func(in *restoreInput) { in.rr.Entries[0].SIV = []byte{} }},
		{"15-byte siv", func(in *restoreInput) { in.rr.Entries[0].SIV = in.rr.Entries[0].SIV[:15] }},
		{"overlapping encrypted entries", func(in *restoreInput) {
			e := in.rr.Entries[0]
			e.MaskedRanges = []tracepack.MaskedRange{{Offset: 30, Length: 9}}
			in.rr.Entries = append(in.rr.Entries, e)
		}},
		{"entries out of order", func(in *restoreInput) {
			first := in.rr.Entries[0]
			first.MaskedRanges = []tracepack.MaskedRange{{Offset: 23, Length: 8}}
			second := in.rr.Entries[0]
			second.MaskedRanges = []tracepack.MaskedRange{{Offset: 31, Length: 8}}
			in.rr.Entries = []tracepack.RedactionEntry{second, first}
		}},
		{"no entries", func(in *restoreInput) { in.rr.Entries = nil }},
		{"control record", func(in *restoreInput) { in.r.Kind = tracepack.KindControl }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			in := maskedInput(t, 30, ppbody)
			tt.change(&in)
			got, err := in.restore(t)
			require.ErrorIs(t, err, tracepack.ErrRedactionDefect)
			assert.Nil(t, got)
		})
	}

	t.Run("annotation entry with siv", func(t *testing.T) {
		t.Parallel()

		c := mustCompile(t, baselinePolicy(true))
		ann := annotationRecord(t, 31, &tracepack.Annotation{AnnotationKind: tracepack.AnnotationKindUnparsedEntry, Raw: []byte("raw")})
		masked, entries, err := c.Mask(t.Context(), captureA, ann, false, &testKeys{})
		require.NoError(t, err)
		require.Len(t, entries, 1)
		entries[0].SIV, entries[0].HasSIV = make([]byte, 16), true
		pol := c.PackPolicy()
		in := restoreInput{r: &masked, rr: &tracepack.RecordRedaction{Entries: entries}, policy: &pol, key: encryptionKey(captureA, encKeyID)}
		got, err := in.restore(t)
		require.ErrorIs(t, err, tracepack.ErrRedactionDefect)
		assert.Nil(t, got)
	})

	t.Run("refused Marking", func(t *testing.T) {
		t.Parallel()

		in := maskedInput(t, 32, ppbody)
		in.rr = nil
		_, err := in.restore(t)
		require.ErrorIs(t, err, redact.ErrNotRestorable)

		in = maskedInput(t, 32, ppbody)
		in.rr.Defects = []tracepack.RedactionDefect{{Seq: 32, Entry: 0, Err: tracepack.ErrRedactionDefect}}
		_, err = in.restore(t)
		require.ErrorIs(t, err, tracepack.ErrRedactionDefect)

		in = maskedInput(t, 32, ppbody)
		in.rr.WhollyMasked = true
		_, err = in.restore(t)
		require.ErrorIs(t, err, tracepack.ErrRedactionDefect)
	})

	t.Run("unsupported algorithm", func(t *testing.T) {
		t.Parallel()

		for _, alg := range []tracepack.EncryptionAlgorithm{0, 200} {
			in := maskedInput(t, 33, ppbody)
			in.policy.EncryptionAlgorithm = &alg
			got, err := in.restore(t)
			require.ErrorIs(t, err, redact.ErrUnsupportedAlgorithm, "%d", alg)
			assert.Nil(t, got)

			// The policy decodes, and exports, with the value as stored.
			back := readBack(t, in.policy)
			assert.Equal(t, alg, *back.EncryptionAlgorithm)
			assert.Equal(t, alg.String(), back.EncryptionAlgorithm.String())
		}
	})
}

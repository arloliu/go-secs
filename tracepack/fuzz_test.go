// Fuzz targets for the three typed decoders of package tracepack.
//
// Each target feeds arbitrary bytes to its Unmarshal function and asserts that it never panics
// and that a decode failure returns a nil result.
// Each Unmarshal function validates everything its MarshalBinary requires before returning,
// UnmarshalPackMeta included, since both enforce the "Required when" rules the metadata itself decides,
// so a successful decode re-encodes, and the result decodes back to an equal value,
// except the pack metadata's retired pack_role, which is never written, and its retired tags, which are dropped.
package tracepack_test

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
)

// retiredPackMetaTags are the retired pack metadata tags, which a decode keeps in Unknown and MarshalBinary drops
// (the tracepack format specification §5).
var retiredPackMetaTags = []uint16{0x0001, 0x0014, 0x0030}

// retiredPackRole is the retired pack_role value 5 (the tracepack format specification §9).
const retiredPackRole tracepack.PackRole = 5

// FuzzUnmarshalPackMeta fuzzes UnmarshalPackMeta.
func FuzzUnmarshalPackMeta(f *testing.F) {
	f.Add(mustHexBytes(f, pmMinimal16Hex))
	f.Add(mustHexBytes(f, pmAlways15Hex))
	f.Add(mustHexBytes(f, pmWithUnknownHex))
	f.Add([]byte{})
	f.Add(mustHexBytes(f, pmMinimal16Hex)[:10])
	repeated, err := twoClassifiersMeta().MarshalBinary()
	require.NoError(f, err)
	f.Add(repeated)
	for _, m := range toleratedRedactionMetas() {
		b, err := m.MarshalBinary()
		require.NoError(f, err)
		f.Add(b)
	}

	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := tracepack.UnmarshalPackMeta(b)
		if err != nil {
			require.Nil(t, m)

			return
		}

		enc, err := m.MarshalBinary()
		if m.PackRole == retiredPackRole {
			require.ErrorIs(t, err, tracepack.ErrFieldValue, "a writer never writes the retired pack_role")

			return
		}
		require.NoError(t, err)

		got, err := tracepack.UnmarshalPackMeta(enc)
		require.NoError(t, err)

		// MarshalBinary drops the retired tags a decode keeps in Unknown (the tracepack format specification §5).
		want := *m
		want.Unknown = slices.DeleteFunc(slices.Clone(m.Unknown), func(e tracepack.RawEntry) bool {
			return slices.Contains(retiredPackMetaTags, e.Tag)
		})
		if len(want.Unknown) == 0 {
			want.Unknown = nil
		}
		require.Equal(t, &want, got)
	})
}

// toleratedRedactionMetas returns pack metadata holding each redaction defect a reader tolerates and reports
// (the tracepack semantics specification §8), each of which decodes and re-encodes as stored:
// a masked_ranges value that is not whole elements, of an odd count or holding an element above 2^63 - 1,
// a present siv of 0 or 15 bytes, and a policy holding one encryption tag.
func toleratedRedactionMetas() []*tracepack.PackMeta {
	entry := func(raw []byte, siv []byte, hasSIV bool) tracepack.RedactionEntry {
		e := tracepack.RedactionEntry{Seq: 5, Domain: "d", Digest: make([]byte, 32), RawMaskedRanges: raw, SIV: siv, HasSIV: hasSIV}
		if raw == nil {
			e.MaskedRanges = []tracepack.MaskedRange{{Offset: 16, Length: 4}}
		}

		return e
	}
	odd := []byte{1, 0, 0, 0, 0, 0, 0, 0}
	over := []byte{0, 0, 0, 0, 0, 0, 0, 0x80, 1, 0, 0, 0, 0, 0, 0, 0}
	entries := []tracepack.RedactionEntry{
		entry(make([]byte, 7), nil, false), entry(odd, nil, false), entry(over, nil, false),
		entry(nil, []byte{}, true), entry(nil, make([]byte, 15), true),
	}

	var metas []*tracepack.PackMeta
	for _, e := range entries {
		m := basePackMeta()
		m.Redaction = []tracepack.RedactionEntry{e}
		metas = append(metas, m)
	}
	algorithm := tracepack.EncryptionAlgorithmAESSIVCMAC512
	m := basePackMeta()
	m.RedactionPolicy = &tracepack.RedactionPolicy{
		PolicyID: "p", PolicyVersion: 1, KeyID: "k", DigestAlgorithm: tracepack.DigestAlgorithmHMACSHA256, EncryptionAlgorithm: &algorithm,
	}

	return append(metas, m)
}

// FuzzUnmarshalTransportEvent fuzzes UnmarshalTransportEvent.
func FuzzUnmarshalTransportEvent(f *testing.F) {
	f.Add(mustHexBytes(f, teMinHex))
	f.Add(mustHexBytes(f, teFullHex))
	f.Add(mustHexBytes(f, teUnknownHex))
	f.Add(mustHexBytes(f, teBadPrimarySystemBytesHex))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, b []byte) {
		te, err := tracepack.UnmarshalTransportEvent(b)
		if err != nil {
			require.Nil(t, te)

			return
		}

		enc, err := te.MarshalBinary()
		require.NoError(t, err)

		got, err := tracepack.UnmarshalTransportEvent(enc)
		require.NoError(t, err)
		require.Equal(t, te, got)
	})
}

// FuzzUnmarshalAnnotation fuzzes UnmarshalAnnotation.
func FuzzUnmarshalAnnotation(f *testing.F) {
	f.Add(mustHexBytes(f, annTextHex))
	f.Add(mustHexBytes(f, annRawHex))
	f.Add(mustHexBytes(f, annNeitherHex))
	f.Add(mustHexBytes(f, annBothHex))
	f.Add(mustHexBytes(f, annUnknownHex))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, b []byte) {
		a, err := tracepack.UnmarshalAnnotation(b)
		if err != nil {
			require.Nil(t, a)

			return
		}

		enc, err := a.MarshalBinary()
		require.NoError(t, err)

		got, err := tracepack.UnmarshalAnnotation(enc)
		require.NoError(t, err)
		require.Equal(t, a, got)
	})
}

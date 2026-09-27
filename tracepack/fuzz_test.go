// Fuzz targets for the three typed decoders of package tracepack.
//
// Each target feeds arbitrary bytes to its Unmarshal function and asserts that it never panics
// and that a decode failure returns a nil result.
// Each Unmarshal function validates everything its MarshalBinary requires before returning,
// UnmarshalPackMeta included, since both enforce the "Required when" rules the metadata itself decides,
// so a successful decode always re-encodes, and the result decodes back to an equal value.
package tracepack_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
)

// FuzzUnmarshalPackMeta fuzzes UnmarshalPackMeta.
func FuzzUnmarshalPackMeta(f *testing.F) {
	f.Add(mustHexBytes(f, pmMinimal17Hex))
	f.Add(mustHexBytes(f, pmAlways16Hex))
	f.Add(mustHexBytes(f, pmWithUnknownHex))
	f.Add([]byte{})
	f.Add(mustHexBytes(f, pmMinimal17Hex)[:10])

	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := tracepack.UnmarshalPackMeta(b)
		if err != nil {
			require.Nil(t, m)

			return
		}

		enc, err := m.MarshalBinary()
		require.NoError(t, err)

		got, err := tracepack.UnmarshalPackMeta(enc)
		require.NoError(t, err)
		require.Equal(t, m, got)
	})
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

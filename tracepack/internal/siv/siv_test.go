package siv_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/arloliu/go-secs/tracepack/internal/siv"
	"github.com/stretchr/testify/require"
)

// testCapture is a capture id whose bytes differ under every byte-order swap the tests try.
var testCapture = [16]byte{
	0xf4, 0x7a, 0xc1, 0x0b, 0x58, 0xcc, 0x43, 0x72,
	0xa5, 0x67, 0x0e, 0x02, 0xb2, 0xc3, 0xd4, 0x79,
}

const (
	testSeq    = uint64(0x0102030405060708)
	testOffset = uint64(30)
)

// wycheproofFile is the AES-SIV-CMAC vector file of Wycheproof, vendored unchanged; see testdata/NOTICE.
type wycheproofFile struct {
	Algorithm  string            `json:"algorithm"`
	TestGroups []wycheproofGroup `json:"testGroups"`
}

type wycheproofGroup struct {
	KeySize int              `json:"keySize"`
	Tests   []wycheproofCase `json:"tests"`
}

// wycheproofCase is one case; ct is the synthetic IV followed by the ciphertext.
type wycheproofCase struct {
	TcID    int    `json:"tcId"`
	Comment string `json:"comment"`
	Key     string `json:"key"`
	AAD     string `json:"aad"`
	Msg     string `json:"msg"`
	CT      string `json:"ct"`
	Result  string `json:"result"`
}

func TestAssociatedData(t *testing.T) {
	t.Parallel()

	// Computed outside Go: the label's ASCII bytes, the capture id as stored,
	// then the seq and the first offset as little-endian u64 values.
	want := mustHex(t, "74726163657061636b2d6f726967696e616c2d7631"+
		"f47ac10b58cc4372a5670e02b2c3d479"+
		"0807060504030201"+
		"1e00000000000000")

	ad := siv.AssociatedData(testCapture, testSeq, testOffset)
	require.Len(t, want, siv.ADSize)
	require.Equal(t, 53, siv.ADSize)
	require.Equal(t, want, ad[:])
	require.Equal(t, mustHex(t, "74726163657061636b2d6f726967696e616c2d7631"), ad[:21])
}

func TestWycheproof512(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("testdata", "aes_siv_cmac_test.json"))
	require.NoError(t, err)

	var file wycheproofFile
	require.NoError(t, json.Unmarshal(raw, &file))
	require.Equal(t, "AES-SIV-CMAC", file.Algorithm)

	groups, valid, invalid := 0, 0, 0
	for _, group := range file.TestGroups {
		if group.KeySize != 8*siv.KeySize {
			continue
		}
		groups++

		for _, tc := range group.Tests {
			key, ad, msg, out := mustHex(t, tc.Key), mustHex(t, tc.AAD), mustHex(t, tc.Msg), mustHex(t, tc.CT)
			require.Len(t, key, siv.KeySize, "tcId %d", tc.TcID)
			require.GreaterOrEqual(t, len(out), siv.SIVSize, "tcId %d", tc.TcID)

			sealedSIV, sealedCT, err := siv.Seal(key, ad, msg)
			require.NoError(t, err, "tcId %d", tc.TcID)

			opened, openErr := siv.Open(key, ad, out[:siv.SIVSize], out[siv.SIVSize:])

			switch tc.Result {
			case "valid":
				valid++
				require.Equal(t, out, append(sealedSIV[:], sealedCT...), "tcId %d %s", tc.TcID, tc.Comment)
				require.NoError(t, openErr, "tcId %d %s", tc.TcID, tc.Comment)
				require.True(t, bytes.Equal(msg, opened), "tcId %d %s", tc.TcID, tc.Comment)
			case "invalid":
				invalid++
				require.NotEqual(t, out, append(sealedSIV[:], sealedCT...), "tcId %d %s", tc.TcID, tc.Comment)
				require.ErrorIs(t, openErr, siv.ErrAuthentication, "tcId %d %s", tc.TcID, tc.Comment)
				require.Nil(t, opened, "tcId %d %s", tc.TcID, tc.Comment)
			default:
				require.Failf(t, "unexpected result", "tcId %d: %q", tc.TcID, tc.Result)
			}
		}
	}

	// Pinned so that a changed file or a broken filter fails loudly.
	require.Equal(t, 1, groups)
	require.Equal(t, 39, valid)
	require.Equal(t, 108, invalid)
}

func TestOpenRejectsADVariants(t *testing.T) {
	t.Parallel()

	key := testKey(0x10)
	plaintext := []byte("PPBODY original bytes of one entry")
	ad := siv.AssociatedData(testCapture, testSeq, testOffset)

	sealedSIV, ct, err := siv.Seal(key, ad[:], plaintext)
	require.NoError(t, err)

	opened, err := siv.Open(key, ad[:], sealedSIV[:], ct)
	require.NoError(t, err)
	require.Equal(t, plaintext, opened)

	otherCapture := testCapture
	otherCapture[15] ^= 0x01

	withNUL := append(ad[:], 0x00)

	tests := []struct {
		name string
		ad   []byte
	}{
		{name: "label plus NUL", ad: withNulLabel(t)},
		{name: "trailing NUL", ad: withNUL},
		{name: "mixed-endian capture id", ad: adOf(mixedEndian(testCapture), testSeq, testOffset)},
		{name: "reversed capture id", ad: adOf(reversed(testCapture), testSeq, testOffset)},
		{name: "other capture", ad: adOf(otherCapture, testSeq, testOffset)},
		{name: "other seq", ad: adOf(testCapture, testSeq+1, testOffset)},
		{name: "big-endian seq", ad: bigEndianSeq(t)},
		{name: "other first offset", ad: adOf(testCapture, testSeq, testOffset+1)},
		{name: "empty", ad: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.NotEqual(t, ad[:], tt.ad)

			got, err := siv.Open(key, tt.ad, sealedSIV[:], ct)
			require.ErrorIs(t, err, siv.ErrAuthentication)
			require.Nil(t, got)
		})
	}

	t.Run("flipped siv byte", func(t *testing.T) {
		t.Parallel()

		for i := range siv.SIVSize {
			flipped := sealedSIV
			flipped[i] ^= 0x01

			got, err := siv.Open(key, ad[:], flipped[:], ct)
			require.ErrorIs(t, err, siv.ErrAuthentication, "byte %d", i)
			require.Nil(t, got)
		}
	})

	t.Run("flipped ciphertext byte", func(t *testing.T) {
		t.Parallel()

		for i := range ct {
			flipped := bytes.Clone(ct)
			flipped[i] ^= 0x80

			got, err := siv.Open(key, ad[:], sealedSIV[:], flipped)
			require.ErrorIs(t, err, siv.ErrAuthentication, "byte %d", i)
			require.Nil(t, got)
		}
	})

	t.Run("other key", func(t *testing.T) {
		t.Parallel()

		got, err := siv.Open(testKey(0x11), ad[:], sealedSIV[:], ct)
		require.ErrorIs(t, err, siv.ErrAuthentication)
		require.Nil(t, got)
	})

	t.Run("ill-sized siv", func(t *testing.T) {
		t.Parallel()

		for _, n := range []int{0, 15, 17} {
			bad := make([]byte, n)
			copy(bad, sealedSIV[:])

			got, err := siv.Open(key, ad[:], bad, ct)
			require.ErrorIs(t, err, siv.ErrSIVSize, "length %d", n)
			require.Nil(t, got)
		}
	})
}

func TestSealDeterministic(t *testing.T) {
	t.Parallel()

	key := testKey(0x20)
	plaintext := []byte("equal originals in two entries of one record")

	ad1 := siv.AssociatedData(testCapture, testSeq, testOffset)
	sivA, ctA, err := siv.Seal(key, ad1[:], plaintext)
	require.NoError(t, err)

	sivB, ctB, err := siv.Seal(key, ad1[:], bytes.Clone(plaintext))
	require.NoError(t, err)
	require.Equal(t, sivA, sivB)
	require.Equal(t, ctA, ctB)
	require.Len(t, ctA, len(plaintext))

	ad2 := siv.AssociatedData(testCapture, testSeq, testOffset+uint64(len(plaintext)))
	sivC, ctC, err := siv.Seal(key, ad2[:], plaintext)
	require.NoError(t, err)
	require.NotEqual(t, sivA, sivC)
	require.NotEqual(t, ctA, ctC)

	ad3 := siv.AssociatedData(testCapture, testSeq+1, testOffset)
	sivD, ctD, err := siv.Seal(key, ad3[:], plaintext)
	require.NoError(t, err)
	require.NotEqual(t, sivA, sivD)
	require.NotEqual(t, ctA, ctD)
}

func TestSealDoesNotAliasInputs(t *testing.T) {
	t.Parallel()

	key := testKey(0x30)
	plaintext := []byte("target bytes")
	ad := siv.AssociatedData(testCapture, testSeq, testOffset)

	sealedSIV, ct, err := siv.Seal(key, ad[:], plaintext)
	require.NoError(t, err)
	require.Equal(t, []byte("target bytes"), plaintext)

	sivCopy, ctCopy := sealedSIV, bytes.Clone(ct)
	opened, err := siv.Open(key, ad[:], sealedSIV[:], ct)
	require.NoError(t, err)
	require.Equal(t, plaintext, opened)
	require.Equal(t, sivCopy, sealedSIV)
	require.Equal(t, ctCopy, ct)

	// The plaintext Open returns is the caller's to overwrite.
	opened[0] ^= 0xff
	require.Equal(t, ctCopy, ct)
}

func TestKeyLength(t *testing.T) {
	t.Parallel()

	ad := siv.AssociatedData(testCapture, testSeq, testOffset)

	for _, n := range []int{0, 32, 48, 63, 65} {
		key := make([]byte, n)

		_, ct, err := siv.Seal(key, ad[:], []byte("x"))
		require.ErrorIs(t, err, siv.ErrKeySize, "length %d", n)
		require.Nil(t, ct)

		got, err := siv.Open(key, ad[:], make([]byte, siv.SIVSize), []byte("x"))
		require.ErrorIs(t, err, siv.ErrKeySize, "length %d", n)
		require.Nil(t, got)
	}
}

func FuzzSIVOpen(f *testing.F) {
	ad := siv.AssociatedData(testCapture, testSeq, testOffset)
	key := testKey(0x40)
	sealedSIV, ct, err := siv.Seal(key, ad[:], []byte("seed plaintext"))
	require.NoError(f, err)

	f.Add(key, ad[:], sealedSIV[:], ct, []byte("seed plaintext"))
	f.Add(key, ad[:], sealedSIV[:], []byte{}, []byte{})
	f.Add(key[:32], []byte{}, []byte{}, []byte{}, []byte("p"))
	f.Add(key, []byte{}, make([]byte, 15), []byte("c"), bytes.Repeat([]byte{0}, 64))

	f.Fuzz(func(t *testing.T, key, ad, sivBytes, ct, plaintext []byte) {
		got, err := siv.Open(key, ad, sivBytes, ct)
		if err != nil {
			require.Nil(t, got)
		} else {
			require.Len(t, got, len(ct))
		}

		sealedSIV, sealedCT, err := siv.Seal(key, ad, plaintext)
		if len(key) != siv.KeySize {
			require.ErrorIs(t, err, siv.ErrKeySize)

			return
		}
		require.NoError(t, err)
		require.Len(t, sealedCT, len(plaintext))

		opened, err := siv.Open(key, ad, sealedSIV[:], sealedCT)
		require.NoError(t, err)
		require.True(t, bytes.Equal(plaintext, opened))
	})
}

func mustHex(t testing.TB, s string) []byte {
	t.Helper()

	b, err := hex.DecodeString(s)
	require.NoError(t, err)

	return b
}

func testKey(seed byte) []byte {
	key := make([]byte, siv.KeySize)
	for i := range key {
		key[i] = seed + byte(i)
	}

	return key
}

func adOf(capture [16]byte, seq, firstOffset uint64) []byte {
	ad := siv.AssociatedData(capture, seq, firstOffset)

	return ad[:]
}

// withNulLabel builds the associated data with a NUL after the label, as a C string would carry it.
func withNulLabel(t *testing.T) []byte {
	t.Helper()

	ad := adOf(testCapture, testSeq, testOffset)
	out := append([]byte{}, ad[:21]...)
	out = append(out, 0x00)

	return append(out, ad[21:]...)
}

// bigEndianSeq builds the associated data with the seq in big-endian order.
func bigEndianSeq(t *testing.T) []byte {
	t.Helper()

	ad := adOf(testCapture, testSeq, testOffset)
	seq := ad[37:45]
	for i, j := 0, len(seq)-1; i < j; i, j = i+1, j-1 {
		seq[i], seq[j] = seq[j], seq[i]
	}

	return ad
}

// mixedEndian swaps the first three fields of a UUID, the byte order of a Microsoft GUID.
func mixedEndian(u [16]byte) [16]byte {
	return [16]byte{
		u[3], u[2], u[1], u[0],
		u[5], u[4],
		u[7], u[6],
		u[8], u[9], u[10], u[11], u[12], u[13], u[14], u[15],
	}
}

func reversed(u [16]byte) [16]byte {
	var r [16]byte
	for i := range u {
		r[15-i] = u[i]
	}

	return r
}

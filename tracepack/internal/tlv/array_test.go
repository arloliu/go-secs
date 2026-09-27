package tlv

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Expected bytes are the hex of python3 struct.pack('<3I', ...) and struct.pack('<2Q', ...).

func TestU32ArrayGolden(t *testing.T) {
	t.Parallel()

	const want = "030000000000000007000000"
	assert.Equal(t, want, hex.EncodeToString(AppendU32Array(nil, []uint32{3, 0, 7})))

	got, err := DecodeU32Array(mustHex(t, want))
	require.NoError(t, err)
	assert.Equal(t, []uint32{3, 0, 7}, got)
}

func TestU64ArrayGolden(t *testing.T) {
	t.Parallel()

	const want = "0100000000000000ffffffffffffff7f"
	assert.Equal(t, want, hex.EncodeToString(AppendU64Array(nil, []uint64{1, MaxU64})))

	got, err := DecodeU64Array(mustHex(t, want))
	require.NoError(t, err)
	assert.Equal(t, []uint64{1, MaxU64}, got)
}

func TestArraysEmpty(t *testing.T) {
	t.Parallel()

	assert.Empty(t, AppendU32Array(nil, nil))
	assert.Empty(t, AppendU64Array(nil, nil))

	u32, err := DecodeU32Array(nil)
	require.NoError(t, err)
	assert.Empty(t, u32)

	u64, err := DecodeU64Array([]byte{})
	require.NoError(t, err)
	assert.Empty(t, u64)
}

func TestArrayAppends(t *testing.T) {
	t.Parallel()

	got := AppendU32Array([]byte{0xEE}, []uint32{1})
	assert.Equal(t, "ee01000000", hex.EncodeToString(got))
}

func TestArrayErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		call func() error
		want error
	}{
		{"u32 array of 5 bytes", func() error { _, err := DecodeU32Array(make([]byte, 5)); return err }, ErrLength},
		{"u32 array of 3 bytes", func() error { _, err := DecodeU32Array(make([]byte, 3)); return err }, ErrLength},
		{"u64 array of 12 bytes", func() error { _, err := DecodeU64Array(make([]byte, 12)); return err }, ErrLength},
		{"u64 array element above the limit", func() error {
			_, err := DecodeU64Array(mustHex(t, "01000000000000000000000000000080"))
			return err
		}, ErrLimit},
		{"seq range of 15 bytes", func() error { _, err := DecodeSeqRange(make([]byte, 15)); return err }, ErrLength},
		{"seq range of 17 bytes", func() error { _, err := DecodeSeqRange(make([]byte, 17)); return err }, ErrLength},
		{"seq range first above the limit", func() error {
			_, err := DecodeSeqRange(mustHex(t, "00000000000000800900000000000000"))
			return err
		}, ErrLimit},
		{"seq range last above the limit", func() error {
			_, err := DecodeSeqRange(mustHex(t, "05000000000000000000000000000080"))
			return err
		}, ErrLimit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.ErrorIs(t, tt.call(), tt.want)
		})
	}
}

func TestSeqRangeGolden(t *testing.T) {
	t.Parallel()

	const want = "05000000000000000900000000000000"
	assert.Equal(t, want, hex.EncodeToString(AppendSeqRange(nil, SeqRange{First: 5, Last: 9})))

	got, err := DecodeSeqRange(mustHex(t, want))
	require.NoError(t, err)
	assert.Equal(t, SeqRange{First: 5, Last: 9}, got)
}

// The order of first and last is a footer-validation rule, not an encoding rule,
// so decoding keeps a reversed range for the caller to judge.
func TestSeqRangeReversedDecodes(t *testing.T) {
	t.Parallel()

	got, err := DecodeSeqRange(AppendSeqRange(nil, SeqRange{First: 9, Last: 5}))
	require.NoError(t, err)
	assert.Equal(t, SeqRange{First: 9, Last: 5}, got)
}

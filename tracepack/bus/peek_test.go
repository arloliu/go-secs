package bus

import (
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// A record header in an array of RecordHeaderLen bytes: the constant is untyped and an array length.
var _ [RecordHeaderLen]byte

func TestRecordHeaderLen(t *testing.T) {
	t.Parallel()

	assert.Equal(t, format.RecordHeaderLen, RecordHeaderLen)
}

func TestPeekHeader(t *testing.T) {
	t.Parallel()

	data := tracepack.Record{
		Seq: 42, TSUTCNs: 1_700_000_000_000_000_001, MonoNs: 77, MonoPresent: true, Epoch: 3,
		Kind: tracepack.KindData, Dir: tracepack.DirHostToEquipment, Fidelity: tracepack.FidelityWireExact,
		Quality: tracepack.QualityOrderingUncertain, Payload: busFrame(1, 3, []byte{1, 2}),
	}

	t.Run("an encoded body", func(t *testing.T) {
		t.Parallel()

		body := busEncode(t, &data)
		got, ok := PeekHeader(body)
		require.True(t, ok)
		want := data
		want.Payload = nil
		assert.Equal(t, want, got)
	})

	t.Run("fields as stored", func(t *testing.T) {
		t.Parallel()

		// Reserved quality bits, decode_status, trailing_bytes, field_validity and every record_flags bit set.
		body := busRawBody(42, 0xFF5F, data.Payload)
		got, ok := PeekHeader(body)
		require.True(t, ok)
		assert.Equal(t, tracepack.Record{
			Seq: 42, TSUTCNs: 1_700_000_000_000_000_123, MonoNs: 555, MonoPresent: true, Epoch: 9,
			Kind: tracepack.KindData, Dir: tracepack.DirEquipmentToHost, Fidelity: tracepack.FidelityWireExact,
			DecodeStatus: 5, TrailingBytes: 7, Quality: 0xFF5F, FieldValidity: 0xFF,
		}, got)
	})

	t.Run("no check beyond the length", func(t *testing.T) {
		t.Parallel()

		// seq 2^64-1, a payload_len the body does not hold, and kind 0, each refused by CheckMessage.
		body := busRawBody(^uint64(0), 0, nil)
		body[29] = 0xFF
		body[38] = 0
		got, ok := PeekHeader(body)
		require.True(t, ok)
		assert.Equal(t, ^uint64(0), got.Seq)
		assert.Equal(t, tracepack.Kind(0), got.Kind)
		_, err := CheckMessage(busEnvelope(t, 42), body)
		require.ErrorIs(t, err, ErrMalformed)
	})

	t.Run("lengths", func(t *testing.T) {
		t.Parallel()

		body := busEncode(t, &data)
		for _, n := range []int{0, 1, RecordHeaderLen - 1} {
			got, ok := PeekHeader(body[:n])
			assert.False(t, ok, "%d bytes", n)
			assert.Equal(t, tracepack.Record{}, got)
		}
		for _, n := range []int{RecordHeaderLen, len(body)} {
			got, ok := PeekHeader(body[:n])
			assert.True(t, ok, "%d bytes", n)
			assert.Equal(t, data.TSUTCNs, got.TSUTCNs)
			assert.Nil(t, got.Payload)
		}
	})
}

// TestPeekHeaderAllocs requires PeekHeader to allocate nothing.
func TestPeekHeaderAllocs(t *testing.T) {
	// Not parallel: testing.AllocsPerRun counts the allocations of every goroutine.
	body := busRawBody(42, 0, busFrame(1, 3, []byte{1, 2}))
	allocs := testing.AllocsPerRun(100, func() {
		if _, ok := PeekHeader(body); !ok {
			t.Fatal("peek failed")
		}
	})
	assert.Zero(t, allocs)
}

func TestParsePayloadDigest(t *testing.T) {
	t.Parallel()

	for _, payload := range [][]byte{nil, {}, []byte("payload"), make([]byte, 1<<16)} {
		got, err := ParsePayloadDigest(PayloadDigest(payload))
		require.NoError(t, err)
		assert.Equal(t, sha256.Sum256(payload), got)
	}

	valid := PayloadDigest([]byte("payload"))
	for _, s := range []string{
		"", "SHA-256=", valid[len("SHA-256="):], "sha-256=" + valid[len("SHA-256="):], "SHA-512=" + valid[len("SHA-256="):],
		valid[:len(valid)-1], valid + "=", valid + "\n", "SHA-256=" + "\n" + valid[len("SHA-256="):],
	} {
		got, err := ParsePayloadDigest(s)
		require.ErrorIs(t, err, ErrMalformed, "%q", s)
		assert.Zero(t, got)
	}
}

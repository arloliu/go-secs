package redact_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/redact"
)

// TestDigestAcrossCaptures masks one PPBODY in two captures, at two seqs and at two offsets:
// under one digest key and domain the digests are equal, while the ciphertexts and siv differ by capture, seq and offset.
// The digest input of a resolved path is the original encoded item, header included,
// so equal content in two item formats gives different digests.
func TestDigestAcrossCaptures(t *testing.T) {
	t.Parallel()

	c := mustCompile(t, baselinePolicy(true))
	body := []byte("SAME-PROCESS-PROGRAM")
	mask := func(capture tracepack.UUID, seq uint64, ppid string) (tracepack.Record, tracepack.RedactionEntry) {
		t.Helper()

		r := dataRecord(seq, frame(7, 3, ppbodyText(ppid, body)))
		masked, entries, err := c.Mask(t.Context(), capture, r, false, &testKeys{})
		require.NoError(t, err)
		require.Len(t, entries, 1)

		return masked, entries[0]
	}
	ciphertext := func(r tracepack.Record, e tracepack.RedactionEntry) []byte {
		mr := e.MaskedRanges[0]
		return r.Payload[mr.Offset : mr.Offset+mr.Length]
	}

	baseRec, base := mask(captureA, 10, "PP1")
	for _, other := range []struct {
		name    string
		capture tracepack.UUID
		seq     uint64
		ppid    string
	}{
		{"another capture", captureB, 10, "PP1"},
		{"another seq", captureA, 11, "PP1"},
		{"another offset", captureA, 10, "PP-LONGER"},
	} {
		rec, e := mask(other.capture, other.seq, other.ppid)
		assert.Equal(t, base.Digest, e.Digest, other.name)
		assert.NotEqual(t, base.SIV, e.SIV, other.name)
		assert.NotEqual(t, ciphertext(baseRec, base), ciphertext(rec, e), other.name)
	}

	// The same process program in S7F6, sharing the domain.
	r := dataRecord(12, frame(7, 6, ppbodyText("PP1", body)))
	_, entries, err := c.Mask(t.Context(), captureB, r, false, &testKeys{})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, base.Digest, entries[0].Digest)

	// The same bytes as a U1 array: another encoded item.
	u1 := dataRecord(13, frame(7, 3, list(ascii("PP1"), item(0o51, body...))))
	_, entries, err = c.Mask(t.Context(), captureA, u1, false, &testKeys{})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, base.MaskedRanges, entries[0].MaskedRanges)
	assert.NotEqual(t, base.Digest, entries[0].Digest)
}

// TestDigest computes HMAC-SHA-256 over the domain's length as a little-endian u16, the domain and the original bytes,
// and refuses a domain the specification does not allow.
func TestDigest(t *testing.T) {
	t.Parallel()

	key, err := (&testKeys{}).DigestKey(t.Context(), digestKeyID)
	require.NoError(t, err)
	got, err := redact.Digest(key, "ppbody", []byte{1, 2, 3})
	require.NoError(t, err)
	assert.Equal(t, digestOf("ppbody", []byte{1, 2, 3}), got)
	assert.Len(t, got, redact.DigestSize)

	// The length prefix separates the domain from the original bytes.
	other, err := redact.Digest(key, "ppbod", []byte{'y', 1, 2, 3})
	require.NoError(t, err)
	assert.NotEqual(t, got, other)

	_, err = redact.Digest(key, "", nil)
	require.ErrorIs(t, err, redact.ErrDomain)
}

package tracepack_test

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"io"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/bus"
	"github.com/arloliu/go-secs/tracepack/classify"
	"github.com/arloliu/go-secs/tracepack/redact"
)

// consumerCapture is the capture the consumer pipeline tests write.
var consumerCapture = tracepack.UUID{0xC0, 0x45, 0x01}

// consumerKeys derives the pipeline's keys in the test:
// the digest key from one label, the encryption key from another and the capture.
type consumerKeys struct{}

func (consumerKeys) DigestKey(context.Context, string) ([]byte, error) {
	k := sha256.Sum256([]byte("consumer pipeline digest key"))
	return k[:], nil
}

func (consumerKeys) EncryptionKey(_ context.Context, capture tracepack.UUID, _ string) ([]byte, error) {
	k := sha512.Sum512(append([]byte("consumer pipeline encryption key"), capture[:]...))
	return k[:], nil
}

// consumerPolicy is the capture's policy: the PPBODY of S7F3, under encryption.
func consumerPolicy(t *testing.T) *redact.Compiled {
	t.Helper()

	c, err := redact.Compile(redact.Policy{
		ID: "recipes", Version: 1, DigestKeyID: "digest-1", Encryption: &redact.Encryption{KeyID: "enc-1"},
		ItemRules: []redact.ItemRule{{Stream: 7, Function: 3, Path: "2", Domain: "ppbody"}},
	})
	require.NoError(t, err)

	return c
}

// secsItem returns a SECS-II item of format code code holding body, with a 1-byte length.
func secsItem(code byte, body ...byte) []byte {
	return append([]byte{code<<2 | 1, byte(len(body))}, body...)
}

// secsList returns a SECS-II list of elements.
func secsList(elements ...[]byte) []byte {
	return slices.Concat(append([][]byte{{1, byte(len(elements))}}, elements...)...)
}

// busFrame returns the HSMS data frame of stream and function whose message text is text.
func busFrame(stream, function byte, text []byte) []byte {
	f := make([]byte, 14, 14+len(text))
	binary.BigEndian.PutUint32(f, uint32(10+len(text)))
	f[5], f[6], f[7], f[13] = 1, 0x80|stream, function, 1

	return append(f, text...)
}

// ppbody returns the S7F3 frame of process program ppid whose PPBODY is body, followed by trailing bytes.
func ppbody(ppid string, body []byte, trailing ...byte) []byte {
	text := append(secsList(secsItem(0o20, []byte(ppid)...), secsItem(0o10, body...)), trailing...)

	return busFrame(7, 3, text)
}

// busMessage returns the bus message of layout 1 a producer publishes for a data record of seq over frame,
// its derived fields zero.
func busMessage(t *testing.T, seq uint64, frame []byte) (bus.Envelope, []byte) {
	t.Helper()

	r := tracepack.Record{
		Seq: seq, TSUTCNs: hourStart + int64(seq), Epoch: 1, Kind: tracepack.KindData,
		Dir: tracepack.DirEquipmentToHost, Fidelity: tracepack.FidelityWireExact, Payload: frame,
	}
	br := bus.NewRecord(&r)
	body, err := bus.EncodeBody(&br)
	require.NoError(t, err)
	id, err := bus.MsgID(consumerCapture, seq)
	require.NoError(t, err)

	return bus.Envelope{Layout: "1", MsgID: id}, body
}

// busMsg is one bus message as a consumer receives it: its headers and its body.
type busMsg struct {
	env  bus.Envelope
	body []byte
}

// busBatch returns the bus messages of frames, the first at seq first.
func busBatch(t *testing.T, first uint64, frames ...[]byte) []busMsg {
	t.Helper()

	msgs := make([]busMsg, len(frames))
	for i, f := range frames {
		env, body := busMessage(t, first+uint64(i), f)
		msgs[i] = busMsg{env: env, body: body}
	}

	return msgs
}

// consumeBatch is the consumer of a durable bus for one flush batch (the tracepack storage specification §4):
// it checks each message, derives the consumer's values with the capture's classifier,
// and masks every record under the policy;
// only then it writes the segment to out, its entries ordered by seq then first masked offset,
// its blocks closing past threshold bytes.
// A failure before the Writer exists writes nothing.
func consumeBatch(ctx context.Context, out io.Writer, msgs []busMsg, c *redact.Compiled, cls tracepack.Classifier, threshold int) error {
	recs := make([]tracepack.Record, 0, len(msgs))
	var entries []tracepack.RedactionEntry
	for _, msg := range msgs {
		br, err := bus.CheckMessage(msg.env, msg.body)
		if err != nil {
			return err
		}
		if err := bus.DeriveRecord(&br.Record, cls); err != nil {
			return err
		}
		masked, es, err := c.Mask(ctx, br.CaptureID, &br.Record, br.Record.Quality.HasAny(tracepack.QualityUnobserved), consumerKeys{})
		if err != nil {
			return err
		}
		recs, entries = append(recs, masked), append(entries, es...)
	}
	slices.SortStableFunc(entries, func(a, b tracepack.RedactionEntry) int {
		return cmp.Or(cmp.Compare(a.Seq, b.Seq), cmp.Compare(a.MaskedRanges[0].Offset, b.MaskedRanges[0].Offset))
	})

	m := hourMeta()
	m.SeqStart = recs[0].Seq
	policy := c.PackPolicy()
	m.RedactionPolicy, m.Redaction, m.Classifiers = &policy, entries, []string{cls.Name()}
	w, err := tracepack.NewWriter(out, tracepack.WriterOptions{
		Meta: m, CaptureID: consumerCapture, BlockThreshold: threshold,
		Facts: tracepack.PackFacts{AnyClassified: true, AnyRedacted: len(entries) > 0, RedactionPolicyApplied: true},
	})
	if err != nil {
		return err
	}
	for i := range recs {
		if err := w.Append(&recs[i]); err != nil {
			return err
		}
	}
	_, err = w.Close()

	return err
}

// readBack is one record of a pack read back, with its Marking and the index of the block holding it.
type readBack struct {
	rec       tracepack.Record
	redaction *tracepack.RecordRedaction
	block     int
}

// readPack opens file and returns its header and every record, payloads included.
func readPack(t *testing.T, file []byte) (*tracepack.PackHeader, []readBack) {
	t.Helper()

	r, err := tracepack.Open(t.Context(), bytes.NewReader(file), int64(len(file)), tracepack.ReaderOptions{})
	require.NoError(t, err)
	var out []readBack
	_, err = r.Iterate(t.Context(), tracepack.Query{Payloads: true}, func(it *tracepack.Item) error {
		rec := it.Record
		rec.Payload = bytes.Clone(rec.Payload)
		out = append(out, readBack{rec: rec, redaction: it.Redaction, block: it.Block})

		return nil
	})
	require.NoError(t, err)

	return r.Header(), out
}

// restore returns the original payload of the masked record rb of a pack whose header is h.
func restore(t *testing.T, h *tracepack.PackHeader, rb readBack) []byte {
	t.Helper()

	key, err := consumerKeys{}.EncryptionKey(t.Context(), h.CaptureID, *h.Meta.RedactionPolicy.EncryptionKeyID)
	require.NoError(t, err)
	orig, err := redact.Restore(h.CaptureID, &rb.rec, rb.redaction, h.Meta.RedactionPolicy, key)
	require.NoError(t, err)

	return orig
}

// A consumer classifies each record from its payload as received, masks the whole batch, then writes the segment
// (the tracepack storage specification §4, Masking): classification survives a whole-text mask,
// a match in the last of several blocks is restored, a batch without a match still carries the policy,
// and a screening failure leaves no output.
func TestConsumerPipeline(t *testing.T) {
	t.Parallel()

	c, cls := consumerPolicy(t), classify.NewNamed("consumer-classifier/1", 0)
	s1f1 := busFrame(1, 1, nil)

	t.Run("a whole-text mask keeps the original classification", func(t *testing.T) {
		t.Parallel()

		frame := ppbody("PP1", []byte("SECRET-RECIPE"), 0xFF, 0xFF, 0xFF)
		var out bytes.Buffer
		require.NoError(t, consumeBatch(t.Context(), &out, busBatch(t, 0, s1f1, frame), c, cls, 0))
		h, recs := readPack(t, out.Bytes())
		require.Len(t, recs, 2)
		rb := recs[1]
		assert.Equal(t, tracepack.DecodeStatusOKWithTrailing, rb.rec.DecodeStatus, "classified as received")
		assert.Equal(t, uint32(3), rb.rec.TrailingBytes)
		require.NotNil(t, rb.redaction)
		require.Len(t, rb.redaction.Entries, 1)
		assert.Nil(t, rb.redaction.Entries[0].ItemPath, "a whole-text mask")
		assert.Equal(t, []tracepack.MaskedRange{{Offset: 14, Length: uint64(len(frame) - 14)}}, rb.redaction.Entries[0].MaskedRanges)
		assert.NotEqual(t, frame, rb.rec.Payload)
		assert.Equal(t, frame, restore(t, h, rb))
	})

	t.Run("the only match in the last of several blocks", func(t *testing.T) {
		t.Parallel()

		frames := slices.Repeat([][]byte{s1f1}, 9)
		secret := ppbody("PP9", []byte("RECIPE-OF-THE-LAST-BLOCK"))
		frames = append(frames, secret)
		var out bytes.Buffer
		require.NoError(t, consumeBatch(t.Context(), &out, busBatch(t, 100, frames...), c, cls, 3*(44+len(s1f1))))
		h, recs := readPack(t, out.Bytes())
		require.Len(t, recs, 10)
		last := recs[9]
		assert.Greater(t, last.block, 1, "several blocks before it")
		assert.Equal(t, uint16(1), h.FormatMinor)
		require.Len(t, h.Meta.Redaction, 1)
		assert.Equal(t, uint64(109), h.Meta.Redaction[0].Seq)
		for _, rb := range recs[:9] {
			assert.Nil(t, rb.redaction, "seq %d", rb.rec.Seq)
		}
		assert.Equal(t, secret, restore(t, h, last))
	})

	t.Run("a batch that matches nothing", func(t *testing.T) {
		t.Parallel()

		var out bytes.Buffer
		require.NoError(t, consumeBatch(t.Context(), &out, busBatch(t, 0, s1f1, busFrame(7, 4, nil)), c, cls, 0))
		h, recs := readPack(t, out.Bytes())
		require.Len(t, recs, 2)
		policy := c.PackPolicy()
		assert.Equal(t, &policy, h.Meta.RedactionPolicy, "the policy is written")
		assert.Empty(t, h.Meta.Redaction)
		assert.Equal(t, uint16(1), h.FormatMinor)
		assert.Zero(t, h.Flags, "no redaction-present")
	})

	t.Run("a screening failure late in the batch", func(t *testing.T) {
		t.Parallel()

		deep := secsItem(0o10, 1)
		for range 65 {
			deep = secsList(deep)
		}
		var out bytes.Buffer
		err := consumeBatch(t.Context(), &out, busBatch(t, 0, s1f1, ppbody("PP1", []byte("BODY")), busFrame(7, 3, secsList(deep, secsItem(0o10, 9)))), c, cls, 0)
		require.ErrorIs(t, err, redact.ErrCannotScreen)
		assert.Zero(t, out.Len(), "no output")
	})

	t.Run("a non-seekable output", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		out := struct{ io.Writer }{&buf}
		frame := ppbody("PP1", []byte("BODY"))
		require.NoError(t, consumeBatch(t.Context(), out, busBatch(t, 0, frame), c, cls, 0))
		h, recs := readPack(t, buf.Bytes())
		require.Len(t, recs, 1)
		assert.Equal(t, frame, restore(t, h, recs[0]))
	})
}

// A record redelivered to another consumer, which places it in another batch and another block,
// is written with the bytes and entries of its first write (the tracepack storage specification §4, Masking).
func TestRedeliveryAcrossBatches(t *testing.T) {
	t.Parallel()

	c, cls := consumerPolicy(t), classify.NewNamed("consumer-classifier/1", 0)
	s1f1 := busFrame(1, 1, nil)
	secret := ppbody("PP5", []byte("REDELIVERED-RECIPE"))

	// The first consumer has the record at seq 5 alone in its batch; the second, at the end of a batch of several blocks.
	var first, second bytes.Buffer
	require.NoError(t, consumeBatch(t.Context(), &first, busBatch(t, 5, secret), c, cls, 0))
	require.NoError(t, consumeBatch(t.Context(), &second, busBatch(t, 1, s1f1, s1f1, s1f1, s1f1, secret), c, cls, 2*(44+len(s1f1))))

	_, a := readPack(t, first.Bytes())
	hb, b := readPack(t, second.Bytes())
	require.Len(t, a, 1)
	require.Len(t, b, 5)
	ra, rb := a[0], b[4]
	assert.Zero(t, ra.block)
	assert.Positive(t, rb.block, "another block")
	assert.Equal(t, ra.rec, rb.rec, "the same record header and payload")
	require.NotNil(t, ra.redaction)
	assert.Equal(t, ra.redaction, rb.redaction, "the same entries: digest, ranges and siv")
	require.Len(t, rb.redaction.Entries, 1)
	assert.Len(t, rb.redaction.Entries[0].SIV, 16)
	assert.Equal(t, secret, restore(t, hb, rb))
}

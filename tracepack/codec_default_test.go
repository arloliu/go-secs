package tracepack

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// packCodecs returns the codec of every block of file, a finalized pack, in file order, and the codec of its footer.
func packCodecs(t testing.TB, file []byte) ([]Codec, Codec) {
	t.Helper()

	r := mustOpen(t, file, ReaderOptions{})
	blocks := make([]Codec, 0, len(r.Blocks()))
	for _, b := range r.Blocks() {
		env, err := format.UnmarshalBlockEnvelope(file[b.Offset:])
		require.NoError(t, err)
		blocks = append(blocks, Codec(env.Codec))
	}
	tr, err := format.UnmarshalTrailer(file[len(file)-format.TrailerLen:])
	require.NoError(t, err)

	return blocks, Codec(tr.FooterCodec)
}

// repeatCodec returns n times c.
func repeatCodec(c Codec, n int) []Codec {
	out := make([]Codec, n)
	for i := range out {
		out[i] = c
	}

	return out
}

func TestWriterDefaultCodecZstd(t *testing.T) {
	t.Parallel()

	steps := hourSteps(0, 19, 5)
	tests := []struct {
		name string
		opts WriterOptions
		want Codec
	}{
		{name: "zero value", opts: WriterOptions{}, want: CodecZstd},
		{name: "uncompressed", opts: WriterOptions{Uncompressed: true}, want: CodecNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := tt.opts
			opts.Meta = repairTestMeta()
			var buf bytes.Buffer
			w, err := NewWriter(&buf, opts)
			require.NoError(t, err)
			for i := range steps {
				require.NoError(t, w.Append(&steps[i].rec))
				if steps[i].flush {
					require.NoError(t, w.Flush())
				}
			}
			_, err = w.Close()
			require.NoError(t, err)

			blocks, footer := packCodecs(t, buf.Bytes())
			assert.Equal(t, repeatCodec(tt.want, 4), blocks, "every block")
			assert.Equal(t, tt.want, footer, "the footer")
		})
	}
}

func TestMergeDefaultCodecZstd(t *testing.T) {
	t.Parallel()

	// The first input's blocks, seqs 10 to 29, are zstd, the second's, seqs 30 to 49, none;
	// the archive takes them in seq order.
	inputs := func(t *testing.T) []MergeInput {
		return inputsOf(
			mergePackCodec(t, CodecZstd, seg1, hourSteps(10, 29, 7)),
			mergePackCodec(t, CodecNone, seg0, hourSteps(30, 49, 5)))
	}
	copied := append(repeatCodec(CodecZstd, 3), repeatCodec(CodecNone, 4)...)
	tests := []struct {
		name         string
		uncompressed bool
		noCoalesce   bool
		blocks       []Codec
		footer       Codec
	}{
		{name: "zero value, blocks coalesced", blocks: []Codec{CodecZstd}, footer: CodecZstd},
		{name: "uncompressed, blocks coalesced", uncompressed: true, blocks: []Codec{CodecNone}, footer: CodecNone},
		{name: "zero value, blocks copied", noCoalesce: true, blocks: copied, footer: CodecZstd},
		{name: "uncompressed, blocks copied", uncompressed: true, noCoalesce: true, blocks: copied, footer: CodecNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := mergeOpts()
			opts.Uncompressed, opts.NoCoalesce = tt.uncompressed, tt.noCoalesce
			out, _, err := mergeBytes(t.Context(), inputs(t), opts)
			require.NoError(t, err)

			blocks, footer := packCodecs(t, out)
			assert.Equal(t, tt.blocks, blocks, "the blocks")
			assert.Equal(t, tt.footer, footer, "the footer")
		})
	}
}

// TestSegmentWriterCodecUnchanged pins the bytes a SegmentWriter writes for a fixed clock,
// with and without Uncompressed, its pack_id and capture_id fixed, since they are random.
func TestSegmentWriterCodecUnchanged(t *testing.T) {
	t.Parallel()

	tests := []struct {
		uncompressed bool
		want         string
	}{
		{uncompressed: false, want: "7a6f64d9d8ad21e65f2beba07c128bb0a1e4bdda387a3ef44ce55ec4ab281e99"},
		{uncompressed: true, want: "563154125019199a1347023aab9575ad7702e9c747682c20cbc93aba1c913c88"},
	}
	for _, tt := range tests {
		w, sink := newSeg(t, func(o *SegmentWriterOptions) { o.Uncompressed = tt.uncompressed })
		for i := range 20 {
			segFrame(t, w, time.Second+time.Duration(i)*time.Millisecond)
		}
		require.NoError(t, w.Close(t.Context()))
		require.Len(t, sink.keys, 1)

		file := withIDs(t, sink.published[sink.keys[0]], seg0, viewCapture)
		sum := sha256.Sum256(file)
		assert.Equal(t, tt.want, hex.EncodeToString(sum[:]), "Uncompressed %v", tt.uncompressed)
	}
}

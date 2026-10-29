package tracepack

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/codec"
	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// oracleEncoder returns a zstd encoder built in the test, independent of the package's level mapping,
// configured as the package's encoders are: concurrency 1 and the library's default window, at level.
func oracleEncoder(t testing.TB, level zstd.EncoderLevel) *zstd.Encoder {
	t.Helper()

	enc, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(level))
	require.NoError(t, err)

	return enc
}

// oracleLevels maps each CompressionLevel to the zstd library's level it must encode at.
var oracleLevels = map[CompressionLevel]zstd.EncoderLevel{
	CompressionDefault: zstd.SpeedDefault,
	CompressionFastest: zstd.SpeedFastest,
	CompressionBest:    zstd.SpeedBestCompression,
}

// requireEncodedAt requires every zstd block body and the zstd footer of file to be what enc writes for its decoded bytes,
// except the blocks whose bytes, envelope and body, are in copied, which are compared by the caller.
// It returns the number of block bodies and footers it compared.
func requireEncodedAt(t *testing.T, file []byte, enc *zstd.Encoder, copied map[string]bool) int {
	t.Helper()

	n := 0
	r := mustOpen(t, file, ReaderOptions{})
	for i, b := range r.blocks {
		raw := file[b.Offset : b.Offset+uint64(b.OnDiskLen)]
		if copied[string(raw)] {
			continue
		}
		env, err := format.UnmarshalBlockEnvelope(raw)
		require.NoError(t, err)
		if env.Codec != codec.Zstd {
			continue
		}
		body := raw[format.EnvelopeLen:]
		decoded, err := codec.Decode(codec.Zstd, nil, body, int(env.UncompressedLen))
		require.NoError(t, err)
		require.True(t, bytes.Equal(enc.EncodeAll(decoded, nil), body), "block %d: encoded at the level", i)
		n++
	}
	tr, err := format.UnmarshalTrailer(file[len(file)-format.TrailerLen:])
	require.NoError(t, err)
	if tr.FooterCodec == codec.Zstd {
		footer := file[tr.FooterOffset : tr.FooterOffset+tr.FooterLen]
		decoded, err := codec.Decode(codec.Zstd, nil, footer, int(tr.FooterUncompressedLen))
		require.NoError(t, err)
		require.True(t, bytes.Equal(enc.EncodeAll(decoded, nil), footer), "the footer encoded at the level")
		n++
	}

	return n
}

// blockBytesOf returns the bytes, envelope and on-disk body, of every block of each file.
func blockBytesOf(t *testing.T, files ...[]byte) map[string]bool {
	t.Helper()

	out := map[string]bool{}
	for _, f := range files {
		r := mustOpen(t, f, ReaderOptions{})
		for _, b := range r.blocks {
			out[string(f[b.Offset:b.Offset+uint64(b.OnDiskLen)])] = true
		}
	}

	return out
}

func TestCompressionLevelString(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "default", CompressionDefault.String())
	assert.Equal(t, "fastest", CompressionFastest.String())
	assert.Equal(t, "best", CompressionBest.String())
	assert.Equal(t, "unknown(3)", CompressionLevel(3).String())
	assert.Equal(t, "unknown(255)", CompressionLevel(255).String())
}

// A Writer writes every block body and the footer at its CompressionLevel, deterministically,
// each level a pack Iterate reads back as written.
func TestWriterCompressionLevels(t *testing.T) {
	t.Parallel()

	want := iterate(t, mustOpen(t, goldenWriterPack(t, true, nil), ReaderOptions{}), Query{Payloads: true})
	defaults := goldenWriterPack(t, true, nil)
	for level, zl := range oracleLevels {
		t.Run(level.String(), func(t *testing.T) {
			t.Parallel()

			mutate := func(o *WriterOptions) { o.CompressionLevel = level }
			file := goldenWriterPack(t, true, mutate)
			assert.Equal(t, file, goldenWriterPack(t, true, mutate), "deterministic")
			assert.Positive(t, requireEncodedAt(t, file, oracleEncoder(t, zl), nil))
			assert.Equal(t, want, iterate(t, mustOpen(t, file, ReaderOptions{}), Query{Payloads: true}))
			if level != CompressionDefault {
				assert.NotEqual(t, defaults, file, "the level changes the bytes")
			}

			// With Uncompressed, the level has no effect.
			uncompressed := func(o *WriterOptions) { o.Uncompressed = true }
			assert.Equal(t, goldenWriterPack(t, false, uncompressed),
				goldenWriterPack(t, false, func(o *WriterOptions) { uncompressed(o); mutate(o) }))
		})
	}
}

// A SegmentWriter writes every segment at its CompressionLevel, across rollovers.
func TestSegmentWriterCompressionLevels(t *testing.T) {
	t.Parallel()

	for level, zl := range oracleLevels {
		t.Run(level.String(), func(t *testing.T) {
			t.Parallel()

			w, sink := newSeg(t, func(o *SegmentWriterOptions) {
				o.CompressionLevel = level
				o.FlushInterval = time.Minute
			})
			for k := range 3 * 60 {
				segFrame(t, w, time.Duration(k)*time.Second)
			}
			require.NoError(t, w.Close(t.Context()))
			require.GreaterOrEqual(t, len(sink.keys), 3, "the writer rolled over")
			enc := oracleEncoder(t, zl)
			for _, k := range sink.keys {
				assert.Positive(t, requireEncodedAt(t, sink.published[k], enc, nil), "segment %s", k)
			}
		})
	}
}

// Merge encodes every block it resolves or coalesces, and the footer, at its CompressionLevel,
// and copies every other block verbatim, whatever the level.
func TestMergeCompressionLevels(t *testing.T) {
	t.Parallel()

	for _, c := range goldenMergeCases(t) {
		for level, zl := range oracleLevels {
			t.Run(c.name+"/"+level.String(), func(t *testing.T) {
				t.Parallel()

				mutate := func(o *MergeOptions) { o.CompressionLevel = level }
				out := goldenMerge(t, c, mutate)
				assert.Equal(t, out, goldenMerge(t, c, mutate), "deterministic")
				inputs := blockBytesOf(t, c.inputs...)
				encoded := requireEncodedAt(t, out, oracleEncoder(t, zl), inputs)
				assert.Positive(t, encoded, "the footer at least")
				if c.name == "verbatim" {
					r := mustOpen(t, out, ReaderOptions{})
					for i, b := range r.blocks {
						assert.True(t, inputs[string(out[b.Offset:b.Offset+uint64(b.OnDiskLen)])], "block %d copied verbatim", i)
					}
				} else if level != CompressionDefault {
					// At the default level a block the merge encodes from one input block's records can equal it byte for byte.
					assert.Greater(t, encoded, 1, "the merge encodes blocks of its own")
				}
				want, _, err := collectMergeIterate(t.Context(), openAll(t, c.inputs...), Query{Payloads: true}, MergeIterateOptions{Order: OrderCapture})
				require.NoError(t, err)
				got := iterate(t, mustOpen(t, out, ReaderOptions{}), Query{Payloads: true})
				require.Len(t, got.items, len(want))
				for i := range want {
					assert.Equal(t, want[i].rec, got.items[i].rec, "record %d", i)
				}
			})
		}
	}
}

// writeCounter counts the bytes written to it, and fails every write when fail is set.
type writeCounter struct {
	n    int
	fail bool
}

func (w *writeCounter) Write(p []byte) (int, error) {
	w.n += len(p)
	if w.fail {
		return 0, errors.New("write")
	}

	return len(p), nil
}

// readCounter is an io.ReaderAt over data that counts its calls.
type readCounter struct {
	data  []byte
	calls int
}

func (r *readCounter) ReadAt(p []byte, off int64) (int, error) {
	r.calls++

	return copyAt(r.data, p, off)
}

var _ io.ReaderAt = (*readCounter)(nil)

// An undefined CompressionLevel fails NewWriter, NewSegmentWriter and Merge before any output, sink or input I/O,
// with Uncompressed set or not, naming the option and the value.
func TestCompressionLevelUndefined(t *testing.T) {
	t.Parallel()

	for _, uncompressed := range []bool{false, true} {
		for _, level := range []CompressionLevel{3, 255} {
			name := fmt.Sprintf("%v/uncompressed=%v", level, uncompressed)
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				var out writeCounter
				m := readerTestMeta()
				m.PeriodStart, m.PeriodEnd = layoutStart.UnixNano(), layoutStart.Add(time.Hour).UnixNano()
				_, err := NewWriter(&out, WriterOptions{Meta: m, Uncompressed: uncompressed, CompressionLevel: level})
				require.ErrorContains(t, err, fmt.Sprintf("WriterOptions.CompressionLevel %v is not defined", level))
				assert.Zero(t, out.n, "nothing written")

				sink := newMemSink()
				opts := segOptions(sink)
				opts.Uncompressed, opts.CompressionLevel = uncompressed, level
				_, err = NewSegmentWriter(t.Context(), opts)
				require.ErrorContains(t, err, fmt.Sprintf("SegmentWriterOptions.CompressionLevel %v is not defined", level))
				assert.Empty(t, sink.infos, "no segment created")

				in := &readCounter{data: mergePackCodec(t, CodecZstd, seg0, hourSteps(1, 4, 2))}
				mo := mergeOpts()
				mo.Uncompressed, mo.CompressionLevel = uncompressed, level
				var dst writeCounter
				_, err = Merge(t.Context(), &dst, []MergeInput{{ReaderAt: in, Size: int64(len(in.data))}}, mo)
				require.ErrorContains(t, err, fmt.Sprintf("MergeOptions.CompressionLevel %v is not defined", level))
				assert.Zero(t, in.calls, "no input read")
				assert.Zero(t, dst.n, "nothing written")
			})
		}
	}
}

package compression

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/s2"
	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)

// codec is one candidate compressor under test.
type codec struct {
	name string
	enc  func(dst, src []byte) []byte
	dec  func(dst, src []byte) ([]byte, error)
}

func mustEnc(level zstd.EncoderLevel, dict []byte) *zstd.Encoder {
	opts := []zstd.EOption{
		zstd.WithEncoderLevel(level),
		zstd.WithEncoderConcurrency(1),
		zstd.WithWindowSize(1 << 20),
	}
	if dict != nil {
		opts = append(opts, zstd.WithEncoderDict(dict))
	}
	e, err := zstd.NewWriter(nil, opts...)
	if err != nil {
		panic(err)
	}

	return e
}

func mustDec(dict []byte) *zstd.Decoder {
	opts := []zstd.DOption{zstd.WithDecoderConcurrency(1)}
	if dict != nil {
		opts = append(opts, zstd.WithDecoderDicts(dict))
	}
	d, err := zstd.NewReader(nil, opts...)
	if err != nil {
		panic(err)
	}

	return d
}

func codecs(dict []byte) []codec {
	zFast, zFastD := mustEnc(zstd.SpeedFastest, nil), mustDec(nil)
	zDef, zDefD := mustEnc(zstd.SpeedDefault, nil), mustDec(nil)

	cs := []codec{
		{
			name: "zstd-fastest",
			enc:  func(dst, src []byte) []byte { return zFast.EncodeAll(src, dst) },
			dec:  func(dst, src []byte) ([]byte, error) { return zFastD.DecodeAll(src, dst) },
		},
		{
			name: "zstd-default",
			enc:  func(dst, src []byte) []byte { return zDef.EncodeAll(src, dst) },
			dec:  func(dst, src []byte) ([]byte, error) { return zDefD.DecodeAll(src, dst) },
		},
		{
			name: "s2",
			enc:  func(dst, src []byte) []byte { return s2.Encode(dst, src) },
			dec:  func(dst, src []byte) ([]byte, error) { return s2.Decode(dst, src) },
		},
		{
			name: "lz4-block",
			// scratch buffers hoisted out of the hot path so lz4 is measured on
			// the same footing as the zstd/s2 codecs (which reuse internal state).
			enc: func() func(dst, src []byte) []byte {
				var c lz4.Compressor
				scratch := make([]byte, 1<<20)
				return func(dst, src []byte) []byte {
					if bound := lz4.CompressBlockBound(len(src)); bound > len(scratch) {
						scratch = make([]byte, bound)
					}
					n, err := c.CompressBlock(src, scratch)
					if err != nil {
						panic(err)
					}
					if n == 0 { // incompressible: store raw
						return append(dst, src...)
					}

					return append(dst, scratch[:n]...)
				}
			}(),
			dec: func() func(dst, src []byte) ([]byte, error) {
				scratch := make([]byte, 1<<21)
				return func(dst, src []byte) ([]byte, error) {
					for {
						n, err := lz4.UncompressBlock(src, scratch)
						if err != nil {
							scratch = make([]byte, len(scratch)*2)
							if len(scratch) > 1<<28 {
								return nil, err
							}
							continue
						}

						return scratch[:n], nil
					}
				}
			}(),
		},
	}

	if dict != nil {
		zdFast, zdFastD := mustEnc(zstd.SpeedFastest, dict), mustDec(dict)
		zdDef, zdDefD := mustEnc(zstd.SpeedDefault, dict), mustDec(dict)
		cs = append(cs,
			codec{
				name: "zstd-fastest+dict",
				enc:  func(dst, src []byte) []byte { return zdFast.EncodeAll(src, dst) },
				dec:  func(dst, src []byte) ([]byte, error) { return zdFastD.DecodeAll(src, dst) },
			},
			codec{
				name: "zstd-default+dict",
				enc:  func(dst, src []byte) []byte { return zdDef.EncodeAll(src, dst) },
				dec:  func(dst, src []byte) ([]byte, error) { return zdDefD.DecodeAll(src, dst) },
			},
		)
	}

	return cs
}

// buildDict trains a zstd dictionary from the training corpus.
func buildDict(t testing.TB) []byte {
	// klauspost's BuildDict does NOT perform content selection — it requires a
	// pre-selected History blob. Use the reference zstd CLI (COVER algorithm)
	// to train a real dictionary, then load it with WithEncoderDict.
	dir := t.TempDir()
	for i, msg := range TrainingCorpus() {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("m%05d.bin", i)), msg, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(dir, "trained.dict")
	files, _ := filepath.Glob(filepath.Join(dir, "m*.bin"))
	args := append([]string{"--train", "-o", out, "--maxdict=8192", "-f"}, files...)
	cmd := exec.Command("zstd", args...)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Logf("zstd --train failed: %v\n%s", err, b)
		return nil
	}
	dict, err := os.ReadFile(out)
	if err != nil {
		t.Logf("read dict: %v", err)
		return nil
	}

	return dict
}

// TestRatios reports compressed size for every codec x sample. Correctness
// (roundtrip) is asserted for each.
func TestRatios(t *testing.T) {
	dict := buildDict(t)
	t.Logf("dictionary size: %d bytes", len(dict))

	cs := codecs(dict)
	samples := Corpus()

	fmt.Printf("\n%-24s %10s", "SAMPLE", "RAW")
	for _, c := range cs {
		fmt.Printf(" %20s", c.name)
	}
	fmt.Println()

	for _, s := range samples {
		fmt.Printf("%-24s %10d", s.Name, len(s.Body))
		for _, c := range cs {
			enc := c.enc(nil, s.Body)
			got, err := c.dec(nil, enc)
			if err != nil {
				t.Fatalf("%s/%s decode: %v", s.Name, c.name, err)
			}
			if !bytes.Equal(got, s.Body) {
				t.Fatalf("%s/%s roundtrip mismatch", s.Name, c.name)
			}
			ratio := 0.0
			if len(s.Body) > 0 {
				ratio = float64(len(enc)) / float64(len(s.Body)) * 100
			}
			fmt.Printf(" %9d (%6.1f%%)", len(enc), ratio)
		}
		fmt.Println()
	}
	fmt.Println()
}

// TestHoldoutDict measures dictionary benefit on messages NOT in the training
// set — the only honest way to report dictionary gains.
func TestHoldoutDict(t *testing.T) {
	dict := buildDict(t)
	if dict == nil {
		t.Skip("no dictionary")
	}
	cs := codecs(dict)

	totals := map[string]int{}
	raw := 0
	for _, s := range HoldoutCorpus() {
		raw += len(s.Body)
		for _, c := range cs {
			totals[c.name] += len(c.enc(nil, s.Body))
		}
	}

	fmt.Printf("\nHOLDOUT (disjoint seeds), raw total = %d bytes across %d msgs\n", raw, len(HoldoutCorpus()))
	for _, c := range cs {
		fmt.Printf("  %-22s %8d bytes (%5.1f%% of raw)\n", c.name, totals[c.name], float64(totals[c.name])/float64(raw)*100)
	}
	fmt.Println()
}

// Benchmarks: per-shape encode cost. Decode measured separately.
func BenchmarkEncode(b *testing.B) {
	dict := buildDict(b)
	for _, s := range Corpus() {
		for _, c := range codecs(dict) {
			b.Run(fmt.Sprintf("%s/%s", s.Name, c.name), func(b *testing.B) {
				dst := make([]byte, 0, len(s.Body)+1024)
				b.SetBytes(int64(len(s.Body)))
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					_ = c.enc(dst[:0], s.Body)
				}
			})
		}
	}
}

func BenchmarkDecode(b *testing.B) {
	dict := buildDict(b)
	for _, s := range Corpus() {
		for _, c := range codecs(dict) {
			enc := c.enc(nil, s.Body)
			b.Run(fmt.Sprintf("%s/%s", s.Name, c.name), func(b *testing.B) {
				dst := make([]byte, 0, len(s.Body)+1024)
				b.SetBytes(int64(len(s.Body)))
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					_, _ = c.dec(dst[:0], enc)
				}
			})
		}
	}
}

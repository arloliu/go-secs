package compression

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// trainDict trains a dictionary of a requested max size via the reference CLI.
func trainDict(t testing.TB, maxdict int) []byte {
	dir := t.TempDir()
	for i, msg := range TrainingCorpus() {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("m%05d.bin", i)), msg, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(dir, "d.dict")
	files, _ := filepath.Glob(filepath.Join(dir, "m*.bin"))
	args := append([]string{"--train", "-o", out, fmt.Sprintf("--maxdict=%d", maxdict), "-f"}, files...)
	if b, err := exec.Command("zstd", args...).CombinedOutput(); err != nil {
		t.Skipf("train failed: %v\n%s", err, b)
	}
	d, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}

	return d
}

// BenchmarkDictCost isolates the question: does EncodeAll pay a per-call cost
// proportional to dictionary size (i.e. reload the dict into match tables on
// every call)? If ns/op scales with dict size on a FIXED small payload, yes.
func BenchmarkDictCost(b *testing.B) {
	payload := s1f4(20, 1).ToBytes()
	b.Logf("payload = %d bytes", len(payload))

	for _, size := range []int{0, 1024, 4096, 8192, 32768, 112640} {
		var dict []byte
		if size > 0 {
			dict = trainDict(b, size)
		}
		enc := mustEnc(zstd.SpeedFastest, dict)
		name := "nodict"
		if size > 0 {
			name = fmt.Sprintf("dict-%dB(actual-%dB)", size, len(dict))
		}
		b.Run(name, func(b *testing.B) {
			dst := make([]byte, 0, 4096)
			b.ReportAllocs()
			for range b.N {
				_ = enc.EncodeAll(payload, dst[:0])
			}
		})
	}
}

// BenchmarkDictCostByPayload checks whether the dict overhead is a fixed
// per-call cost (amortized away by larger payloads) or proportional to input.
func BenchmarkDictCostByPayload(b *testing.B) {
	dict := trainDict(b, 8192)
	encD := mustEnc(zstd.SpeedFastest, dict)
	encN := mustEnc(zstd.SpeedFastest, nil)

	for _, n := range []int{10, 100, 1000} {
		payload := s6f11(n, 42).ToBytes()
		for _, tc := range []struct {
			name string
			e    *zstd.Encoder
		}{{"nodict", encN}, {"dict", encD}} {
			b.Run(fmt.Sprintf("S6F11_%d_%dB/%s", n, len(payload), tc.name), func(b *testing.B) {
				dst := make([]byte, 0, len(payload)+1024)
				b.ReportAllocs()
				for range b.N {
					_ = tc.e.EncodeAll(payload, dst[:0])
				}
			})
		}
	}
}

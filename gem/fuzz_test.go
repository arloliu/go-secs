package gem_test

import (
	"testing"

	"github.com/arloliu/go-secs/v2/secs2"
)

// fuzzDecoder is one generated decoder [FuzzDecoders] runs,
// with the encoded body its builder produces from the generated test samples.
type fuzzDecoder struct {
	name   string
	decode func(secs2.Item) error
	seed   []byte
}

// fuzzDecoders holds every registered decoder, in registration order.
var fuzzDecoders []fuzzDecoder

// registerFuzzDecoder adds a generated decoder and its seed body to the decoders [FuzzDecoders] runs.
//
// The init of each stream's generated decode tests calls it once per decoder,
// so a new stream's decoders are fuzzed without this file listing them.
func registerFuzzDecoder(name string, decode func(secs2.Item) error, seed secs2.Item) {
	fuzzDecoders = append(fuzzDecoders, fuzzDecoder{name: name, decode: decode, seed: seed.ToBytes()})
}

// malformedSeeds are bodies no builder produces,
// aimed at the paths a well-formed seed never takes:
// zero-length items where a decoder may accept one as absent,
// list elements of the wrong format, width, or value count,
// and lists that are empty or nested deeper than any body declares.
func malformedSeeds() [][]byte {
	items := []secs2.Item{
		secs2.L(),
		secs2.L(secs2.L(), secs2.L(secs2.L())),
		secs2.L(secs2.U1(), secs2.U2(), secs2.U4(), secs2.I2(), secs2.F4(), secs2.B(), secs2.BOOLEAN(), secs2.A("")),
		secs2.L(secs2.L(secs2.U2(1), secs2.A("x")), secs2.L(secs2.U2(300), secs2.U1(1)), secs2.L(secs2.A("ok"), secs2.B(1))),
		secs2.L(secs2.U2(1, 2), secs2.B(1, 2), secs2.BOOLEAN(true, false), secs2.F8(0.1)),
		secs2.L(secs2.B(), secs2.L(secs2.U1(), secs2.L()), secs2.A("")),
		secs2.U2(),
		secs2.A(""),
	}

	seeds := make([][]byte, 0, len(items)+3)
	for _, item := range items {
		seeds = append(seeds, item.ToBytes())
	}

	// Empty input, which no item encodes to;
	// truncated: a list that declares five children and carries none, and a U2 header with an odd length.
	return append(seeds, []byte{}, []byte{0x01, 0x05}, []byte{0xa9, 0x01, 0x00})
}

// FuzzDecoders runs every registered decoder over arbitrary wire bytes.
//
// A decoder may reject a body with an error, which most inputs are,
// but it must never panic.
// The corpus starts from each decoder's own builder output and from [malformedSeeds].
func FuzzDecoders(f *testing.F) {
	for _, d := range fuzzDecoders {
		f.Add(d.seed)
	}

	for _, seed := range malformedSeeds() {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		item, err := secs2.Decode(data)
		if err != nil {
			return
		}

		for _, d := range fuzzDecoders {
			_ = d.decode(item) // an error is an acceptable outcome; a panic fails the fuzz run
		}
	})
}

// TestFuzzRegistry proves the generated decode tests registered their decoders,
// each under a distinct name,
// and that each decoder accepts its own seed,
// so the fuzz corpus starts from bodies that pass every decoder's shape checks at least once.
func TestFuzzRegistry(t *testing.T) {
	if len(fuzzDecoders) == 0 {
		t.Fatal("no decoders registered")
	}

	seen := map[string]bool{}

	for _, d := range fuzzDecoders {
		if seen[d.name] {
			t.Errorf("%s registered twice", d.name)
		}

		seen[d.name] = true

		item, err := secs2.Decode(d.seed)
		if err != nil {
			t.Fatalf("%s: secs2.Decode: %v", d.name, err)
		}

		if err := d.decode(item); err != nil {
			t.Errorf("%s: own seed: %v", d.name, err)
		}
	}
}

package gem_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/arloliu/go-secs/v2/gem"
	"github.com/arloliu/go-secs/v2/secs2"
)

// fuzzDecoder is one generated decoder [FuzzDecoders] runs,
// with the encoded body its builder produces from the generated test samples.
type fuzzDecoder struct {
	name   string
	decode func(secs2.Item, ...gem.DecodeOption) error
	seed   []byte
}

// decodeModes are the option sets every registered decoder runs with:
// the strict E5 shape, and trailing children allowed.
var decodeModes = []struct {
	name string
	opts []gem.DecodeOption
}{
	{name: "strict"},
	{name: "trailing", opts: []gem.DecodeOption{gem.AllowTrailing()}},
}

// runDecoder runs d over item once in each of [decodeModes],
// passing visit the mode and the decoder's error.
func runDecoder(d fuzzDecoder, item secs2.Item, visit func(mode string, err error)) {
	for _, m := range decodeModes {
		visit(m.name, d.decode(item, m.opts...))
	}
}

// fuzzDecoders holds every registered decoder, in registration order.
var fuzzDecoders []fuzzDecoder

// registerFuzzDecoder adds a generated decoder and its seed body to the decoders [FuzzDecoders] runs.
//
// The init of each stream's generated decode tests calls it once per decoder,
// so a new stream's decoders are fuzzed without this file listing them.
func registerFuzzDecoder(name string, decode func(secs2.Item, ...gem.DecodeOption) error, seed secs2.Item) {
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

// FuzzDecoders runs every registered decoder over arbitrary wire bytes,
// once in each of [decodeModes].
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
			runDecoder(d, item, func(string, error) {}) // an error is an acceptable outcome; a panic fails the fuzz run
		}
	})
}

// TestFuzzRegistry proves the generated decode tests registered every generated decoder of this package,
// each under a distinct name,
// and that each decoder accepts its own seed in every one of [decodeModes],
// so the fuzz corpus starts from bodies that pass every decoder's shape checks at least once,
// and [FuzzDecoders] runs each decoder in each mode.
func TestFuzzRegistry(t *testing.T) {
	if len(fuzzDecoders) == 0 {
		t.Fatal("no decoders registered")
	}

	ran := map[string]map[string]bool{}

	for _, d := range fuzzDecoders {
		if ran[d.name] != nil {
			t.Errorf("%s registered twice", d.name)
		}

		ran[d.name] = map[string]bool{}

		item, err := secs2.Decode(d.seed)
		if err != nil {
			t.Fatalf("%s: secs2.Decode: %v", d.name, err)
		}

		runDecoder(d, item, func(mode string, err error) {
			ran[d.name][mode] = true

			if err != nil {
				t.Errorf("%s: own seed, %s: %v", d.name, mode, err)
			}
		})
	}

	for name, modes := range ran {
		for _, want := range []string{"strict", "trailing"} {
			if !modes[want] {
				t.Errorf("%s did not run in the %s mode", name, want)
			}
		}

		if len(modes) != 2 {
			t.Errorf("%s ran in %d modes, want 2", name, len(modes))
		}
	}

	for _, name := range generatedDecoders(t) {
		if ran[name] == nil {
			t.Errorf("%s is not registered", name)
		}
	}
}

// generatedDecoders returns the name of every exported Decode function
// that a generated file of the package under test declares.
func generatedDecoders(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}

	var names []string

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}

		file, err := parser.ParseFile(token.NewFileSet(), entry.Name(), nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}

		if !ast.IsGenerated(file) {
			continue
		}

		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.IsExported() && strings.HasPrefix(fn.Name.Name, "Decode") {
				names = append(names, fn.Name.Name)
			}
		}
	}

	if len(names) == 0 {
		t.Fatal("no generated decoders found")
	}

	return names
}

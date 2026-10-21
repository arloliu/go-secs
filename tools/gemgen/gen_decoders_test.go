package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// decoderItems extends paramsItems with the leaf shapes only the decoders distinguish: a U1 item
// whose Go type is uint8, which must decode through the unsigned accessor even though Go cannot
// tell uint8 from byte, plus a boolean, a fixed-width byte array, a variable-length byte string,
// and a signed and a floating-point leaf.
func decoderItems() map[string]Item {
	items := paramsItems()

	items["RMACK"] = Item{Formats: []string{"U1"}, Binding: BindingFixed, GoType: "uint8", Values: []ItemValue{{Name: "OK", Value: IntValue(0)}}}
	items["FCNID"] = Item{Formats: []string{"U1"}, Binding: BindingFixed, GoType: "uint8"}
	items["ACKA"] = Item{Formats: []string{"BOOLEAN"}, Binding: BindingFixed, GoType: "bool"}
	items["MHEAD"] = Item{Formats: []string{"B"}, Binding: BindingFixed, GoType: "[10]byte"}
	items["ABS"] = Item{Formats: []string{"B"}, Binding: BindingFixed, GoType: "[]byte"}
	items["ERRCODE"] = Item{Formats: []string{"U1"}, Binding: BindingOpen}
	items["ERRTEXT"] = Item{Formats: []string{"A"}, Binding: BindingFixed, GoType: "string"}
	items["SIGNED"] = Item{Formats: []string{"I4"}, Binding: BindingFixed, GoType: "int32"}
	items["REAL"] = Item{Formats: []string{"F4"}, Binding: BindingFixed, GoType: "float32"}

	return items
}

// renderOneDecoder renders a single-body message and returns the generated source.
func renderOneDecoder(t *testing.T, function int, actor string, structure *StructureNode, items map[string]Item) string {
	t.Helper()

	mf := MessageFile{Stream: 1, Messages: []Message{{
		Function: function, Name: "Sample", Mnemonic: "S",
		Direction: "bidirectional", Description: "A sample.", Exception: "None", Source: "e5",
		Bodies: []Body{{Actor: actor, ReplyExpected: true, Structure: structure}},
	}}}

	out, err := renderDecoders(mf, items)
	require.NoError(t, err)

	return string(out)
}

// TestRenderDecodersDispatchesOnFormatNotGoType guards the one mapping Go's type system cannot
// express: a B-format item and a U1-format item can both declare goType uint8 (byte is uint8), but
// they decode through different SECS-II accessors.
// Choosing the accessor from the Go type would compile cleanly and read the wrong item type at
// runtime.
func TestRenderDecodersDispatchesOnFormatNotGoType(t *testing.T) {
	items := decoderItems()

	src := renderOneDecoder(t, 2, "both", &StructureNode{Type: "list", Items: []StructureNode{
		{Item: "COMMACK"}, // B, enum over byte
		{Item: "RMACK"},   // U1, enum over uint8
		{Item: "FCNID"},   // U1, plain uint8
		{Item: "SFCD"},    // B, plain byte
	}}, items)

	require.Contains(t, src, `decodeByteAs[COMMACK](root.At(0), "COMMACK")`)
	require.Contains(t, src, `decodeUintAs[RMACK](root.At(1), "RMACK")`)
	require.Contains(t, src, `decodeUintAs[uint8](root.At(2), "FCNID")`)
	require.Contains(t, src, `decodeByteAs[byte](root.At(3), "SFCD")`)
}

// TestRenderDecodersLeafShapes asserts every remaining leaf shape reaches its own accessor and
// declares the field type the builder's parameter uses.
func TestRenderDecodersLeafShapes(t *testing.T) {
	items := decoderItems()

	src := renderOneDecoder(t, 2, "both", &StructureNode{Type: "list", Items: []StructureNode{
		{Item: "MDLN"},   // A
		{Item: "ACKA"},   // BOOLEAN
		{Item: "ABS"},    // B, variable-length
		{Item: "SIGNED"}, // I4
		{Item: "REAL"},   // F4
		{Item: "SVID"},   // open binding
	}}, items)

	require.Contains(t, src, `decodeASCII(root.At(0), "MDLN")`)
	require.Contains(t, src, `decodeBool(root.At(1), "ACKA")`)
	require.Contains(t, src, `decodeBinary(root.At(2), "ABS")`)
	require.Contains(t, src, `decodeIntAs[int32](root.At(3), "SIGNED")`)
	require.Contains(t, src, `decodeFloatAs[float32](root.At(4), "REAL")`)
	require.Contains(t, src, `decodeItem(root.At(5), "SVID")`)

	require.Contains(t, src, "MDLN   string")
	require.Contains(t, src, "ACKA   bool")
	require.Contains(t, src, "ABS    []byte")
	require.Contains(t, src, "SIGNED int32")
	require.Contains(t, src, "REAL   float32")
	require.Contains(t, src, "SVID   secs2.Item")
}

// TestRenderDecodersFixedWidthArrayLeaf asserts a [N]byte leaf is width-checked before the array
// conversion, which would otherwise panic on a short item.
func TestRenderDecodersFixedWidthArrayLeaf(t *testing.T) {
	src := renderOneDecoder(t, 1, "both", &StructureNode{Item: "MHEAD"}, decoderItems())

	require.Contains(t, src, "MHEAD [10]byte")
	require.Contains(t, src, `decodeBinary(root, "MHEAD")`)
	require.Contains(t, src, `errSize("MHEAD", len(b1), 10)`)
	require.Contains(t, src, "out.MHEAD = [10]byte(b1)")
}

// TestRenderDecodersRepeatAndPackedGroups asserts the two group kinds stay apart: a repeat is a
// list of separate items, a packed group is one primitive item holding every value.
func TestRenderDecodersRepeatAndPackedGroups(t *testing.T) {
	items := decoderItems()

	repeat := renderOneDecoder(t, 3, "both", &StructureNode{Type: "list", Repeat: "svids", Of: &StructureNode{Item: "SVID"}}, items)
	require.Contains(t, repeat, "SVIDS []secs2.Item")
	require.Contains(t, repeat, `decodeItems(root, "SVIDS")`)

	packed := renderOneDecoder(t, 10, "both", &StructureNode{Type: "list", Items: []StructureNode{
		{Type: "list", Packed: "tsips", Of: &StructureNode{Item: "TSIP"}},
	}}, items)
	require.Contains(t, packed, "TSIPS []byte")
	require.Contains(t, packed, `decodeBinary(root.At(0), "TSIPS")`)
}

// TestRenderDecodersOptionalGroup asserts a minItems-bounded group accepts both the short and the
// full form, and reads the optional fields only when the full form arrived.
// SEMI E5 declares these groups (an error code and its text) omittable on the success path, so
// requiring the full arity would reject the most common reply.
func TestRenderDecodersOptionalGroup(t *testing.T) {
	src := renderOneDecoder(t, 14, "both", &StructureNode{Type: "list", Items: []StructureNode{
		{Item: "ACKA"},
		{Type: "list", MinItems: new(0), MaxItems: new(2), Items: []StructureNode{{Item: "ERRCODE"}, {Item: "ERRTEXT"}}},
	}}, decoderItems())

	require.Contains(t, src, `optionalArity(root.At(1), "S1F14 body[1]", 0, 2)`)
	require.Contains(t, src, "if n1 == 2 {")
	require.Contains(t, src, `decodeItem(root.At(1, 0), "ERRCODE")`)
	require.Contains(t, src, `decodeASCII(root.At(1, 1), "ERRTEXT")`)

	// The optional fields still exist on the struct; the short form leaves them zero.
	require.Contains(t, src, "ERRCODE secs2.Item")
	require.Contains(t, src, "ERRTEXT string")
}

// TestRenderDecodersSkipsHeaderOnlyBodies asserts a body with no structure produces no decoder:
// there is nothing to decode, and an empty result type would be dead public surface.
func TestRenderDecodersSkipsHeaderOnlyBodies(t *testing.T) {
	mf := MessageFile{Stream: 1, Messages: []Message{{
		Function: 1, Name: "Are You There Request", Mnemonic: "R",
		Direction: "bidirectional", Description: "A sample.", Exception: "None", Source: "e5",
		Bodies: []Body{{Actor: "both", ReplyExpected: true, Structure: nil}},
	}}}

	out, err := renderDecoders(mf, decoderItems())
	require.NoError(t, err)
	require.NotContains(t, string(out), "func Decode")
}

// TestRenderDecodersEmptyListBody asserts a zero-length list body still gets a decoder: the shape
// check is the whole value, so the result struct has no fields.
func TestRenderDecodersEmptyListBody(t *testing.T) {
	src := renderOneDecoder(t, 2, "host", &StructureNode{Type: "list", Items: []StructureNode{}}, decoderItems())

	require.Contains(t, src, "type S1F2HostReply struct{}")
	require.Contains(t, src, `requireArity(root, "S1F2 body", 0)`)
}

// TestResultTypeName asserts the primary/secondary suffix and the host-variant infix.
func TestResultTypeName(t *testing.T) {
	require.Equal(t, "S1F14Reply", ResultTypeName(1, 14, "both"))
	require.Equal(t, "S1F13Body", ResultTypeName(1, 13, "equipment"))
	require.Equal(t, "S1F14HostReply", ResultTypeName(1, 14, "host"))
	require.Equal(t, "S1F13HostBody", ResultTypeName(1, 13, "host"))
}

// TestRenderDecodersRejectsUndecodableShapes asserts the generator fails loudly rather than
// emitting a decoder it cannot justify.
func TestRenderDecodersRejectsUndecodableShapes(t *testing.T) {
	items := decoderItems()
	items["JIS"] = Item{Formats: []string{"J"}, Binding: BindingFixed, GoType: "string"}
	items["WIDE"] = Item{Formats: []string{"U2"}, Binding: BindingFixed, GoType: "uint16"}

	tests := []struct {
		name string
		node *StructureNode
		want string
	}{
		{
			name: "unsupported leaf format",
			node: &StructureNode{Item: "JIS"},
			want: `no decoder for format "J"`,
		},
		{
			name: "packed group of a non-binary item",
			node: &StructureNode{Type: "list", Packed: "wides", Of: &StructureNode{Item: "WIDE"}},
			want: "no decoder for item WIDE",
		},
		{
			name: "maxItems disagreeing with the declared children",
			node: &StructureNode{Type: "list", MinItems: new(0), MaxItems: new(3), Items: []StructureNode{{Item: "MDLN"}}},
			want: "maxItems 3 does not match the 1 declared children",
		},
		{
			name: "minItems above the declared children",
			node: &StructureNode{Type: "list", MinItems: new(2), Items: []StructureNode{{Item: "MDLN"}}},
			want: "minItems 2 exceeds the 1 declared children",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mf := MessageFile{Stream: 1, Messages: []Message{{
				Function: 5, Name: "Sample", Mnemonic: "S",
				Direction: "bidirectional", Description: "A sample.", Exception: "None", Source: "e5",
				Bodies: []Body{{Actor: "both", ReplyExpected: true, Structure: tt.node}},
			}}}

			_, err := renderDecoders(mf, items)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

// TestFieldsAgreeWithParamsOverRealData is the invariant the round-trip test generation rests on:
// the decoder's value fields must describe the builder's parameters, in order, for every body the generator emits.
// Length agreement alone would pass on a swapped pair, so this compares name by name — the same
// regression the name-derived samples were built to expose.
func TestFieldsAgreeWithParamsOverRealData(t *testing.T) {
	itemsData, err := os.ReadFile("data/items.yaml")
	require.NoError(t, err)

	items, err := LoadItems(itemsData)
	require.NoError(t, err)

	paths, err := filepath.Glob(filepath.Join("data", "messages", "*.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, paths)

	for _, path := range paths {
		msgData, err := os.ReadFile(path)
		require.NoError(t, err)

		mf, err := LoadMessageFile(msgData)
		require.NoError(t, err)

		for _, m := range mf.Messages {
			for _, b := range m.Bodies {
				if !bodyHasDecoder(b) {
					continue
				}

				code := ResultTypeName(mf.Stream, m.Function, b.Actor)

				params := BuildParams(b.Structure, items)

				exprs, err := SampleExprs(b.Structure, items)
				require.NoErrorf(t, err, "%s samples", code)

				fields, err := BuildFields(b.Structure, items)
				require.NoErrorf(t, err, "%s fields", code)

				require.Lenf(t, exprs, len(params), "%s: sample count", code)
				requireFieldsMirrorParams(t, code, params, fields)
			}
		}
	}
}

// requireFieldsMirrorParams checks that the non-presence fields mirror params one to one, in order and by name,
// and that every presence field directly follows its value field and mirrors no parameter.
func requireFieldsMirrorParams(t *testing.T, code string, params []Param, fields []decodeField) {
	t.Helper()

	next := 0

	for i, f := range fields {
		if f.Presence {
			require.Positivef(t, i, "%s: presence field %s has no value field before it", code, f.Name)
			require.Equalf(t, fields[i-1].Name+"Present", f.Name, "%s: field %d", code, i)
			require.Equalf(t, -1, f.Param, "%s: presence field %s", code, f.Name)
			require.Equalf(t, "bool", f.Type, "%s: presence field %s", code, f.Name)

			continue
		}

		require.Lessf(t, next, len(params), "%s: field %s has no parameter", code, f.Name)
		require.Equalf(t, next, f.Param, "%s: field %s", code, f.Name)
		require.Equalf(t, strings.ToUpper(params[next].Name), f.Name, "%s: field %d", code, i)

		next++
	}

	require.Equalf(t, len(params), next, "%s: value field count", code)
}

// bodyHasDecoder reports whether the generator must emit a decoder and a decode round-trip test for b.
// A header-only body carries nothing to decode,
// and a variant is decoded by its main body's decoder.
// The real-data tests in this file compute their expectations from this predicate rather than from the generator's loops,
// so a body kind that gains or loses a decoder changes here in one line.
func bodyHasDecoder(b Body) bool {
	return b.Structure != nil && b.Variant == ""
}

// optionalLeafFields returns the decoder field names of the optional leaves in n, in body order.
// It does not descend into a repeat's or packed group's element shape, where no leaf is optional.
func optionalLeafFields(n *StructureNode) []string {
	if n == nil {
		return nil
	}

	switch n.Kind() {
	case "leaf":
		if n.Optional {
			return []string{nodeNames(n).Field}
		}

		return nil
	case "list":
		if n.Repeat != "" || n.Packed != "" {
			return nil
		}

		var out []string
		for i := range n.Items {
			out = append(out, optionalLeafFields(&n.Items[i])...)
		}

		return out
	default:
		return nil
	}
}

// declaredFuncs returns the names of the top-level functions declared in the generated source src,
// apart from init, which only registers the decoders with the fuzz harness.
func declaredFuncs(t *testing.T, src []byte) []string {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), "", src, parser.SkipObjectResolution)
	require.NoError(t, err)

	var names []string

	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name != "init" {
			names = append(names, fn.Name.Name)
		}
	}

	return names
}

// fuzzRegisterCall matches a generated decode test file's registration of one decoder with the fuzz harness.
var fuzzRegisterCall = regexp.MustCompile(`registerFuzzDecoder\("(\w+)"`)

// fuzzRegistrations returns the decoder names the generated decode tests src register with the fuzz harness,
// in source order.
func fuzzRegistrations(src []byte) []string {
	matches := fuzzRegisterCall.FindAllSubmatch(src, -1)

	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, string(m[1]))
	}

	return names
}

// TestEveryDecodableBodyGetsADecoderOverRealData checks the generator's output against the stream files themselves:
// every main body that carries a structure gets exactly one decoder and one decode round-trip test,
// plus an empty-list test when its outermost list may be empty,
// and nothing else does;
// and the decode tests register exactly those decoders with the fuzz harness, each once.
// Unlike a fixed body count, it needs no update when a stream file is added.
func TestEveryDecodableBodyGetsADecoderOverRealData(t *testing.T) {
	itemsData, err := os.ReadFile("data/items.yaml")
	require.NoError(t, err)

	items, err := LoadItems(itemsData)
	require.NoError(t, err)

	files, err := loadMessageFiles(filepath.Join("data", "messages"))
	require.NoError(t, err)
	require.NotEmpty(t, files)

	for _, mf := range files {
		t.Run(fmt.Sprintf("S%d", mf.Stream), func(t *testing.T) {
			var wantDecoders, wantTests []string

			for _, m := range mf.Messages {
				for _, b := range m.Bodies {
					if !bodyHasDecoder(b) {
						continue
					}

					name := fmt.Sprintf("DecodeS%dF%d", mf.Stream, m.Function)
					if b.Actor == "host" {
						name += "Host"
					}

					wantDecoders = append(wantDecoders, name)
					wantTests = append(wantTests, "Test"+name+"RoundTrip")

					// A body whose outermost list may be empty also gets a test decoding a bare L,0.
					if s := b.Structure; s.Kind() == "list" && s.MinItems != nil && *s.MinItems == 0 {
						wantTests = append(wantTests, "Test"+name+"EmptyList")
					}

					// Each optional leaf also gets a test decoding the body with that item sent zero-length.
					for _, field := range optionalLeafFields(b.Structure) {
						wantTests = append(wantTests, "Test"+name+field+"Absent")
					}
				}
			}

			decoders, err := renderDecoders(mf, items)
			require.NoError(t, err)

			decodeTests, err := renderDecodeTests(mf, items)
			require.NoError(t, err)

			require.ElementsMatch(t, wantDecoders, declaredFuncs(t, decoders), "decoders")
			require.ElementsMatch(t, wantTests, declaredFuncs(t, decodeTests), "decode round-trip tests")
			require.ElementsMatch(t, wantDecoders, fuzzRegistrations(decodeTests), "fuzz registrations")
		})
	}
}

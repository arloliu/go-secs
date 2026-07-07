package main

import (
	"go/parser"
	"go/token"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

// parseGoSource asserts that src is a complete, syntactically valid Go file,
// mirroring the go/parser check main_smoke_test.go runs on run(...)'s output.
func parseGoSource(t *testing.T, src []byte) {
	t.Helper()

	fset := token.NewFileSet()
	_, err := parser.ParseFile(fset, "generated_test.go", src, parser.AllErrors)
	require.NoError(t, err, "generated test file must be valid Go:\n%s", src)
}

// TestRenderTestsHeaderOnly proves the header-only path: no decode/body
// assertions, just the empty-item check plus the header helper call.
func TestRenderTestsHeaderOnly(t *testing.T) {
	items := map[string]Item{}
	mf := MessageFile{Stream: 1, Messages: []Message{{
		Function: 1, Name: "Are You There Request", Mnemonic: "R",
		Bodies: []Body{{Actor: "both", ReplyExpected: true, Structure: nil}},
	}}}

	out, err := renderTests(mf, items)
	require.NoError(t, err)
	parseGoSource(t, out)

	src := string(out)
	require.Contains(t, src, "package gem_test")
	require.Contains(t, src, "func assertS1Header(t *testing.T, msg secs2.SECS2Message, function uint8, wantW bool) {")
	require.Contains(t, src, "func TestS1F1(t *testing.T) {")
	require.Contains(t, src, "msg := gem.S1F1()")
	require.Contains(t, src, "assertS1Header(t, msg, 1, true)")
	require.Contains(t, src, "msg.Item().IsEmpty()")
}

// TestRenderTestsFixedListActorVariants is the brief's required RED case: a
// fixed two-item ASCII list (S1F2) plus its host actor variant (S1F2Host).
// The generated call must use each item's own lowercased name as its sample
// (not a single shared literal), so the whole file must parse as valid Go
// and MDLN/SOFTREV — same Go type, different items — must render
// distinguishable values.
func TestRenderTestsFixedListActorVariants(t *testing.T) {
	items := testItems()
	mf := MessageFile{Stream: 1, Messages: []Message{{
		Function: 2, Name: "On Line Data", Mnemonic: "D",
		Bodies: []Body{
			{
				Actor: "equipment", ReplyExpected: false,
				Structure: &StructureNode{Type: "list", Items: []StructureNode{{Item: "MDLN"}, {Item: "SOFTREV"}}},
			},
			{
				Actor: "host", ReplyExpected: false,
				Structure: &StructureNode{Type: "list", Items: []StructureNode{}},
			},
		},
	}}}

	out, err := renderTests(mf, items)
	require.NoError(t, err)
	parseGoSource(t, out)

	src := string(out)
	require.Contains(t, src, "func TestS1F2(t *testing.T) {")
	require.Contains(t, src, `gem.S1F2("mdln", "softrev")`)
	require.Contains(t, src, "assertS1Header(t, msg, 2, false)")
	require.Contains(t, src, "func TestS1F2Host(t *testing.T) {")
	require.Contains(t, src, "gem.S1F2Host()")

	// Both leaves are asserted via ToASCII against their own name — not just
	// a length check — so a swapped-order content bug would fail the test.
	require.Contains(t, src, ".ToASCII()")
	require.Contains(t, src, `!= "mdln"`)
	require.Contains(t, src, `!= "softrev"`)

	// Regression guard for the exact bug this fix addresses: MDLN and
	// SOFTREV are both Go type string, so before this fix they shared the
	// literal "X" and a swap between them was undetectable (gem.S1F2("X",
	// "X") stayed correct either way). Extract the two argument literals
	// from the generated call itself and confirm they differ.
	m := regexp.MustCompile(`gem\.S1F2\((".*?"), (".*?")\)`).FindStringSubmatch(src)
	require.Len(t, m, 3, "expected to find the gem.S1F2(...) call with two string arguments")
	require.NotEqual(t, m[1], m[2], "MDLN and SOFTREV must render distinguishable samples despite sharing Go type string")

	// The host variant's zero-length list is still asserted for length 0.
	require.Contains(t, src, "!= 0")
}

// TestRenderTestsBareFixedLeaf proves the bare-fixed-leaf body path (S1F5
// shape): a top-level fixed byte leaf renders without a surrounding list,
// and its sample (the name-derived byte for "sfcd", 0xd7 — see byteSample)
// is asserted via ToBinary directly on the decoded item.
func TestRenderTestsBareFixedLeaf(t *testing.T) {
	items := paramsItems()
	mf := MessageFile{Stream: 1, Messages: []Message{{
		Function: 5, Name: "Formatted Status Send", Mnemonic: "FSS",
		Bodies: []Body{
			{Actor: "equipment", ReplyExpected: true, Structure: &StructureNode{Item: "SFCD"}},
		},
	}}}

	out, err := renderTests(mf, items)
	require.NoError(t, err)
	parseGoSource(t, out)

	src := string(out)
	require.Contains(t, src, "func TestS1F5(t *testing.T) {")
	require.Contains(t, src, "gem.S1F5(0xd7)")
	require.Contains(t, src, "decoded.ToBinary()")
	require.NotContains(t, src, "decoded.ToList()", "a bare leaf body must not be unwrapped as a list")
}

// TestRenderTestsOpaqueBody proves the opaque body path (S1F6 shape): the
// generated call passes a labeled secs2.A sample, and the decode assertion
// compares raw bytes rather than any concrete-type accessor.
func TestRenderTestsOpaqueBody(t *testing.T) {
	items := map[string]Item{}
	mf := MessageFile{Stream: 1, Messages: []Message{{
		Function: 6, Name: "Multi-block Data Send", Mnemonic: "MBS",
		Bodies: []Body{
			{Actor: "equipment", ReplyExpected: false, Structure: &StructureNode{Type: "opaque"}},
		},
	}}}

	out, err := renderTests(mf, items)
	require.NoError(t, err)
	parseGoSource(t, out)

	src := string(out)
	require.Contains(t, src, "func TestS1F6(t *testing.T) {")
	require.Contains(t, src, `gem.S1F6(secs2.A("body"))`)
	require.Contains(t, src, "bytes.Equal(")
	require.Contains(t, src, `"bytes"`, "the file must import bytes when a ToBytes comparison is emitted")
}

// TestRenderTestsOpenRepeat proves the open-binding repeat body path (S1F3
// shape): the call passes a single labeled sample item as the variadic
// argument, and the assertion unwraps the decoded list before comparing the
// element positionally.
func TestRenderTestsOpenRepeat(t *testing.T) {
	items := paramsItems()
	mf := MessageFile{Stream: 1, Messages: []Message{{
		Function: 3, Name: "Selected Equipment Status Request", Mnemonic: "SSR",
		Bodies: []Body{
			{
				Actor: "both", ReplyExpected: true,
				Structure: &StructureNode{Type: "list", Repeat: "svids", Of: &StructureNode{Item: "SVID"}},
			},
		},
	}}}

	out, err := renderTests(mf, items)
	require.NoError(t, err)
	parseGoSource(t, out)

	src := string(out)
	require.Contains(t, src, "func TestS1F3(t *testing.T) {")
	require.Contains(t, src, `gem.S1F3(secs2.A("svids"))`)
	require.Contains(t, src, "!= 1", "the repeat list must be asserted to hold exactly the one sample element")
}

// TestRenderTestsNestedListEnumLeaf proves the nested-list + enum-leaf body
// path (S1F14 shape): the enum-typed COMMACK parameter takes the bare
// name-derived byte literal (no cast, since it's an untyped constant
// assignable to the named COMMACK type), the decode assertion compares the
// decoded binary value against that same literal, and — since this body also
// repeats the MDLN/SOFTREV collision from S1F2 one level deeper — the two
// string samples must still be distinguishable from each other despite
// sharing Go type string.
func TestRenderTestsNestedListEnumLeaf(t *testing.T) {
	items := paramsItems()
	mf := MessageFile{Stream: 1, Messages: []Message{{
		Function: 14, Name: "Establish Communications Request Acknowledge", Mnemonic: "CRA",
		Bodies: []Body{
			{
				Actor: "equipment", ReplyExpected: false,
				Structure: &StructureNode{Type: "list", Items: []StructureNode{
					{Item: "COMMACK"},
					{Type: "list", Items: []StructureNode{{Item: "MDLN"}, {Item: "SOFTREV"}}},
				}},
			},
			{
				Actor: "host", ReplyExpected: false,
				Structure: &StructureNode{Type: "list", Items: []StructureNode{
					{Item: "COMMACK"},
					{Type: "list", Items: []StructureNode{}},
				}},
			},
		},
	}}}

	out, err := renderTests(mf, items)
	require.NoError(t, err)
	parseGoSource(t, out)

	src := string(out)
	require.Contains(t, src, "func TestS1F14(t *testing.T) {")
	require.Contains(t, src, `gem.S1F14(0xb5, "mdln", "softrev")`)
	require.NotContains(t, src, "COMMACK(0xb5)", "the enum parameter needs no cast at the call site")
	require.Contains(t, src, "func TestS1F14Host(t *testing.T) {")
	require.Contains(t, src, "gem.S1F14Host(0xb5)")

	// Regression guard: MDLN and SOFTREV collide on Go type string here too,
	// nested one level deeper than S1F2. Extract the two trailing string
	// arguments from the call and confirm they differ.
	m := regexp.MustCompile(`gem\.S1F14\(0x[0-9a-f]{2}, (".*?"), (".*?")\)`).FindStringSubmatch(src)
	require.Len(t, m, 3, "expected to find the gem.S1F14(...) call with a byte arg and two string args")
	require.NotEqual(t, m[1], m[2], "MDLN and SOFTREV must render distinguishable samples despite sharing Go type string")
}

// TestRenderTestsFixedLeafSiblingRepeats proves E5's hardest S1 shape (S1F19:
// L[3]{ <OBJTYPE> L{<OBJID>...} L{<ATTRID>...} }): the leading open leaf is
// labeled by its own name, the non-final repeat is wrapped in a
// []secs2.Item{...} slice literal at the call site, and only the final
// repeat is passed bare as the variadic argument. Every sample is labeled by
// its own parameter name, so a positional swap between sibling params would
// fail the generated assertions.
func TestRenderTestsFixedLeafSiblingRepeats(t *testing.T) {
	items := paramsItems()
	mf := MessageFile{Stream: 1, Messages: []Message{{
		Function: 19, Name: "Get Attribute", Mnemonic: "GA",
		Bodies: []Body{
			{
				Actor: "both", ReplyExpected: true,
				Structure: &StructureNode{Type: "list", Items: []StructureNode{
					{Item: "OBJTYPE"},
					{Type: "list", Repeat: "objids", Of: &StructureNode{Item: "OBJID"}},
					{Type: "list", Repeat: "attrids", Of: &StructureNode{Item: "ATTRID"}},
				}},
			},
		},
	}}}

	out, err := renderTests(mf, items)
	require.NoError(t, err)
	parseGoSource(t, out)

	src := string(out)
	require.Contains(t, src, "func TestS1F19(t *testing.T) {")
	require.Contains(t, src,
		`gem.S1F19(secs2.A("objtype"), []secs2.Item{secs2.A("objids")}, secs2.A("attrids"))`)
	require.Contains(t, src, `secs2.A("objtype").ToBytes()`)
	require.Contains(t, src, `secs2.A("objids").ToBytes()`)
	require.Contains(t, src, `secs2.A("attrids").ToBytes()`)
}

// TestRenderTestsPackedGroups proves the packed-node body path (S1F10 shape:
// L{ B[n]{ <tsips>... } B[n]{ <tsops>... } }). Unlike a repeat (which
// contributes a secs2.Item parameter and a generic secs2.A(name) sample), a
// packed group contributes a concrete-typed parameter (tsips []byte, tsops
// ...byte) whose sample is a raw byte literal — NOT wrapped in secs2.A(...) or
// any secs2.<Ctor>(...) — since the generator's own body expression does the
// secs2.B(...) wrapping. The decode-side assertion pulls the single packed
// item's bytes via ToBinary() and checks length 1 plus the sample value, not a
// list of separate sub-items.
func TestRenderTestsPackedGroups(t *testing.T) {
	items := paramsItems()
	mf := MessageFile{Stream: 1, Messages: []Message{{
		Function: 10, Name: "Material Transfer Status Data", Mnemonic: "TSD",
		Bodies: []Body{
			{
				Actor: "both", ReplyExpected: false,
				Structure: &StructureNode{Type: "list", Items: []StructureNode{
					{Type: "list", Packed: "tsips", Of: &StructureNode{Item: "TSIP"}},
					{Type: "list", Packed: "tsops", Of: &StructureNode{Item: "TSOP"}},
				}},
			},
		},
	}}}

	out, err := renderTests(mf, items)
	require.NoError(t, err)
	parseGoSource(t, out)

	src := string(out)
	require.Contains(t, src, "func TestS1F10(t *testing.T) {")

	// The non-final packed group is passed as a concrete []byte slice literal
	// (NOT []secs2.Item), and the final one bare as the variadic byte argument.
	// Neither sample is wrapped in secs2.A(...) or any secs2 constructor.
	require.Contains(t, src, "gem.S1F10([]byte{0x39}, 0xe3)")
	require.NotContains(t, src, "secs2.A(\"tsips\")")
	require.NotContains(t, src, "[]secs2.Item{0x39}")

	// The decode-side assertion pulls each packed group's bytes via ToBinary()
	// and checks it holds exactly the one sample value the call site passed.
	require.Contains(t, src, ".ToBinary()")
	require.Contains(t, src, "0x39")
	require.Contains(t, src, "0xe3")
}

// TestRenderTestsFixedBooleanLeaf proves the third (and last) real-world
// fixed-binding Go type — bool, e.g. the GEM ENABLE item — is sampled as
// `true` and asserted via ToBoolean, alongside string and byte.
func TestRenderTestsFixedBooleanLeaf(t *testing.T) {
	items := map[string]Item{
		"ENABLE": {Formats: []string{"BOOLEAN"}, Binding: BindingFixed, GoType: "bool"},
	}
	mf := MessageFile{Stream: 2, Messages: []Message{{
		Function: 37, Name: "Enable/Disable Event Report", Mnemonic: "EDER",
		Bodies: []Body{
			{Actor: "host", ReplyExpected: true, Structure: &StructureNode{Item: "ENABLE"}},
		},
	}}}

	out, err := renderTests(mf, items)
	require.NoError(t, err)
	parseGoSource(t, out)

	src := string(out)
	require.Contains(t, src, "gem.S2F37Host(true)")
	require.Contains(t, src, "decoded.ToBoolean()")
}

// TestRenderTestsByteArrayLeaf proves the byte-array/byte-slice leaf body
// path (S9 header items / S2F25 ABS shape): a fixed [N]byte leaf is sampled
// as an [N]byte{...} composite literal at the call site and asserted against
// the *entire* decoded byte sequence via bytes.Equal (not a single-element
// check, which only fits a scalar byte), and a []byte leaf is sampled as a
// []byte{...} literal and asserted the same way.
func TestRenderTestsByteArrayLeaf(t *testing.T) {
	items := map[string]Item{
		"MHEAD": {Formats: []string{"B"}, Binding: BindingFixed, GoType: "[10]byte"},
		"ABS":   {Formats: []string{"B"}, Binding: BindingFixed, GoType: "[]byte"},
	}
	mf := MessageFile{Stream: 9, Messages: []Message{
		{
			Function: 1, Name: "Unrecognized Device ID", Mnemonic: "UDN",
			Bodies: []Body{{Actor: "equipment", ReplyExpected: false, Structure: &StructureNode{Item: "MHEAD"}}},
		},
		{
			Function: 2, Name: "Unrecognized Stream Type", Mnemonic: "UST",
			Bodies: []Body{{Actor: "equipment", ReplyExpected: false, Structure: &StructureNode{Item: "ABS"}}},
		},
	}}

	out, err := renderTests(mf, items)
	require.NoError(t, err)
	parseGoSource(t, out)

	src := string(out)

	// Fixed [10]byte leaf: call-site sample is a 10-element [10]byte{...}
	// composite literal, decoded via ToBinary, compared as a whole sequence.
	require.Contains(t, src, "func TestS9F1(t *testing.T) {")
	require.Contains(t, src, "gem.S9F1([10]byte{")
	require.Contains(t, src, ".ToBinary()")
	require.Contains(t, src, "bytes.Equal(")
	require.Contains(t, src, `"bytes"`, "a blob comparison must pull in the bytes import")

	// []byte leaf: call-site sample is a []byte{...} literal passed straight
	// through (no [:] slice, that is a body-expression concern, not a sample).
	require.Contains(t, src, "func TestS9F2(t *testing.T) {")
	require.Contains(t, src, "gem.S9F2([]byte{")

	// The whole-sequence comparison must NOT degrade into the single-element
	// scalar-byte check (`len(v) != 1 || v[0] != ...`), which would only ever
	// inspect the first byte of a multi-byte blob. Match the pattern by regex
	// rather than a hardcoded variable name so it survives a change in
	// variable-ID assignment order.
	singleElemCheck := regexp.MustCompile(`\bv\d+\[0\] !=`)
	require.NotRegexp(t, singleElemCheck, src, "a blob comparison must not degrade into a single-element scalar-byte check")
}

// TestRenderTestsUnsupportedFixedGoType proves renderTests fails loudly
// (rather than emitting broken output) for a fixed-binding goType with no
// known deterministic sample, so the generator never silently renders an
// invalid test.
func TestRenderTestsUnsupportedFixedGoType(t *testing.T) {
	items := map[string]Item{
		"WEIRD": {Formats: []string{"F4"}, Binding: BindingFixed, GoType: "float32"},
	}
	mf := MessageFile{Stream: 9, Messages: []Message{{
		Function: 1, Name: "Weird", Mnemonic: "W",
		Bodies: []Body{{Actor: "both", ReplyExpected: false, Structure: &StructureNode{Item: "WEIRD"}}},
	}}}

	_, err := renderTests(mf, items)
	require.Error(t, err)
}

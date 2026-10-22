package main

import (
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
)

// valueItemsFixture extends optionalItems with a fixed string enumeration,
// so every element kind a value repeat sends and decodes differently is at hand:
// a plain number, a byte enum, a string enum, a plain string, and a boolean.
func valueItemsFixture() map[string]Item {
	items := optionalItems()
	maps.Copy(items, map[string]Item{
		"CARRIERACTION": {
			Formats: []string{"A"}, Binding: BindingFixed, GoType: "string",
			Values: []ItemValue{{Name: "ProceedWithCarrier", Value: StringValue("ProceedWithCarrier")}},
		},
	})

	return items
}

// TestLoadMessageFileRejectsTypedKey proves the typed: key is gone from the DSL:
// every repeat of a fixed scalar item now takes and returns values without it.
func TestLoadMessageFileRejectsTypedKey(t *testing.T) {
	_, err := LoadMessageFile([]byte(namesMessages(`{type: list, repeat: locs, typed: true, of: {item: FNLOC}}`)))
	require.ErrorContains(t, err, "field typed not found")
}

// TestValueRepeatItem proves which repeats send and decode their elements as values:
// those whose of is a leaf of a fixed item with a scalar goType, whatever its name or format.
func TestValueRepeatItem(t *testing.T) {
	tests := []struct {
		name string
		node *StructureNode
		want bool
	}{
		{"number", &StructureNode{Type: "list", Repeat: "locs", Of: &StructureNode{Item: "FNLOC"}}, true},
		{"byte enum", &StructureNode{Type: "list", Repeat: "acks", Of: &StructureNode{Item: "COMMACK"}}, true},
		{"string enum", &StructureNode{Type: "list", Repeat: "actions", Of: &StructureNode{Item: "CARRIERACTION"}}, true},
		{"string", &StructureNode{Type: "list", Repeat: "names", Of: &StructureNode{Item: "MDLN"}}, true},
		{"boolean", &StructureNode{Type: "list", Repeat: "flags", Of: &StructureNode{Item: "ACKA"}}, true},
		{"renamed element", &StructureNode{Type: "list", Repeat: "locs", Of: &StructureNode{Item: "FNLOC", As: "loc"}}, true},
		{"open item", &StructureNode{Type: "list", Repeat: "svids", Of: &StructureNode{Item: "SVID"}}, false},
		{"byte slice", &StructureNode{Type: "list", Repeat: "blobs", Of: &StructureNode{Item: "BLOB"}}, false},
		{"byte array", &StructureNode{Type: "list", Repeat: "heads", Of: &StructureNode{Item: "MHEAD"}}, false},
		{"opaque", &StructureNode{Type: "list", Repeat: "specs", Of: &StructureNode{Type: "opaque"}}, false},
		{"list", &StructureNode{Type: "list", Repeat: "pairs", Of: &StructureNode{Type: "list", Items: []StructureNode{{Item: "FNLOC"}}}}, false},
		{"packed group", &StructureNode{Type: "list", Packed: "tsips", Of: &StructureNode{Item: "TSIP"}}, false},
		{"leaf", &StructureNode{Item: "FNLOC"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := valueRepeatItem(tt.node, valueItemsFixture())
			require.Equal(t, tt.want, ok)
		})
	}
}

// valueBody has a value repeat of each element kind,
// with a repeat of open items and a plain leaf between them.
func valueBody() *StructureNode {
	return &StructureNode{Type: "list", Items: []StructureNode{
		{Type: "list", Repeat: "locs", Of: &StructureNode{Item: "FNLOC"}},
		{Item: "MDLN"},
		{Type: "list", Repeat: "acks", Of: &StructureNode{Item: "COMMACK"}},
		{Type: "list", Repeat: "svids", Of: &StructureNode{Item: "SVID"}},
		{Type: "list", Repeat: "actions", Of: &StructureNode{Item: "CARRIERACTION"}},
		{Type: "list", Repeat: "names", Of: &StructureNode{Item: "MDLN"}},
		{Type: "list", Repeat: "flags", Of: &StructureNode{Item: "ACKA"}},
	}}
}

// TestBuildFieldsValueRepeat proves a value repeat's field is a slice of the element's Go type,
// or of its named type for an enumeration,
// that it records the element's goType,
// and that a repeat of open items keeps raw items.
func TestBuildFieldsValueRepeat(t *testing.T) {
	items := valueItemsFixture()
	structure := valueBody()

	fields, err := BuildFields(structure, items)
	require.NoError(t, err)
	require.Equal(t, []decodeField{
		{Name: "LOCS", Type: "[]uint16", Param: 0, Elem: "uint16", Path: []int{0}},
		{Name: "MDLN", Type: "string", Param: 1, Path: []int{1}},
		{Name: "ACKS", Type: "[]COMMACK", Param: 2, Elem: "byte", Path: []int{2}},
		{Name: "SVIDS", Type: "[]secs2.Item", Param: 3, Path: []int{3}},
		{Name: "ACTIONS", Type: "[]CARRIERACTION", Param: 4, Elem: "string", Path: []int{4}},
		{Name: "NAMES", Type: "[]string", Param: 5, Elem: "string", Path: []int{5}},
		{Name: "FLAGS", Type: "[]bool", Param: 6, Elem: "bool", Path: []int{6}},
	}, fields)

	requireFieldsMirrorParams(t, "values", BuildParams(structure, items), fields)
}

// TestRenderDecodersValueRepeat proves a value repeat decodes element by element through decodeEach,
// with the helper a leaf of the element item uses,
// that a repeat of open items still returns raw items,
// and that the result type mirrors the builder's parameters with no exception.
func TestRenderDecodersValueRepeat(t *testing.T) {
	src, err := renderDecoders(optionalMessage(valueBody()), valueItemsFixture())
	require.NoError(t, err)

	got := string(src)
	require.Regexp(t, `\tLOCS +\[\]uint16\n`, got)
	require.Regexp(t, `\tACKS +\[\]COMMACK\n`, got)
	require.Regexp(t, `\tACTIONS +\[\]CARRIERACTION\n`, got)
	require.Contains(t, got, `if out.LOCS, err = decodeEach(root.At(0), "LOCS", decodeUintAs[uint16]); err != nil {`)
	require.Contains(t, got, `if out.ACKS, err = decodeEach(root.At(2), "ACKS", decodeByteAs[COMMACK]); err != nil {`)
	require.Contains(t, got, `if out.SVIDS, err = decodeItems(root.At(3), "SVIDS"); err != nil {`)
	require.Contains(t, got, `if out.ACTIONS, err = decodeEach(root.At(4), "ACTIONS", decodeASCIIAs[CARRIERACTION]); err != nil {`)
	require.Contains(t, got, `if out.NAMES, err = decodeEach(root.At(5), "NAMES", decodeASCII); err != nil {`)
	require.Contains(t, got, `if out.FLAGS, err = decodeEach(root.At(6), "FLAGS", decodeBool); err != nil {`)
	require.Contains(t, got, "// It is filled by [DecodeS12F1] and mirrors the parameters of [S12F1], in order.\n")
}

// TestRenderValueRepeatBuilder proves a value repeat's builder takes values of the element's Go type,
// sends each through valueItems as an item of the element's format,
// converting a named type back to its goType,
// and that its godoc has no paragraph about the elements.
func TestRenderValueRepeatBuilder(t *testing.T) {
	src, err := renderMessages(optionalMessage(valueBody()), valueItemsFixture())
	require.NoError(t, err)

	got := string(src)
	require.Contains(t, got, "func S12F1(locs []uint16, mdln string, acks []COMMACK, svids []secs2.Item, actions []CARRIERACTION, names []string, flags ...bool) secs2.SECS2Message {")
	require.Contains(t, got, "secs2.L(valueItems(locs, func(v uint16) secs2.Item { return secs2.U2(v) })...)")
	require.Contains(t, got, "secs2.L(valueItems(acks, func(v COMMACK) secs2.Item { return secs2.B(byte(v)) })...)")
	require.Contains(t, got, "secs2.L(svids...)")
	require.Contains(t, got, "secs2.L(valueItems(actions, func(v CARRIERACTION) secs2.Item { return secs2.A(string(v)) })...)")
	require.Contains(t, got, "secs2.L(valueItems(names, secs2.A)...)")
	require.Contains(t, got, "secs2.L(valueItems(flags, func(v bool) secs2.Item { return secs2.BOOLEAN(v) })...)")
	require.NotContains(t, got, "Each element of")
}

// TestRenderTestsValueRepeat proves the builder test and the round trip send each value repeat one bare sample value,
// qualifying a named element type in a slice literal,
// and check it as a value rather than as raw item bytes.
func TestRenderTestsValueRepeat(t *testing.T) {
	items := valueItemsFixture()

	exprs, err := SampleExprs(valueBody(), items)
	require.NoError(t, err)
	require.Equal(t, intLiteral("locs"), exprs[0])
	require.Equal(t, "0x"+byteHex("acks"), exprs[2])
	require.Equal(t, `secs2.A("svids")`, exprs[3])
	require.Equal(t, `"actions"`, exprs[4])
	require.Equal(t, "true", exprs[6])

	builderTests, err := renderTests(optionalMessage(valueBody()), items)
	require.NoError(t, err)

	got := string(builderTests)
	require.Contains(t, got, "gem.S12F1([]uint16{"+intLiteral("locs")+`}, "mdln", []gem.COMMACK{0x`+byteHex("acks")+`}, []secs2.Item{secs2.A("svids")}, []gem.CARRIERACTION{"actions"}, []string{"names"}, true)`)

	body := testFuncBody(t, got, "TestS12F1")
	require.Contains(t, body, "v3, err := list2[0].ToUint()")
	require.Contains(t, body, `v10, err := list9[0].ToASCII()`)

	decodeTests, err := renderDecodeTests(optionalMessage(valueBody()), items)
	require.NoError(t, err)
	require.Contains(t, string(decodeTests), "\twant := gem.S12F1Body{\n"+
		"\t\tLOCS:    []uint16{"+intLiteral("locs")+"},\n"+
		"\t\tMDLN:    \"mdln\",\n"+
		"\t\tACKS:    []gem.COMMACK{0x"+byteHex("acks")+"},\n"+
		"\t\tSVIDS:   []secs2.Item{secs2.A(\"svids\")},\n"+
		"\t\tACTIONS: []gem.CARRIERACTION{\"actions\"},\n"+
		"\t\tNAMES:   []string{\"names\"},\n"+
		"\t\tFLAGS:   []bool{true},\n"+
		"\t}\n\n\tassertDecoded(t, got, want)\n")
}

// byteHex renders the byte sample of name as two hex digits, as the generated tests spell it.
func byteHex(name string) string {
	return fixedSamples["byte"].literal(name)[2:]
}

// TestRenderDecodersRejectsValueRepeatOfNamedNonString proves
// a value repeat of a named enum type decodes through decodeASCIIAs only when the item is format A:
// a named type whose helper returns a plain bool has no element decoder,
// so generation fails rather than emitting a string decoder for a BOOLEAN item.
func TestRenderDecodersRejectsValueRepeatOfNamedNonString(t *testing.T) {
	items := valueItemsFixture()
	items["FLAGENUM"] = Item{
		Formats: []string{"BOOLEAN"}, Binding: BindingFixed, GoType: "bool",
		Values: []ItemValue{{Name: "On", Value: IntValue(1)}},
	}

	structure := &StructureNode{Type: "list", Repeat: "flags", Of: &StructureNode{Item: "FLAGENUM"}}

	_, err := renderDecoders(optionalMessage(structure), items)
	require.ErrorContains(t, err, "repeat flags: item FLAGENUM: no decoder for a named bool type of format BOOLEAN")
}

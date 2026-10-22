package main

import (
	"maps"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// optionalItems extends testItems with one item of each kind an optional: rule distinguishes.
func optionalItems() map[string]Item {
	items := testItems()
	maps.Copy(items, map[string]Item{
		"FNLOC": {Formats: []string{"U2"}, Binding: BindingFixed, GoType: "uint16"},
		"ACKA":  {Formats: []string{"BOOLEAN"}, Binding: BindingFixed, GoType: "bool"},
		"BLOB":  {Formats: []string{"B"}, Binding: BindingFixed, GoType: "[]byte"},
		"COMMACK": {
			Formats: []string{"B"}, Binding: BindingFixed, GoType: "byte",
			Values: []ItemValue{{Name: "Accepted", Value: IntValue(0)}},
		},
	})

	return items
}

// optionalMessage wraps one body structure in a stream-12 messages file.
func optionalMessage(structure *StructureNode) MessageFile {
	return MessageFile{Stream: 12, Messages: []Message{{
		Function: 1, Name: "Optional Test", Direction: "equipment-to-host", Source: "e5",
		Bodies: []Body{{Actor: "both", Structure: structure}},
	}}}
}

// TestValidateMessagesRejectsBadOptional proves optional: is accepted only on a fixed scalar leaf outside any of,
// and not on an ASCII leaf, whose empty string already is the zero-length item.
func TestValidateMessagesRejectsBadOptional(t *testing.T) {
	tests := []struct {
		name      string
		structure string
		wantErr   string
	}{
		{"on a list", `{type: list, optional: true, items: [{item: FNLOC}]}`, "only on a leaf"},
		{"on a repeat", `{type: list, repeat: svids, optional: true, of: {item: SVID}}`, "only on a leaf"},
		{"on an opaque node", `{type: list, items: [{type: opaque, optional: true}]}`, "only on a leaf"},
		{"open item", `{type: list, items: [{item: SVID, optional: true}]}`, "binding: fixed"},
		{"byte slice", `{type: list, items: [{item: BLOB, optional: true}]}`, "scalar goType"},
		{"byte array", `{type: list, items: [{item: MHEAD, optional: true}]}`, "scalar goType"},
		{"ASCII", `{type: list, items: [{item: MDLN, optional: true}]}`, "an empty string already sends the zero-length item"},
		{"inside repeat of", `{type: list, repeat: locs, of: {item: FNLOC, optional: true}}`, "inside a repeat"},
		{
			"inside a list in repeat of",
			`{type: list, repeat: locs, of: {type: list, items: [{item: FNLOC, optional: true}]}}`,
			"declare it in groups.yaml",
		},
		{"inside packed of", `{type: list, packed: tsips, of: {item: TSIP, optional: true}}`, "inside a repeat"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mf, err := LoadMessageFile([]byte(namesMessages(tt.structure)))
			require.NoError(t, err)

			err = ValidateMessages([]MessageFile{mf}, optionalItems())
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestValidateMessagesAcceptsOptional proves every leaf shape optional: is meant for passes validation.
func TestValidateMessagesAcceptsOptional(t *testing.T) {
	tests := []struct {
		name      string
		structure string
	}{
		{"numeric", `{type: list, items: [{item: FNLOC, optional: true}, {item: MDLN}]}`},
		{"boolean, byte and enum", `{type: list, items: [{item: ACKA, optional: true}, {item: TSIP, optional: true}, {item: COMMACK, optional: true}]}`},
		{"whole body", `{item: FNLOC, optional: true}`},
		{"bounded tail", `{type: list, minItems: 1, maxItems: 2, items: [{item: MDLN}, {item: FNLOC, optional: true}]}`},
		{"before a repeat", `{type: list, items: [{item: FNLOC, optional: true}, {type: list, repeat: svids, of: {item: SVID}}]}`},
		{"renamed", `{type: list, items: [{item: FNLOC, optional: true}, {item: FNLOC, as: fnlocB, optional: true}]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mf, err := LoadMessageFile([]byte(namesMessages(tt.structure)))
			require.NoError(t, err)

			require.NoError(t, ValidateMessages([]MessageFile{mf}, optionalItems()))
		})
	}
}

// optionalTailBody has an optional leaf before an ordinary leaf,
// and one in a bounded list's optional tail followed by an ordinary leaf,
// with one more leaf after that list.
func optionalTailBody() *StructureNode {
	return &StructureNode{Type: "list", Items: []StructureNode{
		{Item: "FNLOC", Optional: true},
		{Item: "MDLN"},
		{Type: "list", MinItems: new(1), MaxItems: new(3), Items: []StructureNode{
			{Item: "SOFTREV"},
			{Item: "COMMACK", Optional: true},
			{Item: "TSIP"},
		}},
		{Item: "ACKA"},
	}}
}

// TestBuildFieldsOptionalLeaves proves an optional leaf's builder parameter and decoder field are pointers,
// and that fields keep mirroring builder parameters in order, also past a bounded list's optional tail.
func TestBuildFieldsOptionalLeaves(t *testing.T) {
	items := optionalItems()
	structure := optionalTailBody()

	params := BuildParams(structure, items)
	require.Equal(t, []Param{
		{Name: "fnloc", Type: "*uint16"},
		{Name: "mdln", Type: "string"},
		{Name: "softrev", Type: "string"},
		{Name: "commack", Type: "*COMMACK"},
		{Name: "tsip", Type: "byte"},
		{Name: "acka", Type: "bool"},
	}, params)

	fields, err := BuildFields(structure, items)
	require.NoError(t, err)
	require.Equal(t, []decodeField{
		{Name: "FNLOC", Type: "*uint16", Param: 0, Path: []int{0}},
		{Name: "MDLN", Type: "string", Param: 1, Path: []int{1}},
		{Name: "SOFTREV", Type: "string", Param: 2, Path: []int{2, 0}},
		{Name: "COMMACK", Type: "*COMMACK", Param: 3, Path: []int{2, 1}},
		{Name: "TSIP", Type: "byte", Param: 4, Path: []int{2, 2}},
		{Name: "ACKA", Type: "bool", Param: 5, Path: []int{3}},
	}, fields)

	requireFieldsMirrorParams(t, "optional", params, fields)

	require.Equal(t,
		"secs2.L(optionalItem(fnloc, func(v uint16) secs2.Item { return secs2.U2(v) }, secs2.U2), secs2.A(mdln), "+
			"secs2.L(secs2.A(softrev), optionalItem(commack, func(v COMMACK) secs2.Item { return secs2.B(byte(v)) }, secs2.B), secs2.B(tsip)), "+
			"secs2.BOOLEAN(acka))",
		BodyExpr(structure, items))
}

// TestBuildFieldsRejectsUnvalidatedOptional proves the decoder walk refuses an optional leaf validation would have rejected,
// rather than decoding it as if it were not optional.
func TestBuildFieldsRejectsUnvalidatedOptional(t *testing.T) {
	for item, want := range map[string]string{
		"SVID":  "must be binding: fixed",
		"BLOB":  "must have a scalar goType",
		"MHEAD": "must have a scalar goType",
		"MDLN":  "an empty string already sends the zero-length item",
	} {
		t.Run(item, func(t *testing.T) {
			_, err := BuildFields(&StructureNode{Type: "list", Items: []StructureNode{{Item: item, Optional: true}}}, optionalItems())
			require.ErrorContains(t, err, want)
		})
	}
}

// TestRenderDecodersOptionalLeaf proves an optional leaf decodes into a pointer field through decodeOptional,
// given the item type the leaf's own helper reads, and that helper.
func TestRenderDecodersOptionalLeaf(t *testing.T) {
	items := optionalItems()
	structure := &StructureNode{Type: "list", Items: []StructureNode{
		{Item: "FNLOC", Optional: true},
		{Item: "ACKA", Optional: true},
		{Item: "COMMACK", Optional: true},
		{Item: "SOFTREV"},
	}}

	src, err := renderDecoders(optionalMessage(structure), items)
	require.NoError(t, err)

	got := string(src)
	require.Regexp(t, `\tFNLOC +\*uint16\n\tACKA +\*bool\n\tCOMMACK +\*COMMACK\n\tSOFTREV +string\n`, got)
	require.Contains(t, got, `if out.FNLOC, err = decodeOptional[*secs2.UintItem](root.At(0), "FNLOC", decodeUintAs[uint16]); err != nil {`)
	require.Contains(t, got, `if out.ACKA, err = decodeOptional[*secs2.BooleanItem](root.At(1), "ACKA", decodeBool); err != nil {`)
	require.Contains(t, got, `if out.COMMACK, err = decodeOptional[*secs2.BinaryItem](root.At(2), "COMMACK", decodeByteAs[COMMACK]); err != nil {`)
	require.Contains(t, got, `if out.SOFTREV, err = decodeASCII(root.At(3), "SOFTREV"); err != nil {`)
	require.NotContains(t, got, "Present")
}

// TestOptionalLeafGodoc proves the godoc of a body with optional leaves says the items may be sent zero-length
// and that nil stands for that form, on both the builder and the result type,
// and that a body without one says nothing about it.
func TestOptionalLeafGodoc(t *testing.T) {
	items := optionalItems()

	builders, err := renderMessages(optionalMessage(optionalTailBody()), items)
	require.NoError(t, err)
	require.Contains(t, string(builders), "// SEMI E5 lets FNLOC and COMMACK each be sent as a zero-length item:\n"+
		"// a nil argument sends that form.\n")

	decoders, err := renderDecoders(optionalMessage(optionalTailBody()), items)
	require.NoError(t, err)
	require.Contains(t, string(decoders), "// It is filled by [DecodeS12F1] and mirrors the parameters of [S12F1], in order.\n"+
		"//\n"+
		"// SEMI E5 lets FNLOC and COMMACK each be sent as a zero-length item:\n"+
		"// [DecodeS12F1] decodes that form as a nil field, and [S12F1] sends it for a nil argument.\n")

	single := &StructureNode{Type: "list", Items: []StructureNode{{Item: "FNLOC", Optional: true}}}
	decoders, err = renderDecoders(optionalMessage(single), items)
	require.NoError(t, err)
	require.Contains(t, string(decoders), "// [DecodeS12F1] decodes that form as a nil FNLOC, and [S12F1] sends it for a nil argument.\n")

	plain := &StructureNode{Type: "list", Items: []StructureNode{{Item: "FNLOC"}}}
	builders, err = renderMessages(optionalMessage(plain), items)
	require.NoError(t, err)
	require.NotContains(t, string(builders), "zero-length")

	decoders, err = renderDecoders(optionalMessage(plain), items)
	require.NoError(t, err)
	require.NotContains(t, string(decoders), "zero-length")
	require.Contains(t, string(decoders), "// It is filled by [DecodeS12F1] and mirrors the parameters of [S12F1], in order.\n")
}

// TestOptionalSummary pins how the godoc lists one, two, and more optional items.
func TestOptionalSummary(t *testing.T) {
	require.Equal(t, "SEMI E5 lets A be sent as a zero-length item", optionalSummary([]string{"A"}))
	require.Equal(t, "SEMI E5 lets A and B each be sent as a zero-length item", optionalSummary([]string{"A", "B"}))
	require.Equal(t, "SEMI E5 lets A, B, and C each be sent as a zero-length item", optionalSummary([]string{"A", "B", "C"}))
}

// TestRenderDecodeTestsOptionalLeaf proves the round trip sends and expects a pointer to each optional sample,
// and that each optional leaf gets a test that passes nil for it,
// checks the built item is the zero-length item of its format,
// and expects it nil and the other fields intact.
func TestRenderDecodeTestsOptionalLeaf(t *testing.T) {
	src, err := renderDecodeTests(optionalMessage(optionalTailBody()), optionalItems())
	require.NoError(t, err)

	got := string(src)
	require.Contains(t, got, "\t\"bytes\"\n")

	round := testFuncBody(t, got, "TestDecodeS12F1RoundTrip")
	require.Regexp(t, `msg := gem\.S12F1\(new\(uint16\(\d+\)\), "mdln", "softrev", new\(gem\.COMMACK\(0x[0-9a-f]{2}\)\), 0x[0-9a-f]{2}, true\)`, round)
	require.Regexp(t, `\t\tFNLOC: +new\(uint16\(\d+\)\),\n`, round)
	require.Regexp(t, `\t\tCOMMACK: +new\(gem\.COMMACK\(0x[0-9a-f]{2}\)\),\n`, round)
	require.Contains(t, round, "assertDecoded(t, got, want)")

	nested := testFuncBody(t, got, "TestDecodeS12F1COMMACKAbsent")
	require.Regexp(t, `msg := gem\.S12F1\(new\(uint16\(\d+\)\), "mdln", "softrev", nil, 0x[0-9a-f]{2}, true\)`, nested)
	require.Contains(t, nested, "decoded, err := secs2.Decode(msg.Item().ToBytes())")
	require.Contains(t, nested, ":= secs2.B().ToBytes()")
	require.Contains(t, nested, `t.Errorf("commack: got %x, want the zero-length item %x"`)
	require.Contains(t, nested, "got, err := gem.DecodeS12F1(decoded)")
	require.NotContains(t, nested, "COMMACK:")
	require.Regexp(t, `\t\tFNLOC: +new\(uint16\(\d+\)\),\n`, nested)
	require.Contains(t, nested, "\t\tSOFTREV: \"softrev\",\n")

	top := testFuncBody(t, got, "TestDecodeS12F1FNLOCAbsent")
	require.Contains(t, top, `msg := gem.S12F1(nil, "mdln"`)
	require.Contains(t, top, ":= secs2.U2().ToBytes()")
	require.NotContains(t, top, "FNLOC:")
	require.Regexp(t, `\t\tCOMMACK: +new\(gem\.COMMACK\(0x[0-9a-f]{2}\)\),\n`, top)

	whole, err := renderDecodeTests(optionalMessage(&StructureNode{Item: "ACKA", Optional: true}), optionalItems())
	require.NoError(t, err)

	body := testFuncBody(t, string(whole), "TestDecodeS12F1ACKAAbsent")
	require.Contains(t, body, "msg := gem.S12F1(nil)")
	require.Contains(t, body, ":= secs2.BOOLEAN().ToBytes()")
	require.Contains(t, body, "assertDecoded(t, got, gem.S12F1Body{})")

	round = testFuncBody(t, string(whole), "TestDecodeS12F1RoundTrip")
	require.Contains(t, round, "msg := gem.S12F1(new(true))")
}

// TestRenderVariantTestOptionalLeaf proves a variant's generated test expects the main decoder's pointer field
// to point to the variant's sample when the variant sends that item, as a plain or as an optional leaf,
// and to stay nil when the variant omits it.
func TestRenderVariantTestOptionalLeaf(t *testing.T) {
	main := &StructureNode{Type: "list", MinItems: new(1), MaxItems: new(3), Items: []StructureNode{
		{Item: "MDLN"},
		{Item: "FNLOC", Optional: true},
		{Item: "ACKA", Optional: true},
	}}
	mf := MessageFile{Stream: 12, Messages: []Message{{
		Function: 1, Name: "Optional Test", Direction: "equipment-to-host", Source: "e5",
		Bodies: []Body{
			{Actor: "both", Structure: main},
			{Actor: "both", Variant: "Short", Structure: &StructureNode{Type: "list", Items: []StructureNode{{Item: "MDLN"}}}},
			{Actor: "both", Variant: "Plain", Structure: &StructureNode{Type: "list", Items: []StructureNode{{Item: "MDLN"}, {Item: "FNLOC"}, {Item: "ACKA", Optional: true}}}},
		},
	}}}
	require.NoError(t, ValidateMessages([]MessageFile{mf}, optionalItems()))

	src, err := renderTests(mf, optionalItems())
	require.NoError(t, err)

	short := testFuncBody(t, string(src), "TestS12F1Short")
	require.Contains(t, short, "\twant := gem.S12F1Body{\n\t\tMDLN: \"mdln\",\n\t}\n")

	plain := testFuncBody(t, string(src), "TestS12F1Plain")
	require.Regexp(t, `msg := gem\.S12F1Plain\("mdln", \d+, new\(true\)\)`, plain)
	require.Regexp(t, `\t\tFNLOC: new\(uint16\(\d+\)\),\n`, plain)
	require.Contains(t, plain, "\t\tACKA:  new(true),\n")
}

// testFuncBody returns the source of the generated test function name,
// from its signature up to its closing brace.
func testFuncBody(t *testing.T, src string, name string) string {
	t.Helper()

	start := strings.Index(src, "func "+name+"(t *testing.T) {")
	require.GreaterOrEqualf(t, start, 0, "no test %s in:\n%s", name, src)

	rest := src[start:]
	if before, _, ok := strings.Cut(rest, "\n}\n"); ok {
		return before
	}

	return rest
}

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
		"FNLOC":        {Formats: []string{"U2"}, Binding: BindingFixed, GoType: "uint16"},
		"FNLOCPRESENT": {Formats: []string{"U2"}, Binding: BindingFixed, GoType: "uint16"},
		"ACKA":         {Formats: []string{"BOOLEAN"}, Binding: BindingFixed, GoType: "bool"},
		"BLOB":         {Formats: []string{"B"}, Binding: BindingFixed, GoType: "[]byte"},
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
// and that a presence field may not collide with another field.
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
		{"inside repeat of", `{type: list, repeat: locs, of: {item: FNLOC, optional: true}}`, "inside a repeat"},
		{
			"inside a list in repeat of",
			`{type: list, repeat: locs, of: {type: list, items: [{item: FNLOC, optional: true}]}}`,
			"declare it in groups.yaml",
		},
		{"inside packed of", `{type: list, packed: tsips, of: {item: TSIP, optional: true}}`, "inside a repeat"},
		{"presence field collides", `{type: list, items: [{item: FNLOC, optional: true}, {item: FNLOCPRESENT}]}`, "collide"},
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
		{"ascii, boolean, byte and enum", `{type: list, items: [{item: MDLN, optional: true}, {item: ACKA, optional: true}, {item: TSIP, optional: true}, {item: COMMACK, optional: true}]}`},
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

// TestBuildFieldsOptionalLeaves proves an optional leaf adds a presence field right after its value field,
// and that value fields keep mirroring builder parameters in order, also past a bounded list's optional tail.
func TestBuildFieldsOptionalLeaves(t *testing.T) {
	items := optionalItems()
	structure := optionalTailBody()

	fields, err := BuildFields(structure, items)
	require.NoError(t, err)
	require.Equal(t, []decodeField{
		{Name: "FNLOC", Type: "uint16", Param: 0, Path: []int{0}},
		{Name: "FNLOCPresent", Type: "bool", Param: -1, Presence: true, Path: []int{0}},
		{Name: "MDLN", Type: "string", Param: 1, Path: []int{1}},
		{Name: "SOFTREV", Type: "string", Param: 2, Path: []int{2, 0}},
		{Name: "COMMACK", Type: "COMMACK", Param: 3, Path: []int{2, 1}},
		{Name: "COMMACKPresent", Type: "bool", Param: -1, Presence: true, Path: []int{2, 1}},
		{Name: "TSIP", Type: "byte", Param: 4, Path: []int{2, 2}},
		{Name: "ACKA", Type: "bool", Param: 5, Path: []int{3}},
	}, fields)

	requireFieldsMirrorParams(t, "optional", BuildParams(structure, items), fields)

	optionals, err := optionalLeaves(structure, items)
	require.NoError(t, err)
	require.Equal(t, []optionalLeaf{
		{Field: "FNLOC", Path: []int{0}, Empty: "secs2.U2()"},
		{Field: "COMMACK", Path: []int{2, 1}, Empty: "secs2.B()"},
	}, optionals)
}

// TestBuildFieldsRejectsUnvalidatedOptional proves the decoder walk refuses an optional leaf validation would have rejected,
// rather than decoding it as if it were not optional.
func TestBuildFieldsRejectsUnvalidatedOptional(t *testing.T) {
	for _, item := range []string{"SVID", "BLOB", "MHEAD"} {
		t.Run(item, func(t *testing.T) {
			_, err := BuildFields(&StructureNode{Type: "list", Items: []StructureNode{{Item: item, Optional: true}}}, optionalItems())
			require.ErrorContains(t, err, "optional needs a binding: fixed item with a scalar goType")
		})
	}
}

// TestRenderDecodersOptionalLeaf proves an optional leaf decodes through presentAt,
// given the item type the leaf's own helper reads,
// and reads the value only when the item is present.
func TestRenderDecodersOptionalLeaf(t *testing.T) {
	items := optionalItems()
	structure := &StructureNode{Type: "list", Items: []StructureNode{
		{Item: "FNLOC", Optional: true},
		{Item: "MDLN", Optional: true},
		{Item: "ACKA", Optional: true},
		{Item: "COMMACK", Optional: true},
		{Item: "SOFTREV"},
	}}

	src, err := renderDecoders(optionalMessage(structure), items)
	require.NoError(t, err)

	got := string(src)
	require.Regexp(t, `\tFNLOC +uint16\n\tFNLOCPresent +bool\n`, got)
	require.Contains(t, got, `if out.FNLOCPresent, err = presentAt[*secs2.UintItem](root.At(0), "FNLOC"); err != nil {`)
	require.Contains(t, got, "if out.FNLOCPresent {\n\t\tif out.FNLOC, err = decodeUintAs[uint16](root.At(0), \"FNLOC\"); err != nil {")
	require.Contains(t, got, `presentAt[*secs2.ASCIIItem](root.At(1), "MDLN")`)
	require.Contains(t, got, `presentAt[*secs2.BooleanItem](root.At(2), "ACKA")`)
	require.Contains(t, got, `presentAt[*secs2.BinaryItem](root.At(3), "COMMACK")`)
	require.Contains(t, got, `out.COMMACK, err = decodeByteAs[COMMACK](root.At(3), "COMMACK")`)
	require.Contains(t, got, `if out.SOFTREV, err = decodeASCII(root.At(4), "SOFTREV"); err != nil {`)
	require.NotContains(t, got, "SOFTREVPresent")
}

// TestOptionalLeafGodoc proves the godoc of a body with optional leaves says the items may be sent zero-length,
// on both the builder and the result type,
// and that a body without one says nothing about it.
func TestOptionalLeafGodoc(t *testing.T) {
	items := optionalItems()

	builders, err := renderMessages(optionalMessage(optionalTailBody()), items)
	require.NoError(t, err)
	require.Contains(t, string(builders), "// SEMI E5 lets FNLOC and COMMACK each be sent as a zero-length item,\n"+
		"// but this builder always sends a value:\n"+
		"// callers that need the zero-length form build the body with [secs2] directly.\n")

	decoders, err := renderDecoders(optionalMessage(optionalTailBody()), items)
	require.NoError(t, err)
	require.Contains(t, string(decoders), "// It is filled by [DecodeS12F1] and mirrors the parameters of [S12F1], in order,\n"+
		"// apart from its FNLOCPresent and COMMACKPresent fields.\n"+
		"//\n"+
		"// SEMI E5 lets FNLOC and COMMACK each be sent as a zero-length item:\n"+
		"// [DecodeS12F1] decodes that form as a zero field with its presence field false,\n"+
		"// FNLOCPresent for FNLOC and COMMACKPresent for COMMACK.\n"+
		"// [S12F1] always sends a value,\n"+
		"// so callers that need the zero-length form build the body with [secs2] directly.\n")

	single := &StructureNode{Type: "list", Items: []StructureNode{{Item: "FNLOC", Optional: true}}}
	decoders, err = renderDecoders(optionalMessage(single), items)
	require.NoError(t, err)
	require.Contains(t, string(decoders), "// It is filled by [DecodeS12F1] and mirrors the parameters of [S12F1], in order,\n"+
		"// apart from its FNLOCPresent field.\n")
	require.Contains(t, string(decoders), "// [DecodeS12F1] decodes that form as a zero FNLOC with FNLOCPresent false.\n")

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

// TestRenderDecodeTestsOptionalLeaf proves the round trip expects every presence field true,
// and that each optional leaf gets a test sending it zero-length,
// which rebuilds the lists on the leaf's path and expects that leaf absent and the other fields intact.
func TestRenderDecodeTestsOptionalLeaf(t *testing.T) {
	src, err := renderDecodeTests(optionalMessage(optionalTailBody()), optionalItems())
	require.NoError(t, err)

	got := string(src)
	round := testFuncBody(t, got, "TestDecodeS12F1RoundTrip")
	require.Contains(t, round, "\t\tFNLOCPresent:   true,\n")
	require.Contains(t, round, "\t\tCOMMACKPresent: true,\n")
	require.Contains(t, round, "\t\tMDLN:           \"mdln\",\n")
	require.Contains(t, round, "\t\tTSIP:           0x")
	require.Contains(t, round, "assertDecoded(t, got, want)")

	nested := testFuncBody(t, got, "TestDecodeS12F1COMMACKAbsent")
	require.Contains(t, nested, "list0, err := full.ToList()")
	require.Contains(t, nested, "list1, err := list0[2].ToList()")
	require.Contains(t, nested, "list1[1] = secs2.B()\n\tlist0[2] = secs2.L(list1...)\n")
	require.Contains(t, nested, "body, err := secs2.Decode(secs2.L(list0...).ToBytes())")
	require.NotContains(t, nested, "COMMACK:")
	require.NotContains(t, nested, "COMMACKPresent:")
	require.Contains(t, nested, "\t\tFNLOCPresent: true,\n")
	require.Contains(t, nested, "\t\tSOFTREV:      \"softrev\",\n")

	top := testFuncBody(t, got, "TestDecodeS12F1FNLOCAbsent")
	require.Contains(t, top, "list0[0] = secs2.U2()\n\n\tbody, err := secs2.Decode(secs2.L(list0...).ToBytes())")
	require.NotContains(t, top, "FNLOC:")
	require.NotContains(t, top, "FNLOCPresent:")
	require.Contains(t, top, "\t\tCOMMACKPresent: true,\n")

	whole, err := renderDecodeTests(optionalMessage(&StructureNode{Item: "MDLN", Optional: true}), optionalItems())
	require.NoError(t, err)

	body := testFuncBody(t, string(whole), "TestDecodeS12F1MDLNAbsent")
	require.Contains(t, body, `body, err := secs2.Decode(secs2.A("").ToBytes())`)
	require.NotContains(t, body, "msg :=")
	require.Contains(t, body, "assertDecoded(t, got, gem.S12F1Body{})")
}

// TestRenderVariantTestOptionalLeaf proves a variant's generated test expects the main decoder's presence field true
// when the variant sends that item and false when it omits it.
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
	require.Contains(t, plain, "\t\tFNLOCPresent: true,\n")
	require.Contains(t, plain, "\t\tACKAPresent:  true,\n")
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

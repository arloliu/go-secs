package main

import (
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
)

// typedItems extends optionalItems with a fixed string enumeration,
// so every element kind a typed repeat decodes differently is at hand:
// a plain number, a byte enum, a string enum, a plain string, and a boolean.
func typedItems() map[string]Item {
	items := optionalItems()
	maps.Copy(items, map[string]Item{
		"CARRIERACTION": {
			Formats: []string{"A"}, Binding: BindingFixed, GoType: "string",
			Values: []ItemValue{{Name: "ProceedWithCarrier", Value: StringValue("ProceedWithCarrier")}},
		},
	})

	return items
}

// TestValidateMessagesRejectsBadTyped proves typed: is accepted only on a repeat of one fixed scalar leaf,
// outside any of.
func TestValidateMessagesRejectsBadTyped(t *testing.T) {
	tests := []struct {
		name      string
		structure string
		wantErr   string
	}{
		{"on a leaf", `{type: list, items: [{item: FNLOC, typed: true}]}`, "only on a repeat"},
		{"on a fixed list", `{type: list, typed: true, items: [{item: FNLOC}]}`, "only on a repeat"},
		{"on a bounded list", `{type: list, typed: true, minItems: 0, maxItems: 1, items: [{item: FNLOC}]}`, "only on a repeat"},
		{"on a packed group", `{type: list, packed: tsips, typed: true, of: {item: TSIP}}`, "only on a repeat"},
		{"on an opaque node", `{type: list, items: [{type: opaque, typed: true}]}`, "only on a repeat"},
		{"of is a list", `{type: list, repeat: pairs, typed: true, of: {type: list, items: [{item: FNLOC}, {item: MDLN}]}}`, "single item leaf"},
		{"of is opaque", `{type: list, repeat: specs, typed: true, of: {type: opaque}}`, "single item leaf"},
		{"of is an open item", `{type: list, repeat: svids, typed: true, of: {item: SVID}}`, "binding: fixed"},
		{"of is a byte slice", `{type: list, repeat: blobs, typed: true, of: {item: BLOB}}`, "scalar goType"},
		{"of is a byte array", `{type: list, repeat: heads, typed: true, of: {item: MHEAD}}`, "scalar goType"},
		{
			"inside a repeat of",
			`{type: list, repeat: groups, of: {type: list, items: [{type: list, repeat: locs, typed: true, of: {item: FNLOC}}]}}`,
			"inside a repeat",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mf, err := LoadMessageFile([]byte(namesMessages(tt.structure)))
			require.NoError(t, err)

			err = ValidateMessages([]MessageFile{mf}, typedItems())
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestValidateMessagesAcceptsTyped proves every element kind typed: is meant for passes validation,
// as a final variadic repeat, a non-final one, and the whole body.
func TestValidateMessagesAcceptsTyped(t *testing.T) {
	tests := []struct {
		name      string
		structure string
	}{
		{"numeric, final", `{type: list, items: [{item: MDLN}, {type: list, repeat: locs, typed: true, of: {item: FNLOC}}]}`},
		{"byte enum, not final", `{type: list, items: [{type: list, repeat: acks, typed: true, of: {item: COMMACK}}, {item: MDLN}]}`},
		{"string enum", `{type: list, repeat: actions, typed: true, of: {item: CARRIERACTION}}`},
		{"string and boolean", `{type: list, items: [{type: list, repeat: names, typed: true, of: {item: MDLN}}, {type: list, repeat: flags, typed: true, of: {item: ACKA}}]}`},
		{"renamed element", `{type: list, repeat: locs, typed: true, of: {item: FNLOC, as: loc}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mf, err := LoadMessageFile([]byte(namesMessages(tt.structure)))
			require.NoError(t, err)

			require.NoError(t, ValidateMessages([]MessageFile{mf}, typedItems()))
		})
	}
}

// typedBody has a typed repeat of each element kind,
// with an untyped repeat and a plain leaf between them.
func typedBody() *StructureNode {
	return &StructureNode{Type: "list", Items: []StructureNode{
		{Type: "list", Repeat: "locs", Typed: true, Of: &StructureNode{Item: "FNLOC"}},
		{Item: "MDLN"},
		{Type: "list", Repeat: "acks", Typed: true, Of: &StructureNode{Item: "COMMACK"}},
		{Type: "list", Repeat: "svids", Of: &StructureNode{Item: "SVID"}},
		{Type: "list", Repeat: "actions", Typed: true, Of: &StructureNode{Item: "CARRIERACTION"}},
		{Type: "list", Repeat: "flags", Typed: true, Of: &StructureNode{Item: "ACKA"}},
	}}
}

// TestBuildFieldsTypedRepeat proves a typed repeat's field is a slice of the element's Go type,
// or of its named type for an enumeration,
// that it records the element's goType,
// and that an untyped repeat keeps raw items.
func TestBuildFieldsTypedRepeat(t *testing.T) {
	items := typedItems()
	structure := typedBody()

	fields, err := BuildFields(structure, items)
	require.NoError(t, err)
	require.Equal(t, []decodeField{
		{Name: "LOCS", Type: "[]uint16", Param: 0, Elem: "uint16", Path: []int{0}},
		{Name: "MDLN", Type: "string", Param: 1, Path: []int{1}},
		{Name: "ACKS", Type: "[]COMMACK", Param: 2, Elem: "byte", Path: []int{2}},
		{Name: "SVIDS", Type: "[]secs2.Item", Param: 3, Path: []int{3}},
		{Name: "ACTIONS", Type: "[]CARRIERACTION", Param: 4, Elem: "string", Path: []int{4}},
		{Name: "FLAGS", Type: "[]bool", Param: 5, Elem: "bool", Path: []int{5}},
	}, fields)

	requireFieldsMirrorParams(t, "typed", BuildParams(structure, items), fields)
}

// TestRenderDecodersTypedRepeat proves a typed repeat decodes element by element through decodeEach,
// with the helper a leaf of the element item uses,
// and that an untyped repeat still returns raw items.
func TestRenderDecodersTypedRepeat(t *testing.T) {
	src, err := renderDecoders(optionalMessage(typedBody()), typedItems())
	require.NoError(t, err)

	got := string(src)
	require.Regexp(t, `\tLOCS +\[\]uint16\n`, got)
	require.Regexp(t, `\tACKS +\[\]COMMACK\n`, got)
	require.Regexp(t, `\tACTIONS +\[\]CARRIERACTION\n`, got)
	require.Contains(t, got, `if out.LOCS, err = decodeEach(root.At(0), "LOCS", decodeUintAs[uint16]); err != nil {`)
	require.Contains(t, got, `if out.ACKS, err = decodeEach(root.At(2), "ACKS", decodeByteAs[COMMACK]); err != nil {`)
	require.Contains(t, got, `if out.SVIDS, err = decodeItems(root.At(3), "SVIDS"); err != nil {`)
	require.Contains(t, got, `if out.ACTIONS, err = decodeEach(root.At(4), "ACTIONS", decodeASCIIAs[CARRIERACTION]); err != nil {`)
	require.Contains(t, got, `if out.FLAGS, err = decodeEach(root.At(5), "FLAGS", decodeBool); err != nil {`)
}

// TestRenderTypedRepeatBuilder proves a typed repeat leaves the builder taking secs2 items,
// and that its godoc says the decoder returns the elements as values.
func TestRenderTypedRepeatBuilder(t *testing.T) {
	src, err := renderMessages(optionalMessage(typedBody()), typedItems())
	require.NoError(t, err)

	got := string(src)
	require.Contains(t, got, "func S12F1(locs []secs2.Item, mdln string, acks []secs2.Item, svids []secs2.Item, actions []secs2.Item, flags ...secs2.Item) secs2.SECS2Message {")
	require.Contains(t, got, "// Each element of acks is a [secs2.Item] holding one COMMACK of format B,\n"+
		"// which the caller builds with [secs2.B];\n"+
		"// the decoder returns the elements as values, in a []COMMACK.\n")
	require.Contains(t, got, "// Each element of locs is a [secs2.Item] holding one FNLOC of format U2,\n")
	require.NotContains(t, got, "Each element of svids")

	plain, err := renderMessages(optionalMessage(&StructureNode{Type: "list", Repeat: "svids", Of: &StructureNode{Item: "SVID"}}), typedItems())
	require.NoError(t, err)
	require.NotContains(t, string(plain), "Each element of")
}

// TestRenderTestsTypedRepeat proves the builder test and the round trip send each typed repeat
// one element of the element item's own format,
// and check it as a value rather than as raw item bytes.
func TestRenderTestsTypedRepeat(t *testing.T) {
	items := typedItems()

	exprs, err := SampleExprs(typedBody(), items)
	require.NoError(t, err)
	require.Equal(t, "secs2.U2("+intLiteral("locs")+")", exprs[0])
	require.Equal(t, `secs2.B(0x`+byteHex("acks")+`)`, exprs[2])
	require.Equal(t, `secs2.A("svids")`, exprs[3])
	require.Equal(t, `secs2.A("actions")`, exprs[4])
	require.Equal(t, "secs2.BOOLEAN(true)", exprs[5])

	builderTests, err := renderTests(optionalMessage(typedBody()), items)
	require.NoError(t, err)

	body := testFuncBody(t, string(builderTests), "TestS12F1")
	require.Contains(t, body, "v3, err := list2[0].ToUint()")
	require.Contains(t, body, `v10, err := list9[0].ToASCII()`)

	decodeTests, err := renderDecodeTests(optionalMessage(typedBody()), items)
	require.NoError(t, err)

	got := string(decodeTests)
	require.Contains(t, got, "if len(got.LOCS) != 1 || got.LOCS[0] != "+intLiteral("locs")+" {")
	require.Contains(t, got, "if len(got.ACKS) != 1 || got.ACKS[0] != 0x"+byteHex("acks")+" {")
	require.Contains(t, got, `if len(got.ACTIONS) != 1 || got.ACTIONS[0] != "actions" {`)
	require.Contains(t, got, "if len(got.FLAGS) != 1 || !got.FLAGS[0] {")
	require.Contains(t, got, "if len(got.SVIDS) != 1 {")
}

// byteHex renders the byte sample of name as two hex digits, as the generated tests spell it.
func byteHex(name string) string {
	return fixedSamples["byte"].literal(name)[2:]
}

// TestTypedRepeatResultTypeDoc proves the result type's godoc names each typed repeat's field
// as an exception to mirroring the builder's parameters,
// alone and after the presence fields of optional leaves.
func TestTypedRepeatResultTypeDoc(t *testing.T) {
	items := typedItems()

	tests := []struct {
		name      string
		structure *StructureNode
		want      string
	}{
		{
			"one typed repeat",
			&StructureNode{Type: "list", Items: []StructureNode{
				{Item: "MDLN"},
				{Type: "list", Repeat: "locs", Typed: true, Of: &StructureNode{Item: "FNLOC"}},
			}},
			"// It is filled by [DecodeS12F1] and mirrors the parameters of [S12F1], in order,\n" +
				"// except that LOCS holds decoded values where [S12F1] takes [secs2.Item] elements.\n",
		},
		{
			"several typed repeats",
			typedBody(),
			"// It is filled by [DecodeS12F1] and mirrors the parameters of [S12F1], in order,\n" +
				"// except that LOCS, ACKS, ACTIONS, and FLAGS hold decoded values where [S12F1] takes [secs2.Item] elements.\n",
		},
		{
			"typed repeat after an optional leaf",
			&StructureNode{Type: "list", Items: []StructureNode{
				{Item: "FNLOC", Optional: true},
				{Type: "list", Repeat: "acks", Typed: true, Of: &StructureNode{Item: "COMMACK"}},
			}},
			"// It is filled by [DecodeS12F1] and mirrors the parameters of [S12F1], in order,\n" +
				"// apart from its FNLOCPresent field,\n" +
				"// and except that ACKS holds decoded values where [S12F1] takes [secs2.Item] elements.\n" +
				"//\n" +
				"// SEMI E5 lets FNLOC be sent as a zero-length item:\n",
		},
		{
			"untyped repeat only",
			&StructureNode{Type: "list", Repeat: "svids", Of: &StructureNode{Item: "SVID"}},
			"// It is filled by [DecodeS12F1] and mirrors the parameters of [S12F1], in order.\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, err := renderDecoders(optionalMessage(tt.structure), items)
			require.NoError(t, err)
			require.Contains(t, string(src), tt.want)
		})
	}
}

// TestRenderDecodersRejectsTypedRepeatOfNamedNonString proves a typed repeat of a named enum type
// decodes through decodeASCIIAs only when the item is format A:
// a named type whose helper returns a plain bool has no element decoder, so generation fails
// rather than emitting a string decoder for a BOOLEAN item.
func TestRenderDecodersRejectsTypedRepeatOfNamedNonString(t *testing.T) {
	items := typedItems()
	items["FLAGENUM"] = Item{
		Formats: []string{"BOOLEAN"}, Binding: BindingFixed, GoType: "bool",
		Values: []ItemValue{{Name: "On", Value: IntValue(1)}},
	}

	structure := &StructureNode{Type: "list", Repeat: "flags", Typed: true, Of: &StructureNode{Item: "FLAGENUM"}}

	_, err := renderDecoders(optionalMessage(structure), items)
	require.ErrorContains(t, err, "typed repeat flags: item FLAGENUM: no decoder for a named bool type of format BOOLEAN")
}

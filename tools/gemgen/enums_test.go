package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// enumItems is an item dictionary with a fixed string enumeration and an open item with values.
func enumItems() map[string]Item {
	return map[string]Item{
		"CARRIERACTION": {Formats: []string{"A"}, Binding: BindingFixed, GoType: "string",
			Values: []ItemValue{{Name: "ProceedWithCarrier", Value: StringValue("ProceedWithCarrier")}}},
		"OBJTYPE": {Formats: []string{"A", "U1"}, Binding: BindingOpen,
			Values: []ItemValue{{Name: "Carrier", Value: StringValue("Carrier")}}},
	}
}

// enumStructure is a body with a string-enum leaf, the same item optional and renamed, and an open item with values.
func enumStructure() *StructureNode {
	return &StructureNode{Type: "list", Items: []StructureNode{
		{Item: "CARRIERACTION"},
		{Item: "CARRIERACTION", As: "next", Optional: true},
		{Item: "OBJTYPE"},
	}}
}

// TestStringEnumBuilder proves a fixed string enumeration types its builder parameter with the named type
// and converts it back to string for secs2.A,
// while an open item with values keeps a secs2.Item parameter.
func TestStringEnumBuilder(t *testing.T) {
	items := enumItems()

	require.Equal(t, []Param{
		{Name: "carrieraction", Type: "CARRIERACTION"},
		{Name: "next", Type: "CARRIERACTION"},
		{Name: "objtype", Type: "secs2.Item"},
	}, BuildParams(enumStructure(), items))

	require.Equal(t, "secs2.L(secs2.A(string(carrieraction)), secs2.A(string(next)), objtype)", BodyExpr(enumStructure(), items))
}

// TestStringEnumDecoder proves a fixed string enumeration decodes into a field of the named type,
// converting decodeASCII's string through a local both for a plain and for an optional leaf,
// while an open item with values decodes into a secs2.Item field.
func TestStringEnumDecoder(t *testing.T) {
	items := enumItems()

	fields, err := BuildFields(enumStructure(), items)
	require.NoError(t, err)
	require.Equal(t, []decodeField{
		{Name: "CARRIERACTION", Type: "CARRIERACTION", Param: 0, Path: []int{0}},
		{Name: "NEXT", Type: "CARRIERACTION", Param: 1, Path: []int{1}},
		{Name: "NEXTPresent", Type: "bool", Param: -1, Presence: true, Path: []int{1}},
		{Name: "OBJTYPE", Type: "secs2.Item", Param: 2, Path: []int{2}},
	}, fields)

	src := renderOneDecoder(t, 1, "both", enumStructure(), items)
	require.Contains(t, src, "\tvar s1 string\n"+
		"\tif s1, err = decodeASCII(root.At(0), \"CARRIERACTION\"); err != nil {\n"+
		"\t\treturn out, err\n"+
		"\t}\n\n"+
		"\tout.CARRIERACTION = CARRIERACTION(s1)\n")
	require.Contains(t, src, "\tif out.NEXTPresent {\n"+
		"\t\tvar s2 string\n"+
		"\t\tif s2, err = decodeASCII(root.At(1), \"NEXT\"); err != nil {\n"+
		"\t\t\treturn out, err\n"+
		"\t\t}\n\n"+
		"\t\tout.NEXT = CARRIERACTION(s2)\n"+
		"\t}\n")
	require.Contains(t, src, "if out.OBJTYPE, err = decodeItem(root.At(2), \"OBJTYPE\"); err != nil {")
}

// TestStringEnumTests proves the generated tests pass a bare string literal to a string-enum parameter,
// which an untyped constant may be, and compare the decoded field against it.
func TestStringEnumTests(t *testing.T) {
	mf := MessageFile{Stream: 1, Messages: []Message{{
		Function: 1, Name: "Sample", Mnemonic: "S",
		Direction: "bidirectional", Description: "A sample.", Exception: "None", Source: "e5",
		Bodies: []Body{{Actor: "both", ReplyExpected: true, Structure: enumStructure()}},
	}}}
	items := enumItems()

	tests, err := renderTests(mf, items)
	require.NoError(t, err)
	require.Contains(t, string(tests), `msg := gem.S1F1("carrieraction", "next", secs2.A("objtype"))`)

	decodeTests, err := renderDecodeTests(mf, items)
	require.NoError(t, err)
	require.Contains(t, string(decodeTests), `CARRIERACTION: "carrieraction",`)
	require.Contains(t, string(decodeTests), "func TestDecodeS1F1NEXTAbsent(t *testing.T) {")
}

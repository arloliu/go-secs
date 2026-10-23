package main

import (
	"fmt"
	"go/format"
	"math"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRenderItemsEnum proves the enum named-type mechanism: an item with a
// non-empty Values table renders a named defined type over its concrete
// GoType, plus a const block typed as the named type (not the bare GoType),
// so the generated constants are directly assignable to a generated builder
// parameter of that named type without a cast.
func TestRenderItemsEnum(t *testing.T) {
	items := map[string]Item{
		"COMMACK": {Formats: []string{"B"}, Binding: BindingFixed, GoType: "byte",
			Description: "Establish Communications Acknowledge Code, 1 byte.", Source: "e5",
			Values: []ItemValue{{Name: "Accepted", Value: IntValue(0)}, {Name: "Denied", Value: IntValue(1)}}},
	}
	out, err := renderItems(items)
	require.NoError(t, err)
	src := string(out)
	require.Contains(t, src, "type COMMACK byte")
	// gofmt column-aligns the const block, padding shorter names with extra
	// spaces so the type column lines up (COMMACKDenied is shorter than
	// COMMACKAccepted), so match on whitespace-tolerant regexps rather than a
	// literal single space between tokens.
	require.Regexp(t, regexp.MustCompile(`COMMACKAccepted\s+COMMACK = 0`), src)
	require.Regexp(t, regexp.MustCompile(`COMMACKDenied\s+COMMACK = 1`), src)
}

// TestRenderItemsSkipsPlainItems proves an item with no Values table
// contributes no type/const declarations to the rendered output.
func TestRenderItemsSkipsPlainItems(t *testing.T) {
	items := map[string]Item{
		"MDLN": {Formats: []string{"A"}, Binding: BindingFixed, GoType: "string"},
	}
	out, err := renderItems(items)
	require.NoError(t, err)
	src := string(out)
	require.NotContains(t, src, "type MDLN")
}

// TestRenderItemsNamedTypeWithoutValues proves a fixed item marked namedType without a values table
// renders its named type alone, documented by its item description,
// with no const block and no enumeration wording,
// and that a builder parameter and a decoder field of the item use the type.
func TestRenderItemsNamedTypeWithoutValues(t *testing.T) {
	items := map[string]Item{
		"PFCD": {Formats: []string{"B"}, Binding: BindingFixed, GoType: "byte",
			Description: "Predefined form code, 1 byte.", Source: "e5", NamedType: true},
	}
	out, err := renderItems(items)
	require.NoError(t, err)
	src := string(out)
	require.Contains(t, src, "// PFCD is the type of the PFCD item.\n// Predefined form code, 1 byte.\ntype PFCD byte\n")
	require.NotContains(t, src, "const (")
	require.NotContains(t, src, "enumerates")

	require.True(t, hasNamedType(items["PFCD"]))
	require.Equal(t, "PFCD", fixedGoType("PFCD", items["PFCD"]))
	require.Equal(t, "secs2.B(byte(pfcd))", fixedItemExpr(items["PFCD"], "pfcd"))
}

// TestRenderItemsStringEnum proves a fixed string item with string values renders a named string type
// whose constants are quoted with Go syntax,
// and that a value whose source differs from its item's carries a godoc line naming it.
func TestRenderItemsStringEnum(t *testing.T) {
	items := map[string]Item{
		"CARRIERACTION": {Formats: []string{"A"}, Binding: BindingFixed, GoType: "string", Source: "e5",
			Values: []ItemValue{
				{Name: "ProceedWithCarrier", Value: StringValue("ProceedWithCarrier"), Source: "e87.1"},
				{Name: "PodRelease", Value: StringValue("PodRelease"), Source: "e5"},
				{Name: "Quoted", Value: StringValue(`say "hi"`)},
			}},
	}
	out, err := renderItems(items)
	require.NoError(t, err)
	src := string(out)
	require.Contains(t, src, "type CARRIERACTION string")
	require.Regexp(t, regexp.MustCompile(`// CARRIERACTIONProceedWithCarrier is defined by SEMI E87\.1\.\n\s+CARRIERACTIONProceedWithCarrier\s+CARRIERACTION = "ProceedWithCarrier"`), src)
	require.Regexp(t, regexp.MustCompile(`CARRIERACTIONPodRelease\s+CARRIERACTION = "PodRelease"`), src)
	require.Regexp(t, regexp.MustCompile(`CARRIERACTIONQuoted\s+CARRIERACTION = "say \\"hi\\""`), src)
	require.NotContains(t, src, "CARRIERACTIONPodRelease is defined by", "a value sharing its item's source gets no source line")
	require.NotContains(t, src, "CARRIERACTIONQuoted is defined by", "a value without a source gets no source line")
}

// TestRenderItemsIntegerLiterals proves the integer constants render exactly:
// a negative value and an unsigned value above MaxInt64.
func TestRenderItemsIntegerLiterals(t *testing.T) {
	items := map[string]Item{
		"BLKDEF": {Formats: []string{"I1"}, Binding: BindingFixed, GoType: "int8",
			Values: []ItemValue{{Name: "Minus", Value: IntValue(-1)}}},
		"BIG": {Formats: []string{"U8"}, Binding: BindingFixed, GoType: "uint64",
			Values: []ItemValue{{Name: "Max", Value: UintValue(math.MaxUint64)}}},
	}
	out, err := renderItems(items)
	require.NoError(t, err)
	src := string(out)
	require.Regexp(t, regexp.MustCompile(`BLKDEFMinus\s+BLKDEF = -1\n`), src)
	require.Regexp(t, regexp.MustCompile(`BIGMax\s+BIG = 18446744073709551615\n`), src)
}

// TestRenderItemsOpenValues proves an open item with values renders only untyped constants,
// documented with the item's name and formats,
// in YAML order, with each item's block in name order.
func TestRenderItemsOpenValues(t *testing.T) {
	items := map[string]Item{
		"OBJTYPE": {Formats: []string{"A", "U1"}, Binding: BindingOpen, Source: "e5",
			Values: []ItemValue{
				{Name: "Carrier", Value: StringValue("Carrier"), Source: "e87.1"},
				{Name: "ProcessJob", Value: StringValue("ProcessJob")},
			}},
		"ERRCODE": {Formats: []string{"U1", "U2", "I1"}, Binding: BindingOpen,
			Values: []ItemValue{{Name: "NoError", Value: IntValue(0)}, {Name: "Minus", Value: IntValue(-3)}}},
	}
	out, err := renderItems(items)
	require.NoError(t, err)
	src := string(out)
	require.NotContains(t, src, "type ERRCODE")
	require.NotContains(t, src, "type OBJTYPE")
	require.Contains(t, src, "// Defined values of the ERRCODE item, which may be sent as U1, U2, or I1.\n"+
		"// They are untyped constants:\n"+
		"// pass one to the secs2 constructor of the format to send, e.g. secs2.U1(ERRCODENoError).\n"+
		"const (\n")
	require.Regexp(t, regexp.MustCompile(`ERRCODENoError\s+= 0\n\s+ERRCODEMinus\s+= -3\n`), src)
	require.Contains(t, src, "// Defined values of the OBJTYPE item, which may be sent as A or U1.\n"+
		"// They are untyped constants:\n"+
		"// pass one to the secs2 constructor of the format to send, e.g. secs2.A(OBJTYPECarrier).\n")
	require.Regexp(t, regexp.MustCompile(`// OBJTYPECarrier is defined by SEMI E87\.1\.\n\s+OBJTYPECarrier\s+= "Carrier"\n\s+OBJTYPEProcessJob\s+= "ProcessJob"\n`), src)
	require.Less(t, strings.Index(src, "ERRCODENoError"), strings.Index(src, "OBJTYPECarrier"))
}

// TestRenderItemsRejectsFixedWithoutGoType is defense-in-depth:
// ValidateItems already rejects a fixed item without a goType before rendering,
// but renderItems must not emit a "type <ITEM> " declaration with no type
// if it is ever called on unvalidated input.
func TestRenderItemsRejectsFixedWithoutGoType(t *testing.T) {
	items := map[string]Item{
		"BADENUM": {Formats: []string{"U1"}, Binding: BindingFixed,
			Values: []ItemValue{{Name: "X", Value: IntValue(0)}}},
	}
	_, err := renderItems(items)
	require.Error(t, err)
}

// TestExampleFormat proves which constructor the doc of an open item's const block names:
// a signed format for a negative first value,
// and none when no declared format takes the first value.
func TestExampleFormat(t *testing.T) {
	tests := []struct {
		name    string
		formats []string
		first   EnumValue
		want    string
	}{
		{"string", []string{"U1", "A"}, StringValue("x"), "A"},
		{"non-negative integer", []string{"A", "U1", "I1"}, IntValue(0), "U1"},
		{"negative integer", []string{"U1", "U2", "I2", "I1"}, IntValue(-1), "I2"},
		{"string without A", []string{"U1"}, StringValue("x"), ""},
		{"negative integer without a signed format", []string{"B", "U1"}, IntValue(-1), ""},
		{"integer without an integer format", []string{"A", "F4"}, IntValue(1), ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			it := Item{Formats: tt.formats, Binding: BindingOpen, Values: []ItemValue{{Name: "V", Value: tt.first}}}
			require.Equal(t, tt.want, exampleFormat(it))
		})
	}
}

// TestRenderItemsOpenValuesWithoutExample proves an open item's const block doc still tells the caller how to send a value
// when no declared format takes the first one.
func TestRenderItemsOpenValuesWithoutExample(t *testing.T) {
	items := map[string]Item{
		"BLKDEF": {Formats: []string{"U1", "U2"}, Binding: BindingOpen,
			Values: []ItemValue{{Name: "Minus", Value: IntValue(-1)}}},
	}
	out, err := renderItems(items)
	require.NoError(t, err)
	require.Contains(t, string(out), "// Defined values of the BLKDEF item, which may be sent as U1 or U2.\n"+
		"// They are untyped constants:\n"+
		"// pass one to the secs2 constructor of the format to send.\n"+
		"const (\n")
}

// TestRenderItemsNotes proves an item's note ends the godoc of its named type or untyped const block,
// and a value's note ends the godoc of its constant, after the line naming the value's own source.
func TestRenderItemsNotes(t *testing.T) {
	items := map[string]Item{
		"PRCMDNAME": {Formats: []string{"A"}, Binding: BindingFixed, GoType: "string", Source: "e5",
			Note: "Typed note.",
			Values: []ItemValue{
				{Name: "Start", Value: StringValue("START"), Note: "Spelled STARTPROCESS elsewhere."},
				{Name: "Both", Value: StringValue("BOTH"), Source: "e40.1", Note: "Source then note."},
				{Name: "Stop", Value: StringValue("STOP")},
			}},
		"RCMD": {Formats: []string{"A", "U1"}, Binding: BindingOpen, Source: "e5",
			Note:   "Open note.",
			Values: []ItemValue{{Name: "Reset", Value: StringValue("Reset"), Note: "Open value note."}}},
	}
	out, err := renderItems(items)
	require.NoError(t, err)
	src := string(out)
	require.Contains(t, src, "// PRCMDNAME enumerates the defined values of the PRCMDNAME item.\n"+
		"// Typed note.\n"+
		"type PRCMDNAME string\n")
	require.Regexp(t, regexp.MustCompile(`\t// Spelled STARTPROCESS elsewhere\.\n\tPRCMDNAMEStart\s+PRCMDNAME = "START"\n`), src)
	require.Regexp(t, regexp.MustCompile(`\t// PRCMDNAMEBoth is defined by SEMI E40\.1\.\n\t// Source then note\.\n\tPRCMDNAMEBoth\s+PRCMDNAME = "BOTH"\n`), src)
	require.Regexp(t, regexp.MustCompile(`"BOTH"\n\tPRCMDNAMEStop\s+PRCMDNAME = "STOP"\n`), src, "a value without a note or own source gets no godoc")
	require.Contains(t, src, "// pass one to the secs2 constructor of the format to send, e.g. secs2.A(RCMDReset).\n"+
		"// Open note.\n"+
		"const (\n"+
		"\t// Open value note.\n"+
		"\tRCMDReset = \"Reset\"\n")
}

// TestRenderItemsString proves that a fixed byte or uint8 item with a values table gets a String method after its const block.
// The method has a case per constant in YAML order,
// a fallback that prints the number,
// and a doc comment that names the type's first constant;
// items.go then imports strconv and is gofmt'd.
func TestRenderItemsString(t *testing.T) {
	items := map[string]Item{
		"COMMACK": {Formats: []string{"B"}, Binding: BindingFixed, GoType: "byte", Source: "e5",
			Values: []ItemValue{{Name: "Accepted", Value: IntValue(0)}, {Name: "Denied", Value: IntValue(1)}}},
		"ACCESSMODE": {Formats: []string{"U1"}, Binding: BindingFixed, GoType: "uint8", Source: "e5",
			Values: []ItemValue{{Name: "Auto", Value: IntValue(2)}, {Name: "Manual", Value: IntValue(1)}}},
	}

	out, err := renderItems(items)
	require.NoError(t, err)
	src := string(out)

	require.Contains(t, src, "\nimport \"strconv\"\n")
	require.Contains(t, src, "COMMACKDenied   COMMACK = 1\n)\n\n"+
		"// String returns the name of v's constant, such as \"COMMACKAccepted\",\n"+
		"// or \"COMMACK(n)\" for a value with no constant.\n"+
		"func (v COMMACK) String() string {\n"+
		"\tswitch v {\n"+
		"\tcase COMMACKAccepted:\n\t\treturn \"COMMACKAccepted\"\n"+
		"\tcase COMMACKDenied:\n\t\treturn \"COMMACKDenied\"\n"+
		"\tdefault:\n\t\treturn \"COMMACK(\" + strconv.Itoa(int(v)) + \")\"\n"+
		"\t}\n}\n")
	require.Contains(t, src, "such as \"ACCESSMODEAuto\",")
	require.Regexp(t, regexp.MustCompile(`case ACCESSMODEAuto:\s+return "ACCESSMODEAuto"\s+case ACCESSMODEManual:`), src)

	formatted, err := format.Source(out)
	require.NoError(t, err)
	require.Equal(t, src, string(formatted))
}

// TestRenderItemsNoString proves which items get no String method:
// a string enumeration, the untyped constants of an open item, a named type without values,
// and an item marked customString;
// with none left, items.go does not import strconv.
func TestRenderItemsNoString(t *testing.T) {
	items := map[string]Item{
		"CARRIERACTION": {Formats: []string{"A"}, Binding: BindingFixed, GoType: "string", Source: "e5",
			Values: []ItemValue{{Name: "Proceed", Value: StringValue("Proceed")}}},
		"ERRCODE": {Formats: []string{"U1", "U2"}, Binding: BindingOpen, Source: "e5",
			Values: []ItemValue{{Name: "NoError", Value: IntValue(0)}}},
		"PFCD": {Formats: []string{"B"}, Binding: BindingFixed, GoType: "byte", Source: "e5", NamedType: true},
		"ALCD": {Formats: []string{"B"}, Binding: BindingFixed, GoType: "byte", Source: "e5", CustomString: true,
			Values: []ItemValue{{Name: "PersonalSafety", Value: IntValue(1)}}},
	}

	out, err := renderItems(items)
	require.NoError(t, err)
	src := string(out)

	require.NotContains(t, src, "String()")
	require.NotContains(t, src, "strconv")
	require.Contains(t, src, "type ALCD byte")

	formatted, err := format.Source(out)
	require.NoError(t, err)
	require.Equal(t, src, string(formatted))

	testSrc, err := renderItemStringTests(items)
	require.NoError(t, err)
	require.Nil(t, testSrc, "no String method, so no String test file")
}

// TestRenderItemsStringRejectsDuplicateValue proves two constants sharing a value are an error naming the item,
// since a switch with both cases would not compile.
func TestRenderItemsStringRejectsDuplicateValue(t *testing.T) {
	items := map[string]Item{
		"COMMACK": {Formats: []string{"B"}, Binding: BindingFixed, GoType: "byte", Source: "e5",
			Values: []ItemValue{{Name: "Accepted", Value: IntValue(0)}, {Name: "Ok", Value: IntValue(0)}}},
	}

	_, err := renderItems(items)
	require.ErrorContains(t, err, "item COMMACK: values Accepted and Ok share the value 0")

	_, err = renderItemStringTests(items)
	require.ErrorContains(t, err, "item COMMACK: values Accepted and Ok share the value 0")

	// The same table on a customString item renders, since no String switch is generated for it.
	ci := items["COMMACK"]
	ci.CustomString = true
	_, err = renderItems(map[string]Item{"COMMACK": ci})
	require.NoError(t, err)
}

// TestRenderItemsStringRejectsFullTable proves a values table taking all 256 byte values is an error,
// since the String test then has no value without a constant to check the fallback with.
func TestRenderItemsStringRejectsFullTable(t *testing.T) {
	values := make([]ItemValue, 0, 256)
	for n := range 256 {
		values = append(values, ItemValue{Name: fmt.Sprintf("V%d", n), Value: IntValue(int64(n))})
	}

	items := map[string]Item{"FULL": {Formats: []string{"B"}, Binding: BindingFixed, GoType: "byte", Source: "e5", Values: values}}

	_, err := renderItems(items)
	require.ErrorContains(t, err, "item FULL: the values table takes every byte value")
}

// TestRenderItemStringTests proves the generated String test lists every constant of every type with a String method,
// in name order and then YAML order,
// plus the smallest value of each type that has no constant,
// and is gofmt'd.
func TestRenderItemStringTests(t *testing.T) {
	items := map[string]Item{
		"COMMACK": {Formats: []string{"B"}, Binding: BindingFixed, GoType: "byte", Source: "e5",
			Values: []ItemValue{{Name: "Accepted", Value: IntValue(0)}, {Name: "Denied", Value: IntValue(1)}, {Name: "Other", Value: IntValue(3)}}},
		"ACCESSMODE": {Formats: []string{"U1"}, Binding: BindingFixed, GoType: "uint8", Source: "e5",
			Values: []ItemValue{{Name: "Manual", Value: IntValue(1)}}},
		"CARRIERACTION": {Formats: []string{"A"}, Binding: BindingFixed, GoType: "string", Source: "e5",
			Values: []ItemValue{{Name: "Proceed", Value: StringValue("Proceed")}}},
	}

	out, err := renderItemStringTests(items)
	require.NoError(t, err)
	src := string(out)

	require.True(t, strings.HasPrefix(src, "// Code generated by gemgen; DO NOT EDIT.\n\npackage gem_test\n"))
	require.Contains(t, src, "\t\t{value: gem.ACCESSMODEManual, want: \"ACCESSMODEManual\"},\n"+
		"\t\t{value: gem.ACCESSMODE(0), want: \"ACCESSMODE(0)\"},\n"+
		"\t\t{value: gem.COMMACKAccepted, want: \"COMMACKAccepted\"},\n"+
		"\t\t{value: gem.COMMACKDenied, want: \"COMMACKDenied\"},\n"+
		"\t\t{value: gem.COMMACKOther, want: \"COMMACKOther\"},\n"+
		"\t\t{value: gem.COMMACK(2), want: \"COMMACK(2)\"},\n")
	require.NotContains(t, src, "CARRIERACTION")

	formatted, err := format.Source(out)
	require.NoError(t, err)
	require.Equal(t, src, string(formatted))
}

package main

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const goodItems = `
MDLN:
  formats: [A]
  binding: fixed
  goType: string
  description: Equipment model type.
  source: e5
CEID:
  formats: [A, U1, U2, U4, U8, I1, I2, I4, I8]
  binding: open
  description: Collected event ID.
  source: e5
`

func TestLoadItemsValid(t *testing.T) {
	items, err := LoadItems([]byte(goodItems))
	require.NoError(t, err)
	require.NoError(t, ValidateItems(items))
	require.Equal(t, BindingFixed, items["MDLN"].Binding)
	require.Equal(t, "string", items["MDLN"].GoType)
	require.Equal(t, BindingOpen, items["CEID"].Binding)
}

func TestValidateItems_ByteArrayGoType(t *testing.T) {
	items := map[string]Item{
		"MHEAD": {Formats: []string{"B"}, Binding: BindingFixed, GoType: "[10]byte"},
		"ABS":   {Formats: []string{"B"}, Binding: BindingFixed, GoType: "[]byte"},
		"BAD":   {Formats: []string{"A"}, Binding: BindingFixed, GoType: "[10]byte"}, // format A may NOT use this
	}
	require.NoError(t, ValidateItems(map[string]Item{"MHEAD": items["MHEAD"]}))
	require.NoError(t, ValidateItems(map[string]Item{"ABS": items["ABS"]}))
	err := ValidateItems(map[string]Item{"BAD": items["BAD"]})
	require.Error(t, err)
}

func TestValidateItemsRejectsBadFixed(t *testing.T) {
	cases := map[string]string{
		"fixed multi format": "X:\n  formats: [A, B]\n  binding: fixed\n  goType: string\n  source: e5\n",
		"fixed no goType":    "X:\n  formats: [A]\n  binding: fixed\n  source: e5\n",
		"binding mismatch":   "X:\n  formats: [A]\n  binding: open\n  source: e5\n",
		"goType mismatch":    "X:\n  formats: [A]\n  binding: fixed\n  goType: int8\n  source: e5\n",
	}
	for name, yml := range cases {
		t.Run(name, func(t *testing.T) {
			items, err := LoadItems([]byte(yml))
			require.NoError(t, err)
			require.Error(t, ValidateItems(items))
		})
	}
}

// enumItem renders a one-item items.yaml whose item has the given formats and goType
// (none when goType is empty) and the given values: entries, one per line.
func enumItem(formats string, goType string, values ...string) string {
	binding := "open"
	if goType != "" {
		binding = "fixed"
	}

	var yml strings.Builder
	yml.WriteString("X:\n  formats: " + formats + "\n  binding: " + binding + "\n")
	if goType != "" {
		yml.WriteString("  goType: " + goType + "\n")
	}

	yml.WriteString("  source: e5\n  values:\n")
	for _, v := range values {
		yml.WriteString("    - " + v + "\n")
	}

	return yml.String()
}

// TestEnumValuesLoad proves which YAML scalars a value: holds,
// and that each one validates against the item it belongs to.
func TestEnumValuesLoad(t *testing.T) {
	tests := []struct {
		name string
		yml  string
		want []EnumValue
	}{
		{"negative integer on a signed item", enumItem("[I1]", "int8", "{name: None, value: -1}", "{name: Max, value: 127}"),
			[]EnumValue{IntValue(-1), IntValue(127)}},
		{"integer limits of a byte item", enumItem("[B]", "byte", "{name: Lo, value: 0}", "{name: Hi, value: 255}"),
			[]EnumValue{IntValue(0), IntValue(255)}},
		{"hexadecimal integer", enumItem("[U1]", "uint8", "{name: Hex, value: 0x10}"),
			[]EnumValue{IntValue(16)}},
		{"octal and binary prefixes", enumItem("[U1]", "uint8", "{name: Oct, value: 0o17}", "{name: Bin, value: 0b101}"),
			[]EnumValue{IntValue(15), IntValue(5)}},
		{"zero", enumItem("[U1]", "uint8", "{name: Zero, value: 0}"),
			[]EnumValue{IntValue(0)}},
		{"quoted leading zero on a string item", enumItem("[A]", "string", `{name: Code, value: "010"}`),
			[]EnumValue{StringValue("010")}},
		{"unsigned integer above MaxInt64", enumItem("[U8]", "uint64", "{name: Max, value: 18446744073709551615}"),
			[]EnumValue{UintValue(math.MaxUint64)}},
		{"string on a string item", enumItem("[A]", "string", "{name: Start, value: START}"),
			[]EnumValue{StringValue("START")}},
		{"quoted digits on a string item", enumItem("[A]", "string", `{name: Ten, value: "10"}`),
			[]EnumValue{StringValue("10")}},
		{"integers on an open item", enumItem("[U1, I1]", "", "{name: A, value: -5}", "{name: B, value: 7}"),
			[]EnumValue{IntValue(-5), IntValue(7)}},
		{"strings on an open item", enumItem("[A, U1]", "", "{name: A, value: Carrier}", `{name: B, value: "2"}`),
			[]EnumValue{StringValue("Carrier"), StringValue("2")}},
		{"integer on a float item", enumItem("[F4]", "float32", "{name: Half, value: -3}"),
			[]EnumValue{IntValue(-3)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			items, err := LoadItems([]byte(tt.yml))
			require.NoError(t, err)
			require.NoError(t, ValidateItems(items))

			got := make([]EnumValue, len(items["X"].Values))
			for i, v := range items["X"].Values {
				got[i] = v.Value
			}

			require.Equal(t, tt.want, got)
		})
	}
}

// TestEnumValuesRejectedAtLoad proves a value: that is neither an integer nor a string scalar fails loading,
// and so does an integer written with a leading zero or with underscores Go rejects.
func TestEnumValuesRejectedAtLoad(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr string
	}{
		{"float", "{name: F, value: 1.5}", "not an integer or a string"},
		{"bool", "{name: F, value: true}", "not an integer or a string"},
		{"mapping", "{name: F, value: {a: 1}}", "integer or a string scalar"},
		{"sequence", "{name: F, value: [1]}", "integer or a string scalar"},
		// yaml.v3 resolves a decimal integer beyond 64 bits to a float.
		{"beyond 64 bits", "{name: F, value: 18446744073709551616}", "not an integer or a string"},
		// yaml.v3 reads 010 as octal 8.
		{"leading zero read as octal", "{name: F, value: 010}", "leading zero"},
		// yaml.v3 reads 08 as a float.
		{"leading zero read as float", "{name: F, value: 08}", "leading zero"},
		{"signed leading zero", "{name: F, value: -07}", "leading zero"},
		// yaml.v3 drops every underscore and tags 1__0 as the integer 10.
		{"doubled underscore", "{name: F, value: 1__0}", "not a valid integer literal"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadItems([]byte(enumItem("[I8]", "int64", tt.value)))
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

// TestEnumValuesRejectedByValidation proves every values: rule ValidateItems enforces.
func TestEnumValuesRejectedByValidation(t *testing.T) {
	tests := []struct {
		name    string
		yml     string
		wantErr string
	}{
		{"null value", enumItem("[B]", "byte", "{name: N, value: null}"), "missing or null"},
		{"tilde value", enumItem("[B]", "byte", "{name: N, value: ~}"), "missing or null"},
		{"missing value", enumItem("[B]", "byte", "{name: N}"), "missing or null"},
		{"above a byte", enumItem("[B]", "byte", "{name: N, value: 256}"), "needs an integer in [0, 255]"},
		{"negative on an unsigned item", enumItem("[U2]", "uint16", "{name: N, value: -1}"), "needs an integer in [0, 65535]"},
		{"above int8", enumItem("[I1]", "int8", "{name: N, value: 128}"), "needs an integer in [-128, 127]"},
		{"below int8", enumItem("[I1]", "int8", "{name: N, value: -129}"), "needs an integer in [-128, 127]"},
		{"above MaxInt64 on int64", enumItem("[I8]", "int64", "{name: N, value: 9223372036854775808}"), "needs an integer in"},
		{"string on a numeric item", enumItem("[U1]", "uint8", "{name: N, value: one}"), "needs an integer"},
		{"quoted digits on a numeric item", enumItem("[U1]", "uint8", `{name: N, value: "1"}`), "needs an integer"},
		{"integer on a string item", enumItem("[A]", "string", "{name: N, value: 1}"), "needs a string"},
		{"values on a bool item", enumItem("[BOOLEAN]", "bool", "{name: N, value: 1}"), "cannot carry a values table"},
		{"values on a byte slice", enumItem("[B]", `"[]byte"`, "{name: N, value: 1}"), "cannot carry a values table"},
		{"mixed kinds on an open item", enumItem("[A, U1]", "", "{name: N, value: 1}", "{name: M, value: one}"), "mix integers and strings"},
		{"strings on a JIS-8 item", enumItem("[J]", "string", "{name: N, value: one}"), "string values need format A"},
		{"strings on a Unicode item", enumItem("[W]", "string", "{name: N, value: one}"), "string values need format A"},
		{"strings on an open item without A", enumItem("[U1, I1]", "", "{name: N, value: one}"), "string values need format A"},
		{"integers on an open item without an integer format", enumItem("[A, F4]", "", "{name: N, value: 1}"), "integer values need"},
		{"upper-case source", enumItem("[B]", "byte", "{name: N, value: 1, source: E87.1}"), "lower-case SEMI standard"},
		{"source not a SEMI standard", enumItem("[B]", "byte", "{name: N, value: 1, source: external}"), "lower-case SEMI standard"},
		{"multi-line value note", enumItem("[B]", "byte", `{name: N, value: 1, note: "one\ntwo"}`), "single line"},
		{"multi-line item note", strings.Replace(enumItem("[B]", "byte", "{name: N, value: 1}"), "  values:", "  note: \"one\\ntwo\"\n  values:", 1), "single line"},
		{"item note without values", "X:\n  formats: [B]\n  binding: fixed\n  goType: byte\n  note: Lost.\n", "needs a values table"},
		{"name not an identifier", enumItem("[B]", "byte", "{name: Not OK, value: 1}"), "Go identifier"},
		{"empty name", enumItem("[B]", "byte", "{name: '', value: 1}"), "Go identifier"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			items, err := LoadItems([]byte(tt.yml))
			require.NoError(t, err)

			err = ValidateItems(items)
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestEnumValueNamesCollide proves two values tables may not generate the same constant,
// and a constant may not take the name of another item's named type.
func TestEnumValueNamesCollide(t *testing.T) {
	tests := map[string]string{
		"two constants": `
ACK:
  formats: [B]
  binding: fixed
  goType: byte
  values: [{name: COk, value: 0}]
ACKC:
  formats: [B]
  binding: fixed
  goType: byte
  values: [{name: Ok, value: 0}]
`,
		"constant and a type without values": `
ACK:
  formats: [B]
  binding: fixed
  goType: byte
  values: [{name: C, value: 0}]
ACKC:
  formats: [B]
  binding: fixed
  goType: byte
  namedType: true
`,
		"constant and type": `
ACK:
  formats: [B]
  binding: fixed
  goType: byte
  values: [{name: C, value: 0}]
ACKC:
  formats: [B]
  binding: fixed
  goType: byte
  values: [{name: Ok, value: 0}]
`,
	}

	for name, yml := range tests {
		t.Run(name, func(t *testing.T) {
			items, err := LoadItems([]byte(yml))
			require.NoError(t, err)

			err = ValidateItems(items)
			require.Error(t, err)
			require.Contains(t, err.Error(), "would also name")
		})
	}
}

// TestLoadItemsRejectsUnknownKeys proves a key the item DSL does not define fails loading,
// in an item and in one of its values,
// so a misspelled key is caught instead of silently dropped.
func TestLoadItemsRejectsUnknownKeys(t *testing.T) {
	tests := []struct {
		name string
		yml  string
		key  string
	}{
		{"item", "X:\n  formats: [U1]\n  binding: fixed\n  goType: uint8\n  desciption: typo\n", "desciption"},
		{"value", enumItem("[U1]", "uint8", "{name: Ok, value: 0, sorce: e87.1}"), "sorce"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadItems([]byte(tt.yml))
			require.ErrorContains(t, err, "field "+tt.key+" not found")
		})
	}
}

// TestLoadItemsStrictKeepsEnumValues proves strict loading still reads a values: table
// through EnumValue's own unmarshalling, integers and strings alike, with each value's source.
func TestLoadItemsStrictKeepsEnumValues(t *testing.T) {
	items, err := LoadItems([]byte(enumItem("[A, U1]", "", "{name: Carrier, value: Carrier, source: e87.1}", `{name: Two, value: "2"}`)))
	require.NoError(t, err)
	require.Equal(t, []ItemValue{
		{Name: "Carrier", Value: StringValue("Carrier"), Source: "e87.1"},
		{Name: "Two", Value: StringValue("2")},
	}, items["X"].Values)

	items, err = LoadItems([]byte(enumItem("[I1]", "int8", "{name: None, value: -1}")))
	require.NoError(t, err)
	require.Equal(t, IntValue(-1), items["X"].Values[0].Value)
}

// TestLoadItemsEmptyDocument proves an empty items file loads as no items, as it did before loading became strict.
func TestLoadItemsEmptyDocument(t *testing.T) {
	items, err := LoadItems(nil)
	require.NoError(t, err)
	require.Empty(t, items)
}

// TestLoadItemsNotes proves strict loading accepts a note on an item and on a value.
func TestLoadItemsNotes(t *testing.T) {
	yml := strings.Replace(enumItem("[A]", "string", `{name: Start, value: START, note: "Spelled STARTPROCESS elsewhere."}`),
		"  values:", "  note: Item note.\n  values:", 1)

	items, err := LoadItems([]byte(yml))
	require.NoError(t, err)
	require.NoError(t, ValidateItems(items))
	require.Equal(t, "Item note.", items["X"].Note)
	require.Equal(t, "Spelled STARTPROCESS elsewhere.", items["X"].Values[0].Note)
}

// TestNamedTypeItems proves which items namedType may mark:
// a fixed item whose goType a values table could carry loads and validates,
// and an open item, another goType, or an item that already has values is rejected.
func TestNamedTypeItems(t *testing.T) {
	const named = "X:\n  formats: %s\n  binding: %s\n  goType: %s\n  source: e5\n  namedType: true\n"

	tests := []struct {
		name    string
		yml     string
		wantErr string
	}{
		{"byte", fmt.Sprintf(named, "[B]", "fixed", "byte"), ""},
		{"unsigned", fmt.Sprintf(named, "[U2]", "fixed", "uint16"), ""},
		{"ASCII string", fmt.Sprintf(named, "[A]", "fixed", "string"), ""},
		{"open item", "X:\n  formats: [U1, U2]\n  binding: open\n  source: e5\n  namedType: true\n", "namedType needs a fixed item"},
		{"bool", fmt.Sprintf(named, "[BOOLEAN]", "fixed", "bool"), "namedType needs an integer or float goType"},
		{"byte slice", fmt.Sprintf(named, "[B]", "fixed", `"[]byte"`), "namedType needs an integer or float goType"},
		{"byte array", fmt.Sprintf(named, "[B]", "fixed", `"[10]byte"`), "namedType needs an integer or float goType"},
		{"Unicode string", fmt.Sprintf(named, "[W]", "fixed", "string"), "string on format A"},
		{"with values", fmt.Sprintf(named, "[B]", "fixed", "byte") + "  values:\n    - {name: One, value: 1}\n", "namedType is for an item without values"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			items, err := LoadItems([]byte(tt.yml))
			require.NoError(t, err)

			err = ValidateItems(items)
			if tt.wantErr == "" {
				require.NoError(t, err)
				require.True(t, items["X"].NamedType)
				require.True(t, hasNamedType(items["X"]))

				return
			}

			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

// TestCustomStringItems proves which items customString may mark:
// only an item that would otherwise get a generated String method,
// a fixed item over byte or uint8 with a values table.
func TestCustomStringItems(t *testing.T) {
	const custom = "X:\n  formats: %s\n  binding: fixed\n  goType: %s\n  source: e5\n  customString: true\n"
	const oneValue = "  values:\n    - {name: One, value: 1}\n"
	const wantErr = "customString needs a fixed item with a byte or uint8 goType and a values table"

	tests := []struct {
		name    string
		yml     string
		wantErr string
	}{
		{"byte with values", fmt.Sprintf(custom, "[B]", "byte") + oneValue, ""},
		{"uint8 with values", fmt.Sprintf(custom, "[U1]", "uint8") + oneValue, ""},
		{"byte without values", fmt.Sprintf(custom, "[B]", "byte"), wantErr},
		{"named type without values", fmt.Sprintf(custom, "[B]", "byte") + "  namedType: true\n", wantErr},
		{"uint16 with values", fmt.Sprintf(custom, "[U2]", "uint16") + oneValue, wantErr},
		{"string with values", fmt.Sprintf(custom, "[A]", "string") + "  values:\n    - {name: One, value: One}\n", wantErr},
		{"open item with values", "X:\n  formats: [U1, U2]\n  binding: open\n  source: e5\n  customString: true\n" + oneValue, wantErr},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			items, err := LoadItems([]byte(tt.yml))
			require.NoError(t, err)

			err = ValidateItems(items)
			if tt.wantErr == "" {
				require.NoError(t, err)
				require.True(t, items["X"].CustomString)
				require.True(t, stringerEligible(items["X"]))
				require.False(t, hasStringer(items["X"]))

				return
			}

			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

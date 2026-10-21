package main

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// variantMessages wraps a YAML bodies: list, indented to sit under one message,
// in a stream-16 messages file whose message has the given function number.
func variantMessages(function int, bodies string) string {
	return `
stream: 16
messages:
  - function: ` + strconv.Itoa(function) + `
    name: Variant Test
    mnemonic: VT
    direction: bidirectional
    description: A sample.
    exception: None
    source: e5
    bodies:
` + bodies
}

// variantMain is an equipment main body the variant fixtures below shorten:
// a bounded list whose short form keeps only its first child.
const variantMain = `      - actor: equipment
        structure: {type: list, minItems: 1, maxItems: 3, items: [{item: MDLN}, {item: SOFTREV}, {item: SVID}]}
`

// TestValidateMessagesRejectsBadVariants proves every variant rule ValidateMessages enforces:
// how a variant is named, which main body it needs,
// which decoder fields it may emit,
// and which complete names a stream file may not emit twice.
func TestValidateMessagesRejectsBadVariants(t *testing.T) {
	tests := []struct {
		name     string
		function int
		bodies   string
		wantErr  string
	}{
		{"lower-case name", 1, variantMain + `      - {actor: equipment, variant: short, structure: {type: list, items: [{item: MDLN}]}}
`, "upper-case letter"},
		{"underscore in name", 1, variantMain + `      - {actor: equipment, variant: No_Data, structure: {type: list, items: [{item: MDLN}]}}
`, "upper-case letter"},
		{"no main body", 1, `      - {actor: equipment, variant: Short, structure: {type: list, items: [{item: MDLN}]}}
`, "no main body for actor equipment"},
		{"main body of another actor", 1, `      - {actor: host, structure: {type: list, items: [{item: MDLN}]}}
      - {actor: equipment, variant: Short, structure: {type: list, items: [{item: MDLN}]}}
`, "no main body for actor equipment"},
		{"two main bodies for one actor", 1, variantMain + variantMain, "more than one body without a variant name"},
		{"header-only variant", 1, variantMain + `      - {actor: equipment, variant: Empty}
`, "header-only"},
		{"header-only main body", 1, `      - {actor: equipment}
      - {actor: equipment, variant: Short, structure: {type: list, items: [{item: MDLN}]}}
`, "header-only main body"},
		{"field the main body lacks", 1, variantMain + `      - {actor: equipment, variant: Short, structure: {type: list, items: [{item: TSIP}]}}
`, "field TSIP is not a field of the main body"},
		{"optional leaf over a plain main leaf", 1, variantMain + `      - {actor: equipment, variant: Short, structure: {type: list, items: [{item: MDLN, optional: true}]}}
`, "field MDLNPresent is not a field of the main body"},
		{"field of another type", 1, variantMain + `      - {actor: equipment, variant: Short, structure: {type: list, items: [{item: TSIP, as: mdln}]}}
`, "field MDLN has type byte"},
		{"variant names an even function's result type", 2, variantMain + `      - {actor: equipment, variant: Reply, structure: {type: list, items: [{item: MDLN}]}}
`, "S16F2Reply would name both"},
		{"variant names an odd function's result type", 1, variantMain + `      - {actor: equipment, variant: Body, structure: {type: list, items: [{item: MDLN}]}}
`, "S16F1Body would name both"},
		{"host variant names the host result type", 2, `      - actor: host
        structure: {type: list, minItems: 1, maxItems: 2, items: [{item: MDLN}, {item: SOFTREV}]}
      - {actor: host, variant: Reply, structure: {type: list, items: [{item: MDLN}]}}
`, "S16F2HostReply would name both"},
		{"equipment variant Host names the host builder", 2, variantMain + `      - actor: host
        structure: {type: list, items: [{item: MDLN}]}
      - {actor: equipment, variant: Host, structure: {type: list, items: [{item: MDLN}]}}
`, "S16F2Host would name both"},
		{"two variants of one name", 1, variantMain + `      - {actor: equipment, variant: Short, structure: {type: list, items: [{item: MDLN}]}}
      - {actor: equipment, variant: Short, structure: {type: list, items: [{item: MDLN}]}}
`, "S16F1Short would name both"},
		{"variant parameter collision", 1, variantMain + `      - {actor: equipment, variant: Short, structure: {type: list, items: [{item: MDLN}, {item: MDLN}]}}
`, "collide"},
		{"reply bit differs from the main body", 1, variantMain + `      - {actor: equipment, replyExpected: true, variant: Short, structure: {type: list, items: [{item: MDLN}]}}
`, "variant Short sets replyExpected true, but its main body sets false"},
		{"main body root is not a fixed list", 1, `      - {actor: equipment, structure: {item: MDLN}}
      - {actor: equipment, variant: Short, structure: {type: list, items: [{item: MDLN}]}}
`, "root is not a fixed list"},
		{"field renamed onto a main field of the same type", 1, variantMain + `      - {actor: equipment, variant: Short, structure: {type: list, items: [{item: MDLN, as: softrev}]}}
`, "field SOFTREV is at body[0], but the main body reads it from body[1]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mf, err := LoadMessageFile([]byte(variantMessages(tt.function, tt.bodies)))
			require.NoError(t, err)

			err = ValidateMessages([]MessageFile{mf}, testItems())
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestValidateMessagesRejectsReorderedVariantFields proves a variant must send each field
// where the main body's decoder reads it.
// Two BOOLEAN fields swapped in a variant decode without error,
// and every generated sample of a BOOLEAN field could be the same value,
// so only the position check catches the swap.
func TestValidateMessagesRejectsReorderedVariantFields(t *testing.T) {
	items := testItems()
	items["START"] = Item{Formats: []string{"BOOLEAN"}, Binding: BindingFixed, GoType: "bool"}
	items["STOP"] = Item{Formats: []string{"BOOLEAN"}, Binding: BindingFixed, GoType: "bool"}

	const main = `      - actor: equipment
        structure: {type: list, minItems: 2, maxItems: 3, items: [{item: START}, {item: STOP}, {item: MDLN}]}
`

	tests := []struct {
		name    string
		bodies  string
		wantErr string
	}{
		{"BOOLEAN fields reversed", main + `      - {actor: equipment, variant: Short, structure: {type: list, items: [{item: STOP}, {item: START}]}}
`, "field STOP is at body[0], but the main body reads it from body[1]"},
		{"field moved into a nested list", main + `      - {actor: equipment, variant: Short, structure: {type: list, items: [{item: START}, {type: list, items: [{item: STOP}]}]}}
`, "field STOP is at body[1][0], but the main body reads it from body[1]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mf, err := LoadMessageFile([]byte(variantMessages(1, tt.bodies)))
			require.NoError(t, err)

			err = ValidateMessages([]MessageFile{mf}, items)
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}

	mf, err := LoadMessageFile([]byte(variantMessages(1, main+`      - {actor: equipment, variant: Short, structure: {type: list, items: [{item: START}, {item: STOP}]}}
`)))
	require.NoError(t, err)
	require.NoError(t, ValidateMessages([]MessageFile{mf}, items), "the same fields in the main body's order are accepted")
}

// TestValidateMessagesAcceptsVariants proves the variant rules leave legal messages alone:
// variants on odd and even functions, for each actor,
// one that supplies no fields at all, and several variants of one main body.
func TestValidateMessagesAcceptsVariants(t *testing.T) {
	tests := []struct {
		name     string
		function int
		bodies   string
	}{
		{"equipment variant, odd function", 1, variantMain + `      - {actor: equipment, variant: Short, structure: {type: list, items: [{item: MDLN}]}}
`},
		{"equipment variant, even function", 2, variantMain + `      - {actor: equipment, variant: Short, structure: {type: list, items: [{item: MDLN}]}}
`},
		{"variant of a both body", 1, `      - actor: both
        structure: {type: list, minItems: 0, maxItems: 2, items: [{item: MDLN}, {item: SOFTREV}]}
      - {actor: both, variant: NoData, structure: {type: list, items: []}}
`},
		{"host and equipment variants", 2, variantMain + `      - actor: host
        structure: {type: list, minItems: 1, maxItems: 2, items: [{item: MDLN}, {item: SOFTREV}]}
      - {actor: equipment, variant: Short, structure: {type: list, items: [{item: MDLN}]}}
      - {actor: host, variant: Short, structure: {type: list, items: [{item: MDLN}]}}
`},
		{"two variants of one main body", 1, variantMain + `      - {actor: equipment, variant: Short, structure: {type: list, items: [{item: MDLN}]}}
      - {actor: equipment, variant: Bare, structure: {type: list, items: []}}
`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mf, err := LoadMessageFile([]byte(variantMessages(tt.function, tt.bodies)))
			require.NoError(t, err)

			require.NoError(t, ValidateMessages([]MessageFile{mf}, testItems()))
		})
	}
}

// variantFile is a validated stream file with an equipment main body and a variant that supplies only its first field.
func variantFile(t *testing.T) MessageFile {
	t.Helper()

	mf, err := LoadMessageFile([]byte(variantMessages(28, variantMain+`      - {actor: equipment, variant: Success, structure: {type: list, items: [{item: MDLN}]}}
`)))
	require.NoError(t, err)
	require.NoError(t, ValidateMessages([]MessageFile{mf}, testItems()))

	return mf
}

// TestRenderVariantBuilder proves a variant body gets a builder named after its main builder and its variant,
// documented as the short form of its message.
func TestRenderVariantBuilder(t *testing.T) {
	src, err := renderMessages(variantFile(t), testItems())
	require.NoError(t, err)

	got := string(src)
	require.Contains(t, got, "func S16F28(mdln string, softrev string, svid secs2.Item) secs2.SECS2Message {")
	require.Contains(t, got, "func S16F28Success(mdln string) secs2.SECS2Message {\n\treturn secs2.NewMessage(16, 28, false, secs2.L(secs2.A(mdln)))")
	require.Contains(t, got, "// S16F28Success creates the short form of an S16F28 (Variant Test) message for equipment, direction: bidirectional.\n"+
		"//\n"+
		"// [S16F28] creates the full form,\n"+
		"// and [DecodeS16F28] decodes both.\n"+
		"//\n"+
		"// Body: L[1]{ A[mdln] }.\n"+
		"func S16F28Success(")
}

// TestRenderVariantHasNoDecoder proves a variant body gets no decoder and no decode round-trip test of its own:
// its main body's decoder decodes it.
func TestRenderVariantHasNoDecoder(t *testing.T) {
	mf := variantFile(t)

	decoders, err := renderDecoders(mf, testItems())
	require.NoError(t, err)
	require.Equal(t, []string{"DecodeS16F28"}, declaredFuncs(t, decoders))
	require.Equal(t, 1, strings.Count(string(decoders), "type S16F28Reply struct"))

	decodeTests, err := renderDecodeTests(mf, testItems())
	require.NoError(t, err)
	require.Equal(t, []string{"TestDecodeS16F28RoundTrip"}, declaredFuncs(t, decodeTests))
}

// TestRenderVariantTest proves a variant's builder test checks its header and wire shape,
// then decodes it with the main body's decoder:
// the field it supplies must equal its sample, and the fields it omits must be zero.
func TestRenderVariantTest(t *testing.T) {
	src, err := renderTests(variantFile(t), testItems())
	require.NoError(t, err)

	got := string(src)
	require.Equal(t, []string{"assertS16Header", "TestS16F28", "TestS16F28Success"}, declaredFuncs(t, src))

	start := strings.Index(got, "func TestS16F28Success(")
	require.GreaterOrEqual(t, start, 0)

	body := got[start:]
	for _, want := range []string{
		`msg := gem.S16F28Success("mdln")`,
		"assertS16Header(t, msg, 28, false)",
		"list1, err := decoded.ToList()",
		"got, err := gem.DecodeS16F28(decoded)",
		`if got.MDLN != "mdln" {`,
		"var zero gem.S16F28Reply",
		"if got.SOFTREV != zero.SOFTREV {",
		"if got.SVID != zero.SVID {",
	} {
		require.Contains(t, body, want)
	}
}

// TestRenderVariantTestWithoutMainFields covers a main body whose result has no fields.
// The variant's test still runs the main decoder,
// without declaring a result it never reads.
func TestRenderVariantTestWithoutMainFields(t *testing.T) {
	mf, err := LoadMessageFile([]byte(variantMessages(1, `      - actor: both
        structure: {type: list, minItems: 0, maxItems: 1, items: [{type: list, items: []}]}
      - {actor: both, variant: Bare, structure: {type: list, items: []}}
`)))
	require.NoError(t, err)
	require.NoError(t, ValidateMessages([]MessageFile{mf}, testItems()))

	src, err := renderTests(mf, testItems())
	require.NoError(t, err)
	require.Contains(t, string(src), "if _, err := gem.DecodeS16F1(decoded); err != nil {")
	require.NotContains(t, string(src), "got, err := gem.DecodeS16F1")
}

// TestRenderDecodeTestsEmptyList covers the empty-list decode test.
// A body whose outermost list is bounded with minItems: 0 gets a test decoding a bare L,0,
// which expects every field at its zero value;
// no other body gets one.
func TestRenderDecodeTestsEmptyList(t *testing.T) {
	tests := []struct {
		name      string
		structure string
		want      []string // fragments of the empty-list test, or nil when none is generated
	}{
		{
			"outer list may be empty",
			`{type: list, minItems: 0, maxItems: 3, items: [{item: MDLN}, {type: list, repeat: svids, of: {item: SVID}}, {item: MHEAD}]}`,
			[]string{
				"func TestDecodeS16F2EmptyList(t *testing.T) {",
				"got, err := gem.DecodeS16F2(secs2.L())",
				"var zero gem.S16F2Reply",
				"if got.MDLN != zero.MDLN {",
				"if len(got.SVIDS) != 0 {",
				"if got.MHEAD != zero.MHEAD {",
			},
		},
		{
			"outer list without fields",
			`{type: list, minItems: 0, maxItems: 1, items: [{type: list, items: []}]}`,
			[]string{"if _, err := gem.DecodeS16F2(secs2.L()); err != nil {"},
		},
		{"outer list needs one child", `{type: list, minItems: 1, maxItems: 2, items: [{item: MDLN}, {item: SOFTREV}]}`, nil},
		{"inner list may be empty", `{type: list, items: [{item: MDLN}, {type: list, minItems: 0, maxItems: 1, items: [{item: SOFTREV}]}]}`, nil},
		{"outer repeat", `{type: list, repeat: svids, of: {item: SVID}}`, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mf, err := LoadMessageFile([]byte(variantMessages(2, `      - actor: both
        structure: `+tt.structure+"\n")))
			require.NoError(t, err)
			require.NoError(t, ValidateMessages([]MessageFile{mf}, testItems()))

			src, err := renderDecodeTests(mf, testItems())
			require.NoError(t, err)

			if tt.want == nil {
				require.Equal(t, []string{"TestDecodeS16F2RoundTrip"}, declaredFuncs(t, src))
				return
			}

			require.Equal(t, []string{"TestDecodeS16F2RoundTrip", "TestDecodeS16F2EmptyList"}, declaredFuncs(t, src))

			for _, want := range tt.want {
				require.Contains(t, string(src), want)
			}
		})
	}
}

// TestRenderDecodeTestsRegistersFuzzDecoders proves the generated decode tests register each decoder with the fuzz harness,
// seeded with its main builder's body from the round-trip samples,
// and register nothing for a variant, which has no decoder of its own.
func TestRenderDecodeTestsRegistersFuzzDecoders(t *testing.T) {
	src, err := renderDecodeTests(variantFile(t), testItems())
	require.NoError(t, err)

	got := string(src)
	require.Equal(t, 1, strings.Count(got, "registerFuzzDecoder("))
	require.Contains(t, got, "func init() {\n"+
		"\tregisterFuzzDecoder(\"DecodeS16F28\", func(item secs2.Item) error {\n"+
		"\t\t_, err := gem.DecodeS16F28(item)\n"+
		"\n"+
		"\t\treturn err\n"+
		"\t}, gem.S16F28(\"mdln\", \"softrev\", secs2.A(\"svid\")).Item())\n"+
		"}\n")
}

package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// groupTestItems is the item dictionary the group fixtures draw on.
func groupTestItems() map[string]Item {
	items := testItems()
	items["STATUS"] = Item{
		Formats: []string{"U1"}, Binding: BindingFixed, GoType: "uint8",
		Values: []ItemValue{{Name: "Ok", Value: IntValue(0)}},
	}

	return items
}

// groupTestGroups declares a group of each kind of field:
// Holder holds an optional leaf, an optional group, a flattened sub-list with an opaque node,
// a repeat of a group, and a repeat of values.
const groupTestGroups = `
groups:
  - name: Pair
    description: One pair of a model and a status variable.
    structure: {type: list, items: [{item: MDLN}, {item: SVID}]}
  - name: Band
    description: One band.
    structure: {type: list, items: [{item: SVID}, {item: SOFTREV}]}
  - name: Holder
    description: One holder.
    structure:
      type: list
      items:
        - {item: TSIP, optional: true}
        - {group: Band, as: band, optional: true}
        - {type: list, items: [{item: MDLN}, {type: opaque, as: extra}]}
        - {type: list, repeat: pairs, of: {group: Pair}}
        - {type: list, repeat: tsips, of: {item: TSIP}}
`

// groupTestStructure is a message body repeating a group, a repeat of a group, and an optional group.
const groupTestStructure = `
          type: list
          items:
            - {item: MDLN}
            - {type: list, repeat: holders, of: {group: Holder}}
            - {type: list, repeat: rows, of: {type: list, repeat: pairs, of: {group: Pair}}}
            - {type: list, repeat: bands, of: {group: Band, optional: true}}`

// groupTestMessages wraps one body structure, written as YAML at the indentation of groupTestStructure,
// in a stream-1 messages file.
func groupTestMessages(structure string) string {
	return `
stream: 1
messages:
  - function: 2
    name: Group Test
    direction: equipment-to-host
    bodies:
      - actor: both
        structure:` + structure + "\n"
}

// resolveGroupFixture loads groupsYAML and a message whose body is structure,
// resolves the groups, and validates the message, as run does.
func resolveGroupFixture(t *testing.T, groupsYAML string, structure string) ([]Group, MessageFile, error) {
	t.Helper()

	groups, err := LoadGroups([]byte(groupsYAML))
	require.NoError(t, err)

	mf, err := LoadMessageFile([]byte(groupTestMessages(structure)))
	require.NoError(t, err)

	files := []MessageFile{mf}
	if err := ResolveGroups(groups, files, groupTestItems()); err != nil {
		return nil, mf, err
	}

	return groups, mf, ValidateMessages(files, groupTestItems())
}

// TestLoadGroupsRejectsUnknownKeys proves groups.yaml is read strictly, as the other DSL files are.
func TestLoadGroupsRejectsUnknownKeys(t *testing.T) {
	_, err := LoadGroups([]byte("groups:\n  - name: Pair\n    summary: typo\n"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "summary")
}

// TestResolveGroupsRecordsUsers proves every reference resolves
// and each group learns its users, messages first, for its godoc.
func TestResolveGroupsRecordsUsers(t *testing.T) {
	groups, _, err := resolveGroupFixture(t, groupTestGroups, groupTestStructure)
	require.NoError(t, err)

	byName := map[string]Group{}
	for _, g := range groups {
		byName[g.Name] = g
	}

	require.Equal(t, []string{"S1F2", "[Holder]"}, byName["Pair"].users)
	require.Equal(t, []string{"S1F2", "[Holder]"}, byName["Band"].users)
	require.Equal(t, []string{"S1F2"}, byName["Holder"].users)
}

// TestResolveGroupsRejectsBadGroups proves each rule a group, a reference to one, or a repeat's element must follow.
func TestResolveGroupsRejectsBadGroups(t *testing.T) {
	pair := `
  - name: Pair
    description: One pair.
    structure: {type: list, items: [{item: MDLN}, {item: SVID}]}`
	usePair := `
          type: list
          repeat: pairs
          of: {group: Pair}`

	// group declares one more group after Pair, named name, with structure.
	group := func(name, structure string) string {
		return "groups:" + pair + "\n  - name: " + name + "\n    description: One more.\n    structure: " + structure + "\n"
	}

	// useBoth references Pair and Extra from the message body.
	useBoth := `
          type: list
          items:
            - {type: list, repeat: pairs, of: {group: Pair}}
            - {type: list, repeat: extras, of: {group: Extra}}`

	tests := []struct {
		name      string
		groups    string
		structure string
		wantErr   string
	}{
		{"declared twice", "groups:" + pair + pair, usePair, "declared twice"},
		{"unexported name", strings.Replace("groups:"+pair, "name: Pair", "name: pair", 1), strings.Replace(usePair, "Pair", "pair", 1), "exported Go identifier"},
		{"item name", group("MDLN", "{type: list, items: [{item: MDLN}]}"), usePair, "already the item MDLN"},
		{"value constant", group("STATUSOk", "{type: list, items: [{item: MDLN}]}"), usePair, "already a value of item STATUS"},
		{"builder", group("S1F2", "{type: list, items: [{item: MDLN}]}"), usePair, "already the S1F2 builder"},
		{"decoder", group("DecodeS1F2", "{type: list, items: [{item: MDLN}]}"), usePair, "already the S1F2 decoder"},
		{"result type", group("S1F2Reply", "{type: list, items: [{item: MDLN}]}"), usePair, "already the S1F2 result type"},
		{"decode helper", group("Each", "{type: list, items: [{item: MDLN}]}"), usePair, "decodeEach would collide"},
		{"unused", group("Extra", "{type: list, items: [{item: MDLN}]}"), usePair, "group Extra is not used"},
		{"unknown in message", "groups:" + pair, strings.Replace(usePair, "Pair", "Nope", 1), `unknown group "Nope"`},
		{"unknown in group", group("Extra", "{type: list, items: [{type: list, repeat: nopes, of: {group: Nope}}]}"), useBoth, `group Extra: unknown group "Nope"`},
		{
			"cycle",
			group("Extra", "{type: list, items: [{type: list, repeat: extras, of: {group: Extra}}]}"),
			useBoth,
			"group Extra contains itself",
		},
		{"bounded structure", group("Extra", "{type: list, minItems: 0, maxItems: 1, items: [{item: MDLN}]}"), useBoth, "not bounded"},
		{"repeat structure", group("Extra", "{type: list, repeat: mdlns, of: {item: MDLN}}"), useBoth, "list of declared children"},
		{"bounded sub-list", group("Extra", "{type: list, items: [{type: list, minItems: 0, maxItems: 1, items: [{item: MDLN}]}]}"), useBoth, "cannot be bounded"},
		{"packed", group("Extra", "{type: list, items: [{type: list, packed: tsips, of: {item: TSIP}}]}"), useBoth, "cannot sit inside a group"},
		{"optional ASCII leaf", group("Extra", "{type: list, items: [{item: MDLN, optional: true}]}"), useBoth, "an empty string already sends the zero-length item"},
		{"inline list element", group("Extra", "{type: list, items: [{type: list, repeat: rows, of: {type: list, items: [{item: MDLN}]}}]}"), useBoth, "declare it in groups.yaml"},
		{"child not optional", group("Extra", "{type: list, items: [{group: Pair, as: pair}]}"), useBoth, "needs optional: true and an as: name"},
		{"child without as", group("Extra", "{type: list, items: [{group: Pair, optional: true}]}"), useBoth, "needs optional: true and an as: name"},
		{"as on an element", "groups:" + pair, strings.Replace(usePair, "{group: Pair}", "{group: Pair, as: pair}", 1), "drop as"},
		{"reference with a list key", "groups:" + pair, strings.Replace(usePair, "{group: Pair}", "{group: Pair, type: list}", 1), "may set only as and optional"},
		{"fields collide", group("Extra", "{type: list, items: [{item: MDLN}, {item: SOFTREV, as: mdln}]}"), useBoth, "collide"},
		{
			"child of a body list",
			"groups:" + pair,
			`
          type: list
          items:
            - {item: MDLN}
            - {group: Pair, as: pair, optional: true}`,
			"a message body references a group only as a repeat's of",
		},
		{"inline list element in a body", "groups:" + pair, `
          type: list
          items:
            - {type: list, repeat: pairs, of: {group: Pair}}
            - {type: list, repeat: rows, of: {type: list, items: [{item: MDLN}]}}`, "repeat rows has an inline list element"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := resolveGroupFixture(t, tt.groups, tt.structure)
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestValidateMessagesRejectsUnresolvedGroup proves a group reference ResolveGroups never resolved fails validation.
func TestValidateMessagesRejectsUnresolvedGroup(t *testing.T) {
	mf, err := LoadMessageFile([]byte(groupTestMessages(groupTestStructure)))
	require.NoError(t, err)

	err = ValidateMessages([]MessageFile{mf}, groupTestItems())
	require.Error(t, err)
	require.Contains(t, err.Error(), `unknown group "Holder"`)
}

// TestRenderGroups proves a group renders as a struct, an Item method, and a decode function
// that each walk the same structure:
// an optional leaf, an optional group, a flattened sub-list with an opaque node,
// a repeat of a group, and a repeat of values.
// Errors are labeled relative to the decode function's field parameter.
func TestRenderGroups(t *testing.T) {
	groups, _, err := resolveGroupFixture(t, groupTestGroups, groupTestStructure)
	require.NoError(t, err)

	out, err := renderGroups(groups, groupTestItems())
	require.NoError(t, err)

	got := string(out)

	for _, want := range []string{
		"type Holder struct {\n\tTSIP  *byte\n\tBAND  *Band\n\tMDLN  string\n\tEXTRA secs2.Item\n\tPAIRS []Pair\n\tTSIPS []byte\n}",
		"func (g Holder) Item() secs2.Item {\n\treturn secs2.L(optionalItem(g.TSIP, func(v byte) secs2.Item { return secs2.B(v) }, secs2.B), optionalGroupItem(g.BAND), secs2.L(secs2.A(g.MDLN), present(g.EXTRA, \"Holder.extra\")), " +
			"secs2.L(groupItems(g.PAIRS)...), secs2.L(valueItems(g.TSIPS, func(v byte) secs2.Item { return secs2.B(v) })...))\n}",
		"func decodeHolder(c secs2.Cursor, field string, cfg *decodeConfig) (Holder, error) {",
		"if err = requireArity(c, field, 5, cfg); err != nil {\n\t\treturn g, err",
		`if g.TSIP, err = decodeOptional[*secs2.BinaryItem](c.At(0), field+".TSIP", decodeByteAs[byte]); err != nil {`,
		`if g.BAND, err = decodeOptionalGroup(c.At(1), field+".BAND", 2, cfg, decodeBand); err != nil {`,
		`if err = requireArity(c.At(2), field+"[2]", 2, cfg); err != nil {`,
		// An opaque node reports its parameter name, as at body level.
		`if g.EXTRA, err = decodeItem(c.At(2, 1), field+".extra"); err != nil {`,
		`if g.PAIRS, err = decodeEach(c.At(3), field+".PAIRS", withConfig(cfg, decodePair)); err != nil {`,
		`if g.TSIPS, err = decodeEach(c.At(4), field+".TSIPS", decodeByteAs[byte]); err != nil {`,
		"// Holder is one holder.\n//\n// Shape: L[5]{ B[tsip] <band> L[2]{ A[mdln] <extra> } L[n]{ <pairs>... } L[n]{ <tsips>... } }.\n" +
			"// BAND is nil when it is sent as L[0].\n// TSIP is nil when it is sent as a zero-length item, as SEMI E5 allows.\n//\n// Used by S1F2.\n",
		"// Pair is one pair of a model and a status variable.\n",
		"// Used by S1F2 and [Holder].\n",
	} {
		require.Contains(t, got, want)
	}
}

// TestGroupDocListsFewUsers proves a group's godoc names its users only while there are three or fewer.
func TestGroupDocListsFewUsers(t *testing.T) {
	g := &Group{Name: "Pair", Description: "One pair.", Structure: &StructureNode{Type: "list", Items: []StructureNode{{Item: "MDLN"}}}}

	g.users = []string{"S1F1", "S1F3", "[Holder]"}
	require.Contains(t, groupDoc(g, groupTestItems()), "Used by S1F1, S1F3, and [Holder].")

	g.users = append(g.users, "S1F5")
	for _, line := range groupDoc(g, groupTestItems()) {
		require.NotContains(t, line, "Used by")
	}
}

// TestGroupRepeatsInMessages proves a message body's builder, decoder, and generated tests take and return groups:
// a repeat of a group as []G, a repeat of a repeat as [][]G, and a repeat of an optional group as []*G.
func TestGroupRepeatsInMessages(t *testing.T) {
	_, mf, err := resolveGroupFixture(t, groupTestGroups, groupTestStructure)
	require.NoError(t, err)

	items := groupTestItems()

	builders, err := renderMessages(mf, items)
	require.NoError(t, err)
	require.Contains(t, string(builders), "func S1F2(mdln string, holders []Holder, rows [][]Pair, bands ...*Band) secs2.SECS2Message {")
	require.Contains(t, string(builders), "secs2.L(groupItems(holders)...)")
	require.Contains(t, string(builders), "secs2.L(valueItems(rows, func(v []Pair) secs2.Item { return secs2.L(groupItems(v)...) })...)")
	require.Contains(t, string(builders), "secs2.L(valueItems(bands, optionalGroupItem[Band])...)")

	decoders, err := renderDecoders(mf, items)
	require.NoError(t, err)
	require.Contains(t, string(decoders), "HOLDERS []Holder\n\tROWS    [][]Pair\n\tBANDS   []*Band")
	require.Contains(t, string(decoders), `out.HOLDERS, err = decodeEach(root.At(1), "HOLDERS", withConfig(cfg, decodeHolder))`)
	require.Contains(t, string(decoders), `out.ROWS, err = decodeEach(root.At(2), "ROWS", eachOf(withConfig(cfg, decodePair)))`)
	require.Contains(t, string(decoders), `out.BANDS, err = decodeEach(root.At(3), "BANDS", optionalGroup(cfg, 2, decodeBand))`)

	decodeTests, err := renderDecodeTests(mf, items)
	require.NoError(t, err)

	holder := `{TSIP: new(byte(0x3a)), BAND: &gem.Band{SVID: secs2.A("svid"), SOFTREV: "softrev"}, MDLN: "mdln", EXTRA: secs2.A("extra"), ` +
		`PAIRS: []gem.Pair{{MDLN: "mdln", SVID: secs2.A("svid")}}, TSIPS: []byte{0x39}}`
	require.Contains(t, string(decodeTests),
		`gem.S1F2("mdln", []gem.Holder{`+holder+`}, [][]gem.Pair{{{MDLN: "mdln", SVID: secs2.A("svid")}}}, &gem.Band{SVID: secs2.A("svid"), SOFTREV: "softrev"})`)
	require.Contains(t, string(decodeTests), "HOLDERS: []gem.Holder{"+holder+"},")

	// The group's optional leaf gets a test of its own, sending it nil inside the repeated Holder.
	absent := testFuncBody(t, string(decodeTests), "TestDecodeS1F2HolderTSIPAbsent")
	require.Contains(t, absent, `gem.S1F2("mdln", []gem.Holder{{BAND: &gem.Band{`)
	require.Contains(t, absent, `t.Errorf("tsip: got %x, want the zero-length item %x"`)
	require.Contains(t, absent, "HOLDERS: []gem.Holder{{BAND: &gem.Band{")
	require.Contains(t, absent, "assertDecoded(t, got, want)")

	tests, err := renderTests(mf, items)
	require.NoError(t, err)
	// The builder test walks into the one Holder: its sub-list, its optional Band, and its repeat of Pair.
	require.Contains(t, string(tests), `t.Errorf("extra: got %x, want %x"`)
	require.Contains(t, string(tests), `t.Errorf("softrev: got %q, want %q"`)
}

// TestQualifiedType proves the generated tests qualify a gem type also inside slice and pointer types.
func TestQualifiedType(t *testing.T) {
	for typ, want := range map[string]string{
		"STATUS":           "gem.STATUS",
		"[]Report":         "[]gem.Report",
		"*Deadband":        "*gem.Deadband",
		"[][]*Descriptor":  "[][]*gem.Descriptor",
		"[]secs2.Item":     "[]secs2.Item",
		"[]byte":           "[]byte",
		"secs2.Item":       "secs2.Item",
		"[][]secs2.Item":   "[][]secs2.Item",
		"*gem.Already":     "*gem.Already",
		"uint16":           "uint16",
		"[]CARRIERACTION":  "[]gem.CARRIERACTION",
		"[][]CommandParam": "[][]gem.CommandParam",
	} {
		require.Equalf(t, want, qualifiedType(typ), "qualifiedType(%q)", typ)
	}
}

// TestSliceLiteral proves a one-element slice literal elides the element's own type, as gofmt -s would.
func TestSliceLiteral(t *testing.T) {
	require.Equal(t, "[]gem.Report{{RPTID: x}}", sliceLiteral("Report", "gem.Report{RPTID: x}"))
	require.Equal(t, "[]*gem.Band{{SVID: x}}", sliceLiteral("*Band", "&gem.Band{SVID: x}"))
	require.Equal(t, "[][]gem.Pair{{{MDLN: x}}}", sliceLiteral("[]Pair", "[]gem.Pair{{MDLN: x}}"))
	require.Equal(t, `[]secs2.Item{secs2.A("x")}`, sliceLiteral("secs2.Item", `secs2.A("x")`))
	require.Equal(t, "[]gem.STATUS{3}", sliceLiteral("STATUS", "3"))
}

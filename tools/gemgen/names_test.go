package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// namesMessages wraps one body structure, written as a YAML flow mapping,
// in a stream-1 messages file.
func namesMessages(structure string) string {
	return `
stream: 1
messages:
  - function: 2
    name: Names Test
    bodies:
      - actor: both
        structure: ` + structure + "\n"
}

// TestValidateMessagesRejectsBadNames proves every name rule ValidateMessages enforces:
// where as: may appear, what it may spell,
// and which emitted builder parameters and decoder fields collide.
func TestValidateMessagesRejectsBadNames(t *testing.T) {
	tests := []struct {
		name      string
		structure string
		wantErr   string
	}{
		{"as secs2", `{type: list, items: [{item: MDLN, as: secs2}]}`, "secs2 package"},
		{"as keyword", `{type: list, items: [{item: MDLN, as: type}]}`, "Go keyword"},
		{"as predeclared", `{type: list, items: [{item: MDLN, as: len}]}`, "predeclared"},
		{"as predeclared upper case", `{type: list, items: [{item: MDLN, as: Any}]}`, "predeclared"},
		{"as predeclared builtin", `{type: list, items: [{item: MDLN, as: max}]}`, "predeclared"},
		{"repeat keyword", `{type: list, repeat: func, of: {item: SVID}}`, "Go keyword"},
		{"duplicate item", `{type: list, items: [{item: MDLN}, {item: MDLN}]}`, "collide"},
		{"duplicate nested item", `{type: list, items: [{item: MDLN}, {type: list, items: [{item: MDLN}]}]}`, "collide"},
		{"as collides ignoring case", `{type: list, items: [{item: MDLN}, {item: SOFTREV, as: Mdln}]}`, "collide"},
		{"repeat collides with leaf", `{type: list, items: [{item: SVID}, {type: list, repeat: svid, of: {item: SVID}}]}`, "collide"},
		{"two unnamed opaque nodes", `{type: list, items: [{type: opaque}, {type: list, items: [{type: opaque}]}]}`, "collide"},
		{"as on list", `{type: list, as: info, items: [{item: MDLN}]}`, "only on a leaf, opaque node, or group reference"},
		{"as on repeat", `{type: list, repeat: svids, as: info, of: {item: SVID}}`, "only on a leaf, opaque node, or group reference"},
		{"as on packed", `{type: list, packed: tsips, as: info, of: {item: TSIP}}`, "only on a leaf, opaque node, or group reference"},
		{"as leading digit", `{type: list, items: [{item: MDLN, as: 2mdln}]}`, "letter followed by"},
		{"as underscore", `{type: list, items: [{item: MDLN, as: mdln_b}]}`, "letter followed by"},
		{"as hyphen", `{type: list, items: [{item: MDLN, as: mdln-b}]}`, "letter followed by"},
		{"repeat not an identifier", `{type: list, repeat: sv-ids, of: {item: SVID}}`, "not a Go identifier"},
		{"packed not an identifier", `{type: list, packed: 2tsips, of: {item: TSIP}}`, "not a Go identifier"},
		{"repeat blank identifier", `{type: list, repeat: _, of: {item: SVID}}`, "blank identifier"},
		{"packed blank identifier", `{type: list, packed: _, of: {item: TSIP}}`, "blank identifier"},
		{"repeat field not exported", `{type: list, repeat: _ids, of: {item: SVID}}`, `decoder field "_IDS" is not exported`},
		{"nested packed field not exported", `{type: list, items: [{item: MDLN}, {type: list, packed: _tsips, of: {item: TSIP}}]}`, `decoder field "_TSIPS" is not exported`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mf, err := LoadMessageFile([]byte(namesMessages(tt.structure)))
			require.NoError(t, err)

			err = ValidateMessages([]MessageFile{mf}, testItems())
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestValidateMessagesAcceptsNames proves the name rules leave legal bodies alone:
// an opaque node keeps the default name body,
// an item may repeat across separate repeat of subtrees (they are never emitted),
// as: disambiguates a duplicate item,
// and the decoder's own local names are not reserved.
func TestValidateMessagesAcceptsNames(t *testing.T) {
	tests := []struct {
		name      string
		structure string
	}{
		{"whole-body opaque", `{type: opaque}`},
		{"nested opaque named body", `{type: list, items: [{item: MDLN}, {type: opaque}]}`},
		{"two opaque nodes renamed", `{type: list, items: [{type: opaque, as: params}, {type: list, items: [{type: opaque, as: results}]}]}`},
		{"same items in separate repeats", `{type: list, items: [{type: list, repeat: firsts, of: {item: MDLN}}, {type: list, repeat: seconds, of: {item: MDLN}}]}`},
		{"duplicate item renamed", `{type: list, items: [{item: MDLN}, {item: MDLN, as: mdlnB}]}`},
		{"decoder locals", `{type: list, items: [{item: MDLN, as: out}, {item: MDLN, as: err}, {item: MDLN, as: root}, {item: MDLN, as: item}, {type: opaque, as: body}]}`},
		// Nothing below a repeat's or packed group's of is emitted,
		// so an as: there names nothing and is not checked against Go's reserved names.
		{"as inside repeat of", `{type: list, items: [{item: SVID}, {type: list, repeat: svids, of: {item: SVID, as: type}}]}`},
		{"as inside packed of", `{type: list, packed: tsips, of: {item: TSIP, as: func}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mf, err := LoadMessageFile([]byte(namesMessages(tt.structure)))
			require.NoError(t, err)

			require.NoError(t, ValidateMessages([]MessageFile{mf}, testItems()))
		})
	}
}

// TestNodeNames pins the one naming rule every walker shares:
// the default names shipped messages already use,
// and as: replacing them on a leaf or an opaque node.
func TestNodeNames(t *testing.T) {
	tests := []struct {
		name string
		node StructureNode
		want emittedName
	}{
		{"leaf", StructureNode{Item: "MDLN"}, emittedName{Param: "mdln", Field: "MDLN", Label: "MDLN"}},
		{"leaf as", StructureNode{Item: "MDLN", As: "modelB"}, emittedName{Param: "modelb", Field: "MODELB", Label: "MODELB"}},
		{"opaque", StructureNode{Type: "opaque"}, emittedName{Param: "body", Field: "BODY", Label: "body"}},
		{"opaque as", StructureNode{Type: "opaque", As: "Params"}, emittedName{Param: "params", Field: "PARAMS", Label: "params"}},
		{"repeat", StructureNode{Type: "list", Repeat: "svids"}, emittedName{Param: "svids", Field: "SVIDS", Label: "SVIDS"}},
		{"packed", StructureNode{Type: "list", Packed: "tsips"}, emittedName{Param: "tsips", Field: "TSIPS", Label: "TSIPS"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, nodeNames(&tt.node))
		})
	}
}

// TestRenamedAndNestedOpaqueNodes proves every walker names a renamed leaf and a nested opaque node the same way:
// builder parameters, the body expression, the godoc shorthand,
// the decoder fields and their error labels, and the generated test samples.
func TestRenamedAndNestedOpaqueNodes(t *testing.T) {
	items := testItems()
	structure := &StructureNode{Type: "list", Items: []StructureNode{
		{Item: "MDLN"},
		{Item: "MDLN", As: "mdlnB"},
		{Type: "opaque", As: "params"},
		{Type: "list", Items: []StructureNode{{Type: "opaque"}}},
	}}

	require.Equal(t, []Param{
		{Name: "mdln", Type: "string"},
		{Name: "mdlnb", Type: "string"},
		{Name: "params", Type: "secs2.Item"},
		{Name: "body", Type: "secs2.Item"},
	}, BuildParams(structure, items))

	require.Equal(t, "secs2.L(secs2.A(mdln), secs2.A(mdlnb), params, secs2.L(body))", BodyExpr(structure, items))
	require.Equal(t, "L[4]{ A[mdln] A[mdlnb] <params> L[1]{ <body> } }", BodyDoc(structure, items))
	require.Equal(t, "form-dependent (see description)", BodyDoc(&StructureNode{Type: "opaque"}, items))

	fields, err := BuildFields(structure, items)
	require.NoError(t, err)
	require.Equal(t, []decodeField{
		{Name: "MDLN", Type: "string", Param: 0, Path: []int{0}},
		{Name: "MDLNB", Type: "string", Param: 1, Path: []int{1}},
		{Name: "PARAMS", Type: "secs2.Item", Param: 2, Path: []int{2}},
		{Name: "BODY", Type: "secs2.Item", Param: 3, Path: []int{3, 0}},
	}, fields)

	exprs, err := SampleExprs(structure, items)
	require.NoError(t, err)
	require.Equal(t, []string{`"mdln"`, `"mdlnb"`, `secs2.A("params")`, `secs2.A("body")`}, exprs)

	mf := MessageFile{Stream: 1, Messages: []Message{{
		Function: 2, Name: "Names Test", Source: "e5",
		Bodies: []Body{{Actor: "both", Structure: structure}},
	}}}

	decoders, err := renderDecoders(mf, items)
	require.NoError(t, err)
	require.Contains(t, string(decoders), `out.MDLNB, err = decodeASCII(root.At(1), "MDLNB")`)
	require.Contains(t, string(decoders), `out.PARAMS, err = decodeItem(root.At(2), "params")`)
	require.Contains(t, string(decoders), `out.BODY, err = decodeItem(root.At(3, 0), "body")`)

	tests, err := renderTests(mf, items)
	require.NoError(t, err)
	require.Contains(t, string(tests), `gem.S1F2("mdln", "mdlnb", secs2.A("params"), secs2.A("body"))`)
	require.Contains(t, string(tests), `!= "mdlnb"`)
	require.Contains(t, string(tests), `secs2.A("params").ToBytes()`)

	decodeTests, err := renderDecodeTests(mf, items)
	require.NoError(t, err)
	require.Contains(t, string(decodeTests), `gem.S1F2("mdln", "mdlnb", secs2.A("params"), secs2.A("body"))`)
	require.Contains(t, string(decodeTests), "\t\tMDLN:   \"mdln\",\n"+
		"\t\tMDLNB:  \"mdlnb\",\n"+
		"\t\tPARAMS: secs2.A(\"params\"),\n"+
		"\t\tBODY:   secs2.A(\"body\"),\n")
}

// TestAsInsideRepeatIsInert proves an as: below a repeat's or packed group's of changes no emitted name:
// those walks stop at the group, so only the group's own name is generated.
func TestAsInsideRepeatIsInert(t *testing.T) {
	items := testItems()
	tests := []struct {
		name      string
		structure *StructureNode
		want      []Param
	}{
		{
			"repeat",
			&StructureNode{Type: "list", Repeat: "svids", Of: &StructureNode{Item: "SVID", As: "renamed"}},
			[]Param{{Name: "svids", Type: "secs2.Item", Repeat: true, Variadic: true}},
		},
		{
			"packed",
			&StructureNode{Type: "list", Packed: "tsips", Of: &StructureNode{Item: "TSIP", As: "renamed"}},
			[]Param{{Name: "tsips", Type: "byte", Repeat: true, Variadic: true}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, BuildParams(tt.structure, items))
			require.NotContains(t, BodyExpr(tt.structure, items), "renamed")
			require.NotContains(t, BodyDoc(tt.structure, items), "renamed")

			fields, err := BuildFields(tt.structure, items)
			require.NoError(t, err)
			require.Len(t, fields, 1)
			require.Equal(t, strings.ToUpper(tt.want[0].Name), fields[0].Name)
		})
	}
}

// TestLoadMessageFilesRejectsMismatchedFileName proves a messages file must be named after the stream it declares,
// since the stream number alone picks the gem files it is generated into.
func TestLoadMessageFilesRejectsMismatchedFileName(t *testing.T) {
	tests := []struct {
		name     string
		fileName string
		wantErr  bool
	}{
		{"matching", "s1.yaml", false},
		{"other stream", "s2.yaml", true},
		{"not a stream name", "stream1.yaml", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, tt.fileName), []byte(smokeS1YAML), 0o600))

			_, err := loadMessageFiles(dir)
			if !tt.wantErr {
				require.NoError(t, err)
				return
			}

			require.Error(t, err)
			require.Contains(t, err.Error(), "must be named s1.yaml")
		})
	}
}

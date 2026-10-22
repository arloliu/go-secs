package main

import (
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// listItems extends testItems with one open item of each kind the list rule distinguishes:
// formats that exclude L, formats that include L, and no formats at all.
func listItems() map[string]Item {
	items := testItems()
	maps.Copy(items, map[string]Item{
		"ECV": {Formats: []string{"A", "U4"}, Binding: BindingOpen},
		"V":   {Formats: []string{"L", "A", "U4"}, Binding: BindingOpen},
		"SPR": {Binding: BindingOpen},
	})

	return items
}

// TestRenderDecodersRejectListByFormats proves the decoder of an open leaf rejects a list
// exactly when its item's formats exclude L,
// at body level, per element of a repeat, and per element of a repeat of repeats,
// and that allowEmptyList swaps in the helper that also accepts L[0], at that position only.
// An item that allows a list, one that declares no formats, and an opaque node decode any item.
func TestRenderDecodersRejectListByFormats(t *testing.T) {
	src := renderOneDecoder(t, 2, "both", &StructureNode{Type: "list", Items: []StructureNode{
		{Item: "SVID"},
		{Item: "V"},
		{Item: "SPR"},
		{Type: "opaque", As: "extra"},
		{Item: "ECV"},
		{Item: "ECV", As: "ecvOrNone", AllowEmptyList: true},
		{Type: "list", Repeat: "svids", Of: &StructureNode{Item: "SVID"}},
		{Type: "list", Repeat: "values", Of: &StructureNode{Item: "V"}},
		{Type: "list", Repeat: "ecvs", Of: &StructureNode{Item: "ECV", AllowEmptyList: true}},
		{Type: "list", Repeat: "rows", Of: &StructureNode{Type: "list", Repeat: "cells", Of: &StructureNode{Item: "SVID"}}},
		{Type: "list", Repeat: "tables", Of: &StructureNode{Type: "list", Repeat: "entries", Of: &StructureNode{Item: "V"}}},
	}}, listItems())

	for _, want := range []string{
		`decodeScalarItem(root.At(0), "SVID")`,
		`decodeItem(root.At(1), "V")`,
		`decodeItem(root.At(2), "SPR")`,
		`decodeItem(root.At(3), "extra")`,
		`decodeScalarItem(root.At(4), "ECV")`,
		`decodeScalarOrEmptyList(root.At(5), "ECVORNONE")`,
		`decodeEach(root.At(6), "SVIDS", decodeScalarItem)`,
		`decodeItems(root.At(7), "VALUES")`,
		`decodeEach(root.At(8), "ECVS", decodeScalarOrEmptyList)`,
		`decodeEach(root.At(9), "ROWS", eachOf(decodeScalarItem))`,
		`decodeEach(root.At(10), "TABLES", decodeItems)`,
	} {
		require.Contains(t, src, want)
	}

	// The field types stay secs2 items: only the check changes.
	require.Contains(t, src, "SVIDS     []secs2.Item")
	require.Contains(t, src, "ECVORNONE secs2.Item")
	require.Contains(t, src, "ROWS      [][]secs2.Item")
}

// TestEmptyListDoc proves the result struct and the decoder of a body with an allowEmptyList position
// name that position and quote the exception that gives L[0] its meaning,
// and that a body without one gets no such note.
func TestEmptyListDoc(t *testing.T) {
	m := Message{Exception: "A zero-length list item for ECVi means that ECIDi does not exist."}
	body := &StructureNode{Type: "list", Items: []StructureNode{
		{Item: "ECV", As: "ecvOrNone", AllowEmptyList: true},
		{Item: "ECV"},
		{Type: "list", Repeat: "ecvs", Of: &StructureNode{Item: "ECV", AllowEmptyList: true}},
		{Type: "list", Repeat: "rows", Of: &StructureNode{Type: "list", Repeat: "cells", Of: &StructureNode{Item: "ECV", AllowEmptyList: true}}},
	}}

	require.Equal(t, []string{
		"",
		"ECVORNONE, an element of ECVS, and an element of an element of ROWS may also be L[0], an empty list,",
		"which SEMI E5 gives a meaning in this message; any other list is an error.",
		"",
		"Exception: A zero-length list item for ECVi means that ECIDi does not exist.",
	}, emptyListDoc(body, m))

	require.Nil(t, emptyListDoc(&StructureNode{Type: "list", Repeat: "ecvs", Of: &StructureNode{Item: "ECV"}}, m))

	src := renderOneDecoder(t, 2, "both", &StructureNode{Type: "list", Repeat: "ecvs", Of: &StructureNode{Item: "ECV", AllowEmptyList: true}}, listItems())
	require.Equal(t, 2, strings.Count(src, "// An element of ECVS may also be L[0]"), "the result struct and the decoder each carry the note")
}

// TestRenderGroupsRejectListByFormats proves a group's decode function
// applies the same rule to its own leaves and repeats,
// labeling the errors relative to its field parameter.
func TestRenderGroupsRejectListByFormats(t *testing.T) {
	g := &Group{Name: "Entry", Description: "One entry.", Structure: &StructureNode{Type: "list", Items: []StructureNode{
		{Item: "SVID"},
		{Item: "V"},
		{Type: "list", Repeat: "ecvs", Of: &StructureNode{Item: "ECV"}},
	}}}

	e, err := walkGroupDecoder(g, listItems())
	require.NoError(t, err)

	got := strings.Join(e.lines, "\n")
	require.Contains(t, got, `decodeScalarItem(c.At(0), field+".SVID")`)
	require.Contains(t, got, `decodeItem(c.At(1), field+".V")`)
	require.Contains(t, got, `decodeEach(c.At(2), field+".ECVS", decodeScalarItem)`)
}

// TestValidateMessagesAllowEmptyList proves allowEmptyList: is accepted only on a leaf whose decoder rejects lists,
// wherever that leaf sits.
func TestValidateMessagesAllowEmptyList(t *testing.T) {
	accepted := []struct {
		name      string
		structure string
	}{
		{"body leaf", `{type: list, items: [{item: ECV, allowEmptyList: true}]}`},
		{"whole body", `{item: ECV, allowEmptyList: true}`},
		{"repeat element", `{type: list, repeat: ecvs, of: {item: ECV, allowEmptyList: true}}`},
	}

	for _, tt := range accepted {
		t.Run(tt.name, func(t *testing.T) {
			mf, err := LoadMessageFile([]byte(namesMessages(tt.structure)))
			require.NoError(t, err)

			require.NoError(t, ValidateMessages([]MessageFile{mf}, listItems()))
		})
	}

	rejected := []struct {
		name      string
		structure string
		wantErr   string
	}{
		{"on a list", `{type: list, allowEmptyList: true, items: [{item: ECV}]}`, "only on a leaf"},
		{"on a repeat", `{type: list, repeat: ecvs, allowEmptyList: true, of: {item: ECV}}`, "only on a leaf"},
		{"on an opaque node", `{type: list, items: [{type: opaque, allowEmptyList: true}]}`, "only on a leaf"},
		{"fixed item", `{type: list, items: [{item: MDLN, allowEmptyList: true}]}`, "must be binding: open"},
		{"item that allows a list", `{type: list, items: [{item: V, allowEmptyList: true}]}`, "exclude L"},
		{"item with no formats", `{type: list, items: [{item: SPR, allowEmptyList: true}]}`, "declared formats"},
	}

	for _, tt := range rejected {
		t.Run(tt.name, func(t *testing.T) {
			mf, err := LoadMessageFile([]byte(namesMessages(tt.structure)))
			require.NoError(t, err)

			err = ValidateMessages([]MessageFile{mf}, listItems())
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestAllowEmptyListOnlyWhereE5GivesItMeaning pins the positions of the real data that accept L[0]
// for an item whose formats exclude L.
// SEMI E5 gives that form a meaning only for S2F14's equipment constant values:
// "A zero-length list item for ECVi means that ECIDi does not exist.
// The list format for this data item is not allowed, except in this case."
// So S2F15's constant values, the same item, stay strict.
func TestAllowEmptyListOnlyWhereE5GivesItMeaning(t *testing.T) {
	_, groups, files := loadRealData(t)

	var got []string

	var walk func(where string, n *StructureNode)
	walk = func(where string, n *StructureNode) {
		if n == nil {
			return
		}

		if n.AllowEmptyList {
			got = append(got, where+" "+n.Item)
		}

		for i := range n.Items {
			walk(where, &n.Items[i])
		}

		walk(where, n.Of)
	}

	for _, mf := range files {
		for _, m := range mf.Messages {
			for _, b := range m.Bodies {
				walk(fmt.Sprintf("S%dF%d", mf.Stream, m.Function), b.Structure)
			}
		}
	}

	for _, g := range groups {
		walk("group "+g.Name, g.Structure)
	}

	require.Equal(t, []string{"S2F14 ECV"}, got)
}

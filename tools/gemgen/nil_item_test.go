package main

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// nilItemItems is the item dictionary the nil-item expression tests draw on:
// an open item, a fixed scalar, and a fixed byte array, which a repeat takes as secs2 items.
func nilItemItems() map[string]Item {
	return map[string]Item{
		"SVID":  {Formats: []string{"U1", "U2", "U4", "U8"}, Binding: BindingOpen},
		"MDLN":  {Formats: []string{"A"}, Binding: BindingFixed, GoType: "string"},
		"MHEAD": {Formats: []string{"B"}, Binding: BindingFixed, GoType: "[10]byte"},
	}
}

// TestBodyExprWrapsEveryItem proves each shape whose emitted Go type is secs2.Item, or a slice of them,
// goes through present or presentEach,
// while fixed leaves, fixed byte-array leaves outside a repeat, and repeats of values stay as they were.
func TestBodyExprWrapsEveryItem(t *testing.T) {
	items := nilItemItems()

	tests := []struct {
		name string
		node *StructureNode
		want string
	}{
		{"open leaf", &StructureNode{Item: "SVID"}, `present(svid, "S1F1 SVID")`},
		{"opaque body", &StructureNode{Type: "opaque"}, `present(body, "S1F1 body")`},
		{"renamed opaque", &StructureNode{Type: "opaque", As: "data"}, `present(data, "S1F1 data")`},
		{"fixed leaf", &StructureNode{Item: "MDLN"}, "secs2.A(mdln)"},
		{"fixed byte-array leaf", &StructureNode{Item: "MHEAD"}, "secs2.B(mhead[:])"},
		{
			"opaque in a list",
			&StructureNode{Type: "list", Items: []StructureNode{{Item: "MDLN"}, {Type: "opaque", As: "extra"}}},
			`secs2.L(secs2.A(mdln), present(extra, "S1F1 extra"))`,
		},
		{
			"open-leaf repeat",
			&StructureNode{Type: "list", Repeat: "svids", Of: &StructureNode{Item: "SVID"}},
			`secs2.L(presentEach(svids, "S1F1 SVIDS")...)`,
		},
		{
			"fixed byte-array repeat",
			&StructureNode{Type: "list", Repeat: "mheads", Of: &StructureNode{Item: "MHEAD"}},
			`secs2.L(presentEach(mheads, "S1F1 MHEADS")...)`,
		},
		{
			"opaque repeat",
			&StructureNode{Type: "list", Repeat: "bodies", Of: &StructureNode{Type: "opaque"}},
			`secs2.L(presentEach(bodies, "S1F1 BODIES")...)`,
		},
		{
			"repeat of inner lists",
			&StructureNode{Type: "list", Repeat: "rows", Of: &StructureNode{Type: "list", Items: []StructureNode{{Item: "MDLN"}}}},
			`secs2.L(presentEach(rows, "S1F1 ROWS")...)`,
		},
		{
			"fixed scalar repeat",
			&StructureNode{Type: "list", Repeat: "names", Of: &StructureNode{Item: "MDLN"}},
			"secs2.L(valueItems(names, secs2.A)...)",
		},
		{
			"repeat of repeats of items",
			&StructureNode{Type: "list", Repeat: "rows", Of: &StructureNode{Type: "list", Repeat: "cells", Of: &StructureNode{Item: "SVID"}}},
			`secs2.L(indexedItems(rows, func(i0 int, v []secs2.Item) secs2.Item { return secs2.L(presentEach(v, "S1F1 ROWS", i0)...) })...)`,
		},
		{
			"three-deep repeat of items",
			&StructureNode{Type: "list", Repeat: "cubes", Of: &StructureNode{
				Type: "list", Repeat: "rows", Of: &StructureNode{Type: "list", Repeat: "cells", Of: &StructureNode{Type: "opaque"}},
			}},
			`secs2.L(indexedItems(cubes, func(i0 int, v [][]secs2.Item) secs2.Item { return secs2.L(indexedItems(v, ` +
				`func(i1 int, v []secs2.Item) secs2.Item { return secs2.L(presentEach(v, "S1F1 CUBES", i0, i1)...) })...) })...)`,
		},
		{
			"repeat of repeats of values",
			&StructureNode{Type: "list", Repeat: "rows", Of: &StructureNode{Type: "list", Repeat: "names", Of: &StructureNode{Item: "MDLN"}}},
			"secs2.L(valueItems(rows, func(v []string) secs2.Item { return secs2.L(valueItems(v, secs2.A)...) })...)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, BodyExpr(tt.node, items, "S1F1"))
		})
	}
}

// TestGroupItemExprWrapsEveryItem proves a group's Item method names a nil field or element after the group and the field's label,
// the name its decode errors report: an opaque field by its parameter name.
func TestGroupItemExprWrapsEveryItem(t *testing.T) {
	groups, _, err := resolveGroupFixture(t, groupTestGroups, groupTestStructure)
	require.NoError(t, err)

	out, err := renderGroups(groups, groupTestItems())
	require.NoError(t, err)

	got := string(out)
	require.Contains(t, got, `return secs2.L(secs2.A(g.MDLN), present(g.SVID, "Pair.SVID"))`)
	require.Contains(t, got, `secs2.L(secs2.A(g.MDLN), present(g.EXTRA, "Holder.extra"))`)
	require.Contains(t, got, "// A nil secs2.Item field or element makes the item carry an error wrapping [ErrNilItem].\n")
	require.NotContains(t, got, "shortens the list")
}

// itemClosureRE matches a generated function literal that takes a slice of secs2 items, at any depth, as v,
// and captures what its body passes v through.
var itemClosureRE = regexp.MustCompile(`v (?:\[\])+secs2\.Item\) secs2\.Item \{ return secs2\.L\((\w*)\(?v\b`)

// stringLiteralRE matches an interpreted Go string literal without escapes, as the generator renders a name.
var stringLiteralRE = regexp.MustCompile(`"[^"\\]*"`)

// requireItemRefsWrapped asserts every reference in expr to a value whose Go type is secs2.Item, or a slice of them,
// goes through present, presentEach, or indexedItems, and returns how many it checked.
// refs maps each reference, such as a parameter or g.FIELD, to its Go type.
func requireItemRefsWrapped(t *testing.T, where string, expr string, refs map[string]string) int {
	t.Helper()

	// Drop string literals, so a name such as "S1F6 body" never reads as a reference.
	expr = stringLiteralRE.ReplaceAllString(expr, `""`)
	checked := 0

	for ref, typ := range refs {
		var wrapper string

		switch {
		case typ == "secs2.Item":
			wrapper = "present("
		case typ == "[]secs2.Item":
			wrapper = "presentEach("
		case strings.HasPrefix(typ, "[]") && strings.HasSuffix(typ, "]secs2.Item"):
			wrapper = "indexedItems("
		default:
			continue
		}

		re := regexp.MustCompile(`(^|[^\w.])` + regexp.QuoteMeta(ref) + `\b`)
		matches := re.FindAllStringIndex(expr, -1)
		require.NotEmptyf(t, matches, "%s: %s (%s) is not referenced in %s", where, ref, typ, expr)

		for _, m := range matches {
			start := m[0] + len(re.FindStringSubmatch(expr[m[0]:m[1]])[1])
			require.Truef(t, strings.HasSuffix(expr[:start], wrapper),
				"%s: %s (%s) is not passed through %s in %s", where, ref, typ, wrapper, expr)
			checked++
		}
	}

	for _, m := range itemClosureRE.FindAllStringSubmatch(expr, -1) {
		require.Containsf(t, []string{"presentEach", "indexedItems"}, m[1],
			"%s: a repeat's inner slice of items is sent unchecked in %s", where, expr)
		checked++
	}

	return checked
}

// TestRealDataSendsNoItemUnchecked walks every message body and group of the real data
// and proves no value whose Go type is secs2.Item, or a slice of them, reaches secs2.L or secs2.NewMessage unchecked.
func TestRealDataSendsNoItemUnchecked(t *testing.T) {
	items, groups, files := loadRealData(t)

	checked := 0

	for _, mf := range files {
		for _, m := range mf.Messages {
			code := fmt.Sprintf("S%dF%d", mf.Stream, m.Function)

			for _, b := range m.Bodies {
				refs := map[string]string{}
				for _, p := range BuildParams(b.Structure, items) {
					typ := p.Type
					if p.Repeat {
						typ = "[]" + typ
					}

					refs[p.Name] = typ
				}

				checked += requireItemRefsWrapped(t, builderName(mf.Stream, m.Function, b), BodyExpr(b.Structure, items, code), refs)
			}
		}
	}

	for i := range groups {
		g := &groups[i]

		e, err := walkGroupDecoder(g, items)
		require.NoError(t, err)

		refs := map[string]string{}
		for _, f := range e.fields {
			refs["g."+f.Name] = f.Type
		}

		checked += requireItemRefsWrapped(t, g.Name, groupItemExpr(g, items), refs)
	}

	// The real data holds hundreds of caller-supplied items; a handful would mean the walk missed them.
	require.Greater(t, checked, 300)
}

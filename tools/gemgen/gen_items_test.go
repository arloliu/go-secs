package main

import (
	"regexp"
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
			Values: []ItemValue{{Name: "Accepted", Value: 0}, {Name: "Denied", Value: 1}}},
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

// TestRenderItemsRejectsOpenWithValues is defense-in-depth: ValidateItems
// (Task 4) already rejects an open-binding item with a values: table before
// rendering is ever reached, but renderItems must not silently emit a
// meaningless "type <ITEM> " declaration with no goType if it is ever called
// on unvalidated input.
func TestRenderItemsRejectsOpenWithValues(t *testing.T) {
	items := map[string]Item{
		"BADENUM": {Formats: []string{"U1", "U2", "U4", "U8"}, Binding: BindingOpen,
			Values: []ItemValue{{Name: "X", Value: 0}}},
	}
	_, err := renderItems(items)
	require.Error(t, err)
}

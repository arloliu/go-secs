package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// paramsItems supplies the item dictionary the §6 table rows reference.
//
// MDLN/SOFTREV are fixed A (string); SFCD is fixed B (byte, no values table);
// COMMACK is fixed B (byte) WITH a non-empty values table, so it generates the
// named type COMMACK and a byte() conversion at the call site; every *ID/*VALUE
// item is open (renders as secs2.Item).
func paramsItems() map[string]Item {
	return map[string]Item{
		"MDLN":    {Formats: []string{"A"}, Binding: BindingFixed, GoType: "string"},
		"SOFTREV": {Formats: []string{"A"}, Binding: BindingFixed, GoType: "string"},
		"SFCD":    {Formats: []string{"B"}, Binding: BindingFixed, GoType: "byte"},
		"COMMACK": {Formats: []string{"B"}, Binding: BindingFixed, GoType: "byte", Values: []ItemValue{{Name: "OK", Value: 0}}},
		"SVID":    {Formats: []string{"U1", "U2", "U4", "U8"}, Binding: BindingOpen},
		"TSIP":    {Formats: []string{"B"}, Binding: BindingFixed, GoType: "byte"},
		"TSOP":    {Formats: []string{"B"}, Binding: BindingFixed, GoType: "byte"},
		"OBJTYPE": {Formats: []string{"U1", "U2", "U4", "U8"}, Binding: BindingOpen},
		"OBJID":   {Formats: []string{"U1", "U2", "U4", "U8"}, Binding: BindingOpen},
		"ATTRID":  {Formats: []string{"U1", "U2", "U4", "U8"}, Binding: BindingOpen},
	}
}

func intp(i int) *int { return &i }

func TestBuildParams(t *testing.T) {
	items := paramsItems()

	tests := []struct {
		name string
		node *StructureNode
		want []Param
	}{
		{
			name: "header-only",
			node: nil,
			want: nil,
		},
		{
			name: "fixed list S1F2",
			node: &StructureNode{Type: "list", Items: []StructureNode{{Item: "MDLN"}, {Item: "SOFTREV"}}},
			want: []Param{
				{Name: "mdln", Type: "string"},
				{Name: "softrev", Type: "string"},
			},
		},
		{
			name: "nested list + enum leaf S1F14",
			node: &StructureNode{Type: "list", Items: []StructureNode{
				{Item: "COMMACK"},
				{Type: "list", Items: []StructureNode{{Item: "MDLN"}, {Item: "SOFTREV"}}},
			}},
			want: []Param{
				{Name: "commack", Type: "COMMACK"},
				{Name: "mdln", Type: "string"},
				{Name: "softrev", Type: "string"},
			},
		},
		{
			name: "bare fixed leaf S1F5",
			node: &StructureNode{Item: "SFCD"},
			want: []Param{{Name: "sfcd", Type: "byte"}},
		},
		{
			name: "opaque S1F6",
			node: &StructureNode{Type: "opaque"},
			want: []Param{{Name: "body", Type: "secs2.Item"}},
		},
		{
			name: "open repeat final variadic S1F3",
			node: &StructureNode{Type: "list", Repeat: "svids", Of: &StructureNode{Item: "SVID"}},
			want: []Param{{Name: "svids", Type: "secs2.Item", Repeat: true, Variadic: true}},
		},
		{
			name: "sibling packed groups S1F10",
			node: &StructureNode{Type: "list", Items: []StructureNode{
				{Type: "list", Packed: "tsips", Of: &StructureNode{Item: "TSIP"}},
				{Type: "list", Packed: "tsops", Of: &StructureNode{Item: "TSOP"}},
			}},
			want: []Param{
				{Name: "tsips", Type: "byte", Repeat: true, Variadic: false},
				{Name: "tsops", Type: "byte", Repeat: true, Variadic: true},
			},
		},
		{
			name: "fixed leaf + sibling repeats S1F19",
			node: &StructureNode{Type: "list", Items: []StructureNode{
				{Item: "OBJTYPE"},
				{Type: "list", Repeat: "objids", Of: &StructureNode{Item: "OBJID"}},
				{Type: "list", Repeat: "attrids", Of: &StructureNode{Item: "ATTRID"}},
			}},
			want: []Param{
				{Name: "objtype", Type: "secs2.Item"},
				{Name: "objids", Type: "secs2.Item", Repeat: true, Variadic: false},
				{Name: "attrids", Type: "secs2.Item", Repeat: true, Variadic: true},
			},
		},
		{
			name: "sibling repeats of lists S1F20",
			node: &StructureNode{Type: "list", Items: []StructureNode{
				{Type: "list", Repeat: "objects", Of: &StructureNode{Type: "list", Items: []StructureNode{{Item: "OBJTYPE"}}}},
				{Type: "list", Repeat: "errors", Of: &StructureNode{Type: "list", Items: []StructureNode{{Item: "OBJID"}}}},
			}},
			want: []Param{
				{Name: "objects", Type: "secs2.Item", Repeat: true, Variadic: false},
				{Name: "errors", Type: "secs2.Item", Repeat: true, Variadic: true},
			},
		},
		{
			name: "bounded-arity list",
			node: &StructureNode{Type: "list", MinItems: intp(0), MaxItems: intp(4), Items: []StructureNode{{Item: "MDLN"}}},
			want: []Param{{Name: "mdln", Type: "string"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildParams(tt.node, items)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestBodyExpr(t *testing.T) {
	items := paramsItems()

	tests := []struct {
		name string
		node *StructureNode
		want string
	}{
		{"header-only", nil, "secs2.NewEmptyItem()"},
		{
			"fixed list S1F2",
			&StructureNode{Type: "list", Items: []StructureNode{{Item: "MDLN"}, {Item: "SOFTREV"}}},
			"secs2.L(secs2.A(mdln), secs2.A(softrev))",
		},
		{
			"nested list + enum leaf S1F14",
			&StructureNode{Type: "list", Items: []StructureNode{
				{Item: "COMMACK"},
				{Type: "list", Items: []StructureNode{{Item: "MDLN"}, {Item: "SOFTREV"}}},
			}},
			"secs2.L(secs2.B(byte(commack)), secs2.L(secs2.A(mdln), secs2.A(softrev)))",
		},
		{"bare fixed leaf S1F5", &StructureNode{Item: "SFCD"}, "secs2.B(sfcd)"},
		{"opaque S1F6", &StructureNode{Type: "opaque"}, "body"},
		{
			"open repeat final variadic S1F3",
			&StructureNode{Type: "list", Repeat: "svids", Of: &StructureNode{Item: "SVID"}},
			"secs2.L(svids...)",
		},
		{
			"sibling packed groups S1F10",
			&StructureNode{Type: "list", Items: []StructureNode{
				{Type: "list", Packed: "tsips", Of: &StructureNode{Item: "TSIP"}},
				{Type: "list", Packed: "tsops", Of: &StructureNode{Item: "TSOP"}},
			}},
			"secs2.L(secs2.B(tsips), secs2.B(tsops))",
		},
		{
			"fixed leaf + sibling repeats S1F19",
			&StructureNode{Type: "list", Items: []StructureNode{
				{Item: "OBJTYPE"},
				{Type: "list", Repeat: "objids", Of: &StructureNode{Item: "OBJID"}},
				{Type: "list", Repeat: "attrids", Of: &StructureNode{Item: "ATTRID"}},
			}},
			"secs2.L(objtype, secs2.L(objids...), secs2.L(attrids...))",
		},
		{
			"sibling repeats of lists S1F20",
			&StructureNode{Type: "list", Items: []StructureNode{
				{Type: "list", Repeat: "objects", Of: &StructureNode{Type: "list", Items: []StructureNode{{Item: "OBJTYPE"}}}},
				{Type: "list", Repeat: "errors", Of: &StructureNode{Type: "list", Items: []StructureNode{{Item: "OBJID"}}}},
			}},
			"secs2.L(secs2.L(objects...), secs2.L(errors...))",
		},
		{
			"bounded-arity list",
			&StructureNode{Type: "list", MinItems: intp(0), MaxItems: intp(4), Items: []StructureNode{{Item: "MDLN"}}},
			"secs2.L(secs2.A(mdln))",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, BodyExpr(tt.node, items))
		})
	}
}

func TestBodyExpr_FixedByteArray(t *testing.T) {
	items := map[string]Item{"MHEAD": {Formats: []string{"B"}, Binding: BindingFixed, GoType: "[10]byte"}}
	got := BodyExpr(&StructureNode{Item: "MHEAD"}, items)
	require.Equal(t, "secs2.B(mhead[:])", got)
}

func TestBodyExpr_ByteSlice(t *testing.T) {
	items := map[string]Item{"ABS": {Formats: []string{"B"}, Binding: BindingFixed, GoType: "[]byte"}}
	got := BodyExpr(&StructureNode{Item: "ABS"}, items)
	require.Equal(t, "secs2.B(abs)", got)
}

func TestBodyDoc(t *testing.T) {
	items := paramsItems()

	tests := []struct {
		name string
		node *StructureNode
		want string
	}{
		{"header-only", nil, "Header only."},
		{
			"fixed list S1F2",
			&StructureNode{Type: "list", Items: []StructureNode{{Item: "MDLN"}, {Item: "SOFTREV"}}},
			"L[2]{ A[mdln] A[softrev] }",
		},
		{
			"nested list + enum leaf S1F14",
			&StructureNode{Type: "list", Items: []StructureNode{
				{Item: "COMMACK"},
				{Type: "list", Items: []StructureNode{{Item: "MDLN"}, {Item: "SOFTREV"}}},
			}},
			"L[2]{ B[commack] L[2]{ A[mdln] A[softrev] } }",
		},
		{"bare fixed leaf S1F5", &StructureNode{Item: "SFCD"}, "B[sfcd]"},
		{"opaque S1F6", &StructureNode{Type: "opaque"}, "form-dependent (see description)"},
		{
			"open repeat final variadic S1F3",
			&StructureNode{Type: "list", Repeat: "svids", Of: &StructureNode{Item: "SVID"}},
			"L[n]{ <svids>... }",
		},
		{
			"sibling packed groups S1F10",
			&StructureNode{Type: "list", Items: []StructureNode{
				{Type: "list", Packed: "tsips", Of: &StructureNode{Item: "TSIP"}},
				{Type: "list", Packed: "tsops", Of: &StructureNode{Item: "TSOP"}},
			}},
			"L[2]{ B[n]{ <tsips>... } B[n]{ <tsops>... } }",
		},
		{
			"fixed leaf + sibling repeats S1F19",
			&StructureNode{Type: "list", Items: []StructureNode{
				{Item: "OBJTYPE"},
				{Type: "list", Repeat: "objids", Of: &StructureNode{Item: "OBJID"}},
				{Type: "list", Repeat: "attrids", Of: &StructureNode{Item: "ATTRID"}},
			}},
			"L[3]{ <objtype> L[n]{ <objids>... } L[n]{ <attrids>... } }",
		},
		{
			"bounded-arity list",
			&StructureNode{Type: "list", MinItems: intp(0), MaxItems: intp(4), Items: []StructureNode{{Item: "MDLN"}}},
			"L[0,4]{ A[mdln] }",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, BodyDoc(tt.node, items))
		})
	}
}

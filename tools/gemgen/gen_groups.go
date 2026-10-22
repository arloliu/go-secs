package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/format"
	"text/template"
	"unicode"
)

//go:embed templates/groups.go.tmpl
var groupsTemplate string

// maxUsersInDoc is the most users a group's godoc lists;
// a group with more is shared too widely for the list to help.
const maxUsersInDoc = 3

// groupView is the per-group view model the groups template renders.
type groupView struct {
	Name     string
	Doc      []string      // godoc lines for the struct (without leading "// ")
	Fields   []decodeField // struct fields, in list order
	ItemExpr string        // the expression the Item method returns
	Lines    []string      // pre-rendered Go statement lines of the decode function body
}

// renderGroups renders gem/groups.go: for each group, its struct,
// the Item method that builds the group's list,
// and the unexported function that decodes one.
//
// The struct serves both directions:
// a builder takes it as a repeat's element and a decoder returns it so,
// with the same fields a message's result struct would have for the same list.
func renderGroups(groups []Group, items map[string]Item) ([]byte, error) {
	tmpl, err := template.New("groups.go.tmpl").Parse(groupsTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse groups template: %w", err)
	}

	views := make([]groupView, 0, len(groups))

	for i := range groups {
		g := &groups[i]

		e, err := walkGroupDecoder(g, items)
		if err != nil {
			return nil, err
		}

		views = append(views, groupView{
			Name:     g.Name,
			Doc:      groupDoc(g, items),
			Fields:   e.fields,
			ItemExpr: nodeExpr(g.Structure, items, func(n *StructureNode) string { return "g." + nodeNames(n).Field }),
			Lines:    e.lines,
		})
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, struct{ Groups []groupView }{Groups: views}); err != nil {
		return nil, fmt.Errorf("execute groups template: %w", err)
	}

	out, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated groups source: %w", err)
	}

	return out, nil
}

// groupDoc renders the godoc lines of a group's struct:
// a name-first summary made of its description, its wire shape in the Body: shorthand of a builder's godoc,
// what its optional fields hold for the zero-length list,
// and, when few enough use it, the messages and groups that do.
func groupDoc(g *Group, items map[string]Item) []string {
	doc := wrapParagraph(ensurePeriod(g.Name + " is " + lowerFirst(g.Description)))
	doc = append(doc, "")
	doc = append(doc, wrapParagraph(ensurePeriod("Shape: "+nodeDoc(g.Structure, items)))...)

	var optional []string
	for i := range g.Structure.Items {
		if n := &g.Structure.Items[i]; n.Kind() == "group" {
			optional = append(optional, nodeNames(n).Field)
		}
	}

	switch len(optional) {
	case 0:
	case 1:
		doc = append(doc, optional[0]+" is nil when it is sent as L[0].")
	default:
		doc = append(doc, joinAnd(optional)+" are each nil when sent as L[0].")
	}

	if len(g.users) <= maxUsersInDoc {
		doc = append(doc, "", "Used by "+joinAnd(g.users)+".")
	}

	return doc
}

// lowerFirst lower-cases the first letter of s, a sentence that a name-first summary continues,
// unless the first word is an acronym such as PDE.
func lowerFirst(s string) string {
	r := []rune(s)
	if len(r) < 2 || !unicode.IsUpper(r[0]) || unicode.IsUpper(r[1]) {
		return s
	}

	r[0] = unicode.ToLower(r[0])

	return string(r)
}

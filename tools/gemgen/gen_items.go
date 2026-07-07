package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/format"
	"slices"
	"text/template"
)

//go:embed templates/items.go.tmpl
var itemsTemplate string

// constView is one named constant rendered in an enum item's const block.
type constView struct {
	Name  string
	Value int64
}

// typeView is one enum item's named defined type plus its const block.
type typeView struct {
	Name   string
	GoType string
	Consts []constView
}

// renderItems renders gem/items.go: a named defined type and a const block
// of named values for every item with a non-empty `values:` table. Items
// without a values table contribute no output. Item order is name-sorted so
// the generated file is deterministic across runs.
func renderItems(items map[string]Item) ([]byte, error) {
	names := make([]string, 0, len(items))
	for name := range items {
		names = append(names, name)
	}
	slices.Sort(names)

	var types []typeView
	for _, name := range names {
		it := items[name]
		if len(it.Values) == 0 {
			continue
		}

		// Defense-in-depth: ValidateItems (Task 4) already rejects an
		// open-binding item with a values: table before rendering is ever
		// reached, but renderItems must not silently emit a meaningless
		// "type <ITEM> " declaration with no goType if it is ever called on
		// unvalidated input.
		if it.Binding != BindingFixed || it.GoType == "" {
			return nil, fmt.Errorf("item %s: values table requires a fixed binding with a concrete goType", name)
		}

		consts := make([]constView, len(it.Values))
		for i, v := range it.Values {
			consts[i] = constView{Name: name + v.Name, Value: v.Value}
		}

		types = append(types, typeView{Name: name, GoType: it.GoType, Consts: consts})
	}

	tmpl, err := template.New("items.go.tmpl").Parse(itemsTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse items template: %w", err)
	}

	data := struct{ Types []typeView }{Types: types}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("execute items template: %w", err)
	}

	out, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated items source: %w", err)
	}

	return out, nil
}

package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/format"
	"regexp"
	"slices"
	"strings"
	"text/template"
)

//go:embed templates/items.go.tmpl
var itemsTemplate string

// semiSourceRE matches an item or value source that names a SEMI standard, e.g. e5 or e87.1.
var semiSourceRE = regexp.MustCompile(`^e[0-9]`)

// constView is one named constant rendered in an enum item's const block.
type constView struct {
	Name  string
	Value string   // Go literal: a decimal integer or a quoted string
	Doc   []string // godoc lines (without leading "// "): the value's own source, then its note
}

// typeView is one enum item's const block,
// and, for a fixed item, the named defined type its constants have;
// a fixed item marked namedType without a values table has the type and no constants.
type typeView struct {
	Name   string
	Typed  bool     // true: a named type over GoType; false: untyped constants of an open item
	GoType string   // the named type's underlying type; empty when not Typed
	Doc    []string // godoc lines of the named type or the untyped const block (without leading "// ")
	Consts []constView
}

// renderItems renders gem/items.go from every item with a non-empty `values:` table,
// and from every fixed item marked namedType.
//
// A fixed item gets a named defined type over its goType and a const block of that type,
// so its constants are assignable to the builder parameters and decoder fields that use the type.
// A fixed item marked namedType without a values table gets the named type alone,
// documented by its item description.
// An open item, whose parameter and field stay secs2.Item,
// gets an untyped const block only:
// the caller passes a constant to the secs2 constructor of the format they send.
// An item's note ends the godoc of its type or const block,
// and a value's note ends the godoc of its constant.
// Other items contribute no output.
// Item order is name-sorted and value order follows the YAML,
// so the generated file is deterministic across runs.
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
			if hasNamedType(it) {
				types = append(types, plainTypeView(name, it))
			}

			continue
		}

		consts := make([]constView, len(it.Values))
		for i, v := range it.Values {
			if !v.Value.IsSet() {
				return nil, fmt.Errorf("item %s: value %s has no value", name, v.Name)
			}

			consts[i] = constView{Name: name + v.Name, Value: v.Value.Literal()}
			if v.Source != "" && v.Source != it.Source {
				consts[i].Doc = append(consts[i].Doc, fmt.Sprintf("%s is defined by %s.", consts[i].Name, sourceName(v.Source)))
			}

			if v.Note != "" {
				consts[i].Doc = append(consts[i].Doc, v.Note)
			}
		}

		tv := typeView{Name: name, Consts: consts}

		switch {
		case it.Binding == BindingFixed && it.GoType != "":
			tv.Typed = true
			tv.GoType = it.GoType
			tv.Doc = []string{fmt.Sprintf("%s enumerates the defined values of the %s item.", name, name)}
		case it.Binding == BindingOpen:
			tv.Doc = untypedValuesDoc(name, it)
		default:
			// Defense-in-depth: ValidateItems rejects a fixed item without a goType before rendering,
			// but renderItems must not emit a "type <ITEM> " declaration with no type on unvalidated input.
			return nil, fmt.Errorf("item %s: a fixed item's values table needs a concrete goType", name)
		}

		if it.Note != "" {
			tv.Doc = append(tv.Doc, it.Note)
		}

		types = append(types, tv)
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

// plainTypeView returns the declaration of a fixed item's named type that has no values table:
// the type alone, documented by the item's description.
func plainTypeView(name string, it Item) typeView {
	doc := []string{fmt.Sprintf("%s is the type of the %s item.", name, name)}
	if it.Description != "" {
		doc = append(doc, it.Description)
	}

	return typeView{Name: name, Typed: true, GoType: it.GoType, Doc: doc}
}

// untypedValuesDoc renders the godoc lines of an open item's untyped const block:
// which item the values belong to, the formats it may be sent as,
// and how a caller sends one.
func untypedValuesDoc(name string, it Item) []string {
	doc := []string{
		fmt.Sprintf("Defined values of the %s item, which may be sent as %s.", name, orList(it.Formats)),
		"They are untyped constants:",
	}

	ctor := exampleFormat(it)
	if ctor == "" {
		return append(doc, "pass one to the secs2 constructor of the format to send.")
	}

	return append(doc, fmt.Sprintf("pass one to the secs2 constructor of the format to send, e.g. secs2.%s(%s%s).", ctor, name, it.Values[0].Name))
}

// exampleFormat returns a format of an open item whose secs2 constructor takes the item's first value,
// or "" when it has none:
// A for a string value,
// the first signed integer format the item declares for a negative value,
// and its first integer format of any kind for another integer value.
func exampleFormat(it Item) string {
	first := it.Values[0].Value
	if first.IsString() {
		if slices.Contains(it.Formats, "A") {
			return "A"
		}

		return ""
	}

	for _, f := range it.Formats {
		if isIntegerFormat(f) && (!first.IsNegative() || strings.HasPrefix(f, "I")) {
			return f
		}
	}

	return ""
}

// orList joins words as an English list ending in "or", e.g. "U1, U2, or U4".
func orList(words []string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	case 2:
		return words[0] + " or " + words[1]
	default:
		return strings.Join(words[:len(words)-1], ", ") + ", or " + words[len(words)-1]
	}
}

// sourceName renders an item or value source for godoc:
// a SEMI standard reads as e.g. SEMI E87.1, and any other source verbatim.
func sourceName(source string) string {
	if semiSourceRE.MatchString(source) {
		return "SEMI " + strings.ToUpper(source)
	}

	return source
}

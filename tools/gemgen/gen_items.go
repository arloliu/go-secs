package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/format"
	"maps"
	"math"
	"regexp"
	"slices"
	"strings"
	"text/template"
)

//go:embed templates/items.go.tmpl
var itemsTemplate string

//go:embed templates/items_string_test.go.tmpl
var itemsStringTestTemplate string

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
	Name     string
	Typed    bool     // true: a named type over GoType; false: untyped constants of an open item
	GoType   string   // the named type's underlying type; empty when not Typed
	Doc      []string // godoc lines of the named type or the untyped const block (without leading "// ")
	Consts   []constView
	Stringer bool // true: a String method follows the const block (see hasStringer)
}

// stringCaseView is one row of the generated String test:
// a value of a named type, as a Go expression, and the text its String method returns.
type stringCaseView struct {
	Expr string
	Want string
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

		tv := typeView{Name: name, Consts: consts, Stringer: hasStringer(it)}
		if tv.Stringer {
			if _, err := undefinedValue(name, it); err != nil {
				return nil, err
			}
		}

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

	data := struct {
		Types       []typeView
		NeedStrconv bool
	}{Types: types, NeedStrconv: slices.ContainsFunc(types, func(tv typeView) bool { return tv.Stringer })}

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

// renderItemStringTests renders gem/items_string_test.go:
// a table of every constant of every named type with a generated String method, paired with the constant's name,
// and one value per type that has no constant, paired with the type's fallback text,
// checked by a single test.
// It returns nil when no item has a generated String method, and then no file is written.
// Item order is name-sorted and value order follows the YAML, as in items.go.
func renderItemStringTests(items map[string]Item) ([]byte, error) {
	var cases []stringCaseView

	for _, name := range slices.Sorted(maps.Keys(items)) {
		it := items[name]
		if !hasStringer(it) {
			continue
		}

		for _, v := range it.Values {
			cases = append(cases, stringCaseView{Expr: "gem." + name + v.Name, Want: name + v.Name})
		}

		u, err := undefinedValue(name, it)
		if err != nil {
			return nil, err
		}

		cases = append(cases, stringCaseView{
			Expr: fmt.Sprintf("gem.%s(%d)", name, u),
			Want: fmt.Sprintf("%s(%d)", name, u),
		})
	}

	if len(cases) == 0 {
		return nil, nil
	}

	tmpl, err := template.New("items_string_test.go.tmpl").Parse(itemsStringTestTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse items string test template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, struct{ Cases []stringCaseView }{Cases: cases}); err != nil {
		return nil, fmt.Errorf("execute items string test template: %w", err)
	}

	out, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated items string test source: %w", err)
	}

	return out, nil
}

// stringerEligible reports whether the generator writes a String method for an item's named type
// unless the item is marked customString:
// a fixed item over byte or uint8 with a values table.
// A named type without constants would print every value as its fallback,
// and a string type's value is already readable, so neither gets one.
func stringerEligible(it Item) bool {
	return it.Binding == BindingFixed && (it.GoType == "byte" || it.GoType == "uint8") && len(it.Values) > 0
}

// hasStringer reports whether items.go declares a String method for an item's named type.
func hasStringer(it Item) bool {
	return stringerEligible(it) && !it.CustomString
}

// undefinedValue returns the smallest value of an item's named type that none of its constants has,
// for the generated String test's fallback case.
// It is an error for two constants to share a value,
// since String's switch could not name both,
// or for the constants to take every value the type holds.
func undefinedValue(name string, it Item) (uint64, error) {
	used := map[uint64]string{}

	for _, v := range it.Values {
		n, ok := v.Value.uintValue()
		if !ok {
			return 0, fmt.Errorf("item %s: value %s is %s, but a String method needs a non-negative integer", name, v.Name, v.Value)
		}

		if prev, dup := used[n]; dup {
			return 0, fmt.Errorf("item %s: values %s and %s share the value %d, so String cannot name both", name, prev, v.Name, n)
		}

		used[n] = v.Name
	}

	for n := range uint64(math.MaxUint8) + 1 {
		if _, ok := used[n]; !ok {
			return n, nil
		}
	}

	return 0, fmt.Errorf("item %s: the values table takes every byte value, so no value is left without a constant", name)
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

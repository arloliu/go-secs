package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/format"
	"slices"
	"strings"
	"text/template"
)

//go:embed templates/decode_tests.go.tmpl
var decodeTestsTemplate string

// decodeTestView is the per-round-trip-test view model the decode tests template renders.
type decodeTestView struct {
	Name       string   // "TestDecodeS1F14RoundTrip"
	CallName   string   // "S1F14"
	DecodeName string   // "DecodeS1F14"
	CallArgs   string   // rendered builder argument list
	HasFields  bool     // false for a body whose result struct has no fields
	Lines      []string // pre-rendered Go statement lines asserting the decoded result
	EmptyName  string   // "TestDecodeS3F2EmptyList", or "" when the body gets no empty-list test
	EmptyLines []string // pre-rendered Go statement lines of the empty-list test
	Absent     []absentTestView
}

// absentTestView is one generated test that sends an optional leaf zero-length.
type absentTestView struct {
	Name  string   // "TestDecodeS12F1FNLOCAbsent", or "TestDecodeS5F6AlarmALCDAbsent" for a group's leaf
	Lines []string // pre-rendered Go statement lines of the test
}

// renderDecodeTests renders gem/sN_decode_test.go: one round-trip test per generated decoder.
//
// Each test builds the message with the same deterministic samples the builder tests use, encodes
// the body to wire bytes, decodes it back through [secs2.Decode], runs the generated decoder over
// the result, and compares it with the result the samples describe,
// built field by field from those samples rather than by decoding anything,
// through the hand-written assertDecoded helper in gem/assert_decoded_test.go.
// Driving the decoder from wire bytes rather than from the builder's own items is what makes the
// test able to catch a decoder that reads the wrong SECS-II format for a field.
// A variant body has no decoder of its own;
// its builder test decodes it with the main body's decoder instead.
//
// The file's init registers each decoder with the hand-written fuzz harness in gem/fuzz_test.go,
// seeded with the body its builder produces from the same samples,
// so the harness reaches every stream's decoders without listing them itself.
func renderDecodeTests(mf MessageFile, items map[string]Item) ([]byte, error) {
	tmpl, err := template.New("decode_tests.go.tmpl").Parse(decodeTestsTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse decode tests template: %w", err)
	}

	var tests []decodeTestView

	usesBytes := false

	for _, m := range mf.Messages {
		for _, b := range m.Bodies {
			if b.Structure == nil || b.Variant != "" {
				continue // header-only or variant: no decoder of its own to round-trip
			}

			tv, tb, err := newDecodeTestView(mf.Stream, m, b, items)
			if err != nil {
				return nil, fmt.Errorf("S%dF%d: %w", mf.Stream, m.Function, err)
			}

			tests = append(tests, tv)
			usesBytes = usesBytes || tb
		}
	}

	data := struct {
		Stream    int
		Tests     []decodeTestView
		UsesBytes bool
	}{Stream: mf.Stream, Tests: tests, UsesBytes: usesBytes}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("execute decode tests template: %w", err)
	}

	out, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated decode tests source: %w", err)
	}

	return out, nil
}

// newDecodeTestView builds the view model for one (message, body) pair's round-trip test,
// and reports whether its lines need the "bytes" import.
func newDecodeTestView(stream int, m Message, b Body, items map[string]Item) (decodeTestView, bool, error) {
	name := callName(stream, m.Function, b.Actor)

	params := BuildParams(b.Structure, items)

	exprs, err := SampleExprs(b.Structure, items)
	if err != nil {
		return decodeTestView{}, false, err
	}

	fields, err := BuildFields(b.Structure, items)
	if err != nil {
		return decodeTestView{}, false, err
	}

	if n := len(fields); n != len(params) || n != len(exprs) {
		return decodeTestView{}, false, fmt.Errorf("%s: %d fields, %d parameters, %d samples", name, n, len(params), len(exprs))
	}

	typeName := ResultTypeName(stream, m.Function, b.Actor)

	tv := decodeTestView{
		Name:       "TestDecode" + name + "RoundTrip",
		CallName:   name,
		DecodeName: "Decode" + name,
		CallArgs:   callArgs(params, exprs),
		HasFields:  len(fields) > 0,
		Lines:      assertDecodedLines(typeName, sampleFields(params, fields, exprs)),
	}

	if topListMayBeEmpty(b.Structure) {
		tv.EmptyName = "TestDecode" + name + "EmptyList"
		tv.EmptyLines = decodeZeroLines("Decode"+name, "secs2.L()", typeName, fields)
	}

	usesBytes := false

	for _, opt := range optionalLeaves(b.Structure) {
		lines, ub, err := absentTestLines(tv, typeName, b.Structure, items, opt.node)
		if err != nil {
			return decodeTestView{}, false, fmt.Errorf("%s: %w", opt.name, err)
		}

		tv.Absent = append(tv.Absent, absentTestView{Name: "TestDecode" + name + opt.name + "Absent", Lines: lines})
		usesBytes = usesBytes || ub
	}

	return tv, usesBytes, nil
}

// optionalLeaf is one optional leaf a body reaches: in the body itself, or in a group the body uses.
type optionalLeaf struct {
	node *StructureNode
	name string // its field, prefixed with its group's name for a group's leaf, e.g. AlarmALCD
}

// optionalLeaves returns the optional leaves the body n reaches, in body order, each once:
// its own, and those of the groups it references, at any depth.
func optionalLeaves(n *StructureNode) []optionalLeaf {
	var out []optionalLeaf

	seen := map[*StructureNode]bool{}

	var walk func(n *StructureNode, prefix string)
	walk = func(n *StructureNode, prefix string) {
		switch {
		case n == nil || seen[n]:
			return
		case n.Kind() == "leaf":
			if n.Optional {
				seen[n] = true
				out = append(out, optionalLeaf{node: n, name: prefix + nodeNames(n).Field})
			}
		case n.Kind() == "group":
			if n.def != nil {
				walk(n.def.Structure, n.Group)
			}
		case n.Kind() == "list" && n.Repeat != "":
			walk(n.Of, prefix)
		case n.Kind() == "list" && n.Packed == "":
			for i := range n.Items {
				walk(&n.Items[i], prefix)
			}
		default:
		}
	}
	walk(n, "")

	return out
}

// absentTestLines renders the statements of a test that sends the optional leaf opt of the body n zero-length,
// and reports whether they need the "bytes" import.
//
// The test calls the builder with the round-trip samples, but nil for opt,
// checks the built body as the builder test does, with opt the zero-length item of its format,
// and decodes the body from its wire bytes.
// The decoded field of opt must be nil, and every other field must still equal its sample.
// tv is the body's round-trip test, which supplies the builder and the decoder.
func absentTestLines(tv decodeTestView, typeName string, n *StructureNode, items map[string]Item, opt *StructureNode) ([]string, bool, error) {
	absent := absentSet{opt: true}

	params := BuildParams(n, items)

	exprs, err := sampleExprsWithAbsent(n, items, absent)
	if err != nil {
		return nil, false, err
	}

	fields, err := BuildFields(n, items)
	if err != nil {
		return nil, false, err
	}

	built, usesBytes, err := renderTestBody(n, items, absent)
	if err != nil {
		return nil, false, err
	}

	lines := slices.Concat([]string{fmt.Sprintf("msg := gem.%s(%s)", tv.CallName, callArgs(params, exprs)), ""}, built, []string{
		"",
		fmt.Sprintf("got, err := gem.%s(decoded)", tv.DecodeName),
		"if err != nil {",
		fmt.Sprintf("t.Fatalf(%q, err)", tv.DecodeName+": %v"),
		"}",
		"",
	})

	return slices.Concat(lines, assertDecodedLines(typeName, sampleFields(params, fields, exprs))), usesBytes, nil
}

// topListMayBeEmpty reports whether a body's outermost node is a list bounded with minItems: 0.
//
// SEMI E5 often says a zero-length list means "no data" without naming the list;
// the stream files then bound the outermost list with minItems: 0,
// so the body may arrive as a bare L,0.
// Such a body gets a second generated test, besides its round trip,
// that decodes a bare L,0 with the body's decoder and expects success with every field at its zero value:
// the builder always sends the full form, so the round trip alone never reaches the empty form.
func topListMayBeEmpty(n *StructureNode) bool {
	return n != nil && n.Kind() == "list" && n.Repeat == "" && n.Packed == "" &&
		n.MinItems != nil && *n.MinItems == 0
}

// decodeZeroLines renders the statements that decode the item expr with decoder
// and check that it succeeds with every field in fields at its zero value.
// typeName is the decoder's result type.
func decodeZeroLines(decoder string, expr string, typeName string, fields []decodeField) []string {
	if len(fields) == 0 {
		return []string{
			fmt.Sprintf("if _, err := gem.%s(%s); err != nil {", decoder, expr),
			fmt.Sprintf("t.Fatalf(%q, err)", decoder+": %v"),
			"}",
		}
	}

	decode := []string{
		fmt.Sprintf("got, err := gem.%s(%s)", decoder, expr),
		"if err != nil {",
		fmt.Sprintf("t.Fatalf(%q, err)", decoder+": %v"),
		"}",
		"",
	}

	return slices.Concat(decode, assertDecodedLines(typeName, nil))
}

// fieldValue is one field of an expected decoder result: its name and the Go expression of its value.
type fieldValue struct {
	Name  string
	Value string
}

// assertDecodedLines renders the statements that compare the decoded result got,
// through the hand-written assertDecoded helper,
// with the result of type typeName whose fields are set as values lists,
// every other field at its zero value.
func assertDecodedLines(typeName string, values []fieldValue) []string {
	if len(values) == 0 {
		return []string{fmt.Sprintf("assertDecoded(t, got, gem.%s{})", typeName)}
	}

	lines := []string{fmt.Sprintf("want := gem.%s{", typeName)}
	for _, v := range values {
		lines = append(lines, fmt.Sprintf("%s: %s,", v.Name, v.Value))
	}

	return append(lines, "}", "", "assertDecoded(t, got, want)")
}

// sampleFields returns the expected value of each of fields
// when the body was built from the samples exprs.
//
// params and exprs are index-aligned by construction: both come from the same structure walk.
// A field finds its parameter and sample through its Param;
// A repeat's field holds the one sample element its call sent.
// A field whose sample is nil, an optional leaf sent zero-length, is left out: it decodes as nil.
func sampleFields(params []Param, fields []decodeField, exprs []string) []fieldValue {
	values := make([]fieldValue, 0, len(fields))

	for _, f := range fields {
		i := f.Param
		value := exprs[i]

		if value == "nil" {
			continue
		}

		if params[i].Repeat {
			// A repeat or packed group: one element, the sample its call sent.
			value = sliceLiteral(strings.TrimPrefix(f.Type, "[]"), exprs[i])
		}

		// Any other field's sample is the field's own value,
		// and an untyped constant converts to a named enum type as well as to a plain one.
		values = append(values, fieldValue{f.Name, value})
	}

	return values
}

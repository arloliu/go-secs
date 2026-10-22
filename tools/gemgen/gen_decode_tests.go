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
	Name  string   // "TestDecodeS12F1FNLOCAbsent"
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

	for _, m := range mf.Messages {
		for _, b := range m.Bodies {
			if b.Structure == nil || b.Variant != "" {
				continue // header-only or variant: no decoder of its own to round-trip
			}

			tv, err := newDecodeTestView(mf.Stream, m, b, items)
			if err != nil {
				return nil, fmt.Errorf("S%dF%d: %w", mf.Stream, m.Function, err)
			}

			tests = append(tests, tv)
		}
	}

	data := struct {
		Stream int
		Tests  []decodeTestView
	}{Stream: mf.Stream, Tests: tests}

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

// newDecodeTestView builds the view model for one (message, body) pair's round-trip test.
func newDecodeTestView(stream int, m Message, b Body, items map[string]Item) (decodeTestView, error) {
	name := callName(stream, m.Function, b.Actor)

	params := BuildParams(b.Structure, items)

	exprs, err := SampleExprs(b.Structure, items)
	if err != nil {
		return decodeTestView{}, err
	}

	fields, err := BuildFields(b.Structure, items)
	if err != nil {
		return decodeTestView{}, err
	}

	if n := valueFieldCount(fields); n != len(params) || n != len(exprs) {
		return decodeTestView{}, fmt.Errorf("%s: %d value fields, %d parameters, %d samples", name, n, len(params), len(exprs))
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

	optionals, err := optionalLeaves(b.Structure, items)
	if err != nil {
		return decodeTestView{}, err
	}

	for _, opt := range optionals {
		tv.Absent = append(tv.Absent, absentTestView{
			Name:  "TestDecode" + name + opt.Field + "Absent",
			Lines: absentTestLines(tv, typeName, opt, params, fields, exprs),
		})
	}

	return tv, nil
}

// valueFieldCount returns how many of fields mirror a builder parameter: every field but a presence field.
func valueFieldCount(fields []decodeField) int {
	n := 0

	for _, f := range fields {
		if !f.Presence {
			n++
		}
	}

	return n
}

// absentTestLines renders the statements of a test that sends the optional leaf opt zero-length.
//
// The test builds the full body with the round-trip samples,
// replaces the leaf with a zero-length item of its format,
// and decodes the result from wire bytes.
// The leaf's field must come back at its zero value with its presence field false,
// and every other field must still equal its sample.
// tv is the body's round-trip test, which supplies the builder call and the decoder.
func absentTestLines(tv decodeTestView, typeName string, opt optionalLeaf, params []Param, fields []decodeField, exprs []string) []string {
	var lines []string

	if len(opt.Path) == 0 {
		// The whole body is the optional leaf.
		lines = append(lines, fmt.Sprintf("body, err := secs2.Decode(%s.ToBytes())", opt.Empty))
	} else {
		lines = append(lines,
			fmt.Sprintf("msg := gem.%s(%s)", tv.CallName, tv.CallArgs),
			"",
			"full, err := secs2.Decode(msg.Item().ToBytes())",
			"if err != nil {",
			`t.Fatalf("secs2.Decode: %v", err)`,
			"}",
			"",
		)

		// Unwrap every list on the path, replace the leaf, then rebuild the lists from the inside out.
		parent := "full"
		for depth, idx := range opt.Path {
			list := fmt.Sprintf("list%d", depth)
			lines = append(lines,
				fmt.Sprintf("%s, err := %s.ToList()", list, parent),
				"if err != nil {",
				fmt.Sprintf("t.Fatalf(%q, err)", list+": %v"),
				"}",
				"",
			)
			parent = fmt.Sprintf("%s[%d]", list, idx)
		}

		last := len(opt.Path) - 1
		lines = append(lines, fmt.Sprintf("list%d[%d] = %s", last, opt.Path[last], opt.Empty))

		for depth := last; depth > 0; depth-- {
			lines = append(lines, fmt.Sprintf("list%d[%d] = secs2.L(list%d...)", depth-1, opt.Path[depth-1], depth))
		}

		lines = append(lines, "", "body, err := secs2.Decode(secs2.L(list0...).ToBytes())")
	}

	lines = append(lines,
		"if err != nil {",
		`t.Fatalf("secs2.Decode: %v", err)`,
		"}",
		"",
		fmt.Sprintf("got, err := gem.%s(body)", tv.DecodeName),
		"if err != nil {",
		fmt.Sprintf("t.Fatalf(%q, err)", tv.DecodeName+": %v"),
		"}",
		"",
	)

	var others []decodeField

	for _, f := range fields {
		if f.Name != opt.Field && f.Name != opt.Field+"Present" {
			others = append(others, f)
		}
	}

	return slices.Concat(lines, assertDecodedLines(typeName, sampleFields(params, others, exprs)))
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
// a presence field mirrors none and is true, since the builder always sends a value.
// A repeat's field holds the one sample element its call sent.
func sampleFields(params []Param, fields []decodeField, exprs []string) []fieldValue {
	values := make([]fieldValue, 0, len(fields))

	for _, f := range fields {
		if f.Presence {
			values = append(values, fieldValue{f.Name, "true"})

			continue
		}

		i := f.Param
		value := exprs[i]

		switch {
		case f.Elem != "":
			// Value repeat: one element, the bare literal its call sent.
			value = fmt.Sprintf("%s{%s}", qualifiedSliceType(f.Type), exprs[i])
		case f.Type == "[]secs2.Item":
			value = fmt.Sprintf("[]secs2.Item{%s}", exprs[i])
		case f.Type == "[]byte" && params[i].Repeat:
			value = fmt.Sprintf("[]byte{%s}", exprs[i]) // packed group: the sample is one bare element
		default:
			// A leaf, opaque node, or byte slice or array: the sample is the field's own value,
			// and an untyped constant converts to a named enum type as well as to a plain one.
		}

		values = append(values, fieldValue{f.Name, value})
	}

	return values
}

// qualifiedSliceType spells the slice type typ, e.g. []STATUS, as the generated tests must: see [qualifiedType].
func qualifiedSliceType(typ string) string {
	return "[]" + qualifiedType(strings.TrimPrefix(typ, "[]"))
}

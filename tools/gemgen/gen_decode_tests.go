package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/format"
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
	Lines      []string // pre-rendered Go statement lines asserting the decoded fields
}

// renderDecodeTests renders gem/sN_decode_test.go: one round-trip test per generated decoder.
//
// Each test builds the message with the same deterministic samples the builder tests use, encodes
// the body to wire bytes, decodes it back through [secs2.Decode], runs the generated decoder over
// the result, and compares every field against the sample it was built from.
// Driving the decoder from wire bytes rather than from the builder's own items is what makes the
// test able to catch a decoder that reads the wrong SECS-II format for a field.
func renderDecodeTests(mf MessageFile, items map[string]Item) ([]byte, error) {
	tmpl, err := template.New("decode_tests.go.tmpl").Parse(decodeTestsTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse decode tests template: %w", err)
	}

	var tests []decodeTestView

	usesBytes := false

	for _, m := range mf.Messages {
		for _, b := range m.Bodies {
			if b.Structure == nil {
				continue // header-only: no decoder to round-trip
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

// newDecodeTestView builds the view model for one (message, body) pair's round-trip test.
func newDecodeTestView(stream int, m Message, b Body, items map[string]Item) (decodeTestView, bool, error) {
	code := fmt.Sprintf("S%dF%d", stream, m.Function)

	callName := code
	if b.Actor == "host" {
		callName = code + "Host"
	}

	params := BuildParams(b.Structure, items)

	exprs, err := SampleExprs(b.Structure, items)
	if err != nil {
		return decodeTestView{}, false, err
	}

	fields, err := BuildFields(b.Structure, items)
	if err != nil {
		return decodeTestView{}, false, err
	}

	if len(fields) != len(params) || len(fields) != len(exprs) {
		return decodeTestView{}, false, fmt.Errorf("%s: %d fields, %d parameters, %d samples", callName, len(fields), len(params), len(exprs))
	}

	lines, usesBytes := decodeAssertLines(params, fields, exprs)

	return decodeTestView{
		Name:       "TestDecode" + callName + "RoundTrip",
		CallName:   callName,
		DecodeName: "Decode" + callName,
		CallArgs:   callArgs(params, exprs),
		HasFields:  len(fields) > 0,
		Lines:      lines,
	}, usesBytes, nil
}

// decodeAssertLines renders the per-field comparisons of a round-trip test, and reports whether
// any of them needs the "bytes" import.
//
// params, fields, and exprs are index-aligned by construction: all three come from the same
// structure walk, so entry i of each describes the same body position.
func decodeAssertLines(params []Param, fields []decodeField, exprs []string) ([]string, bool) {
	var lines []string

	usesBytes := false

	for i, f := range fields {
		got := "got." + f.Name

		switch f.Type {
		case "secs2.Item":
			lines = append(lines, compareBytesLines(f.Name, got+".ToBytes()", exprs[i]+".ToBytes()")...)
			usesBytes = true
		case "[]secs2.Item":
			lines = append(lines,
				fmt.Sprintf("if len(%s) != 1 {", got),
				fmt.Sprintf("t.Fatalf(%q, len(%s))", f.Name+": got %d elements, want 1", got),
				"}",
				"",
			)
			lines = append(lines, compareBytesLines(f.Name, got+"[0].ToBytes()", exprs[i]+".ToBytes()")...)
			usesBytes = true
		case "[]byte":
			want := exprs[i]
			if params[i].Repeat {
				want = "[]byte{" + want + "}" // packed group: the sample is one bare element
			}

			lines = append(lines, compareBytesLines(f.Name, got, want)...)
			usesBytes = true
		case "bool":
			lines = append(lines,
				fmt.Sprintf("if !%s {", got),
				fmt.Sprintf("t.Errorf(%q, %s)", f.Name+": got %v, want true", got),
				"}",
				"",
			)
		default:
			lines = append(lines,
				fmt.Sprintf("if %s != %s {", got, exprs[i]),
				fmt.Sprintf("t.Errorf(%q, %s, %s)", f.Name+": got %v, want %v", got, exprs[i]),
				"}",
				"",
			)
		}
	}

	return lines, usesBytes
}

// compareBytesLines renders a bytes.Equal comparison of a decoded field against its sample.
func compareBytesLines(name string, got string, want string) []string {
	return []string{
		fmt.Sprintf("if !bytes.Equal(%s, %s) {", got, want),
		fmt.Sprintf("t.Errorf(%q, %s, %s)", name+": got %x, want %x", got, want),
		"}",
		"",
	}
}

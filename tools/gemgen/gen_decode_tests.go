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
	Lines      []string // pre-rendered Go statement lines asserting the decoded fields
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
// the result, and compares every field against the sample it was built from.
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

// newDecodeTestView builds the view model for one (message, body) pair's round-trip test.
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

	if n := valueFieldCount(fields); n != len(params) || n != len(exprs) {
		return decodeTestView{}, false, fmt.Errorf("%s: %d value fields, %d parameters, %d samples", name, n, len(params), len(exprs))
	}

	lines, usesBytes := decodeAssertLines(params, fields, exprs)

	tv := decodeTestView{
		Name:       "TestDecode" + name + "RoundTrip",
		CallName:   name,
		DecodeName: "Decode" + name,
		CallArgs:   callArgs(params, exprs),
		HasFields:  len(fields) > 0,
		Lines:      lines,
	}

	typeName := ResultTypeName(stream, m.Function, b.Actor)

	if topListMayBeEmpty(b.Structure) {
		tv.EmptyName = "TestDecode" + name + "EmptyList"
		tv.EmptyLines = decodeZeroLines("Decode"+name, "secs2.L()", typeName, fields)
	}

	optionals, err := optionalLeaves(b.Structure, items)
	if err != nil {
		return decodeTestView{}, false, err
	}

	for _, opt := range optionals {
		absent, absentBytes := absentTestLines(tv, typeName, opt, params, fields, exprs)
		tv.Absent = append(tv.Absent, absentTestView{Name: "TestDecode" + name + opt.Field + "Absent", Lines: absent})
		usesBytes = usesBytes || absentBytes
	}

	return tv, usesBytes, nil
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
// It reports whether any emitted line needs the "bytes" import.
func absentTestLines(tv decodeTestView, typeName string, opt optionalLeaf, params []Param, fields []decodeField, exprs []string) ([]string, bool) {
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

	var others, absent []decodeField

	for _, f := range fields {
		if f.Name == opt.Field || f.Name == opt.Field+"Present" {
			absent = append(absent, f)
		} else {
			others = append(others, f)
		}
	}

	assertLines, usesBytes := decodeAssertLines(params, others, exprs)
	lines = slices.Concat(lines, assertLines, zeroAssertLines(typeName, absent))

	return trimTrailingBlank(lines), usesBytes
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

	return trimTrailingBlank(slices.Concat(decode, zeroAssertLines(typeName, fields)))
}

// zeroAssertLines renders the checks that every field in fields of the decoded result got is at its zero value.
//
// typeName is the result type the fields belong to.
// A slice field must be empty;
// any other field is compared with the same field of a zero result,
// which is declared only when at least one field needs it, so the generated test never holds an unused variable.
func zeroAssertLines(typeName string, fields []decodeField) []string {
	var lines []string

	declared := false

	for _, f := range fields {
		got := "got." + f.Name

		if strings.HasPrefix(f.Type, "[]") {
			lines = append(lines,
				fmt.Sprintf("if len(%s) != 0 {", got),
				fmt.Sprintf("t.Errorf(%q, len(%s))", f.Name+": got %d elements, want none", got),
				"}",
				"",
			)

			continue
		}

		if !declared {
			lines = append(lines, fmt.Sprintf("var zero gem.%s", typeName), "")
			declared = true
		}

		lines = append(lines,
			fmt.Sprintf("if %s != zero.%s {", got, f.Name),
			fmt.Sprintf("t.Errorf(%q, %s)", f.Name+": got %v, want the zero value", got),
			"}",
			"",
		)
	}

	return lines
}

// decodeAssertLines renders the per-field comparisons of a round-trip test, and reports whether
// any of them needs the "bytes" import.
//
// params and exprs are index-aligned by construction: both come from the same structure walk.
// A field finds its parameter and sample through its Param;
// a presence field mirrors none and must be true, since the builder always sends a value.
func decodeAssertLines(params []Param, fields []decodeField, exprs []string) ([]string, bool) {
	var lines []string

	usesBytes := false

	for _, f := range fields {
		got := "got." + f.Name

		if f.Presence {
			lines = append(lines,
				fmt.Sprintf("if !%s {", got),
				fmt.Sprintf("t.Errorf(%q)", f.Name+": got false, want true"),
				"}",
				"",
			)

			continue
		}

		i := f.Param

		if f.Elem != "" {
			lines = append(lines, typedAssertLines(f, params[i].Name)...)

			continue
		}

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
			cond := "!" + got
			if exprs[i] == "false" {
				cond = got
			}

			lines = append(lines,
				fmt.Sprintf("if %s {", cond),
				fmt.Sprintf("t.Errorf(%q, %s)", f.Name+": got %v, want "+exprs[i], got),
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

// typedAssertLines renders the comparison of a typed repeat's decoded field against its one sample element,
// whose value typedElementSample derives from the parameter name param.
// An untyped constant compares with a named enum element type as well as with a plain one.
func typedAssertLines(f decodeField, param string) []string {
	got := "got." + f.Name
	literal := fixedSamples[f.Elem].literal(param)

	cond := fmt.Sprintf("%s[0] != %s", got, literal)
	if f.Elem == "bool" {
		cond = "!" + got + "[0]"
	}

	return []string{
		fmt.Sprintf("if len(%s) != 1 || %s {", got, cond),
		fmt.Sprintf("t.Errorf(%q, %s)", f.Name+": got %v, want ["+strings.Trim(literal, `"`)+"]", got),
		"}",
		"",
	}
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

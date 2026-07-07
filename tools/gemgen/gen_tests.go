package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/format"
	"hash/fnv"
	"strconv"
	"strings"
	"text/template"
)

//go:embed templates/tests.go.tmpl
var testsTemplate string

// fixedSample describes how a fixed-binding item's concrete Go type is
// sampled at the generated call site and re-checked against the decoded
// item. These are the only three goTypes the GEM item dictionary actually
// uses for a fixed (non-open) leaf: fixed ASCII (string), fixed single-byte
// (byte — including enum items like COMMACK, whose named parameter type
// still accepts the bare literal with no cast), and fixed BOOLEAN (bool).
//
// literal is derived from the item's own (already-lowercased) name rather
// than a single value shared by every item of that GoType: two sibling items
// of the same GoType (e.g. S1F2's MDLN/SOFTREV, both string) must still get
// distinguishable samples, or a generator regression that swaps their body-
// expression order would render identical output and pass undetected.
type fixedSample struct {
	literal func(name string) string // Go literal used both as the call argument and the decode-assert comparand
	method  string                   // decoded Item accessor, e.g. "ToASCII", "ToBinary", "ToBoolean"
	scalar  bool                     // true when method returns a bare value (ToASCII); false when it returns a []T slice
}

// fixedSamples maps a fixed-binding item's GoType to its deterministic,
// name-derived sample. A GoType with no entry here has no known sample, and
// renderTests reports an error rather than guessing.
//
// bool is the one exception that still uses a single shared literal: a
// boolean only has two possible values, so no per-name derivation can give
// every item a distinguishable sample the way string/byte can, and no GEM
// message declares two BOOLEAN-typed siblings in the same body, so the
// swapped-sibling failure mode this fix targets doesn't arise for bool.
var fixedSamples = map[string]fixedSample{
	"string": {literal: func(name string) string { return fmt.Sprintf("%q", name) }, method: "ToASCII", scalar: true},
	"byte":   {literal: func(name string) string { return fmt.Sprintf("0x%02x", byteSample(name)) }, method: "ToBinary", scalar: false},
	"bool":   {literal: func(string) string { return "true" }, method: "ToBoolean", scalar: false},
}

// byteSample derives a deterministic sample byte from an item's (already-
// lowercased) name via FNV-1a, folded from 32 bits down to one byte by
// XORing its four bytes together. This is not collision-free — a single
// byte only has 256 possible values — but the only place a collision could
// hide a real bug is between two fixed-byte-typed siblings inside the very
// same generated message body (e.g. two enum items in one list), and no GEM
// message in the item dictionary declares two such siblings. The fold's job
// is just to make each item's call-site sample self-documenting and
// distinguishable from its actual siblings, not to be globally unique
// across the full ~336-item dictionary.
func byteSample(name string) byte {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	sum := h.Sum32()

	return byte(sum) ^ byte(sum>>8) ^ byte(sum>>16) ^ byte(sum>>24)
}

// funcTestView is the per-test-function view model the tests template
// renders.
type funcTestView struct {
	Name     string // "TestS1F2" or "TestS1F2Host"
	CallName string // "S1F2" or "S1F2Host"
	CallArgs string // rendered call argument list, e.g. `"X", "X"`
	Func     int
	WaitBit  bool
	Lines    []string // pre-rendered Go statement lines asserting the body
}

// renderTests renders gem/sN_test.go: a per-stream assertS<N>Header helper
// plus one TestS<N>F<M>[Host] per generated builder function. Each test
// builds the message with deterministic sample inputs, asserts
// stream/function/wait bit, then decodes the body and walks the same
// structure tree BuildParams/BodyExpr (Task 6) used to build the function,
// this time asserting each node's secs2 type and value instead of building a
// parameter list.
func renderTests(mf MessageFile, items map[string]Item) ([]byte, error) {
	tmpl, err := template.New("tests.go.tmpl").Parse(testsTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse tests template: %w", err)
	}

	var funcs []funcTestView

	usesBytes := false

	for _, m := range mf.Messages {
		for _, b := range m.Bodies {
			fv, fb, err := newFuncTestView(mf.Stream, m, b, items)
			if err != nil {
				return nil, fmt.Errorf("S%dF%d: %w", mf.Stream, m.Function, err)
			}

			funcs = append(funcs, fv)
			usesBytes = usesBytes || fb
		}
	}

	data := struct {
		Stream    int
		Funcs     []funcTestView
		UsesBytes bool
	}{Stream: mf.Stream, Funcs: funcs, UsesBytes: usesBytes}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("execute tests template: %w", err)
	}

	out, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated tests source: %w", err)
	}

	return out, nil
}

// newFuncTestView builds the view model for one (message, body) pair's test
// function, reusing BuildParams for parameter ordering/repeat-ness and
// SampleExprs for the parallel per-parameter sample expression, so the call
// argument list always lines up with the body assertions built from the same
// structure node.
func newFuncTestView(stream int, m Message, b Body, items map[string]Item) (funcTestView, bool, error) {
	code := fmt.Sprintf("S%dF%d", stream, m.Function)

	callName := code
	if b.Actor == "host" {
		callName = code + "Host"
	}

	exprs, err := SampleExprs(b.Structure, items)
	if err != nil {
		return funcTestView{}, false, err
	}

	lines, usesBytes, err := renderTestBody(b.Structure, items)
	if err != nil {
		return funcTestView{}, false, err
	}

	return funcTestView{
		Name:     "Test" + callName,
		CallName: callName,
		CallArgs: callArgs(BuildParams(b.Structure, items), exprs),
		Func:     m.Function,
		WaitBit:  b.ReplyExpected,
		Lines:    lines,
	}, usesBytes, nil
}

// callArgs renders the generated call's argument list from the parameters
// BuildParams derived and the matching sample expressions SampleExprs
// derived from the same structure node (so params[i] and exprs[i] always
// describe the same parameter). A non-final slice parameter (a repeat's
// []secs2.Item or a packed group's concrete []byte/[]string/...) needs its
// single sample element wrapped in a slice literal of that parameter's own
// element type; every other parameter (fixed/open leaf, opaque body, or the
// final variadic repeat/packed group) is passed as-is.
func callArgs(params []Param, exprs []string) string {
	parts := make([]string, len(params))
	for i, p := range params {
		if p.Repeat && !p.Variadic {
			parts[i] = fmt.Sprintf("[]%s{%s}", p.Type, exprs[i])
			continue
		}

		parts[i] = exprs[i]
	}

	return strings.Join(parts, ", ")
}

// SampleExprs walks a validated structure node in the same order and with
// the same stopping rules as BuildParams (a repeat or packed node contributes
// a single entry and does NOT recurse into Of) and returns one deterministic
// sample-value Go expression per resulting parameter: a fixed leaf's (or a
// packed group's of.item's) underlying-GoType literal (an enum-typed parameter
// like COMMACK still gets a bare, cast-free literal, since an untyped constant
// is assignable to a named defined type), or a secs2.A("<name>") sample labeled
// by the parameter's own name for an open leaf, an opaque body, or a repeat
// element. Labeling every non-fixed sample by its own parameter name (rather
// than a single shared value) means a bug that swaps two sibling parameters
// changes the expected values at their positions, and the generated
// assertion catches it.
func SampleExprs(n *StructureNode, items map[string]Item) ([]string, error) {
	var out []string

	var walkErr error

	var walk func(*StructureNode)
	walk = func(n *StructureNode) {
		if n == nil || walkErr != nil {
			return
		}

		switch n.Kind() {
		case "leaf":
			it := items[n.Item]
			name := lower(n.Item)

			if it.Binding != BindingFixed {
				out = append(out, fmt.Sprintf("secs2.A(%q)", name))
				return
			}

			fs, ok := fixedSamples[it.GoType]
			if !ok {
				walkErr = fmt.Errorf("item %s: no deterministic sample for goType %q", n.Item, it.GoType)
				return
			}

			out = append(out, fs.literal(name))
		case "opaque":
			out = append(out, `secs2.A("body")`)
		default: // list
			if n.Repeat != "" {
				out = append(out, fmt.Sprintf("secs2.A(%q)", n.Repeat))
				return
			}

			if n.Packed != "" {
				// Packed group: the parameter is a concrete primitive (byte/string/...),
				// not a secs2.Item, so its sample must be a bare underlying-goType literal
				// labeled by the packed name -- NOT secs2.A(...) or any secs2.<Ctor>(...),
				// since the caller supplies raw values and BodyExpr does the secs2.B(...)
				// wrapping.
				it := items[n.Of.Item]

				fs, ok := fixedSamples[it.GoType]
				if !ok {
					walkErr = fmt.Errorf("packed %s: no deterministic sample for goType %q", n.Packed, it.GoType)
					return
				}

				out = append(out, fs.literal(n.Packed))

				return
			}

			for i := range n.Items {
				walk(&n.Items[i])
			}
		}
	}
	walk(n)

	if walkErr != nil {
		return nil, walkErr
	}

	return out, nil
}

// testCtx accumulates the Go statement lines that assert a decoded body
// tree, and hands out unique variable-name suffixes so consecutive
// extractions (list unwraps, scalar decodes) never collide within the
// generated test function.
type testCtx struct {
	lines     []string
	counter   int
	usesBytes bool
}

// nextID returns a fresh, monotonically increasing suffix for a generated
// variable name.
func (c *testCtx) nextID() int {
	c.counter++

	return c.counter
}

// emit appends one or more pre-rendered Go statement lines.
func (c *testCtx) emit(lines ...string) {
	c.lines = append(c.lines, lines...)
}

// assertList emits the decode-and-length-check for a list found at expr,
// returning the fresh variable name bound to its children.
func (c *testCtx) assertList(expr string, wantLen int) string {
	v := fmt.Sprintf("list%d", c.nextID())

	c.emit(
		fmt.Sprintf("%s, err := %s.ToList()", v, expr),
		"if err != nil {",
		fmt.Sprintf("t.Fatalf(%q, err)", expr+".ToList: %v"),
		"}",
		fmt.Sprintf("if len(%s) != %d {", v, wantLen),
		fmt.Sprintf("t.Fatalf(%q, len(%s))", "list length: got %d, want "+strconv.Itoa(wantLen), v),
		"}",
	)

	return v
}

// assertFixed emits the decode-and-compare check for a fixed-binding leaf
// found at expr, using its GoType's deterministic sample and accessor.
func (c *testCtx) assertFixed(expr, name string, it Item) error {
	fs, ok := fixedSamples[it.GoType]
	if !ok {
		return fmt.Errorf("no deterministic sample for goType %q", it.GoType)
	}

	v := fmt.Sprintf("v%d", c.nextID())
	literal := fs.literal(name)

	c.emit(
		fmt.Sprintf("%s, err := %s.%s()", v, expr, fs.method),
		"if err != nil {",
		fmt.Sprintf("t.Fatalf(%q, err)", expr+"."+fs.method+": %v"),
		"}",
	)

	if fs.scalar {
		c.emit(
			fmt.Sprintf("if %s != %s {", v, literal),
			fmt.Sprintf("t.Errorf(%q, %s, %s)", name+": got %q, want %q", v, literal),
			"}",
		)

		return nil
	}

	c.emit(
		fmt.Sprintf("if len(%s) != 1 || %s[0] != %s {", v, v, literal),
		fmt.Sprintf("t.Errorf(%q, %s, %s)", name+": got %v, want %v", v, literal),
		"}",
	)

	return nil
}

// assertSample emits the decode-and-compare check for a caller-supplied
// secs2.Item found at expr (an open leaf, an opaque body, or a repeat
// element): it compares the decoded item's wire bytes against the same
// labeled secs2.A(name) sample SampleExprs used at the call site.
func (c *testCtx) assertSample(expr, name string) {
	id := c.nextID()
	got := fmt.Sprintf("got%d", id)
	want := fmt.Sprintf("want%d", id)

	c.emit(
		fmt.Sprintf("%s := %s.ToBytes()", got, expr),
		fmt.Sprintf("%s := secs2.A(%q).ToBytes()", want, name),
		fmt.Sprintf("if !bytes.Equal(%s, %s) {", got, want),
		fmt.Sprintf("t.Errorf(%q, %s, %s)", name+": got %x, want %x", got, want),
		"}",
	)
	c.usesBytes = true
}

// assertNode recursively asserts that the Item found at expr matches n's
// shape, mirroring BuildParams/BodyExpr's traversal exactly: a repeat node
// contributes one wrapping list (asserted to hold exactly the one sample
// element SampleExprs supplied) and does NOT recurse into Of.
func (c *testCtx) assertNode(n *StructureNode, items map[string]Item, expr string) error {
	switch n.Kind() {
	case "leaf":
		it := items[n.Item]
		name := lower(n.Item)

		if it.Binding == BindingFixed {
			return c.assertFixed(expr, name, it)
		}

		c.assertSample(expr, name)

		return nil
	case "opaque":
		c.assertSample(expr, "body")

		return nil
	default: // list
		if n.Repeat != "" {
			v := c.assertList(expr, 1)
			c.assertSample(v+"[0]", n.Repeat)

			return nil
		}

		if n.Packed != "" {
			// Packed group: on the wire this is a single primitive item (e.g. one
			// secs2.B(...) BinaryItem), NOT a list of separate sub-items. Decode it
			// with the underlying item's accessor and check it holds exactly the one
			// sample value the call site passed -- the same assertFixed path a bare
			// fixed leaf uses, keyed by the packed name and its of.item's goType.
			return c.assertFixed(expr, n.Packed, items[n.Of.Item])
		}

		v := c.assertList(expr, len(n.Items))
		for i := range n.Items {
			if err := c.assertNode(&n.Items[i], items, fmt.Sprintf("%s[%d]", v, i)); err != nil {
				return err
			}
		}

		return nil
	}
}

// renderTestBody renders the Go statement lines that decode and assert a
// message body: for header-only (a nil structure), a single IsEmpty check;
// otherwise a secs2.Decode preamble followed by the recursive assertNode
// walk. It also reports whether any emitted line needs the "bytes" import,
// so renderTests can omit it from bodies that never compare a caller-
// supplied secs2.Item.
func renderTestBody(n *StructureNode, items map[string]Item) ([]string, bool, error) {
	if n == nil {
		return []string{
			"if !msg.Item().IsEmpty() {",
			`t.Errorf("item type: got %s, want empty", msg.Item().Type())`,
			"}",
		}, false, nil
	}

	c := &testCtx{}
	c.emit(
		"decoded, err := secs2.Decode(msg.Item().ToBytes())",
		"if err != nil {",
		`t.Fatalf("secs2.Decode: %v", err)`,
		"}",
	)

	if err := c.assertNode(n, items, "decoded"); err != nil {
		return nil, false, err
	}

	return c.lines, c.usesBytes, nil
}

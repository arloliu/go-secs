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
// item. Besides the three string/byte/bool goTypes, this also covers every
// plain numeric goType a fixed (non-open, non-enum) leaf can declare: the
// signed widths (int8..int64), the unsigned widths (uint8..uint64), and the
// float widths (float32/float64). Width matters only for encode-time range
// validation, not the decode accessor: a signed leaf of any width decodes
// through ToInt ([]int64), any unsigned width through ToUint ([]uint64), and
// any float width through ToFloat ([]float64) — so the ten widths collapse to
// just three accessors, each reused across the widths that map to it. An
// enum item like COMMACK keeps its named byte parameter type, which still
// accepts the bare literal with no cast.
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

	// Plain signed integers of every width decode through the single ToInt
	// ([]int64) accessor; the untyped-integer sample fits the narrowest int8.
	"int8":  {literal: intLiteral, method: "ToInt", scalar: false},
	"int16": {literal: intLiteral, method: "ToInt", scalar: false},
	"int32": {literal: intLiteral, method: "ToInt", scalar: false},
	"int64": {literal: intLiteral, method: "ToInt", scalar: false},

	// Plain unsigned integers of every width decode through the single ToUint
	// ([]uint64) accessor; the same non-negative sample fits the narrowest uint8.
	"uint8":  {literal: intLiteral, method: "ToUint", scalar: false},
	"uint16": {literal: intLiteral, method: "ToUint", scalar: false},
	"uint32": {literal: intLiteral, method: "ToUint", scalar: false},
	"uint64": {literal: intLiteral, method: "ToUint", scalar: false},

	// Floats of either width decode through the single ToFloat ([]float64)
	// accessor; the half-integer sample is exactly representable in float32, so
	// the float32 -> float64 decode round-trip compares equal with no drift.
	"float32": {literal: floatLiteral, method: "ToFloat", scalar: false},
	"float64": {literal: floatLiteral, method: "ToFloat", scalar: false},
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

// numericSample derives a deterministic small non-negative magnitude (0..99)
// from an item's (already-lowercased) name by reducing byteSample's FNV fold
// modulo 100. The range is chosen so the resulting untyped-integer literal
// converts safely to the narrowest supported width at the call site (int8's
// max is 127, uint8's 255) while remaining trivially convertible to the wide
// int64/uint64/float64 comparison type at the assertion site. It shares
// byteSample's name derivation for the same reason: two same-goType siblings
// in one generated body must get distinguishable samples, or a swapped-order
// regression would render identical output and pass undetected.
func numericSample(name string) int {
	return int(byteSample(name)) % 100
}

// intLiteral renders numericSample as a bare untyped-integer Go literal, used
// as both the call argument (assignable to any int8..int64 / uint8..uint64
// parameter) and the ToInt/ToUint decode comparand (an untyped constant
// compares against the int64/uint64 slice element).
func intLiteral(name string) string {
	return strconv.Itoa(numericSample(name))
}

// floatLiteral renders numericSample as a half-integer Go float literal
// (e.g. "42.5"). The .5 fraction is exactly representable in float32, so a
// float32 leaf's encode-decode round-trip (which widens to float64 via
// ToFloat) compares equal with no rounding drift.
func floatLiteral(name string) string {
	return strconv.Itoa(numericSample(name)) + ".5"
}

// byteSliceSampleLen is the fixed number of bytes sampled for a variable-
// length "[]byte" leaf: the type imposes no length, so any positive count
// exercises the whole-sequence encode/decode round-trip; a small value keeps
// the generated literal readable.
const byteSliceSampleLen = 4

// byteArraySampleLen reports how many bytes to sample for a byte-array/byte-
// slice goType: the N of a fixed-size "[N]byte", or byteSliceSampleLen for the
// variable-length "[]byte". It reports false for any other goType, so scalar
// and non-binary types fall through to the fixedSamples table instead.
func byteArraySampleLen(gt string) (int, bool) {
	if gt == "[]byte" {
		return byteSliceSampleLen, true
	}
	if !byteArrayGoTypeRE.MatchString(gt) {
		return 0, false
	}

	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(gt, "["), "]byte"))
	if err != nil {
		return 0, false
	}

	return n, true
}

// blobSample derives a deterministic n-byte sample sequence from name,
// reusing byteSample's per-byte FNV derivation seeded by each index so two
// blob-typed siblings (or two bytes within one sample) stay distinguishable.
func blobSample(name string, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byteSample(name + strconv.Itoa(i))
	}

	return b
}

// byteBodyLiteral renders a byte sequence as the comma-separated body of a Go
// composite literal, e.g. "0x39, 0xe3", for use inside an [N]byte{...} call
// argument or a []byte{...} decode comparand.
func byteBodyLiteral(b []byte) string {
	parts := make([]string, len(b))
	for i, x := range b {
		parts[i] = fmt.Sprintf("0x%02x", x)
	}

	return strings.Join(parts, ", ")
}

// byteArraySampleExpr renders the call-site sample for a byte-array/byte-slice
// leaf: an "[N]byte{...}" or "[]byte{...}" composite literal matching the
// parameter's own type, filled with name's deterministic sample bytes. It
// reports false for any non-binary goType. The decode-side comparand is built
// from the same blobSample so the two always agree.
func byteArraySampleExpr(gt, name string) (string, bool) {
	n, ok := byteArraySampleLen(gt)
	if !ok {
		return "", false
	}

	return fmt.Sprintf("%s{%s}", gt, byteBodyLiteral(blobSample(name, n))), true
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

			if expr, ok := byteArraySampleExpr(it.GoType, name); ok {
				// Byte-array/byte-slice leaf: the parameter is an [N]byte or []byte, so its
				// sample is a composite literal of that same type -- not a fixedSamples entry.
				out = append(out, expr)
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
	if n, ok := byteArraySampleLen(it.GoType); ok {
		c.assertBlob(expr, name, n)

		return nil
	}

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

// assertBlob emits the decode-and-compare check for a byte-array/byte-slice
// leaf found at expr: it decodes the binary item and compares the *entire*
// decoded byte sequence against name's deterministic n-byte sample via
// bytes.Equal -- not the single-element check assertFixed uses for a scalar
// byte, which would inspect only the first byte of a multi-byte blob.
func (c *testCtx) assertBlob(expr, name string, n int) {
	v := fmt.Sprintf("v%d", c.nextID())
	want := fmt.Sprintf("[]byte{%s}", byteBodyLiteral(blobSample(name, n)))

	c.emit(
		fmt.Sprintf("%s, err := %s.ToBinary()", v, expr),
		"if err != nil {",
		fmt.Sprintf("t.Fatalf(%q, err)", expr+".ToBinary: %v"),
		"}",
		fmt.Sprintf("if !bytes.Equal(%s, %s) {", v, want),
		fmt.Sprintf("t.Errorf(%q, %s, %s)", name+": got %x, want %x", v, want),
		"}",
	)
	c.usesBytes = true
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

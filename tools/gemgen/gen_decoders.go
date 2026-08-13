package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/format"
	"strconv"
	"strings"
	"text/template"
)

//go:embed templates/decoders.go.tmpl
var decodersTemplate string

//go:embed templates/decode_support.go.tmpl
var decodeSupportSource string

// decodeField is one field of a generated result struct.
//
// Name is the E5 data item (or repeated-group) name upper-cased, which is already the form the
// generated enum types in items.go use, so a result struct reads as the E5 body it decodes.
type decodeField struct {
	Name string
	Type string
}

// decoderView is the per-decoder view model the decoders template renders.
type decoderView struct {
	TypeName string        // "S1F14Reply"
	FuncName string        // "DecodeS1F14"
	Fields   []decodeField // result-struct fields, in body order
	Lines    []string      // pre-rendered Go statement lines of the decoder body
	TypeDoc  []string      // godoc lines for the result struct (without leading "// ")
	FuncDoc  []string      // godoc lines for the decoder function (without leading "// ")
}

// renderDecodeSupport returns the shared decode helpers every generated decoder calls.
//
// The source is a fixed file rather than a rendered template: it carries no per-message data, and
// keeping it under templates/ means the generator remains the single writer of every gem file the
// decoders need, including the one the compile guards must plant alongside them.
func renderDecodeSupport() ([]byte, error) {
	out, err := format.Source([]byte(decodeSupportSource))
	if err != nil {
		return nil, fmt.Errorf("format decode support source: %w", err)
	}

	return out, nil
}

// renderDecoders renders one stream's body decoders into gofmt-normalized Go source.
//
// Every body the generator builds gets a decoder except a header-only one, which carries no body
// to decode.
func renderDecoders(mf MessageFile, items map[string]Item) ([]byte, error) {
	tmpl, err := template.New("decoders.go.tmpl").Parse(decodersTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse decoders template: %w", err)
	}

	var decoders []decoderView

	for _, m := range mf.Messages {
		for _, b := range m.Bodies {
			if b.Structure == nil {
				continue // header-only: no body to decode
			}

			dv, err := newDecoderView(mf.Stream, m, b, items)
			if err != nil {
				return nil, fmt.Errorf("S%dF%d: %w", mf.Stream, m.Function, err)
			}

			decoders = append(decoders, dv)
		}
	}

	data := struct{ Decoders []decoderView }{Decoders: decoders}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("execute decoders template: %w", err)
	}

	out, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated decoders source: %w", err)
	}

	return out, nil
}

// ResultTypeName returns the generated result-struct name for one (message, body) pair.
//
// A secondary (even) function carries a reply, a primary (odd) one carries a request body; the
// suffix keeps the type from colliding with the builder function of the same name.
func ResultTypeName(stream int, function int, actor string) string {
	name := fmt.Sprintf("S%dF%d", stream, function)
	if actor == "host" {
		name += "Host"
	}

	if function%2 == 0 {
		return name + "Reply"
	}

	return name + "Body"
}

// newDecoderView builds the view model for one (message, body) pair's decoder.
func newDecoderView(stream int, m Message, b Body, items map[string]Item) (decoderView, error) {
	code := fmt.Sprintf("S%dF%d", stream, m.Function)

	callName := code
	if b.Actor == "host" {
		callName = code + "Host"
	}

	typeName := ResultTypeName(stream, m.Function, b.Actor)

	e := &decodeEmitter{code: code, items: items}
	if err := e.emitNode(b.Structure, nil); err != nil {
		return decoderView{}, err
	}

	return decoderView{
		TypeName: typeName,
		FuncName: "Decode" + callName,
		Fields:   e.fields,
		Lines:    e.lines,
		TypeDoc:  resultTypeDoc(typeName, code, callName, m, b, items),
		FuncDoc:  decoderDoc(typeName, code, callName, m, b),
	}, nil
}

// BuildFields returns the ordered result-struct fields a generated decoder fills.
//
// It walks the structure with the same ordering and the same stopping rules as [BuildParams], so
// the decoder's field i always describes the builder's parameter i. gen_decoders_test.go asserts
// that agreement name-by-name over every message in data/messages.
func BuildFields(n *StructureNode, items map[string]Item) ([]decodeField, error) {
	e := &decodeEmitter{code: "S0F0", items: items}
	if err := e.emitNode(n, nil); err != nil {
		return nil, err
	}

	return e.fields, nil
}

// decodeEmitter accumulates the fields and the Go statement lines of one generated decoder as it
// walks a validated structure node.
type decodeEmitter struct {
	code    string
	items   map[string]Item
	fields  []decodeField
	lines   []string
	counter int
}

// nextID returns a fresh, monotonically increasing suffix for a generated variable name.
func (e *decodeEmitter) nextID() int {
	e.counter++

	return e.counter
}

// emit appends one or more pre-rendered Go statement lines.
// Indentation is left to gofmt.
func (e *decodeEmitter) emit(lines ...string) {
	e.lines = append(e.lines, lines...)
}

// field records one result-struct field and returns its Go name.
func (e *decodeEmitter) field(name string, goType string) string {
	e.fields = append(e.fields, decodeField{Name: name, Type: goType})

	return name
}

// assign emits the guarded "decode into the field, or return the error" statement pair every
// scalar and item-valued field shares.
func (e *decodeEmitter) assign(name string, call string) {
	e.emit(
		fmt.Sprintf("if out.%s, err = %s; err != nil {", name, call),
		"return out, err",
		"}",
		"",
	)
}

// pathExpr renders the cursor expression addressing a body position.
func pathExpr(path []int) string {
	if len(path) == 0 {
		return "root"
	}

	parts := make([]string, len(path))
	for i, idx := range path {
		parts[i] = strconv.Itoa(idx)
	}

	return "root.At(" + strings.Join(parts, ", ") + ")"
}

// pathLabel renders the human-readable label a shape error names for a body position, e.g.
// "S1F14 body[1]".
func (e *decodeEmitter) pathLabel(path []int) string {
	var b strings.Builder

	b.WriteString(e.code)
	b.WriteString(" body")

	for _, idx := range path {
		b.WriteString("[")
		b.WriteString(strconv.Itoa(idx))
		b.WriteString("]")
	}

	return b.String()
}

// emitNode walks one structure node, recording its fields and emitting the statements that fill
// them. path is the list-index path from the body root to n.
func (e *decodeEmitter) emitNode(n *StructureNode, path []int) error {
	switch n.Kind() {
	case "leaf":
		return e.emitLeaf(n, path)
	case "opaque":
		e.assign(e.field("BODY", "secs2.Item"), fmt.Sprintf("decodeItem(%s, %q)", pathExpr(path), "body"))

		return nil
	default: // list
		return e.emitList(n, path)
	}
}

// emitList walks a list node: a repeated group, a packed group, or a fixed group of children.
func (e *decodeEmitter) emitList(n *StructureNode, path []int) error {
	switch {
	case n.Repeat != "":
		name := strings.ToUpper(n.Repeat)
		e.assign(e.field(name, "[]secs2.Item"), fmt.Sprintf("decodeItems(%s, %q)", pathExpr(path), name))

		return nil
	case n.Packed != "":
		return e.emitPacked(n, path)
	default:
		return e.emitFixedList(n, path)
	}
}

// emitPacked walks a packed multi-value group, which is a single primitive item on the wire, not
// a list of separate items.
func (e *decodeEmitter) emitPacked(n *StructureNode, path []int) error {
	it := e.items[n.Of.Item]
	if itemFormat := firstFormat(it); itemFormat != "B" || it.GoType != "byte" {
		return fmt.Errorf("packed %s: no decoder for item %s (format %q, goType %q)", n.Packed, n.Of.Item, firstFormat(it), it.GoType)
	}

	name := strings.ToUpper(n.Packed)
	e.assign(e.field(name, "[]byte"), fmt.Sprintf("decodeBinary(%s, %q)", pathExpr(path), name))

	return nil
}

// emitFixedList walks a fixed group of children, emitting its arity check first.
//
// A group the DSL bounds with minItems below its declared child count is optional: SEMI E5 lets
// the sender omit it entirely, so both the short and the full form decode, and the fields the
// short form omits keep their zero value.
func (e *decodeEmitter) emitFixedList(n *StructureNode, path []int) error {
	label := e.pathLabel(path)
	full := len(n.Items)

	if n.MaxItems != nil && *n.MaxItems != full {
		return fmt.Errorf("%s: maxItems %d does not match the %d declared children", label, *n.MaxItems, full)
	}

	short := full
	if n.MinItems != nil {
		short = *n.MinItems
	}

	if short > full {
		return fmt.Errorf("%s: minItems %d exceeds the %d declared children", label, short, full)
	}

	if short == full {
		e.emit(
			fmt.Sprintf("if err = requireArity(%s, %q, %d); err != nil {", pathExpr(path), label, full),
			"return out, err",
			"}",
			"",
		)

		return e.emitChildren(n, path, 0, full)
	}

	count := fmt.Sprintf("n%d", e.nextID())
	e.emit(
		fmt.Sprintf("%s, err := optionalArity(%s, %q, %d, %d)", count, pathExpr(path), label, short, full),
		"if err != nil {",
		"return out, err",
		"}",
		"",
	)

	if err := e.emitChildren(n, path, 0, short); err != nil {
		return err
	}

	// Fence the optional tail: the short form stops at `short` children, so the fields beyond it
	// are read only when the full form arrived.
	tail := &decodeEmitter{code: e.code, items: e.items, counter: e.counter}
	if err := tail.emitChildren(n, path, short, full); err != nil {
		return err
	}

	e.counter = tail.counter
	e.fields = append(e.fields, tail.fields...)

	e.emit(fmt.Sprintf("if %s == %d {", count, full))
	e.emit(trimTrailingBlank(tail.lines)...)
	e.emit("}", "")

	return nil
}

// emitChildren walks children [from, to) of a fixed list node.
func (e *decodeEmitter) emitChildren(n *StructureNode, path []int, from int, to int) error {
	for i := from; i < to; i++ {
		if err := e.emitNode(&n.Items[i], append(append([]int{}, path...), i)); err != nil {
			return err
		}
	}

	return nil
}

// emitLeaf walks a leaf node, dispatching on the E5 data item's declared SECS-II format.
//
// The format, not the Go type, picks the accessor: a byte-typed item is B (binary) while a
// uint8-typed one is U1, and Go cannot tell byte from uint8.
func (e *decodeEmitter) emitLeaf(n *StructureNode, path []int) error {
	it := e.items[n.Item]
	name := strings.ToUpper(n.Item)
	expr := pathExpr(path)

	if it.Binding != BindingFixed {
		e.assign(e.field(name, "secs2.Item"), fmt.Sprintf("decodeItem(%s, %q)", expr, name))

		return nil
	}

	goType := it.GoType
	if len(it.Values) > 0 {
		goType = n.Item // enum item: the named defined type generated into items.go
	}

	itemFormat := firstFormat(it)

	switch itemFormat {
	case "A":
		e.assign(e.field(name, goType), fmt.Sprintf("decodeASCII(%s, %q)", expr, name))
	case "BOOLEAN":
		e.assign(e.field(name, goType), fmt.Sprintf("decodeBool(%s, %q)", expr, name))
	case "B":
		return e.emitBinaryLeaf(n, name, goType, expr)
	case "U1", "U2", "U4", "U8":
		e.assign(e.field(name, goType), fmt.Sprintf("decodeUintAs[%s](%s, %q)", goType, expr, name))
	case "I1", "I2", "I4", "I8":
		e.assign(e.field(name, goType), fmt.Sprintf("decodeIntAs[%s](%s, %q)", goType, expr, name))
	case "F4", "F8":
		e.assign(e.field(name, goType), fmt.Sprintf("decodeFloatAs[%s](%s, %q)", goType, expr, name))
	default:
		return fmt.Errorf("item %s: no decoder for format %q", n.Item, itemFormat)
	}

	return nil
}

// emitBinaryLeaf walks a B-format leaf, whose Go type decides how much of the item it reads: one
// byte, a fixed-width array, or the whole variable-length sequence.
func (e *decodeEmitter) emitBinaryLeaf(n *StructureNode, name string, goType string, expr string) error {
	rawType := e.items[n.Item].GoType

	switch {
	case rawType == "byte":
		e.assign(e.field(name, goType), fmt.Sprintf("decodeByteAs[%s](%s, %q)", goType, expr, name))
	case rawType == "[]byte":
		e.assign(e.field(name, goType), fmt.Sprintf("decodeBinary(%s, %q)", expr, name))
	case byteArrayGoTypeRE.MatchString(rawType):
		width, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rawType, "["), "]byte"))
		if err != nil {
			return fmt.Errorf("item %s: cannot read the array width out of goType %q", n.Item, rawType)
		}

		blob := fmt.Sprintf("b%d", e.nextID())
		e.emit(
			fmt.Sprintf("var %s []byte", blob),
			fmt.Sprintf("if %s, err = decodeBinary(%s, %q); err != nil {", blob, expr, name),
			"return out, err",
			"}",
			"",
			fmt.Sprintf("if len(%s) != %d {", blob, width),
			fmt.Sprintf("return out, errSize(%q, len(%s), %d)", name, blob, width),
			"}",
			"",
			fmt.Sprintf("out.%s = %s(%s)", e.field(name, goType), goType, blob),
			"",
		)
	default:
		return fmt.Errorf("item %s: no decoder for B-format goType %q", n.Item, goType)
	}

	return nil
}

// trimTrailingBlank drops the blank separator line a statement block ends with, so a block nested
// inside a generated guard does not leave a stray empty line before its closing brace.
func trimTrailingBlank(lines []string) []string {
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		return lines[:len(lines)-1]
	}

	return lines
}

// firstFormat returns the item's declared SECS-II format, or "" when it declares none.
func firstFormat(it Item) string {
	if len(it.Formats) == 0 {
		return ""
	}

	return it.Formats[0]
}

// resultTypeDoc renders the godoc lines for a result struct: a name-first summary naming the
// message it decodes, then the same Body: shorthand the builder's godoc carries, so the struct
// and the shape it mirrors read together.
func resultTypeDoc(typeName string, code string, callName string, m Message, b Body, items map[string]Item) []string {
	doc := []string{
		fmt.Sprintf("%s is the decoded body of the %s (%s) message%s.", typeName, code, m.Name, actorClause(b.Actor)),
		"",
		ensurePeriod("Body: " + BodyDoc(b.Structure, items)),
		"",
		fmt.Sprintf("It is filled by [Decode%s] and mirrors the parameters of [%s], in order.", callName, callName),
	}

	if m.Source == "external" {
		doc = append(doc, "", externalSourceDisclaimer)
	}

	return doc
}

// decoderDoc renders the godoc lines for a decoder function.
func decoderDoc(typeName string, code string, callName string, m Message, b Body) []string {
	doc := []string{
		fmt.Sprintf("Decode%s decodes the body of an %s (%s) message%s into a [%s].", callName, code, m.Name, actorClause(b.Actor), typeName),
		"",
		"item is the message body, as returned by SECS2Message.Item.",
		"The body must match the shape [" + typeName + "] documents:",
		"a wrong list length, a wrong SECS-II type, or a missing position returns an error naming the E5 field that failed.",
	}

	if m.Source == "external" {
		doc = append(doc, "", externalSourceDisclaimer)
	}

	return doc
}

// actorClause renders the " for equipment" / " for host" godoc fragment for a body's actor.
func actorClause(actor string) string {
	switch actor {
	case "equipment":
		return " for equipment"
	case "host":
		return " for host"
	default: // "both": no actor clause
		return ""
	}
}

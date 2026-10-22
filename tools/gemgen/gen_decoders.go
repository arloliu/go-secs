package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/format"
	"slices"
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
// A node that sets as: is named after that instead.
//
// Param is the index of the builder parameter the field mirrors.
// An optional leaf's field is a pointer, nil when the item arrived zero-length, as its parameter is.
// Elem is the element item's goType for the field of a value repeat, and empty for any other field:
// such a field is a slice of decoded values, as its parameter is a slice of values to send.
// Path is the list-index path from the body root to the item the field is read from.
type decodeField struct {
	Name  string
	Type  string
	Param int
	Elem  string
	Path  []int
}

// decoderView is the per-decoder view model the decoders template renders.
type decoderView struct {
	TypeName string        // "S1F14Reply"
	FuncName string        // "DecodeS1F14"
	Fields   []decodeField // result-struct fields, in body order
	Lines    []string      // pre-rendered Go statement lines of the decoder body
	UsesCfg  bool          // whether Lines read the decode options, which the decoder then builds as cfg
	TypeDoc  []string      // godoc lines for the result struct (without leading "// ")
	FuncDoc  []string      // godoc lines for the decoder function (without leading "// ")
}

// renderDecodeSupport returns the shared helpers every generated decoder calls,
// and the one a builder calls to send a repeat of values.
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
// to decode, and a variant, which the main body's decoder for its actor decodes.
func renderDecoders(mf MessageFile, items map[string]Item) ([]byte, error) {
	tmpl, err := template.New("decoders.go.tmpl").Parse(decodersTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse decoders template: %w", err)
	}

	var decoders []decoderView

	for _, m := range mf.Messages {
		for _, b := range m.Bodies {
			if b.Structure == nil || b.Variant != "" {
				continue // header-only: no body to decode; variant: decoded by its main body's decoder
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
	name := callName(stream, function, actor)

	if function%2 == 0 {
		return name + "Reply"
	}

	return name + "Body"
}

// newDecoderView builds the view model for one (message, body) pair's decoder.
func newDecoderView(stream int, m Message, b Body, items map[string]Item) (decoderView, error) {
	code := fmt.Sprintf("S%dF%d", stream, m.Function)
	name := callName(stream, m.Function, b.Actor)
	typeName := ResultTypeName(stream, m.Function, b.Actor)

	e, err := walkDecoder(code, b.Structure, items)
	if err != nil {
		return decoderView{}, err
	}

	return decoderView{
		TypeName: typeName,
		FuncName: "Decode" + name,
		Fields:   e.fields,
		Lines:    e.lines,
		UsesCfg:  e.usesCfg,
		TypeDoc:  resultTypeDoc(typeName, code, name, m, b, items),
		FuncDoc:  decoderDoc(typeName, code, name, m, b, e.usesCfg),
	}, nil
}

// BuildFields returns the ordered result-struct fields a generated decoder fills.
//
// It walks the structure with the same ordering and the same stopping rules as [BuildParams],
// so every field describes the builder parameter its Param names,
// and those parameters appear in order.
// gen_decoders_test.go asserts that agreement name-by-name over every message in data/messages.
func BuildFields(n *StructureNode, items map[string]Item) ([]decodeField, error) {
	e, err := walkDecoder("S0F0", n, items)
	if err != nil {
		return nil, err
	}

	return e.fields, nil
}

// walkDecoder walks a validated body structure with a fresh emitter.
// code labels the shape errors the emitted statements report.
func walkDecoder(code string, n *StructureNode, items map[string]Item) (*decodeEmitter, error) {
	e := &decodeEmitter{code: code, items: items, recv: "out", root: "root"}
	if err := e.emitNode(n, nil); err != nil {
		return nil, err
	}

	return e, nil
}

// walkGroupDecoder walks a validated group structure with a fresh emitter,
// for the body of the group's decode function:
// it fills g from the cursor c and labels its errors relative to the function's field parameter.
func walkGroupDecoder(g *Group, items map[string]Item) (*decodeEmitter, error) {
	e := &decodeEmitter{items: items, recv: "g", root: "c", group: true}
	if err := e.emitNode(g.Structure, nil); err != nil {
		return nil, fmt.Errorf("group %s: %w", g.Name, err)
	}

	return e, nil
}

// decodeEmitter accumulates the fields and the Go statement lines of one generated decoder as it
// walks a validated structure node.
//
// The statements fill the struct named recv from the cursor named root.
// A message decoder fills out from root, and labels each error with a constant:
// a field name, or a body position such as "S1F14 body[1]".
// A group's decode function fills g from c,
// and labels each error by appending to its field parameter, which names the group's own position,
// e.g. field+".VIDS" for a field and field+"[1]" for a sub-list.
type decodeEmitter struct {
	code    string
	items   map[string]Item
	recv    string
	root    string
	group   bool
	fields  []decodeField
	lines   []string
	counter int
	params  int  // builder parameters mirrored so far: the next field's Param
	usesCfg bool // whether a statement reads the decode options, so a message decoder must build cfg
}

// cfg renders the expression of the decode options the emitted statements pass on,
// and records that they read them.
// A message decoder builds cfg from its options; a group's decode function takes it as a parameter.
func (e *decodeEmitter) cfg() string {
	e.usesCfg = true

	return "cfg"
}

// label renders the Go expression of the label the errors of the field name report.
func (e *decodeEmitter) label(name string) string {
	if e.group {
		return fmt.Sprintf("field+%q", "."+name)
	}

	return strconv.Quote(name)
}

// listLabel renders the Go expression of the label the shape errors of the list at path report.
func (e *decodeEmitter) listLabel(path []int) string {
	switch {
	case !e.group:
		return strconv.Quote(e.pathLabel(path))
	case len(path) == 0:
		return "field"
	default:
		return fmt.Sprintf("field+%q", pathIndex(path))
	}
}

// fail renders the statement that returns err from the generated function.
func (e *decodeEmitter) fail() string {
	return fmt.Sprintf("return %s, err", e.recv)
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

// field records one result-struct field that mirrors the next builder parameter and is read from path,
// and returns its Go name.
func (e *decodeEmitter) field(name string, goType string, path []int) string {
	e.fields = append(e.fields, decodeField{Name: name, Type: goType, Param: e.params, Path: slices.Clone(path)})
	e.params++

	return name
}

// assign emits the guarded "decode into the field, or return the error" statement pair every
// scalar and item-valued field shares.
func (e *decodeEmitter) assign(name string, call string) {
	e.emit(
		fmt.Sprintf("if %s.%s, err = %s; err != nil {", e.recv, name, call),
		e.fail(),
		"}",
		"",
	)
}

// pathExpr renders the cursor expression addressing a position below the emitter's root.
func (e *decodeEmitter) pathExpr(path []int) string {
	if len(path) == 0 {
		return e.root
	}

	parts := make([]string, len(path))
	for i, idx := range path {
		parts[i] = strconv.Itoa(idx)
	}

	return e.root + ".At(" + strings.Join(parts, ", ") + ")"
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
		name := nodeNames(n)
		e.assign(e.field(name.Field, "secs2.Item", path), fmt.Sprintf("decodeItem(%s, %s)", e.pathExpr(path), e.label(name.Label)))

		return nil
	case "group":
		// Only an optional reference sits outside a repeat, as a child of a group's fixed list.
		name := nodeNames(n)
		e.assign(e.field(name.Field, elemType(n, e.items), path),
			fmt.Sprintf("decodeOptionalGroup(%s, %s, %d, %s, decode%s)", e.pathExpr(path), e.label(name.Label), len(n.def.Structure.Items), e.cfg(), n.Group))

		return nil
	default: // list
		return e.emitList(n, path)
	}
}

// emitList walks a list node: a repeated group, a packed group, or a fixed group of children.
func (e *decodeEmitter) emitList(n *StructureNode, path []int) error {
	switch {
	case n.Repeat != "":
		return e.emitRepeat(n, path)
	case n.Packed != "":
		return e.emitPacked(n, path)
	default:
		return e.emitFixedList(n, path)
	}
}

// emitRepeat walks a repeat: a list of any number of elements of the shape its of declares.
//
// A repeat of secs2 items returns the list's children as they are,
// or checks each one when its item excludes L (see [decodeEmitter.repeatCall]).
// Any other repeat decodes each element with the function [elemDecodeFunc] gives,
// so an element's decode error names the field and the element's index, e.g. REPORTS[1].
func (e *decodeEmitter) emitRepeat(n *StructureNode, path []int) error {
	name := nodeNames(n)
	typ := "[]" + elemType(n.Of, e.items)

	call, err := e.repeatCall(n, e.pathExpr(path), e.label(name.Label))
	if err != nil {
		return err
	}

	field := decodeField{Name: name.Field, Type: typ, Param: e.params, Path: slices.Clone(path)}
	if it, ok := valueRepeatItem(n, e.items); ok {
		field.Elem = it.GoType
	}

	e.fields = append(e.fields, field)
	e.params++

	e.assign(field.Name, call)

	return nil
}

// repeatCall renders the call that decodes the repeat n from the cursor expression cursor,
// labeling its errors with the Go expression label.
//
// A repeat of secs2 items returns the list's children as they are,
// unless its element is a leaf whose item excludes L:
// each element is then checked with that leaf's decode helper, as any other element is decoded.
func (e *decodeEmitter) repeatCall(n *StructureNode, cursor string, label string) (string, error) {
	if elemType(n.Of, e.items) == "secs2.Item" {
		if fn := itemDecodeFunc(n.Of, e.items); fn != "decodeItem" {
			return fmt.Sprintf("decodeEach(%s, %s, %s)", cursor, label, fn), nil
		}

		return fmt.Sprintf("decodeItems(%s, %s)", cursor, label), nil
	}

	fn, err := e.elemDecodeFunc(n)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("decodeEach(%s, %s, %s)", cursor, label, fn), nil
}

// elemDecodeFunc renders the function that decodes one element of the repeat n
// from a cursor and a label, as decodeEach calls it:
// the decode helper a leaf of a fixed scalar element item uses,
// a group's decode function bound to the decode options by withConfig, or by optionalGroup when the element is optional,
// or, for a repeat element, the decoding of that inner repeat.
func (e *decodeEmitter) elemDecodeFunc(n *StructureNode) (string, error) {
	of := n.Of

	switch of.Kind() {
	case "group":
		if of.Optional {
			return fmt.Sprintf("optionalGroup(%s, %d, decode%s)", e.cfg(), len(of.def.Structure.Items), of.Group), nil
		}

		return fmt.Sprintf("withConfig(%s, decode%s)", e.cfg(), of.Group), nil
	case "list":
		if elemType(of.Of, e.items) == "secs2.Item" {
			if fn := itemDecodeFunc(of.Of, e.items); fn != "decodeItem" {
				return fmt.Sprintf("eachOf(%s)", fn), nil
			}

			return "decodeItems", nil
		}

		fn, err := e.elemDecodeFunc(of)
		if err != nil {
			return "", err
		}

		return fmt.Sprintf("eachOf(%s)", fn), nil
	default: // a fixed scalar leaf: elemType sent every other element to decodeItems
		return e.valueDecodeFunc(n)
	}
}

// valueDecodeFunc renders the decode helper that reads one element of the value repeat n.
func (e *decodeEmitter) valueDecodeFunc(n *StructureNode) (string, error) {
	it, ok := valueRepeatItem(n, e.items)
	if !ok {
		return "", fmt.Errorf("repeat %s: no decoder for its element", n.Repeat)
	}

	goType := fixedGoType(n.Of.Item, it)

	sf, err := scalarDecodeFunc(firstFormat(it), goType)
	if err != nil {
		return "", fmt.Errorf("repeat %s: item %s: %w", n.Repeat, n.Of.Item, err)
	}

	if sf.result == goType {
		return sf.fn, nil
	}

	// decodeEach cannot convert a helper's plain result to a named type.
	// decodeASCIIAs reads a named string enum directly;
	// no other format has such a helper.
	if firstFormat(it) != "A" {
		return "", fmt.Errorf("repeat %s: item %s: no decoder for a named %s type of format %s", n.Repeat, n.Of.Item, sf.result, firstFormat(it))
	}

	return fmt.Sprintf("decodeASCIIAs[%s]", goType), nil
}

// emitPacked walks a packed multi-value group, which is a single primitive item on the wire, not
// a list of separate items.
func (e *decodeEmitter) emitPacked(n *StructureNode, path []int) error {
	it := e.items[n.Of.Item]
	if itemFormat := firstFormat(it); itemFormat != "B" || it.GoType != "byte" {
		return fmt.Errorf("packed %s: no decoder for item %s (format %q, goType %q)", n.Packed, n.Of.Item, firstFormat(it), it.GoType)
	}

	name := nodeNames(n)
	e.assign(e.field(name.Field, "[]byte", path), fmt.Sprintf("decodeBinary(%s, %s)", e.pathExpr(path), e.label(name.Label)))

	return nil
}

// emitFixedList walks a fixed group of children, emitting its arity check first.
//
// A group the DSL bounds with minItems below its declared child count is optional: SEMI E5 lets
// the sender omit it entirely, so both the short and the full form decode, and the fields the
// short form omits keep their zero value.
func (e *decodeEmitter) emitFixedList(n *StructureNode, path []int) error {
	label := e.pathLabel(path)
	if e.group {
		label = "group list" + pathIndex(path)
	}

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
			fmt.Sprintf("if err = requireArity(%s, %s, %d, %s); err != nil {", e.pathExpr(path), e.listLabel(path), full, e.cfg()),
			e.fail(),
			"}",
			"",
		)

		return e.emitChildren(n, path, 0, full)
	}

	count := fmt.Sprintf("n%d", e.nextID())
	e.emit(
		fmt.Sprintf("%s, err := optionalArity(%s, %s, %d, %d, %s)", count, e.pathExpr(path), e.listLabel(path), short, full, e.cfg()),
		"if err != nil {",
		e.fail(),
		"}",
		"",
	)

	if err := e.emitChildren(n, path, 0, short); err != nil {
		return err
	}

	// Fence the optional tail: the short form stops at `short` children, so the fields beyond it
	// are read only when the full form arrived.
	tail := &decodeEmitter{code: e.code, items: e.items, recv: e.recv, root: e.root, group: e.group, counter: e.counter, params: e.params}
	if err := tail.emitChildren(n, path, short, full); err != nil {
		return err
	}

	e.counter = tail.counter
	e.params = tail.params
	e.usesCfg = e.usesCfg || tail.usesCfg
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
	name := nodeNames(n)
	expr := e.pathExpr(path)

	// Validation rejects these cases first;
	// the check keeps an unvalidated node from being decoded as if it were not optional.
	if n.Optional {
		if err := optionalLeafError(n.Item, it); err != nil {
			return err
		}
	}

	if it.Binding != BindingFixed {
		e.assign(e.field(name.Field, "secs2.Item", path), fmt.Sprintf("%s(%s, %s)", itemDecodeFunc(n, e.items), expr, e.label(name.Label)))

		return nil
	}

	goType := fixedGoType(n.Item, it)

	itemFormat := firstFormat(it)
	if itemFormat == "B" && it.GoType != "byte" {
		return e.emitBinaryLeaf(n, name, goType, path)
	}

	sc, err := scalarDecodeCall(itemFormat, goType, expr, e.label(name.Label))
	if err != nil {
		return fmt.Errorf("item %s: %w", n.Item, err)
	}

	if !n.Optional {
		e.emit(e.decodeInto(e.field(name.Field, goType, path), goType, sc)...)

		return nil
	}

	return e.emitOptionalLeaf(n, name, goType, itemFormat, path)
}

// itemDecodeFunc returns the decode helper that reads n as a secs2.Item:
// a leaf whose item [rejectsList] reads through decodeScalarItem,
// or decodeScalarOrEmptyList when the leaf sets allowEmptyList;
// any other node, such as an opaque node or a leaf whose item allows a list, through decodeItem.
func itemDecodeFunc(n *StructureNode, items map[string]Item) string {
	switch {
	case n.Kind() != "leaf" || !rejectsList(items[n.Item]):
		return "decodeItem"
	case n.AllowEmptyList:
		return "decodeScalarOrEmptyList"
	default:
		return "decodeScalarItem"
	}
}

// scalarCall is a decode helper call that reads one single-value leaf.
type scalarCall struct {
	call   string // the call, e.g. decodeUintAs[uint16](root.At(0), "SVID")
	family string // the secs2 item type the call reads, e.g. *secs2.UintItem
	result string // the Go type the call returns
}

// scalarFunc is a decode helper that reads one single-value item, named as a function value.
type scalarFunc struct {
	fn     string // the helper, e.g. decodeUintAs[uint16]
	family string // the secs2 item type the helper reads, e.g. *secs2.UintItem
	result string // the Go type the helper returns
}

// scalarDecodeFunc returns the decode helper that reads a single-value item of format itemFormat as goType.
//
// The numeric and byte helpers are generic and return goType itself;
// the ASCII and boolean helpers return a plain string or bool,
// which a named enum type must be converted from.
func scalarDecodeFunc(itemFormat string, goType string) (scalarFunc, error) {
	switch itemFormat {
	case "A":
		return scalarFunc{"decodeASCII", "*secs2.ASCIIItem", "string"}, nil
	case "BOOLEAN":
		return scalarFunc{"decodeBool", "*secs2.BooleanItem", "bool"}, nil
	case "B":
		return scalarFunc{fmt.Sprintf("decodeByteAs[%s]", goType), "*secs2.BinaryItem", goType}, nil
	case "U1", "U2", "U4", "U8":
		return scalarFunc{fmt.Sprintf("decodeUintAs[%s]", goType), "*secs2.UintItem", goType}, nil
	case "I1", "I2", "I4", "I8":
		return scalarFunc{fmt.Sprintf("decodeIntAs[%s]", goType), "*secs2.IntItem", goType}, nil
	case "F4", "F8":
		return scalarFunc{fmt.Sprintf("decodeFloatAs[%s]", goType), "*secs2.FloatItem", goType}, nil
	default:
		return scalarFunc{}, fmt.Errorf("no decoder for format %q", itemFormat)
	}
}

// scalarDecodeCall returns the decode helper call
// that reads a single-value leaf of format itemFormat at expr as goType.
// label is the Go expression of the name the call's errors report.
func scalarDecodeCall(itemFormat string, goType string, expr string, label string) (scalarCall, error) {
	sf, err := scalarDecodeFunc(itemFormat, goType)
	if err != nil {
		return scalarCall{}, err
	}

	return scalarCall{fmt.Sprintf("%s(%s, %s)", sf.fn, expr, label), sf.family, sf.result}, nil
}

// decodeInto renders the statements that decode sc into the field name of type goType, or return the error.
//
// When sc returns another type than goType,
// as decodeASCII does for a named string enum,
// the value goes through a local s<k> and is converted on assignment.
func (e *decodeEmitter) decodeInto(name string, goType string, sc scalarCall) []string {
	if sc.result == goType {
		return []string{
			fmt.Sprintf("if %s.%s, err = %s; err != nil {", e.recv, name, sc.call),
			e.fail(),
			"}",
			"",
		}
	}

	v := fmt.Sprintf("s%d", e.nextID())

	return []string{
		fmt.Sprintf("var %s %s", v, sc.result),
		fmt.Sprintf("if %s, err = %s; err != nil {", v, sc.call),
		e.fail(),
		"}",
		"",
		fmt.Sprintf("%s.%s = %s(%s)", e.recv, name, goType, v),
		"",
	}
}

// emitOptionalLeaf emits the statement that decodes an optional leaf into its pointer field.
//
// A zero-length item of the type the leaf's decode helper reads leaves the field nil.
// Any other item goes to that helper, exactly as for a leaf that is not optional,
// so a wrong-format item, empty or not, is still an error.
func (e *decodeEmitter) emitOptionalLeaf(n *StructureNode, name emittedName, goType string, itemFormat string, path []int) error {
	sf, err := scalarDecodeFunc(itemFormat, goType)
	if err != nil {
		return fmt.Errorf("item %s: %w", n.Item, err)
	}

	// Only the ASCII helper returns another type than goType, and validation rejects an optional ASCII leaf.
	if sf.result != goType {
		return fmt.Errorf("item %s: no optional decoder for a %s read as %s", n.Item, sf.result, goType)
	}

	e.assign(e.field(name.Field, "*"+goType, path),
		fmt.Sprintf("decodeOptional[%s](%s, %s, %s)", sf.family, e.pathExpr(path), e.label(name.Label), sf.fn))

	return nil
}

// emitBinaryLeaf walks a B-format leaf whose Go type is a byte slice or a byte array,
// and so reads the whole item or a fixed-width part of it rather than one byte.
func (e *decodeEmitter) emitBinaryLeaf(n *StructureNode, name emittedName, goType string, path []int) error {
	rawType := e.items[n.Item].GoType
	expr := e.pathExpr(path)
	label := e.label(name.Label)

	switch {
	case rawType == "[]byte":
		e.assign(e.field(name.Field, goType, path), fmt.Sprintf("decodeBinary(%s, %s)", expr, label))
	case byteArrayGoTypeRE.MatchString(rawType):
		width, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rawType, "["), "]byte"))
		if err != nil {
			return fmt.Errorf("item %s: cannot read the array width out of goType %q", n.Item, rawType)
		}

		blob := fmt.Sprintf("b%d", e.nextID())
		e.emit(
			fmt.Sprintf("var %s []byte", blob),
			fmt.Sprintf("if %s, err = decodeBinary(%s, %s); err != nil {", blob, expr, label),
			e.fail(),
			"}",
			"",
			fmt.Sprintf("if len(%s) != %d {", blob, width),
			fmt.Sprintf("return %s, errSize(%s, len(%s), %d)", e.recv, label, blob, width),
			"}",
			"",
			fmt.Sprintf("%s.%s = %s(%s)", e.recv, e.field(name.Field, goType, path), goType, blob),
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
func resultTypeDoc(typeName string, code string, mainName string, m Message, b Body, items map[string]Item) []string {
	doc := []string{
		fmt.Sprintf("%s is the decoded body of the %s (%s) message%s.", typeName, code, m.Name, actorClause(b.Actor)),
		"",
		ensurePeriod("Body: " + BodyDoc(b.Structure, items)),
		"",
	}

	names := optionalFieldNames(b.Structure)
	doc = append(doc, fmt.Sprintf("It is filled by [Decode%s] and mirrors the parameters of [%s], in order.", mainName, mainName))
	doc = append(doc, emptyListDoc(b.Structure, m)...)

	if len(names) > 0 {
		field := "a nil " + names[0]
		if len(names) > 1 {
			field = "a nil field"
		}

		doc = append(doc,
			"",
			optionalSummary(names)+":",
			fmt.Sprintf("[Decode%s] decodes that form as %s, and [%s] sends it for a nil argument.", mainName, field, mainName),
		)
	}

	if m.Source == "external" {
		doc = append(doc, "", externalSourceDisclaimer)
	}

	return doc
}

// decoderDoc renders the godoc lines for a decoder function.
// usesCfg says whether the decoder reads its options, which only a body with a list of fixed length does.
func decoderDoc(typeName string, code string, mainName string, m Message, b Body, usesCfg bool) []string {
	doc := []string{
		fmt.Sprintf("Decode%s decodes the body of an %s (%s) message%s into a [%s].", mainName, code, m.Name, actorClause(b.Actor), typeName),
		"",
		"item is the message body, as returned by SECS2Message.Item.",
		"The body must match the shape [" + typeName + "] documents:",
		"a wrong list length, a wrong SECS-II type, or a missing position returns an error naming the E5 field that failed.",
	}

	if usesCfg {
		doc = append(doc, "With [AllowTrailing], a list may hold more children than the shape declares; the extra ones are ignored.")
	} else {
		doc = append(doc, "No [DecodeOption] changes how it decodes: the body declares no list of fixed length.")
	}

	doc = append(doc, emptyListDoc(b.Structure, m)...)

	if m.Source == "external" {
		doc = append(doc, "", externalSourceDisclaimer)
	}

	return doc
}

// emptyListDoc renders the godoc paragraph naming the positions of a body that set allowEmptyList,
// which accept the empty list L[0] where every other list is an error,
// followed by the message's exception, which says what L[0] means there;
// or nil when the body has no such position.
func emptyListDoc(n *StructureNode, m Message) []string {
	positions := emptyListPositions(n, "")
	if len(positions) == 0 {
		return nil
	}

	sentence := joinAnd(positions) + " may also be L[0], an empty list, which SEMI E5 gives a meaning in this message;"
	doc := append([]string{""}, wrapParagraph(strings.ToUpper(sentence[:1])+sentence[1:]+" any other list is an error.")...)

	if m.Exception != "" && m.Exception != "None" {
		doc = append(doc, "")
		doc = append(doc, wrapParagraph(ensurePeriod("Exception: "+m.Exception))...)
	}

	return doc
}

// emptyListPositions returns the positions below n whose leaf sets allowEmptyList, in body order,
// each named after its field, e.g. "ECV", or as an element of a repeat field, e.g. "an element of ECVS".
// name is the position n itself stands for, or "" for a body root.
// Group references are not walked: a group's positions belong to the group's own godoc.
func emptyListPositions(n *StructureNode, name string) []string {
	switch {
	case n == nil:
		return nil
	case n.Kind() == "leaf":
		if !n.AllowEmptyList {
			return nil
		}

		if name == "" {
			name = nodeNames(n).Field
		}

		return []string{name}
	case n.Kind() != "list" || n.Packed != "":
		return nil
	case n.Repeat != "":
		if name == "" {
			name = nodeNames(n).Field
		}

		return emptyListPositions(n.Of, "an element of "+name)
	}

	var positions []string
	for i := range n.Items {
		positions = append(positions, emptyListPositions(&n.Items[i], "")...)
	}

	return positions
}

// optionalFieldNames returns the field names of a body's or a group's optional leaves, in body order.
//
// It walks the same nodes as [BuildParams]:
// nothing below a repeat's or packed group's of may be optional.
func optionalFieldNames(n *StructureNode) []string {
	if n == nil {
		return nil
	}

	if n.Kind() == "leaf" && n.Optional {
		return []string{nodeNames(n).Field}
	}

	var names []string
	for i := range n.Items {
		names = append(names, optionalFieldNames(&n.Items[i])...)
	}

	return names
}

// optionalSummary renders the godoc clause naming the optional items of a body, e.g.
// "SEMI E5 lets FNLOC and FFROT each be sent as a zero-length item".
// names must not be empty.
func optionalSummary(names []string) string {
	if len(names) == 1 {
		return fmt.Sprintf("SEMI E5 lets %s be sent as a zero-length item", names[0])
	}

	return fmt.Sprintf("SEMI E5 lets %s each be sent as a zero-length item", joinAnd(names))
}

// joinAnd joins words as an English list:
// "A", "A and B", or "A, B, and C".
func joinAnd(words []string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	case 2:
		return words[0] + " and " + words[1]
	default:
		return strings.Join(words[:len(words)-1], ", ") + ", and " + words[len(words)-1]
	}
}

// optionalBuilderDoc renders the godoc paragraph a builder of a body with optional leaves carries,
// or nil when the body has none.
func optionalBuilderDoc(n *StructureNode) []string {
	names := optionalFieldNames(n)
	if len(names) == 0 {
		return nil
	}

	return []string{
		"",
		optionalSummary(names) + ":",
		"a nil argument sends that form.",
	}
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

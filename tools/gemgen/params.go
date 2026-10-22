package main

import (
	"fmt"
	"strings"
)

// Param is one generated function parameter.
//
// Repeat is true when the parameter originates from a repeat or packed node,
// and Type is then its element type.
// Such a parameter renders as name []<Type>,
// or name ...<Type> when it is also the final parameter (Variadic).
// A non-repeat leaf renders as name <Type>.
// Repeat is independent of position: a non-final repeat is still Repeat: true
// (a slice) without being Variadic.
type Param struct {
	Name     string
	Type     string
	Repeat   bool
	Variadic bool
}

// emittedName is the set of names one leaf, opaque, repeat, or packed node is generated under.
type emittedName struct {
	Param string // builder parameter; also labels the node's generated test sample
	Field string // decoder result-struct field
	Label string // the name the node's decode errors report
}

// BuildParams walks a validated structure node
// and returns the ordered function parameters a generated builder needs.
// A repeat node contributes a single caller-supplied slice parameter and does NOT recurse into Of:
// its element type is [elemType] of Of.
// A group reference, which only a group's own structure holds outside a repeat,
// contributes a parameter of its group's struct type, a pointer when it is optional.
// After the walk, the final parameter is promoted to variadic
// when (and only when) it is itself a repeat.
func BuildParams(n *StructureNode, items map[string]Item) []Param {
	var out []Param

	var walk func(*StructureNode)
	walk = func(n *StructureNode) {
		if n == nil {
			return
		}

		switch n.Kind() {
		case "leaf":
			it := items[n.Item]
			typ := "secs2.Item"
			if it.Binding == BindingFixed {
				typ = fixedGoType(n.Item, it)
			}
			out = append(out, Param{Name: nodeNames(n).Param, Type: typ})
		case "opaque":
			out = append(out, Param{Name: nodeNames(n).Param, Type: "secs2.Item"})
		case "group":
			out = append(out, Param{Name: nodeNames(n).Param, Type: elemType(n, items)})
		default: // list
			if n.Repeat != "" {
				out = append(out, Param{Name: nodeNames(n).Param, Type: elemType(n.Of, items), Repeat: true})

				return // do NOT recurse into Of
			}
			if n.Packed != "" {
				// Packed group: concrete goType, never secs2.Item -- the caller supplies raw
				// primitive values, not pre-built Items, since packing into one item header is
				// a generator-owned concern.
				it := items[n.Of.Item]
				out = append(out, Param{Name: nodeNames(n).Param, Type: it.GoType, Repeat: true})
				return // do NOT recurse into Of
			}
			for i := range n.Items {
				walk(&n.Items[i])
			}
		}
	}
	walk(n)

	if len(out) > 0 && out[len(out)-1].Repeat {
		out[len(out)-1].Variadic = true
	}

	return out
}

// BodyExpr renders the secs2 expression that builds a message body from the
// generated parameters. A bare leaf or opaque top-level node is used directly
// (not wrapped in secs2.L). Enum-typed fixed leaves are converted back to their
// underlying goType at the constructor call site so the value satisfies the
// exact-type switch inside the secs2 combiners.
func BodyExpr(n *StructureNode, items map[string]Item) string {
	if n == nil {
		return "secs2.NewEmptyItem()"
	}

	return nodeExpr(n, items, func(n *StructureNode) string { return nodeNames(n).Param })
}

// nodeExpr renders the secs2 expression that builds the node n,
// reading the value of each leaf, opaque node, repeat, packed group, and group reference from ref(node):
// a builder parameter for a message body, a struct field for a group.
func nodeExpr(n *StructureNode, items map[string]Item, ref func(*StructureNode) string) string {
	switch n.Kind() {
	case "leaf":
		it := items[n.Item]
		if it.Binding == BindingFixed {
			return fixedItemExpr(it, ref(n))
		}

		return ref(n)
	case "opaque":
		return ref(n)
	case "group":
		// Only an optional reference sits outside a repeat: nil is sent as L[0].
		return fmt.Sprintf("optionalGroupItem(%s)", ref(n))
	default: // list
		if n.Repeat != "" {
			return fmt.Sprintf("secs2.L(%s...)", elemItems(n.Of, items, ref(n)))
		}
		if n.Packed != "" {
			// Packed group: call the underlying item's own constructor directly -- NEVER
			// secs2.L(...), which would produce the wrong (list-of-separate-items) wire shape.
			it := items[n.Of.Item]
			return fmt.Sprintf("secs2.%s(%s)", it.Formats[0], ref(n))
		}

		parts := make([]string, len(n.Items))
		for i := range n.Items {
			parts[i] = nodeExpr(&n.Items[i], items, ref)
		}

		return "secs2.L(" + strings.Join(parts, ", ") + ")"
	}
}

// elemItems renders the expression that turns arg, the slice of elements of a repeat whose of is of,
// into the []secs2.Item that secs2.L takes:
// values through valueItems and the element item's constructor,
// groups through their Item methods,
// optional groups through optionalGroupItem,
// and a repeat's slices each into a list of their own.
// A slice of secs2 items is already that.
func elemItems(of *StructureNode, items map[string]Item, arg string) string {
	switch of.Kind() {
	case "leaf":
		if it, ok := valueLeafItem(of, items); ok {
			return fmt.Sprintf("valueItems(%s, %s)", arg, valueItemFunc(of.Item, it))
		}

		return arg
	case "opaque":
		return arg
	case "group":
		if of.Optional {
			return fmt.Sprintf("valueItems(%s, optionalGroupItem[%s])", arg, of.Group)
		}

		return fmt.Sprintf("groupItems(%s)", arg)
	default: // list
		if of.Repeat == "" {
			return arg // elemType makes any other list a secs2.Item
		}

		return fmt.Sprintf("valueItems(%s, func(v %s) secs2.Item { return secs2.L(%s...) })",
			arg, elemType(of, items), elemItems(of.Of, items, "v"))
	}
}

// fixedItemExpr renders the secs2 expression that builds one item of the fixed item it
// from the Go value arg, which has the type [fixedGoType] gives it.
func fixedItemExpr(it Item, arg string) string {
	switch {
	case hasNamedType(it):
		// Enum item: the value has the named type (e.g. COMMACK),
		// but the secs2 constructor combiners type-switch on the exact primitive (case byte:),
		// which a named type does not satisfy, and secs2.A takes a plain string.
		// Convert back to goType at the call site,
		// so the value encodes instead of becoming a deferred-error item or failing to compile.
		arg = fmt.Sprintf("%s(%s)", it.GoType, arg) // e.g. byte(commack), string(carrieraction)
	case byteArrayGoTypeRE.MatchString(it.GoType):
		// Fixed-size byte array: secs2.B takes ...any,
		// and a [N]byte does not satisfy its []byte type-switch case directly -- slice it.
		// NEVER add a "..." spread here too;
		// spreading a []byte into ...any is a Go compile error.
		arg += "[:]"
	default:
		// Empty on purpose: it exists solely to satisfy golangci-lint's revive enforce-switch-style rule,
		// which requires a default clause.
	}

	return fmt.Sprintf("secs2.%s(%s)", it.Formats[0], arg)
}

// valueItemFunc renders the function a value repeat's builder passes to valueItems,
// which builds one element item from one value of the fixed item it, named name.
// A plain string element needs no conversion, so secs2.A itself serves;
// any other element gets a one-line function literal around [fixedItemExpr].
func valueItemFunc(name string, it Item) string {
	goType := fixedGoType(name, it)
	if goType == "string" && it.Formats[0] == "A" {
		return "secs2.A"
	}

	return fmt.Sprintf("func(v %s) secs2.Item { return %s }", goType, fixedItemExpr(it, "v"))
}

// BodyDoc renders the human-readable godoc shorthand describing a body shape.
// It uses this shorthand:
// header-only is "Header only.";
// a fixed leaf is <Ctor>[name];
// an open leaf is <name>;
// a whole-body opaque is form-dependent,
// while one nested in a list reads <name>, like an open leaf;
// a plain list is L[k]{ ... }, and an empty one L[0]{};
// a bounded list is L[a,b]{ ... };
// a repeat is L[n]{ <name>... };
// a packed group is <Ctor>[n]{ <name>... }
// (the packed item's own constructor letter instead of L, so packed and repeat read apart).
func BodyDoc(n *StructureNode, items map[string]Item) string {
	if n == nil {
		return "Header only."
	}

	if n.Kind() == "opaque" {
		return "form-dependent (see description)"
	}

	return nodeDoc(n, items)
}

// nodeDoc renders the [BodyDoc] shorthand of a node below the body root,
// or of a root that is not opaque.
func nodeDoc(n *StructureNode, items map[string]Item) string {
	switch n.Kind() {
	case "leaf":
		it := items[n.Item]
		name := nodeNames(n).Param
		if it.Binding == BindingFixed {
			return fmt.Sprintf("%s[%s]", it.Formats[0], name)
		}

		return fmt.Sprintf("<%s>", name)
	case "opaque", "group":
		return fmt.Sprintf("<%s>", nodeNames(n).Param)
	default: // list
		if n.Repeat != "" {
			return fmt.Sprintf("L[n]{ <%s>... }", nodeNames(n).Param)
		}
		if n.Packed != "" {
			it := items[n.Of.Item]
			return fmt.Sprintf("%s[n]{ <%s>... }", it.Formats[0], nodeNames(n).Param)
		}

		parts := make([]string, len(n.Items))
		for i := range n.Items {
			parts[i] = nodeDoc(&n.Items[i], items)
		}

		header := fmt.Sprintf("L[%d]", len(n.Items))
		if n.MinItems != nil || n.MaxItems != nil {
			header = fmt.Sprintf("L[%d,%d]", derefOr(n.MinItems), derefOr(n.MaxItems))
		}

		if len(parts) == 0 {
			return header + "{}"
		}

		return header + "{ " + strings.Join(parts, " ") + " }"
	}
}

// nodeNames derives the names a leaf, opaque, repeat, or packed node is generated under.
// It is the only place those names are derived,
// so the builder, the decoder, and both generated tests always agree on them.
//
// A leaf is named after its item and an opaque node is named body,
// unless the node sets as:, which replaces that name.
// Both give parameter lower(name) and field upper(name).
// A repeat or packed node keeps its repeat:/packed: name as the parameter,
// verbatim, and upper-cases it for the field.
// A group reference is named after its group, unless it sets as:.
// Decode errors report the field name, except for an opaque node,
// which has no E5 item name and reports its parameter name.
func nodeNames(n *StructureNode) emittedName {
	var param, field string

	switch {
	case n.As != "":
		param, field = strings.ToLower(n.As), strings.ToUpper(n.As)
	case n.Kind() == "leaf":
		param, field = strings.ToLower(n.Item), strings.ToUpper(n.Item)
	case n.Kind() == "opaque":
		param, field = "body", "BODY"
	case n.Kind() == "group":
		param, field = strings.ToLower(n.Group), strings.ToUpper(n.Group)
	case n.Repeat != "":
		param, field = n.Repeat, strings.ToUpper(n.Repeat)
	default: // packed
		param, field = n.Packed, strings.ToUpper(n.Packed)
	}

	if n.Kind() == "opaque" {
		return emittedName{Param: param, Field: field, Label: param}
	}

	return emittedName{Param: param, Field: field, Label: field}
}

// valueRepeatItem returns the element item of a repeat whose elements are values,
// and reports whether n is such a repeat:
// one whose of is a leaf of a binding: fixed item with a scalar goType.
// The builder takes those elements as values of the item's [fixedGoType] and the decoder returns them so.
func valueRepeatItem(n *StructureNode, items map[string]Item) (Item, bool) {
	if n.Repeat == "" || n.Of == nil {
		return Item{}, false
	}

	return valueLeafItem(n.Of, items)
}

// valueLeafItem returns the item of the leaf n and reports whether, as a repeat's element, it is a value:
// a binding: fixed item with a scalar goType.
func valueLeafItem(n *StructureNode, items map[string]Item) (Item, bool) {
	if n.Kind() != "leaf" {
		return Item{}, false
	}

	it, ok := items[n.Item]
	if !ok || it.Binding != BindingFixed || isByteArrayGoType(it.GoType) {
		return Item{}, false
	}

	return it, true
}

// elemType returns the Go type of one element of a repeat whose of is n,
// which is also the type of a group reference n outside a repeat:
// a value's [fixedGoType] for a fixed scalar leaf,
// the group's struct for a group reference, a pointer to it when the reference is optional,
// a slice of the inner element type for a repeat,
// and secs2.Item for any other element.
func elemType(n *StructureNode, items map[string]Item) string {
	switch n.Kind() {
	case "leaf":
		if it, ok := valueLeafItem(n, items); ok {
			return fixedGoType(n.Item, it)
		}

		return "secs2.Item"
	case "group":
		if n.Optional {
			return "*" + n.Group
		}

		return n.Group
	case "list":
		if n.Repeat != "" {
			return "[]" + elemType(n.Of, items)
		}

		return "secs2.Item"
	default: // opaque
		return "secs2.Item"
	}
}

// fixedGoType returns the Go type a value of the fixed item it, named name, has in parameters and decoder fields:
// the named type items.go declares for it, if any, and its goType otherwise.
func fixedGoType(name string, it Item) string {
	if hasNamedType(it) {
		return name // enum item: named defined type, e.g. COMMACK (over goType byte)
	}

	return it.GoType
}

// hasNamedType reports whether an item's parameters and decoder fields use the named type items.go declares for it:
// a fixed item with a values table.
// An open item's values are untyped constants, and its parameters and fields stay secs2.Item.
func hasNamedType(it Item) bool {
	return it.Binding == BindingFixed && len(it.Values) > 0
}

// derefOr returns the pointed-to int, or 0 when the pointer is nil.
func derefOr(p *int) int {
	if p == nil {
		return 0
	}

	return *p
}

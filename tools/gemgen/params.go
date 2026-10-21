package main

import (
	"fmt"
	"strings"
)

// Param is one generated function parameter.
//
// Repeat is true when the parameter originates from a repeat node. Such a
// parameter renders as name []secs2.Item, or name ...secs2.Item when it is
// also the final parameter (Variadic). A non-repeat leaf renders as name <Type>.
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

// BuildParams walks a validated structure node and returns the ordered function
// parameters a generated builder needs. A repeat node contributes a single
// caller-supplied slice parameter and does NOT recurse into Of. After the walk,
// the final parameter is promoted to variadic when (and only when) it is itself
// a repeat.
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
				typ = it.GoType
				if hasNamedType(it) {
					typ = n.Item // enum item: named defined type, e.g. COMMACK (over goType byte)
				}
			}
			out = append(out, Param{Name: nodeNames(n).Param, Type: typ})
		case "opaque":
			out = append(out, Param{Name: nodeNames(n).Param, Type: "secs2.Item"})
		default: // list
			if n.Repeat != "" {
				out = append(out, Param{Name: nodeNames(n).Param, Type: "secs2.Item", Repeat: true})
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

	switch n.Kind() {
	case "leaf":
		it := items[n.Item]
		if it.Binding == BindingFixed {
			arg := nodeNames(n).Param
			switch {
			case hasNamedType(it):
				// Enum item: the parameter is the named type (e.g. COMMACK), but the secs2
				// constructor combiners type-switch on the exact primitive (case byte:), which a
				// named type does not satisfy, and secs2.A takes a plain string. Convert back to
				// goType at the call site so the value encodes instead of becoming a deferred-error
				// item or failing to compile.
				arg = fmt.Sprintf("%s(%s)", it.GoType, arg) // e.g. byte(commack), string(carrieraction)
			case byteArrayGoTypeRE.MatchString(it.GoType):
				// Fixed-size byte array: secs2.B takes ...any, and a [N]byte does not satisfy its
				// []byte type-switch case directly -- slice it. NEVER add a "..." spread here too;
				// spreading a []byte into ...any is a Go compile error (constraint 14).
				arg += "[:]"
			default:
				// Empty on purpose: exists solely to satisfy golangci-lint's revive
				// enforce-switch-style rule, which requires a default clause.
			}

			return fmt.Sprintf("secs2.%s(%s)", it.Formats[0], arg)
		}

		return nodeNames(n).Param
	case "opaque":
		return nodeNames(n).Param
	default: // list
		if n.Repeat != "" {
			return fmt.Sprintf("secs2.L(%s...)", nodeNames(n).Param)
		}
		if n.Packed != "" {
			// Packed group: call the underlying item's own constructor directly -- NEVER
			// secs2.L(...), which would produce the wrong (list-of-separate-items) wire shape.
			it := items[n.Of.Item]
			return fmt.Sprintf("secs2.%s(%s)", it.Formats[0], nodeNames(n).Param)
		}

		parts := make([]string, len(n.Items))
		for i := range n.Items {
			parts[i] = BodyExpr(&n.Items[i], items)
		}

		return "secs2.L(" + strings.Join(parts, ", ") + ")"
	}
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
	case "opaque":
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

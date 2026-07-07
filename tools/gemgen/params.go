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
				if len(it.Values) > 0 {
					typ = n.Item // enum item: named defined type, e.g. COMMACK (over goType byte)
				}
			}
			out = append(out, Param{Name: lower(n.Item), Type: typ})
		case "opaque":
			out = append(out, Param{Name: "body", Type: "secs2.Item"})
		default: // list
			if n.Repeat != "" {
				out = append(out, Param{Name: n.Repeat, Type: "secs2.Item", Repeat: true})
				return // do NOT recurse into Of
			}
			if n.Packed != "" {
				// Packed group: concrete goType, never secs2.Item -- the caller supplies raw
				// primitive values, not pre-built Items, since packing into one item header is
				// a generator-owned concern.
				it := items[n.Of.Item]
				out = append(out, Param{Name: n.Packed, Type: it.GoType, Repeat: true})
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
			arg := lower(n.Item)
			switch {
			case len(it.Values) > 0:
				// Enum item: the parameter is the named type (e.g. COMMACK), but the secs2
				// constructor combiners type-switch on the exact primitive (case byte:), which a
				// named type does not satisfy. Convert back to goType at the call site so the
				// value encodes instead of becoming a deferred-error item.
				arg = fmt.Sprintf("%s(%s)", it.GoType, arg) // e.g. byte(commack)
			case byteArrayGoTypeRE.MatchString(it.GoType):
				// Fixed-size byte array: secs2.B takes ...any, and a [N]byte does not satisfy its
				// []byte type-switch case directly -- slice it. NEVER add a "..." spread here too;
				// spreading a []byte into ...any is a Go compile error (constraint 14).
				arg += "[:]"
			default:
			}

			return fmt.Sprintf("secs2.%s(%s)", it.Formats[0], arg)
		}

		return lower(n.Item)
	case "opaque":
		return "body"
	default: // list
		if n.Repeat != "" {
			return fmt.Sprintf("secs2.L(%s...)", n.Repeat)
		}
		if n.Packed != "" {
			// Packed group: call the underlying item's own constructor directly -- NEVER
			// secs2.L(...), which would produce the wrong (list-of-separate-items) wire shape.
			it := items[n.Of.Item]
			return fmt.Sprintf("secs2.%s(%s)", it.Formats[0], n.Packed)
		}

		parts := make([]string, len(n.Items))
		for i := range n.Items {
			parts[i] = BodyExpr(&n.Items[i], items)
		}

		return "secs2.L(" + strings.Join(parts, ", ") + ")"
	}
}

// BodyDoc renders the human-readable godoc shorthand describing a body shape.
// It mirrors the §6 shorthand column: header-only is "Header only."; a fixed
// leaf is <Ctor>[name]; an open leaf is <name>; opaque is form-dependent; a
// plain list is L[k]{ ... }; a bounded list is L[a,b]{ ... }; a repeat is
// L[n]{ <name>... }; a packed group is <Ctor>[n]{ <name>... } (the packed
// item's own constructor letter instead of L, so packed and repeat read apart).
func BodyDoc(n *StructureNode, items map[string]Item) string {
	if n == nil {
		return "Header only."
	}

	switch n.Kind() {
	case "leaf":
		it := items[n.Item]
		name := lower(n.Item)
		if it.Binding == BindingFixed {
			return fmt.Sprintf("%s[%s]", it.Formats[0], name)
		}

		return fmt.Sprintf("<%s>", name)
	case "opaque":
		return "form-dependent (see description)"
	default: // list
		if n.Repeat != "" {
			return fmt.Sprintf("L[n]{ <%s>... }", n.Repeat)
		}
		if n.Packed != "" {
			it := items[n.Of.Item]
			return fmt.Sprintf("%s[n]{ <%s>... }", it.Formats[0], n.Packed)
		}

		parts := make([]string, len(n.Items))
		for i := range n.Items {
			parts[i] = BodyDoc(&n.Items[i], items)
		}

		header := fmt.Sprintf("L[%d]", len(n.Items))
		if n.MinItems != nil || n.MaxItems != nil {
			header = fmt.Sprintf("L[%d,%d]", derefOr(n.MinItems), derefOr(n.MaxItems))
		}

		return header + "{ " + strings.Join(parts, " ") + " }"
	}
}

// lower lowercases an item or repeat name for use as a Go parameter identifier.
func lower(name string) string { return strings.ToLower(name) }

// derefOr returns the pointed-to int, or 0 when the pointer is nil.
func derefOr(p *int) int {
	if p == nil {
		return 0
	}

	return *p
}

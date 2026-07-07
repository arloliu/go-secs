package main

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// singleFormatGoType maps a single secs2 constructor to its concrete Go type
// for fixed-binding items. `L` is intentionally absent (lists are always open).
var singleFormatGoType = map[string]string{
	"A": "string", "J": "string", "W": "string",
	"B": "byte", "BOOLEAN": "bool",
	"I1": "int8", "I2": "int16", "I4": "int32", "I8": "int64",
	"U1": "uint8", "U2": "uint16", "U4": "uint32", "U8": "uint64",
	"F4": "float32", "F8": "float64",
}

// deriveBinding derives whether an item with the given SECS-II format-code
// shortcuts should be treated as fixed or open. An item is fixed only when it
// declares exactly one format and that format has a known Go type; every
// other case (zero, multiple, or unknown formats) is open.
func deriveBinding(formats []string) Binding {
	if len(formats) == 1 {
		if _, ok := singleFormatGoType[formats[0]]; ok {
			return BindingFixed
		}
	}

	return BindingOpen
}

// deriveGoType derives the concrete Go type for a fixed-binding item's format
// list. It reports false when the formats do not derive a fixed binding.
func deriveGoType(formats []string) (string, bool) {
	if deriveBinding(formats) != BindingFixed {
		return "", false
	}
	return singleFormatGoType[formats[0]], true
}

// LoadItems parses the items.yaml DSL content into a name-keyed map of Item.
func LoadItems(data []byte) (map[string]Item, error) {
	items := map[string]Item{}
	if err := yaml.Unmarshal(data, &items); err != nil {
		return nil, fmt.Errorf("parse items: %w", err)
	}

	return items, nil
}

// ValidateItems checks every loaded item against the Data Item Dictionary
// rules: a fixed-binding item must declare exactly one format and a goType
// matching what deriveGoType derives for it; every item's stored Binding must
// match what deriveBinding derives from its Formats; and a values: enum table
// is only valid on a fixed-binding item, since the named type it generates is
// defined over the item's concrete GoType.
func ValidateItems(items map[string]Item) error {
	for name, it := range items {
		want := deriveBinding(it.Formats)
		if it.Binding != want {
			return fmt.Errorf("item %s: binding %q, want %q for formats %v", name, it.Binding, want, it.Formats)
		}

		if it.Binding == BindingFixed {
			if len(it.Formats) != 1 {
				return fmt.Errorf("item %s: fixed binding needs exactly one format, got %v", name, it.Formats)
			}

			gt, _ := deriveGoType(it.Formats)
			if it.GoType == "" {
				return fmt.Errorf("item %s: fixed binding needs goType", name)
			}
			if it.GoType != gt {
				return fmt.Errorf("item %s: goType %q, want %q", name, it.GoType, gt)
			}
		}

		if len(it.Values) > 0 && it.Binding != BindingFixed {
			return fmt.Errorf("item %s: values table requires binding fixed (a named enum type needs a concrete goType)", name)
		}
	}

	return nil
}

// LoadMessageFile parses one stream's messages YAML (e.g. messages/s1.yaml)
// into a MessageFile.
func LoadMessageFile(data []byte) (MessageFile, error) {
	var mf MessageFile
	if err := yaml.Unmarshal(data, &mf); err != nil {
		return mf, fmt.Errorf("parse messages: %w", err)
	}

	return mf, nil
}

// ValidateMessages checks every message across all loaded message files
// against the DSL rules: every {item: X} reference at any structure depth
// must exist in items; no (stream, function) pair may repeat across files;
// every body's actor must be one of both/equipment/host; a list node may not
// combine more than one of repeat, packed, or minItems/maxItems, since they are
// mutually exclusive node shapes; and a packed node's of.item must resolve to a
// binding: fixed item, since packing needs one shared format for every value.
func ValidateMessages(files []MessageFile, items map[string]Item) error {
	seen := map[[2]int]bool{}
	for _, f := range files {
		for _, m := range f.Messages {
			key := [2]int{f.Stream, m.Function}
			if seen[key] {
				return fmt.Errorf("duplicate S%dF%d", f.Stream, m.Function)
			}
			seen[key] = true

			for _, b := range m.Bodies {
				switch b.Actor {
				case "both", "equipment", "host":
				default:
					return fmt.Errorf("S%dF%d: bad actor %q", f.Stream, m.Function, b.Actor)
				}

				if err := walkNode(b.Structure, items, f.Stream, m.Function); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// walkNode recursively validates a structure node and its descendants
// (Items list children and the Of repeat/packed/bounded element shape) against
// the items dictionary, the repeat/packed/minItems/maxItems mutual-exclusion
// rule, and the packed-item binding: fixed requirement.
func walkNode(n *StructureNode, items map[string]Item, s, fn int) error {
	if n == nil {
		return nil
	}

	switch n.Kind() {
	case "leaf":
		if _, ok := items[n.Item]; !ok {
			return fmt.Errorf("S%dF%d: unknown item %q", s, fn, n.Item)
		}
	case "list":
		exclusive := 0
		if n.Repeat != "" {
			exclusive++
		}
		if n.Packed != "" {
			exclusive++
		}
		if n.MinItems != nil || n.MaxItems != nil {
			exclusive++
		}
		if exclusive > 1 {
			return fmt.Errorf("S%dF%d: repeat, packed, and minItems/maxItems are mutually exclusive", s, fn)
		}

		if n.Packed != "" {
			if n.Of == nil || n.Of.Item == "" {
				return fmt.Errorf("S%dF%d: packed %q needs an of.item leaf", s, fn, n.Packed)
			}
			it, ok := items[n.Of.Item]
			if !ok {
				return fmt.Errorf("S%dF%d: unknown item %q", s, fn, n.Of.Item)
			}
			if it.Binding != BindingFixed {
				return fmt.Errorf("S%dF%d: packed %q's item %q must be binding: fixed (packing needs one shared format)", s, fn, n.Packed, n.Of.Item)
			}
		}

		for i := range n.Items {
			if err := walkNode(&n.Items[i], items, s, fn); err != nil {
				return err
			}
		}

		if err := walkNode(n.Of, items, s, fn); err != nil {
			return err
		}
	default:
	}

	return nil
}

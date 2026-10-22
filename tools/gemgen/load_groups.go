package main

import (
	"cmp"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"slices"
	"strings"
)

// LoadGroups parses the groups.yaml DSL content.
// A key the DSL does not define is an error (see decodeStrict).
func LoadGroups(data []byte) ([]Group, error) {
	var gf GroupFile
	if err := decodeStrict(data, &gf); err != nil {
		return nil, fmt.Errorf("parse groups: %w", err)
	}

	return gf.Groups, nil
}

// ResolveGroups validates the groups against the items and the message files,
// and points every group reference, in a message body or in a group, at the group it names.
// [ValidateMessages] then rejects any reference left unresolved.
//
// Every group needs a unique name that no other generated gem identifier takes (see checkGroupNames),
// a fixed-list structure that walkNode accepts as a group's (see checkGroupStructure),
// field names that compile and stay distinct,
// and at least one reference.
// No group may contain itself, directly or through other groups.
// The groups must not move in memory afterwards: references point into the slice.
func ResolveGroups(groups []Group, files []MessageFile, items map[string]Item) error {
	byName := map[string]*Group{}
	for i := range groups {
		g := &groups[i]
		if _, ok := byName[g.Name]; ok {
			return fmt.Errorf("group %s is declared twice", g.Name)
		}

		byName[g.Name] = g
	}

	if err := checkGroupNames(groups, files, items); err != nil {
		return err
	}

	users := map[string][]groupUser{}

	for i := range groups {
		g := &groups[i]
		if err := resolveRefs(g.Structure, byName, func(name string) {
			users[name] = append(users[name], groupUser{group: g.Name})
		}); err != nil {
			return fmt.Errorf("group %s: %w", g.Name, err)
		}
	}

	for _, f := range files {
		for _, m := range f.Messages {
			for _, b := range m.Bodies {
				if err := resolveRefs(b.Structure, byName, func(name string) {
					users[name] = append(users[name], groupUser{stream: f.Stream, function: m.Function})
				}); err != nil {
					return fmt.Errorf("S%dF%d: %w", f.Stream, m.Function, err)
				}
			}
		}
	}

	for i := range groups {
		g := &groups[i]
		if err := checkGroupStructure(g, items); err != nil {
			return err
		}

		if len(users[g.Name]) == 0 {
			return fmt.Errorf("group %s is not used by any message or group", g.Name)
		}

		g.users = userNames(users[g.Name])
	}

	return checkGroupCycles(groups)
}

// groupUser is one message or group that references a group.
type groupUser struct {
	stream, function int    // the message, when group is empty
	group            string // the referencing group
}

// userNames renders a group's users for its godoc, each once:
// the messages first, as SxFy in stream and function order,
// then the groups, as doc links in name order.
func userNames(users []groupUser) []string {
	slices.SortFunc(users, func(a, b groupUser) int {
		return cmp.Or(
			strings.Compare(a.group, b.group),
			cmp.Compare(a.stream, b.stream),
			cmp.Compare(a.function, b.function),
		)
	})

	var names []string
	for _, u := range slices.Compact(users) {
		if u.group != "" {
			names = append(names, "["+u.group+"]")
		} else {
			names = append(names, fmt.Sprintf("S%dF%d", u.stream, u.function))
		}
	}

	return names
}

// resolveRefs points every group reference in the structure n at the group of byName it names,
// and reports each one to use.
// It does not descend into the referenced groups, whose own references resolve separately.
func resolveRefs(n *StructureNode, byName map[string]*Group, use func(name string)) error {
	if n == nil {
		return nil
	}

	if n.Group != "" {
		g, ok := byName[n.Group]
		if !ok {
			return fmt.Errorf("unknown group %q", n.Group)
		}

		n.def = g
		use(g.Name)
	}

	for i := range n.Items {
		if err := resolveRefs(&n.Items[i], byName, use); err != nil {
			return err
		}
	}

	return resolveRefs(n.Of, byName, use)
}

// checkGroupStructure checks one group's structure:
// a fixed list whose nodes walkNode accepts inside a group,
// and whose fields are exported and distinct ignoring case, as a result struct's are.
func checkGroupStructure(g *Group, items map[string]Item) error {
	where := "group " + g.Name

	if g.Structure == nil || !isFixedList(g.Structure) || g.Structure.MinItems != nil || g.Structure.MaxItems != nil {
		return fmt.Errorf("%s: structure must be a list of declared children, not bounded", where)
	}

	if err := walkNode(g.Structure, items, nodeCtx{where: where, inGroup: true}, roleRoot); err != nil {
		return err
	}

	fields, err := BuildFields(g.Structure, items)
	if err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}

	seen := map[string]string{}
	for _, f := range fields {
		if !token.IsExported(f.Name) {
			return fmt.Errorf("%s: field %q is not exported (start the name with a letter)", where, f.Name)
		}

		key := strings.ToUpper(f.Name)
		if prev, ok := seen[key]; ok {
			return fmt.Errorf("%s: fields %q and %q collide (rename one with as)", where, prev, f.Name)
		}
		seen[key] = f.Name
	}

	return nil
}

// checkGroupCycles rejects a group that contains itself, directly or through other groups:
// its struct would have infinite size.
// It expects resolved references.
func checkGroupCycles(groups []Group) error {
	const (
		unvisited = iota
		visiting
		done
	)

	state := map[string]int{}

	var visit func(g *Group, path []string) error
	visit = func(g *Group, path []string) error {
		switch state[g.Name] {
		case visiting:
			return fmt.Errorf("group %s contains itself: %s", g.Name, strings.Join(append(path, g.Name), " -> "))
		case done:
			return nil
		}

		state[g.Name] = visiting

		var refs []*Group
		collectRefs(g.Structure, &refs)

		for _, ref := range refs {
			if err := visit(ref, append(path, g.Name)); err != nil {
				return err
			}
		}

		state[g.Name] = done

		return nil
	}

	for i := range groups {
		if err := visit(&groups[i], nil); err != nil {
			return err
		}
	}

	return nil
}

// collectRefs appends the group of every resolved group reference in n to refs.
func collectRefs(n *StructureNode, refs *[]*Group) {
	if n == nil {
		return
	}

	if n.def != nil {
		*refs = append(*refs, n.def)
	}

	for i := range n.Items {
		collectRefs(&n.Items[i], refs)
	}

	collectRefs(n.Of, refs)
}

// checkGroupNames rejects a group name that would not compile as the name of its struct,
// or that another generated gem identifier already takes.
//
// A name must be an exported Go identifier.
// It must not be the name of an item, whose named type items.go may declare,
// nor any value constant, builder, decoder, or result type.
// The group's unexported decode function, decode<Name>,
// must not be a top-level name of the decode support file.
func checkGroupNames(groups []Group, files []MessageFile, items map[string]Item) error {
	taken := generatedNames(files, items)

	support, err := supportNames()
	if err != nil {
		return err
	}

	for _, g := range groups {
		switch {
		case !token.IsIdentifier(g.Name) || !token.IsExported(g.Name):
			return fmt.Errorf("group %q: name must be an exported Go identifier", g.Name)
		case taken[g.Name] != "":
			return fmt.Errorf("group %s: name is already %s", g.Name, taken[g.Name])
		case support["decode"+g.Name]:
			return fmt.Errorf("group %s: its decode function decode%s would collide with a decode support helper", g.Name, g.Name)
		}
	}

	return nil
}

// generatedNames returns every package-level name the generated gem files declare outside groups.go,
// each mapped to what it names.
func generatedNames(files []MessageFile, items map[string]Item) map[string]string {
	taken := map[string]string{}

	for _, name := range slices.Sorted(maps.Keys(items)) {
		taken[name] = "the item " + name

		for _, v := range items[name].Values {
			taken[name+v.Name] = "a value of item " + name
		}
	}

	for _, f := range files {
		for _, m := range f.Messages {
			for _, b := range m.Bodies {
				taken[builderName(f.Stream, m.Function, b)] = fmt.Sprintf("the S%dF%d builder", f.Stream, m.Function)

				if b.Variant != "" || b.Structure == nil {
					continue
				}

				name := callName(f.Stream, m.Function, b.Actor)
				taken["Decode"+name] = fmt.Sprintf("the S%dF%d decoder", f.Stream, m.Function)
				taken[ResultTypeName(f.Stream, m.Function, b.Actor)] = fmt.Sprintf("the S%dF%d result type", f.Stream, m.Function)
			}
		}
	}

	return taken
}

// supportNames returns the top-level names the decode support file declares.
func supportNames() (map[string]bool, error) {
	f, err := parser.ParseFile(token.NewFileSet(), "decode.go", decodeSupportSource, 0)
	if err != nil {
		return nil, fmt.Errorf("parse decode support source: %w", err)
	}

	names := map[string]bool{}

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil {
				names[d.Name.Name] = true
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					names[s.Name.Name] = true
				case *ast.ValueSpec:
					for _, id := range s.Names {
						names[id.Name] = true
					}
				default: // an import declares no name of its own
				}
			}
		default:
		}
	}

	return names, nil
}

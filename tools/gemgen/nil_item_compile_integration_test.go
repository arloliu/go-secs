//go:build integration

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// nilItemsYAML is the item dictionary the nil-item fixture draws on:
// an open item, a fixed string, and a fixed byte array.
const nilItemsYAML = `
SVID:
  formats: [U1, U2, U4, U8]
  binding: open
MDLN:
  formats: [A]
  binding: fixed
  goType: string
MHEAD:
  formats: [B]
  binding: fixed
  goType: "[10]byte"
`

// nilGroupsYAML declares a group with a secs2.Item field and a group with a repeat of them.
const nilGroupsYAML = `
groups:
  - name: Pair
    description: One pair of a model and a status variable.
    structure: {type: list, items: [{item: MDLN}, {item: SVID}]}
  - name: Bag
    description: One bag of status variables.
    structure: {type: list, items: [{item: MDLN}, {type: list, repeat: svids, of: {item: SVID}}]}
`

// nilS1YAML holds a message whose whole body is opaque,
// and one whose body holds a secs2.Item in each other shape a builder takes one in.
const nilS1YAML = `
stream: 1
messages:
  - function: 1
    name: Opaque Sample
    direction: equipment-to-host
    description: Carries an opaque body.
    exception: None
    source: e5
    bodies:
      - actor: both
        replyExpected: false
        structure: {type: opaque}
  - function: 2
    name: Item Sample
    direction: equipment-to-host
    description: Carries items in every shape.
    exception: None
    source: e5
    bodies:
      - actor: both
        replyExpected: false
        structure:
          type: list
          items:
            - {item: SVID}
            - {type: opaque, as: extra}
            - {type: list, repeat: svids, of: {item: SVID}}
            - {type: list, repeat: mheads, of: {item: MHEAD}}
            - {type: list, repeat: rows, of: {type: list, repeat: cells, of: {item: SVID}}}
            - {type: list, repeat: pairs, of: {group: Pair}}
            - {type: list, repeat: bags, of: {group: Bag}}
`

// nilItemExtraTest sends a nil in each shape and checks the error names where it was.
// It is written into the generated package after its package clause is rewritten.
const nilItemExtraTest = `package PKG

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/arloliu/go-secs/v2/secs2"
)

// sample builds an S1F2 with no nil, then lets change put one in.
func sample(change func(*s1f2Args)) secs2.Item {
	a := s1f2Args{
		svid:   secs2.U4(1),
		extra:  secs2.A("x"),
		svids:  []secs2.Item{secs2.U4(2), secs2.U4(3)},
		mheads: []secs2.Item{secs2.B(1, 2), secs2.B(3)},
		rows:   [][]secs2.Item{{secs2.U4(4)}, {secs2.U4(5), secs2.U4(6)}},
		pairs:  []Pair{{MDLN: "m", SVID: secs2.U4(7)}},
		bags:   []Bag{{MDLN: "b", SVIDS: []secs2.Item{secs2.U4(8), secs2.U4(9)}}},
	}
	if change != nil {
		change(&a)
	}

	return S1F2(a.svid, a.extra, a.svids, a.mheads, a.rows, a.pairs, a.bags...).Item()
}

type s1f2Args struct {
	svid, extra   secs2.Item
	svids, mheads []secs2.Item
	rows          [][]secs2.Item
	pairs         []Pair
	bags          []Bag
}

func TestNilItemNamesItsPlace(t *testing.T) {
	tests := []struct {
		name  string
		item  secs2.Item
		where string
	}{
		{"opaque body", S1F1(nil).Item(), "S1F1 body"},
		{"open leaf", sample(func(a *s1f2Args) { a.svid = nil }), "S1F2 SVID"},
		{"opaque in a list", sample(func(a *s1f2Args) { a.extra = (*secs2.ListItem)(nil) }), "S1F2 extra"},
		{"open-leaf repeat", sample(func(a *s1f2Args) { a.svids[1] = nil }), "S1F2 SVIDS[1]"},
		{"fixed byte-array repeat", sample(func(a *s1f2Args) { a.mheads[0] = nil }), "S1F2 MHEADS[0]"},
		{"repeat of repeats", sample(func(a *s1f2Args) { a.rows[1][0] = nil }), "S1F2 ROWS[1][0]"},
		{"group field", sample(func(a *s1f2Args) { a.pairs[0].SVID = nil }), "Pair.SVID"},
		{"group repeat element", sample(func(a *s1f2Args) { a.bags[0].SVIDS[1] = nil }), "Bag.SVIDS[1]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.item.Error()
			if !errors.Is(err, ErrNilItem) {
				t.Fatalf("Error: got %v, want an error wrapping ErrNilItem", err)
			}

			if want := "gem: nil item at " + tt.where; !strings.Contains(err.Error(), want) {
				t.Errorf("Error: got %q, want it to contain %q", err, want)
			}
		})
	}
}

func TestNoNilSendsTheFullBody(t *testing.T) {
	got := sample(nil)
	if err := got.Error(); err != nil {
		t.Fatalf("Error: %v", err)
	}

	want := secs2.L(secs2.U4(1), secs2.A("x"),
		secs2.L(secs2.U4(2), secs2.U4(3)),
		secs2.L(secs2.B(1, 2), secs2.B(3)),
		secs2.L(secs2.L(secs2.U4(4)), secs2.L(secs2.U4(5), secs2.U4(6))),
		secs2.L(secs2.L(secs2.A("m"), secs2.U4(7))),
		secs2.L(secs2.L(secs2.A("b"), secs2.L(secs2.U4(8), secs2.U4(9)))))
	if !bytes.Equal(got.ToBytes(), want.ToBytes()) {
		t.Errorf("S1F2 body: got %x, want %x", got.ToBytes(), want.ToBytes())
	}
}
`

// TestNilItemsCompileAndRunAgainstRealSecs2 is the real-compile guard for nil items.
//
// It runs the whole generator over a YAML fixture that takes a secs2.Item in every shape a builder or a group can:
// an opaque body, an open leaf, an opaque node in a list, a repeat of open leaves,
// a repeat of a fixed byte array, a repeat of repeats, a group's field, and a group's repeat.
// The generated code and its tests must build, vet, and pass,
// and a hand-written test sends a nil in each shape and checks the error names where it was.
//
// It is gated behind the `integration` build tag
// because it shells out to the go toolchain and reaches across module boundaries;
// run it with `go test -tags integration ./...`.
func TestNilItemsCompileAndRunAgainstRealSecs2(t *testing.T) {
	src := t.TempDir()
	itemsPath := filepath.Join(src, "items.yaml")
	require.NoError(t, os.WriteFile(itemsPath, []byte(nilItemsYAML), 0o600))

	groupsPath := filepath.Join(src, "groups.yaml")
	require.NoError(t, os.WriteFile(groupsPath, []byte(nilGroupsYAML), 0o600))

	msgsDir := filepath.Join(src, "messages")
	require.NoError(t, os.MkdirAll(msgsDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(msgsDir, "s1.yaml"), []byte(nilS1YAML), 0o600))

	// Locate the root go-secs module (two levels up from tools/gemgen),
	// which provides the real secs2 package the generated code imports.
	wd, err := os.Getwd()
	require.NoError(t, err)
	rootDir := filepath.Clean(filepath.Join(wd, "..", ".."))
	rootMod, err := os.ReadFile(filepath.Join(rootDir, "go.mod"))
	require.NoError(t, err, "expected root go.mod at %s", rootDir)
	require.True(t, strings.Contains(string(rootMod), "module github.com/arloliu/go-secs/v2"),
		"root module at %s is not the go-secs root module", rootDir)

	// Generate into a throwaway package directory inside the root module,
	// so the go tool resolves the real secs2 import.
	checkDir, err := os.MkdirTemp(rootDir, "gemnilcheck-")
	require.NoError(t, err)
	defer os.RemoveAll(checkDir)

	require.NoError(t, run(itemsPath, groupsPath, msgsDir, checkDir))

	s1Src, err := os.ReadFile(filepath.Join(checkDir, "s1.go"))
	require.NoError(t, err)
	require.Contains(t, string(s1Src), `presentEach(mheads, "S1F2 MHEADS")`)
	require.Contains(t, string(s1Src), "func(i0 int, v []secs2.Item) secs2.Item")

	// Rewrite `package gem` and `package gem_test` to one unique name,
	// so the generated tests become internal tests of the generated builders and decoders.
	pkg := strings.ReplaceAll(filepath.Base(checkDir), "-", "")

	entries, err := os.ReadDir(checkDir)
	require.NoError(t, err)

	for _, entry := range entries {
		path := filepath.Join(checkDir, entry.Name())

		data, err := os.ReadFile(path)
		require.NoError(t, err)

		out := strings.Replace(string(data), "package gem_test\n", "package "+pkg+"\n", 1)
		out = strings.Replace(out, "package gem\n", "package "+pkg+"\n", 1)
		require.NotEqualf(t, string(data), out, "%s did not declare package gem or gem_test", entry.Name())

		out = strings.ReplaceAll(out, "\t\"github.com/arloliu/go-secs/v2/gem\"\n", "")
		out = strings.ReplaceAll(out, "gem.", "")

		require.NoError(t, os.WriteFile(path, []byte(out), 0o600))
	}

	plantTestSupport(t, rootDir, checkDir, pkg)

	extra := strings.Replace(nilItemExtraTest, "package PKG\n", "package "+pkg+"\n", 1)
	require.NoError(t, os.WriteFile(filepath.Join(checkDir, "nil_item_extra_test.go"), []byte(extra), 0o600))

	target := "./" + filepath.Base(checkDir) + "/"
	for _, args := range [][]string{{"build", target}, {"vet", target}, {"test", "-count=1", target}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = rootDir
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "go %s failed on the generated nil-item fixture:\n%s", args[0], string(out))
	}
}

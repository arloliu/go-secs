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

// groupsItemsYAML is the item dictionary the group fixture draws on.
const groupsItemsYAML = `
MDLN:
  formats: [A]
  binding: fixed
  goType: string
SOFTREV:
  formats: [A]
  binding: fixed
  goType: string
SVID:
  formats: [U1, U2, U4, U8]
  binding: open
TSIP:
  formats: [B]
  binding: fixed
  goType: byte
`

// groupsS1YAML repeats groupTestGroups' Holder, a repeat of a repeat of Pair, and an optional Band,
// as groupTestStructure does.
const groupsS1YAML = `
stream: 1
messages:
  - function: 2
    name: Group Sample
    mnemonic: GS
    direction: equipment-to-host
    description: Carries groups.
    exception: None
    source: e5
    bodies:
      - actor: both
        replyExpected: false
        structure:` + groupTestStructure + "\n"

// groupsExtraTest holds the cases the generated tests do not reach:
// nil optional groups sent as L[0] and decoded back as nil,
// several elements of each repeat,
// and malformed elements, whose errors must name their path down from the body.
// It is written into the generated package after its package clause is rewritten.
const groupsExtraTest = `package PKG

import (
	"bytes"
	"strings"
	"testing"

	"github.com/arloliu/go-secs/v2/secs2"
)

// groupsBody builds an S1F2 body around the given holders, rows, and bands lists,
// then round-trips it through wire bytes, as a peer's message would arrive.
func groupsBody(t *testing.T, holders, rows, bands secs2.Item) secs2.Item {
	t.Helper()

	decoded, err := secs2.Decode(secs2.L(secs2.A("model"), holders, rows, bands).ToBytes())
	if err != nil {
		t.Fatalf("secs2.Decode: %v", err)
	}

	return decoded
}

// holderItem is the wire form of a Holder with no TSIP, no band, the given pairs, and no values.
func holderItem(pairs ...secs2.Item) secs2.Item {
	return secs2.L(secs2.B(), secs2.L(), secs2.L(secs2.A("m"), secs2.U4(9)), secs2.L(pairs...), secs2.L())
}

func TestGroupBuilderSendsNilAsEmptyList(t *testing.T) {
	msg := S1F2("model", []Holder{{MDLN: "m", EXTRA: secs2.U4(9)}}, [][]Pair{{}, {{MDLN: "a", SVID: secs2.U2(1)}}},
		nil, &Band{SVID: secs2.U1(2), SOFTREV: "r"})

	want := secs2.L(secs2.A("model"),
		secs2.L(holderItem()),
		secs2.L(secs2.L(), secs2.L(secs2.L(secs2.A("a"), secs2.U2(1)))),
		secs2.L(secs2.L(), secs2.L(secs2.U1(2), secs2.A("r"))))
	if !bytes.Equal(msg.Item().ToBytes(), want.ToBytes()) {
		t.Errorf("S1F2 body: got %x, want %x", msg.Item().ToBytes(), want.ToBytes())
	}
}

func TestGroupDecodesEmptyListAsNil(t *testing.T) {
	got, err := DecodeS1F2(groupsBody(t,
		secs2.L(holderItem(secs2.L(secs2.A("p"), secs2.U4(3)))),
		secs2.L(secs2.L()),
		secs2.L(secs2.L(), secs2.L(secs2.U1(2), secs2.A("r")))))
	if err != nil {
		t.Fatalf("DecodeS1F2: %v", err)
	}

	assertDecoded(t, got, S1F2Reply{
		MDLN:    "model",
		HOLDERS: []Holder{{MDLN: "m", EXTRA: secs2.U4(9), PAIRS: []Pair{{MDLN: "p", SVID: secs2.U4(3)}}, TSIPS: []byte{}}},
		ROWS:    [][]Pair{{}},
		BANDS:   []*Band{nil, {SVID: secs2.U1(2), SOFTREV: "r"}},
	})
}

func TestGroupOptionalLeafNilIsZeroLength(t *testing.T) {
	msg := S1F2("model", []Holder{{MDLN: "m", EXTRA: secs2.U4(9)}, {TSIP: new(byte(0)), MDLN: "m", EXTRA: secs2.U4(9)}}, nil)

	holders, err := msg.Item().Get(1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	zero := secs2.L(secs2.B(0), secs2.L(), secs2.L(secs2.A("m"), secs2.U4(9)), secs2.L(), secs2.L())
	if want := secs2.L(holderItem(), zero); !bytes.Equal(holders.ToBytes(), want.ToBytes()) {
		t.Errorf("HOLDERS: got %x, want %x", holders.ToBytes(), want.ToBytes())
	}

	got, err := DecodeS1F2(groupsBody(t, holders, secs2.L(), secs2.L()))
	if err != nil {
		t.Fatalf("DecodeS1F2: %v", err)
	}

	if got.HOLDERS[0].TSIP != nil {
		t.Errorf("HOLDERS[0].TSIP: got %v, want nil", *got.HOLDERS[0].TSIP)
	}

	if got.HOLDERS[1].TSIP == nil || *got.HOLDERS[1].TSIP != 0 {
		t.Errorf("HOLDERS[1].TSIP: got %v, want a non-nil 0", got.HOLDERS[1].TSIP)
	}
}

func TestGroupRejectsMalformedElements(t *testing.T) {
	band := secs2.L(secs2.U1(2), secs2.A("r"))

	tests := []struct {
		name                 string
		holders, rows, bands secs2.Item
		wantErr              string
	}{
		{"element not a list", secs2.L(secs2.A("x")), secs2.L(), secs2.L(), "HOLDERS[0]"},
		{"element too short", secs2.L(secs2.L(secs2.B(7))), secs2.L(), secs2.L(), "HOLDERS[0]"},
		{"pair in a holder", secs2.L(holderItem(secs2.L(secs2.A("p")))), secs2.L(), secs2.L(), "HOLDERS[0].PAIRS[0]"},
		{"sub-list in a holder", secs2.L(secs2.L(secs2.B(7), secs2.L(), secs2.L(secs2.A("m")), secs2.L(), secs2.L())), secs2.L(), secs2.L(), "HOLDERS[0][2]"},
		{"band partly sent", secs2.L(secs2.L(secs2.B(7), secs2.L(secs2.U1(2)), secs2.L(secs2.A("m"), secs2.U4(9)), secs2.L(), secs2.L())), secs2.L(), secs2.L(), "HOLDERS[0].BAND"},
		{"pair in a row", secs2.L(), secs2.L(secs2.L(), secs2.L(secs2.L(secs2.A("p"), secs2.U4(1)), secs2.U4(1))), secs2.L(), "ROWS[1][1]"},
		{"optional element partly sent", secs2.L(), secs2.L(), secs2.L(band, secs2.L(secs2.U1(2))), "BANDS[1]"},
		{"optional element over-full", secs2.L(), secs2.L(), secs2.L(secs2.L(secs2.U1(2), secs2.A("r"), secs2.A("x"))), "BANDS[0]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeS1F2(groupsBody(t, tt.holders, tt.rows, tt.bands))
			if err == nil {
				t.Fatal("DecodeS1F2: got nil error, want one")
			}

			if !strings.Contains(err.Error(), "gem: "+tt.wantErr+":") {
				t.Errorf("DecodeS1F2: got %q, want it to name %s", err, tt.wantErr)
			}
		})
	}
}
`

// TestGroupsCompileAndRunAgainstRealSecs2 is the real-compile guard for groups.
//
// It runs the whole generator over a YAML fixture whose groups hold every kind of field
// (a leaf, an optional group, a flattened sub-list with an opaque node, a repeat of a group, a repeat of values),
// and whose message repeats a group, a repeat of a group, and an optional group.
// The generated groups.go, builders, decoders, and their tests must build, vet, and pass.
// A hand-written test adds the cases the generated ones cannot express:
// nil optional groups both ways, empty and several elements,
// and malformed elements, which must fail naming their path from the body.
//
// It is gated behind the `integration` build tag
// because it shells out to the go toolchain and reaches across module boundaries;
// run it with `go test -tags integration ./...`.
func TestGroupsCompileAndRunAgainstRealSecs2(t *testing.T) {
	src := t.TempDir()
	itemsPath := filepath.Join(src, "items.yaml")
	require.NoError(t, os.WriteFile(itemsPath, []byte(groupsItemsYAML), 0o600))

	groupsPath := filepath.Join(src, "groups.yaml")
	require.NoError(t, os.WriteFile(groupsPath, []byte(groupTestGroups), 0o600))

	msgsDir := filepath.Join(src, "messages")
	require.NoError(t, os.MkdirAll(msgsDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(msgsDir, "s1.yaml"), []byte(groupsS1YAML), 0o600))

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
	checkDir, err := os.MkdirTemp(rootDir, "gemgroupscheck-")
	require.NoError(t, err)
	defer os.RemoveAll(checkDir)

	require.NoError(t, run(itemsPath, groupsPath, msgsDir, checkDir))

	groupsSrc, err := os.ReadFile(filepath.Join(checkDir, "groups.go"))
	require.NoError(t, err)
	require.Contains(t, string(groupsSrc), "func decodeHolder(c secs2.Cursor, field string, cfg *decodeConfig) (Holder, error) {")

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

	extra := strings.Replace(groupsExtraTest, "package PKG\n", "package "+pkg+"\n", 1)
	require.NoError(t, os.WriteFile(filepath.Join(checkDir, "groups_extra_test.go"), []byte(extra), 0o600))

	target := "./" + filepath.Base(checkDir) + "/"
	for _, args := range [][]string{{"build", target}, {"vet", target}, {"test", "-count=1", target}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = rootDir
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "go %s failed on the generated group fixture:\n%s", args[0], string(out))
	}
}

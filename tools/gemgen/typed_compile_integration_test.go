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

// typedItemsYAML is the item dictionary the typed: fixture draws on:
// one element item of each kind a typed repeat decodes differently,
// and the ordinary items around them.
const typedItemsYAML = `
FNLOC:
  formats: [U2]
  binding: fixed
  goType: uint16
STATUS:
  formats: [U1]
  binding: fixed
  goType: uint8
  values:
    - {name: Ok, value: 0}
    - {name: Failed, value: 1}
CARRIERACTION:
  formats: [A]
  binding: fixed
  goType: string
  values:
    - {name: ProceedWithCarrier, value: ProceedWithCarrier}
ACKA:
  formats: [BOOLEAN]
  binding: fixed
  goType: bool
MDLN:
  formats: [A]
  binding: fixed
  goType: string
SVID:
  formats: [U1, U2, U4, U8]
  binding: open
`

// typedS18YAML has a typed repeat of a plain number, of a numeric enum, of a string enum, and of a boolean,
// an untyped repeat between them, and a short-form variant that sends one typed repeat.
// S18F3's whole body is one typed repeat.
const typedS18YAML = `
stream: 18
messages:
  - function: 1
    name: Typed Sample
    mnemonic: TS
    direction: equipment-to-host
    description: Carries lists of typed values.
    exception: None
    source: e5
    bodies:
      - actor: both
        replyExpected: false
        structure:
          type: list
          minItems: 2
          maxItems: 6
          items:
            - {item: MDLN}
            - {type: list, repeat: locs, typed: true, of: {item: FNLOC}}
            - {type: list, repeat: statuslist, typed: true, of: {item: STATUS}}
            - {type: list, repeat: svids, of: {item: SVID}}
            - {type: list, repeat: actions, typed: true, of: {item: CARRIERACTION}}
            - {type: list, repeat: flags, typed: true, of: {item: ACKA}}
      - actor: both
        replyExpected: false
        variant: Short
        structure:
          type: list
          items:
            - {item: MDLN}
            - {type: list, repeat: locs, typed: true, of: {item: FNLOC}}
  - function: 3
    name: Typed Body Sample
    mnemonic: TBS
    direction: host-to-equipment
    description: Carries one list of typed values.
    exception: None
    source: e5
    bodies:
      - actor: both
        replyExpected: false
        structure: {type: list, repeat: statuslist, typed: true, of: {item: STATUS}}
`

// typedExtraTest holds the decoder cases the generated tests do not reach:
// several elements, an empty list, and elements of the wrong shape, which must fail naming their index.
// It is written into the generated package after its package clause is rewritten.
const typedExtraTest = `package PKG

import (
	"strings"
	"testing"

	"github.com/arloliu/go-secs/v2/secs2"
)

// typedBody builds an S18F1 body around the given locs, statuslist, and actions lists,
// then round-trips it through wire bytes, as a peer's message would arrive.
func typedBody(t *testing.T, locs, statuslist, actions secs2.Item) secs2.Item {
	t.Helper()

	body := secs2.L(secs2.A("model"), locs, statuslist, secs2.L(secs2.U4(7)), actions, secs2.L(secs2.BOOLEAN(true), secs2.BOOLEAN(false)))

	decoded, err := secs2.Decode(body.ToBytes())
	if err != nil {
		t.Fatalf("secs2.Decode: %v", err)
	}

	return decoded
}

func TestTypedRepeatDecodesEveryElement(t *testing.T) {
	got, err := DecodeS18F1(typedBody(t,
		secs2.L(secs2.U2(1), secs2.U4(65535), secs2.U1(3)),
		secs2.L(secs2.U1(0), secs2.U1(1)),
		secs2.L(secs2.A("ProceedWithCarrier"), secs2.A("Other"))))
	if err != nil {
		t.Fatalf("DecodeS18F1: %v", err)
	}

	if len(got.LOCS) != 3 || got.LOCS[0] != 1 || got.LOCS[1] != 65535 || got.LOCS[2] != 3 {
		t.Errorf("LOCS: got %v, want [1 65535 3]", got.LOCS)
	}

	if len(got.STATUSLIST) != 2 || got.STATUSLIST[0] != STATUSOk || got.STATUSLIST[1] != STATUSFailed {
		t.Errorf("STATUSLIST: got %v, want [Ok Failed]", got.STATUSLIST)
	}

	if len(got.ACTIONS) != 2 || got.ACTIONS[0] != CARRIERACTIONProceedWithCarrier || got.ACTIONS[1] != "Other" {
		t.Errorf("ACTIONS: got %v", got.ACTIONS)
	}

	if len(got.FLAGS) != 2 || !got.FLAGS[0] || got.FLAGS[1] {
		t.Errorf("FLAGS: got %v, want [true false]", got.FLAGS)
	}

	if len(got.SVIDS) != 1 {
		t.Errorf("SVIDS: got %d elements, want 1", len(got.SVIDS))
	}
}

func TestTypedRepeatDecodesEmptyList(t *testing.T) {
	got, err := DecodeS18F1(typedBody(t, secs2.L(), secs2.L(), secs2.L()))
	if err != nil {
		t.Fatalf("DecodeS18F1: %v", err)
	}

	if len(got.LOCS) != 0 || len(got.STATUSLIST) != 0 || len(got.ACTIONS) != 0 {
		t.Errorf("got %v %v %v, want all empty", got.LOCS, got.STATUSLIST, got.ACTIONS)
	}
}

func TestTypedRepeatRejectsWrongElements(t *testing.T) {
	tests := []struct {
		name                     string
		locs, statuslist, actions secs2.Item
		wantErr                  string
	}{
		{"wrong format at index 1", secs2.L(secs2.U2(1), secs2.A("x")), secs2.L(), secs2.L(), "LOCS[1]"},
		{"signed element", secs2.L(secs2.I2(1)), secs2.L(), secs2.L(), "LOCS[0]"},
		{"two values in one element", secs2.L(secs2.U2(1), secs2.U2(1), secs2.U2(2, 3)), secs2.L(), secs2.L(), "LOCS[2]"},
		{"enum element overflows", secs2.L(), secs2.L(secs2.U1(1), secs2.U2(300)), secs2.L(), "STATUSLIST[1]"},
		{"string enum element of another format", secs2.L(), secs2.L(), secs2.L(secs2.B(1)), "ACTIONS[0]"},
		{"not a list", secs2.U2(1), secs2.L(), secs2.L(), "LOCS"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeS18F1(typedBody(t, tt.locs, tt.statuslist, tt.actions))
			if err == nil {
				t.Fatal("DecodeS18F1: got nil error, want one")
			}

			if !strings.Contains(err.Error(), "gem: "+tt.wantErr+":") {
				t.Errorf("DecodeS18F1: got %q, want it to name %s", err, tt.wantErr)
			}
		})
	}
}
`

// TestTypedRepeatsCompileAndRunAgainstRealSecs2 is the real-compile guard for typed: repeats.
//
// It runs the whole generator over a YAML fixture,
// so typed: is read by the loader, accepted by validation, and honored by every walker.
// The generated builder tests and round trips send each typed repeat one element of the element item's own format
// and expect it back as a value,
// and the variant's test reads its typed repeat through the main decoder.
// A hand-written test adds the cases the generated ones cannot express:
// several elements, an empty list,
// and elements of the wrong format, width, or size, which must fail naming the element's index.
//
// It is gated behind the `integration` build tag
// because it shells out to the go toolchain and reaches across module boundaries;
// run it with `go test -tags integration ./...`.
func TestTypedRepeatsCompileAndRunAgainstRealSecs2(t *testing.T) {
	src := t.TempDir()
	itemsPath := filepath.Join(src, "items.yaml")
	require.NoError(t, os.WriteFile(itemsPath, []byte(typedItemsYAML), 0o600))

	msgsDir := filepath.Join(src, "messages")
	require.NoError(t, os.MkdirAll(msgsDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(msgsDir, "s18.yaml"), []byte(typedS18YAML), 0o600))

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
	checkDir, err := os.MkdirTemp(rootDir, "gemtypedcheck-")
	require.NoError(t, err)
	defer os.RemoveAll(checkDir)

	require.NoError(t, run(itemsPath, msgsDir, checkDir))

	builders, err := os.ReadFile(filepath.Join(checkDir, "s18.go"))
	require.NoError(t, err)
	require.Contains(t, string(builders),
		"func S18F1(mdln string, locs []secs2.Item, statuslist []secs2.Item, svids []secs2.Item, actions []secs2.Item, flags ...secs2.Item)")

	decoders, err := os.ReadFile(filepath.Join(checkDir, "s18_decode.go"))
	require.NoError(t, err)
	require.Contains(t, string(decoders), `out.STATUSLIST, err = decodeEach(root.At(2), "STATUSLIST", decodeUintAs[STATUS])`)
	require.Contains(t, string(decoders), `out.STATUSLIST, err = decodeEach(root, "STATUSLIST", decodeUintAs[STATUS])`)

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

	plantFuzzHarness(t, rootDir, checkDir, pkg)

	extra := strings.Replace(typedExtraTest, "package PKG\n", "package "+pkg+"\n", 1)
	require.NoError(t, os.WriteFile(filepath.Join(checkDir, "typed_extra_test.go"), []byte(extra), 0o600))

	target := "./" + filepath.Base(checkDir) + "/"
	for _, args := range [][]string{{"build", target}, {"vet", target}, {"test", "-count=1", target}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = rootDir
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "go %s failed on the generated typed: fixture:\n%s", args[0], string(out))
	}
}

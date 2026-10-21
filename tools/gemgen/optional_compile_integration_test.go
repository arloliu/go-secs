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

// optionalItemsYAML is the item dictionary the optional: fixture draws on:
// one optional item of each format family a decoder tells apart,
// an enum-typed one, and the ordinary items around them.
const optionalItemsYAML = `
FNLOC:
  formats: [U2]
  binding: fixed
  goType: uint16
ORLOC:
  formats: [B]
  binding: fixed
  goType: byte
TRAUTOD:
  formats: [BOOLEAN]
  binding: fixed
  goType: bool
TIMESTAMP:
  formats: [A]
  binding: fixed
  goType: string
VERIFYRSPSTAT:
  formats: [U1]
  binding: fixed
  goType: uint8
  values:
    - {name: Success, value: 0}
MDLN:
  formats: [A]
  binding: fixed
  goType: string
OBJSPEC:
  formats: [A]
  binding: fixed
  goType: string
SVID:
  formats: [U1, U2, U4, U8]
  binding: open
`

// optionalS12YAML places optional items before an ordinary leaf, before a repeat,
// inside a bounded list's optional tail, and last in the body.
// A short-form variant sends FNLOC as a plain leaf and omits the optional tail,
// and S12F3's whole body is one optional item.
const optionalS12YAML = `
stream: 12
messages:
  - function: 1
    name: Optional Sample
    mnemonic: OS
    direction: equipment-to-host
    description: Carries items E5 lets the sender send zero-length.
    exception: None
    source: e5
    bodies:
      - actor: both
        replyExpected: false
        structure:
          type: list
          items:
            - {item: FNLOC, optional: true}
            - {item: MDLN}
            - {item: ORLOC, optional: true}
            - {type: list, repeat: svids, of: {item: SVID}}
            - type: list
              minItems: 1
              maxItems: 3
              items:
                - {item: OBJSPEC}
                - {item: TRAUTOD, optional: true}
                - {item: VERIFYRSPSTAT, optional: true}
            - {item: TIMESTAMP, optional: true}
      - actor: both
        replyExpected: false
        variant: Short
        structure:
          type: list
          items:
            - {item: FNLOC}
            - {item: MDLN}
            - {item: ORLOC, optional: true}
            - {type: list, repeat: svids, of: {item: SVID}}
            - {type: list, items: [{item: OBJSPEC}]}
            - {item: TIMESTAMP, optional: true}
  - function: 3
    name: Optional Body Sample
    mnemonic: OBS
    direction: host-to-equipment
    description: Carries one item E5 lets the sender send zero-length.
    exception: None
    source: e5
    bodies:
      - actor: both
        replyExpected: false
        structure: {item: FNLOC, optional: true}
`

// optionalExtraTest holds the decoder cases the generated tests do not reach:
// zero-length and multi-value items of the wrong shape, and real ASCII values.
// It is written into the generated package after its package clause is rewritten.
const optionalExtraTest = `package PKG

import (
	"testing"

	"github.com/arloliu/go-secs/v2/secs2"
)

// optionalBody builds an S12F1 body around the given FNLOC, ORLOC, and TIMESTAMP items,
// then round-trips it through wire bytes, as a peer's message would arrive.
func optionalBody(t *testing.T, fnloc, orloc, timestamp secs2.Item) secs2.Item {
	t.Helper()

	body := secs2.L(fnloc, secs2.A("model"), orloc, secs2.L(secs2.U4(7)),
		secs2.L(secs2.A("spec"), secs2.BOOLEAN(true), secs2.U1(0)), timestamp)

	decoded, err := secs2.Decode(body.ToBytes())
	if err != nil {
		t.Fatalf("secs2.Decode: %v", err)
	}

	return decoded
}

func TestOptionalRejectsWrongShapes(t *testing.T) {
	tests := []struct {
		name                     string
		fnloc, orloc, timestamp  secs2.Item
	}{
		{"wrong-format empty U2", secs2.A(""), secs2.B(1), secs2.A("")},
		{"two-value U2", secs2.U2(1, 2), secs2.B(1), secs2.A("")},
		{"wrong-format non-empty U2", secs2.I2(1), secs2.B(1), secs2.A("")},
		{"wrong-format empty B", secs2.U2(1), secs2.U1(), secs2.A("")},
		{"two-value B", secs2.U2(1), secs2.B(1, 2), secs2.A("")},
		{"wrong-format empty A", secs2.U2(1), secs2.B(1), secs2.B()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := DecodeS12F1(optionalBody(t, tt.fnloc, tt.orloc, tt.timestamp)); err == nil {
				t.Fatal("DecodeS12F1: got nil error, want one")
			}
		})
	}
}

func TestOptionalAcceptsZeroLengthOfAnyWidth(t *testing.T) {
	got, err := DecodeS12F1(optionalBody(t, secs2.U4(), secs2.B(), secs2.A("")))
	if err != nil {
		t.Fatalf("DecodeS12F1: %v", err)
	}

	if got.FNLOCPresent || got.ORLOCPresent || got.TIMESTAMPPresent {
		t.Errorf("presence: got %v %v %v, want all false", got.FNLOCPresent, got.ORLOCPresent, got.TIMESTAMPPresent)
	}

	if !got.TRAUTODPresent || !got.VERIFYRSPSTATPresent {
		t.Errorf("tail presence: got %v %v, want both true", got.TRAUTODPresent, got.VERIFYRSPSTATPresent)
	}
}

func TestOptionalASCII(t *testing.T) {
	tests := []struct {
		name        string
		timestamp   string
		wantPresent bool
	}{
		{"empty", "", false},
		{"16-character timestamp", "2026102012345678", true},
		{"one character", "x", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeS12F1(optionalBody(t, secs2.U2(1), secs2.B(1), secs2.A(tt.timestamp)))
			if err != nil {
				t.Fatalf("DecodeS12F1: %v", err)
			}

			if got.TIMESTAMPPresent != tt.wantPresent || got.TIMESTAMP != tt.timestamp {
				t.Errorf("TIMESTAMP: got %q (present %v), want %q (present %v)",
					got.TIMESTAMP, got.TIMESTAMPPresent, tt.timestamp, tt.wantPresent)
			}

			if !got.FNLOCPresent || got.FNLOC != 1 || !got.ORLOCPresent || got.ORLOC != 1 {
				t.Errorf("FNLOC, ORLOC: got %v %v %v %v, want 1 and present", got.FNLOC, got.FNLOCPresent, got.ORLOC, got.ORLOCPresent)
			}
		})
	}
}
`

// TestOptionalLeavesCompileAndRunAgainstRealSecs2 is the real-compile guard for optional: leaves.
//
// It runs the whole generator over a YAML fixture,
// so optional: is read by the loader, accepted by validation, and honored by every walker.
// The generated round-trip tests expect every presence field true,
// the generated absent tests send each optional item zero-length and expect it absent,
// and the variant's test expects the main decoder's presence fields to follow what the variant sends.
// A hand-written test adds the cases the generated ones cannot express:
// wrong-format and multi-value items, which must still fail,
// and ASCII values, where any non-empty string is present.
//
// It is gated behind the `integration` build tag
// because it shells out to the go toolchain and reaches across module boundaries;
// run it with `go test -tags integration ./...`.
func TestOptionalLeavesCompileAndRunAgainstRealSecs2(t *testing.T) {
	src := t.TempDir()
	itemsPath := filepath.Join(src, "items.yaml")
	require.NoError(t, os.WriteFile(itemsPath, []byte(optionalItemsYAML), 0o600))

	msgsDir := filepath.Join(src, "messages")
	require.NoError(t, os.MkdirAll(msgsDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(msgsDir, "s12.yaml"), []byte(optionalS12YAML), 0o600))

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
	checkDir, err := os.MkdirTemp(rootDir, "gemoptionalcheck-")
	require.NoError(t, err)
	defer os.RemoveAll(checkDir)

	require.NoError(t, run(itemsPath, msgsDir, checkDir))

	builders, err := os.ReadFile(filepath.Join(checkDir, "s12.go"))
	require.NoError(t, err)
	require.Contains(t, string(builders),
		"func S12F1(fnloc uint16, mdln string, orloc byte, svids []secs2.Item, objspec string, trautod bool, verifyrspstat VERIFYRSPSTAT, timestamp string)")

	decodeTests, err := os.ReadFile(filepath.Join(checkDir, "s12_decode_test.go"))
	require.NoError(t, err)

	for _, name := range []string{"FNLOC", "ORLOC", "TRAUTOD", "VERIFYRSPSTAT", "TIMESTAMP"} {
		require.Contains(t, string(decodeTests), "func TestDecodeS12F1"+name+"Absent(t *testing.T) {")
	}

	require.Contains(t, string(decodeTests), "func TestDecodeS12F3FNLOCAbsent(t *testing.T) {")

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

	extra := strings.Replace(optionalExtraTest, "package PKG\n", "package "+pkg+"\n", 1)
	require.NoError(t, os.WriteFile(filepath.Join(checkDir, "optional_extra_test.go"), []byte(extra), 0o600))

	target := "./" + filepath.Base(checkDir) + "/"
	for _, args := range [][]string{{"build", target}, {"vet", target}, {"test", "-count=1", target}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = rootDir
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "go %s failed on the generated optional: fixture:\n%s", args[0], string(out))
	}
}

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

// enumsItemsYAML is the item dictionary the enumeration fixture draws on:
// a fixed string item with string values, one of them from another standard,
// an open item with integer values, and an open item with string values.
// Notes on items and values, one beside a value's own source, must render as godoc that compiles.
const enumsItemsYAML = `
CARRIERACTION:
  formats: [A]
  binding: fixed
  goType: string
  source: e5
  note: "A note on a named type."
  values:
    - {name: ProceedWithCarrier, value: ProceedWithCarrier, source: e87.1, note: "A note after a source line."}
    - {name: PodRelease, value: PodRelease, note: "A note on a constant."}
ERRCODE:
  formats: [U1, U2, I1]
  binding: open
  source: e5
  values:
    - {name: NoError, value: 0}
    - {name: UnknownObject, value: 1}
    - {name: Minus, value: -2}
OBJTYPE:
  formats: [A, U1]
  binding: open
  source: e5
  note: "A note on an untyped const block."
  values:
    - {name: Carrier, value: Carrier}
    - {name: Ten, value: "10"}
`

// enumsS3YAML carries the string enumeration as a plain and as an optional leaf,
// beside the two open items.
const enumsS3YAML = `
stream: 3
messages:
  - function: 17
    name: Enum Sample
    mnemonic: ES
    direction: host-to-equipment
    description: Carries every enumeration shape.
    exception: None
    source: e5
    bodies:
      - actor: both
        replyExpected: true
        structure:
          type: list
          items:
            - {item: CARRIERACTION}
            - {item: CARRIERACTION, as: next, optional: true}
            - {item: ERRCODE}
            - {item: OBJTYPE}
`

// enumsConstantsTest is a hand-written test planted beside the generated code.
// It passes the generated constants where callers would:
// typed string constants to the builder,
// and untyped constants of the open items to the secs2 constructor of the format sent.
// The decoded fields must give back the same constants.
const enumsConstantsTest = `package PKG

import (
	"testing"

	"github.com/arloliu/go-secs/v2/secs2"
)

func TestEnumConstantsRoundTrip(t *testing.T) {
	msg := S3F17(CARRIERACTIONProceedWithCarrier, CARRIERACTIONPodRelease, secs2.I1(ERRCODEMinus), secs2.A(OBJTYPECarrier))

	body, err := secs2.Decode(msg.Item().ToBytes())
	if err != nil {
		t.Fatalf("secs2.Decode: %v", err)
	}

	list, err := body.ToList()
	if err != nil {
		t.Fatalf("ToList: %v", err)
	}

	if s, err := list[0].ToASCII(); err != nil || s != "ProceedWithCarrier" {
		t.Errorf("wire CARRIERACTION: got %q, %v", s, err)
	}

	got, err := DecodeS3F17(body)
	if err != nil {
		t.Fatalf("DecodeS3F17: %v", err)
	}

	var action CARRIERACTION = got.CARRIERACTION
	if action != CARRIERACTIONProceedWithCarrier {
		t.Errorf("CARRIERACTION: got %q", action)
	}

	if !got.NEXTPresent || got.NEXT != CARRIERACTIONPodRelease {
		t.Errorf("NEXT: got %q, present %v", got.NEXT, got.NEXTPresent)
	}

	if v, err := got.ERRCODE.ToInt(); err != nil || len(v) != 1 || v[0] != ERRCODEMinus {
		t.Errorf("ERRCODE: got %v, %v", v, err)
	}

	if s, err := got.OBJTYPE.ToASCII(); err != nil || s != OBJTYPECarrier {
		t.Errorf("OBJTYPE: got %q, %v", s, err)
	}

	if v, err := secs2.U1(ERRCODEUnknownObject).ToUint(); err != nil || len(v) != 1 || v[0] != ERRCODEUnknownObject {
		t.Errorf("secs2.U1(ERRCODEUnknownObject): got %v, %v", v, err)
	}

	if s, err := secs2.A(OBJTYPETen).ToASCII(); err != nil || s != "10" {
		t.Errorf("secs2.A(OBJTYPETen): got %q, %v", s, err)
	}
}
`

// TestEnumsCompileAndRunAgainstRealSecs2 is the real-compile guard for string enumerations
// and for the untyped constants of open items.
//
// It runs the whole generator over a YAML fixture,
// plants a hand-written test that uses the generated constants,
// then builds, vets, and runs every test against the real secs2.
// A decoder that assigned decodeASCII's string straight to a named string field would not compile,
// and neither would a builder that passed the named type to secs2.A.
//
// It is gated behind the `integration` build tag,
// because it shells out to the go toolchain and reaches across module boundaries;
// run it with `go test -tags integration ./...`.
func TestEnumsCompileAndRunAgainstRealSecs2(t *testing.T) {
	src := t.TempDir()
	itemsPath := filepath.Join(src, "items.yaml")
	require.NoError(t, os.WriteFile(itemsPath, []byte(enumsItemsYAML), 0o600))

	msgsDir := filepath.Join(src, "messages")
	require.NoError(t, os.MkdirAll(msgsDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(msgsDir, "s3.yaml"), []byte(enumsS3YAML), 0o600))

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
	checkDir, err := os.MkdirTemp(rootDir, "gemenumscheck-")
	require.NoError(t, err)
	defer os.RemoveAll(checkDir)

	require.NoError(t, run(itemsPath, "", msgsDir, checkDir))

	enums, err := os.ReadFile(filepath.Join(checkDir, "items.go"))
	require.NoError(t, err)
	require.Contains(t, string(enums), "// A note on a named type.\ntype CARRIERACTION string")
	require.Contains(t, string(enums), "// A note on a constant.\n")
	require.Contains(t, string(enums), "// A note on an untyped const block.\nconst (")
	require.NotContains(t, string(enums), "type ERRCODE")
	require.NotContains(t, string(enums), "type OBJTYPE")

	builders, err := os.ReadFile(filepath.Join(checkDir, "s3.go"))
	require.NoError(t, err)
	require.Contains(t, string(builders), "func S3F17(carrieraction CARRIERACTION, next CARRIERACTION, errcode secs2.Item, objtype secs2.Item) secs2.SECS2Message")

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

	constants := strings.Replace(enumsConstantsTest, "package PKG\n", "package "+pkg+"\n", 1)
	require.NoError(t, os.WriteFile(filepath.Join(checkDir, "constants_test.go"), []byte(constants), 0o600))

	target := "./" + filepath.Base(checkDir) + "/"
	for _, args := range [][]string{{"build", target}, {"vet", target}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = rootDir
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "go %s failed on the generated enumeration fixture:\n%s", args[0], string(out))
	}

	cmd := exec.Command("go", "test", "-count=1", "-v", target)
	cmd.Dir = rootDir
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "go test failed on the generated enumeration fixture:\n%s", string(out))

	for _, name := range []string{
		"TestS3F17",
		"TestDecodeS3F17RoundTrip",
		"TestDecodeS3F17NEXTAbsent",
		"TestEnumConstantsRoundTrip",
	} {
		require.Containsf(t, string(out), "--- PASS: "+name+" ", "%s did not run and pass:\n%s", name, string(out))
	}
}

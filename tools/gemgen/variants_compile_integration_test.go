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

// variantsItemsYAML is the item dictionary the variant fixture draws on:
// one item of each field kind a short form can omit.
const variantsItemsYAML = `
OBJSPEC:
  formats: [A]
  binding: fixed
  goType: string
SVCACK:
  formats: [B]
  binding: fixed
  goType: byte
  values:
    - {name: Accepted, value: 0}
ERRCODE:
  formats: [U1, U2, U4, U8]
  binding: open
DATALENGTH:
  formats: [U4]
  binding: fixed
  goType: uint32
ACKA:
  formats: [BOOLEAN]
  binding: fixed
  goType: bool
SHEAD:
  formats: [B]
  binding: fixed
  goType: "[4]byte"
`

// variantsS16YAML carries a variant on an odd and on an even function,
// one for each actor,
// and a body whose outermost list may be empty.
const variantsS16YAML = `
stream: 16
messages:
  - function: 27
    name: Odd Sample
    mnemonic: OS
    direction: host-to-equipment
    description: Carries optional parameters.
    exception: A zero-length list means no parameters.
    source: e5
    bodies:
      - actor: both
        replyExpected: true
        structure:
          type: list
          minItems: 0
          maxItems: 3
          items:
            - {item: OBJSPEC}
            - {type: opaque, as: data}
            - {type: list, repeat: params, of: {item: ERRCODE}}
      - actor: both
        replyExpected: true
        variant: NoParams
        structure: {type: list, items: []}
  - function: 28
    name: Even Sample
    mnemonic: ES
    direction: bidirectional
    description: Acknowledges with optional errors.
    exception: None
    source: e5
    bodies:
      - actor: equipment
        replyExpected: false
        structure:
          type: list
          minItems: 1
          maxItems: 2
          items:
            - {item: SVCACK}
            - {type: list, repeat: errors, of: {item: ERRCODE}}
      - actor: equipment
        replyExpected: false
        variant: Success
        structure: {type: list, items: [{item: SVCACK}]}
      - actor: host
        replyExpected: false
        structure:
          type: list
          minItems: 1
          maxItems: 5
          items:
            - {item: OBJSPEC}
            - {item: DATALENGTH}
            - {item: ACKA}
            - {item: SHEAD}
            - {item: SVCACK}
      - actor: host
        replyExpected: false
        variant: Name
        structure: {type: list, items: [{item: OBJSPEC}]}
`

// TestVariantsCompileAndRunAgainstRealSecs2 is the real-compile guard for named builder variants
// and for the empty-list decode test of a body whose outermost list may be empty.
//
// It runs the whole generator over a YAML fixture,
// then builds, vets, and runs the generated tests against the real secs2.
// A variant that gained a decoder of its own would declare its main body's decoder twice and fail to compile.
// Each variant's generated test decodes the short form with its main body's decoder,
// so it fails when the variant and its main body disagree on a field,
// or when an omitted field does not decode to its zero value.
//
// It is gated behind the `integration` build tag,
// because it shells out to the go toolchain and reaches across module boundaries;
// run it with `go test -tags integration ./...`.
func TestVariantsCompileAndRunAgainstRealSecs2(t *testing.T) {
	src := t.TempDir()
	itemsPath := filepath.Join(src, "items.yaml")
	require.NoError(t, os.WriteFile(itemsPath, []byte(variantsItemsYAML), 0o600))

	msgsDir := filepath.Join(src, "messages")
	require.NoError(t, os.MkdirAll(msgsDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(msgsDir, "s16.yaml"), []byte(variantsS16YAML), 0o600))

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
	checkDir, err := os.MkdirTemp(rootDir, "gemvariantscheck-")
	require.NoError(t, err)
	defer os.RemoveAll(checkDir)

	require.NoError(t, run(itemsPath, msgsDir, checkDir))

	builders, err := os.ReadFile(filepath.Join(checkDir, "s16.go"))
	require.NoError(t, err)
	require.Contains(t, string(builders), "func S16F27NoParams() secs2.SECS2Message")
	require.Contains(t, string(builders), "func S16F28Success(svcack SVCACK) secs2.SECS2Message")
	require.Contains(t, string(builders), "func S16F28HostName(objspec string) secs2.SECS2Message")

	decoders, err := os.ReadFile(filepath.Join(checkDir, "s16_decode.go"))
	require.NoError(t, err)
	require.NotContains(t, string(decoders), "NoParams")
	require.NotContains(t, string(decoders), "Success")
	require.NotContains(t, string(decoders), "HostName")

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

	target := "./" + filepath.Base(checkDir) + "/"
	for _, args := range [][]string{{"build", target}, {"vet", target}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = rootDir
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "go %s failed on the generated variant fixture:\n%s", args[0], string(out))
	}

	cmd := exec.Command("go", "test", "-count=1", "-v", target)
	cmd.Dir = rootDir
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "go test failed on the generated variant fixture:\n%s", string(out))

	for _, name := range []string{
		"TestS16F27NoParams",
		"TestS16F28Success",
		"TestS16F28HostName",
		"TestDecodeS16F27RoundTrip",
		"TestDecodeS16F27EmptyList",
		"TestDecodeS16F28RoundTrip",
		"TestDecodeS16F28HostRoundTrip",
	} {
		require.Containsf(t, string(out), "--- PASS: "+name+" ", "%s did not run and pass:\n%s", name, string(out))
	}
}

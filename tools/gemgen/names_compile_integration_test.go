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

// namesItemsYAML is the item dictionary the as: fixture draws on.
const namesItemsYAML = `
SVCACK:
  formats: [B]
  binding: fixed
  goType: byte
  values:
    - {name: Accepted, value: 0}
OBJSPEC:
  formats: [A]
  binding: fixed
  goType: string
`

// namesS14YAML carries the two shapes as: exists for:
// the same item twice in one body, and two opaque nodes below the root.
// S14F22 keeps a single nested opaque node under its default name, body,
// and renames two leaves to item and out, names the generated decoder also uses for locals.
const namesS14YAML = `
stream: 14
messages:
  - function: 20
    name: Renamed Sample
    mnemonic: RS
    direction: equipment-to-host
    description: Carries one item twice and two caller-built parts.
    exception: None
    source: e5
    bodies:
      - actor: both
        replyExpected: false
        structure:
          type: list
          items:
            - {item: SVCACK}
            - type: list
              items:
                - {item: SVCACK, as: linkAck}
                - {type: opaque, as: params}
                - {type: list, items: [{type: opaque, as: results}]}
  - function: 22
    name: Nested Opaque Sample
    mnemonic: NOS
    direction: equipment-to-host
    description: Carries one caller-built part below the root.
    exception: None
    source: e5
    bodies:
      - actor: both
        replyExpected: false
        structure: {type: list, items: [{item: OBJSPEC}, {item: OBJSPEC, as: item}, {item: OBJSPEC, as: out}, {type: opaque}]}
`

// TestRenamedNodesCompileAndRunAgainstRealSecs2 is the real-compile guard for as:
// and for opaque nodes below the root.
//
// It runs the whole generator over a YAML fixture,
// so as: is read by the loader, accepted by the name checks, and applied by every walker.
// Two leaves of the same item only compile when as: renames one of them,
// and the generated builder tests, decoders, and round-trip tests only pass
// when every walker names the renamed leaf and both opaque nodes the same way.
//
// It is gated behind the `integration` build tag
// because it shells out to the go toolchain and reaches across module boundaries;
// run it with `go test -tags integration ./...`.
func TestRenamedNodesCompileAndRunAgainstRealSecs2(t *testing.T) {
	src := t.TempDir()
	itemsPath := filepath.Join(src, "items.yaml")
	require.NoError(t, os.WriteFile(itemsPath, []byte(namesItemsYAML), 0o600))

	msgsDir := filepath.Join(src, "messages")
	require.NoError(t, os.MkdirAll(msgsDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(msgsDir, "s14.yaml"), []byte(namesS14YAML), 0o600))

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
	checkDir, err := os.MkdirTemp(rootDir, "gemnamescheck-")
	require.NoError(t, err)
	defer os.RemoveAll(checkDir)

	require.NoError(t, run(itemsPath, "", msgsDir, checkDir))

	builders, err := os.ReadFile(filepath.Join(checkDir, "s14.go"))
	require.NoError(t, err)
	require.Contains(t, string(builders), "func S14F20(svcack SVCACK, linkack SVCACK, params secs2.Item, results secs2.Item)")
	require.Contains(t, string(builders), "func S14F22(objspec string, item string, out string, body secs2.Item)")

	// Rename `package gem` to a unique name and `package gem_test` to its external test package,
	// and point the tests' gem import at the generated package under the gem alias.
	// The tests stay outside the generated package, as they are in gem,
	// so they compile only if every name they read from it is exported.
	base := filepath.Base(checkDir)
	pkg := strings.ReplaceAll(base, "-", "")

	entries, err := os.ReadDir(checkDir)
	require.NoError(t, err)

	for _, entry := range entries {
		path := filepath.Join(checkDir, entry.Name())

		data, err := os.ReadFile(path)
		require.NoError(t, err)

		out := strings.Replace(string(data), "package gem_test\n", "package "+pkg+"_test\n", 1)
		out = strings.Replace(out, "package gem\n", "package "+pkg+"\n", 1)
		require.NotEqualf(t, string(data), out, "%s did not declare package gem or gem_test", entry.Name())

		out = strings.ReplaceAll(out, "\t\"github.com/arloliu/go-secs/v2/gem\"\n", "\tgem \"github.com/arloliu/go-secs/v2/"+base+"\"\n")

		require.NoError(t, os.WriteFile(path, []byte(out), 0o600))
	}

	plantTestSupport(t, rootDir, checkDir, pkg+"_test")

	target := "./" + filepath.Base(checkDir) + "/"
	for _, args := range [][]string{{"build", target}, {"vet", target}, {"test", "-count=1", target}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = rootDir
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "go %s failed on the generated as: fixture:\n%s", args[0], string(out))
	}
}

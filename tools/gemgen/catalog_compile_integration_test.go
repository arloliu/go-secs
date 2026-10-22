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

// catalogItemsYAML is the item dictionary the catalog fixture draws on.
const catalogItemsYAML = `
MDLN:
  formats: [A]
  binding: fixed
  goType: string
ACK:
  formats: [B]
  binding: fixed
  goType: byte
  values:
    - {name: Accepted, value: 0}
    - {name: Denied, value: 1}
CODE:
  formats: [U1]
  binding: fixed
  goType: uint8
  values:
    - {name: Done, value: 0}
ERRCODE:
  formats: [U1, U2, U4]
  binding: open
`

// catalogS1YAML holds a request whose host and equipment forms differ, with its reply accepted only in the host's form,
// a header-only request answered by a code and an error list,
// a request whose reply carries data and is not an acceptance,
// a notice the equipment sends without expecting a reply,
// and a request whose equipment form expects a reply and whose host form does not,
// answered by an acknowledgement accepted in either role.
const catalogS1YAML = `
stream: 1
messages:
  - function: 1
    name: Split Request
    mnemonic: SR
    direction: bidirectional
    description: Either side asks.
    exception: None
    source: e5
    bodies:
      - actor: equipment
        replyExpected: true
        structure: {type: list, items: [{item: MDLN}]}
      - actor: host
        replyExpected: true
        structure: {type: list, items: []}
  - function: 2
    name: Split Acknowledge
    mnemonic: SA
    direction: bidirectional
    description: Either side answers.
    exception: None
    source: e5
    bodies:
      - actor: equipment
        replyExpected: false
        structure: {type: list, items: [{item: ACK}, {item: MDLN}]}
      - actor: host
        replyExpected: false
        acceptReply: {value: Accepted}
        structure: {type: list, items: [{item: ACK}, {type: list, items: []}]}
  - function: 3
    name: Command
    mnemonic: CMD
    direction: host-to-equipment
    description: The host commands.
    exception: None
    source: external
    bodies:
      - actor: both
        replyExpected: true
        structure: null
  - function: 4
    name: Command Acknowledge
    mnemonic: CMDA
    direction: equipment-to-host
    description: The equipment answers.
    exception: An empty list means no errors.
    source: e5
    bodies:
      - actor: both
        replyExpected: false
        acceptReply: {value: Done}
        structure: {type: list, items: [{item: CODE}, {type: list, repeat: errors, of: {item: ERRCODE}}]}
  - function: 5
    name: Data Request
    mnemonic: DR
    direction: host-to-equipment
    description: The host asks for data.
    exception: None
    source: e5
    bodies:
      - actor: both
        replyExpected: true
        structure: null
  - function: 6
    name: Data
    mnemonic: D
    direction: equipment-to-host
    description: The equipment sends data.
    exception: None
    source: e5
    bodies:
      - actor: both
        replyExpected: false
        structure: {type: list, items: [{item: ACK}, {type: list, repeat: values, of: {item: ERRCODE}}]}
  - function: 7
    name: Notice
    mnemonic: N
    direction: equipment-to-host
    description: The equipment notifies.
    exception: None
    source: e5
    bodies:
      - actor: both
        replyExpected: false
        structure: {item: MDLN}
  - function: 9
    name: Split Wait Request
    mnemonic: SWR
    direction: bidirectional
    description: Either side asks; only the equipment waits for the answer.
    exception: None
    source: e5
    bodies:
      - actor: equipment
        replyExpected: true
        structure: {item: MDLN}
      - actor: host
        replyExpected: false
        structure: {item: MDLN}
  - function: 10
    name: Split Wait Acknowledge
    mnemonic: SWA
    direction: bidirectional
    description: Either side answers.
    exception: None
    source: e5
    bodies:
      - actor: both
        replyExpected: false
        acceptReply: {value: Accepted}
        structure: {item: ACK}
`

// catalogExtraTest checks AcceptReply over the fixture: each acceptance's wire body,
// and false for every other stream, function, and role.
// S1F10 is annotated for both roles, but only the equipment's S1F9 expects a reply,
// so only the host, answering the equipment, may accept it.
// It is written into the generated package after its package clause is rewritten.
const catalogExtraTest = `package PKG

import (
	"bytes"
	"testing"

	"github.com/arloliu/go-secs/v2/secs2"
)

func TestFixtureAcceptReply(t *testing.T) {
	want := map[[3]byte]secs2.Item{
		{1, 1, byte(RoleHost)}:      secs2.L(secs2.B(0), secs2.L()),
		{1, 3, byte(RoleEquipment)}: secs2.L(secs2.U1(0), secs2.L()),
		{1, 9, byte(RoleHost)}:      secs2.B(0),
	}

	for function := range 256 {
		for _, replier := range []Role{0, RoleHost, RoleEquipment, 3} {
			msg, ok := AcceptReply(1, byte(function), replier)

			body, accepted := want[[3]byte{1, byte(function), byte(replier)}]
			if ok != accepted {
				t.Fatalf("AcceptReply(1, %d, %d): got %t, want %t", function, replier, ok, accepted)
			}

			if !ok {
				continue
			}

			if msg.StreamCode() != 1 || int(msg.FunctionCode()) != function+1 || msg.WaitBit() {
				t.Errorf("AcceptReply(1, %d, %d): got S%dF%d W=%t", function, replier, msg.StreamCode(), msg.FunctionCode(), msg.WaitBit())
			}

			if !bytes.Equal(msg.Item().ToBytes(), body.ToBytes()) {
				t.Errorf("AcceptReply(1, %d, %d): got % x, want % x", function, replier, msg.Item().ToBytes(), body.ToBytes())
			}
		}
	}
}

func TestFixtureLookup(t *testing.T) {
	command, ok := Lookup(1, 3, RoleHost)
	if !ok || command.Verified || command.HasBody || command.Decode != nil || command.ReplyFunction != 4 {
		t.Errorf("Lookup(1, 3, RoleHost): got %+v, %t", command, ok)
	}

	notice, ok := Lookup(1, 7, RoleHost)
	if !ok || notice.Direction != EquipmentToHost || notice.ReplyFunction != 0 {
		t.Errorf("Lookup(1, 7, RoleHost): got %+v, %t", notice, ok)
	}

	if _, ok := Lookup(1, 8, RoleHost); ok {
		t.Error("Lookup(1, 8, RoleHost): got a form of an undefined message")
	}
}
`

// TestCatalogCompilesAndRunsAgainstRealSecs2 is the real-compile guard for the catalog.
//
// It runs the whole generator over a YAML fixture with a message whose roles send different forms,
// a header-only request from an external reference, three annotated replies, and two unannotated ones,
// one annotated reply answering a primary whose two forms set different reply bits.
// The generated catalog.go and its generated Lookup test must build, vet, and pass,
// and a hand-written test checks AcceptReply over every function and role.
//
// It is gated behind the `integration` build tag
// because it shells out to the go toolchain and reaches across module boundaries;
// run it with `go test -tags integration ./...`.
func TestCatalogCompilesAndRunsAgainstRealSecs2(t *testing.T) {
	src := t.TempDir()
	itemsPath := filepath.Join(src, "items.yaml")
	require.NoError(t, os.WriteFile(itemsPath, []byte(catalogItemsYAML), 0o600))

	msgsDir := filepath.Join(src, "messages")
	require.NoError(t, os.MkdirAll(msgsDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(msgsDir, "s1.yaml"), []byte(catalogS1YAML), 0o600))

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
	checkDir, err := os.MkdirTemp(rootDir, "gemcatalogcheck-")
	require.NoError(t, err)
	defer os.RemoveAll(checkDir)

	require.NoError(t, run(itemsPath, "", msgsDir, checkDir))

	catalogSrc, err := os.ReadFile(filepath.Join(checkDir, "catalog.go"))
	require.NoError(t, err)
	require.Contains(t, string(catalogSrc), "return S1F2Host(ACKAccepted)")
	require.Contains(t, string(catalogSrc), "return S1F4(CODEDone)")
	require.Contains(t, string(catalogSrc), "return S1F10(ACKAccepted)")

	// Rewrite `package gem` and `package gem_test` to one unique name,
	// so the generated tests become internal tests of the generated catalog.
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

	extra := strings.Replace(catalogExtraTest, "package PKG\n", "package "+pkg+"\n", 1)
	require.NoError(t, os.WriteFile(filepath.Join(checkDir, "catalog_extra_test.go"), []byte(extra), 0o600))

	target := "./" + filepath.Base(checkDir) + "/"
	for _, args := range [][]string{{"build", target}, {"vet", target}, {"test", "-count=1", target}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = rootDir
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "go %s failed on the generated catalog fixture:\n%s", args[0], string(out))
	}
}

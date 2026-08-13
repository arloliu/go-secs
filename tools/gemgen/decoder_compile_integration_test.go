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

// TestDecodersCompileAndRoundTripAgainstRealSecs2 is the real-compile guard for the decode half of
// the generator, matching the guards the builder half already has.
//
// A decoder is where the generator's type decisions actually bind: the accessor it picks must
// exist on [secs2.Cursor], the generic instantiation must satisfy the helper's constraint, the
// result-struct field type must accept what the accessor returns, and the array conversion must be
// width-checked.
// None of that is visible to a syntax-only or gofmt pass.
//
// The message rendered here carries one leaf of every shape class the DSL can express — the two
// formats that share a Go type (B/byte and U1/uint8), a boolean, a fixed-width byte array, a
// variable-length byte string, a signed and a floating-point width, an open leaf, an opaque body,
// a repeated group, a packed group, a nested list, and a minItems-bounded optional group — so the
// build covers every emitter branch at once, and the generated round-trip test then proves the
// decoder actually reads back what the builder wrote.
//
// It is gated behind the `integration` build tag because it shells out to the go toolchain and
// reaches across module boundaries; run it with `go test -tags integration ./...`.
func TestDecodersCompileAndRoundTripAgainstRealSecs2(t *testing.T) {
	items := map[string]Item{
		"COMMACK": {Formats: []string{"B"}, Binding: BindingFixed, GoType: "byte", Values: []ItemValue{{Name: "OK", Value: 0}}},
		"SFCD":    {Formats: []string{"B"}, Binding: BindingFixed, GoType: "byte"},
		"RMACK":   {Formats: []string{"U1"}, Binding: BindingFixed, GoType: "uint8", Values: []ItemValue{{Name: "Done", Value: 0}}},
		"LINKID":  {Formats: []string{"U4"}, Binding: BindingFixed, GoType: "uint32"},
		"SIGNED":  {Formats: []string{"I4"}, Binding: BindingFixed, GoType: "int32"},
		"REAL":    {Formats: []string{"F4"}, Binding: BindingFixed, GoType: "float32"},
		"ACKA":    {Formats: []string{"BOOLEAN"}, Binding: BindingFixed, GoType: "bool"},
		"MDLN":    {Formats: []string{"A"}, Binding: BindingFixed, GoType: "string"},
		"ERRTEXT": {Formats: []string{"A"}, Binding: BindingFixed, GoType: "string"},
		"MHEAD":   {Formats: []string{"B"}, Binding: BindingFixed, GoType: "[10]byte"},
		"ABS":     {Formats: []string{"B"}, Binding: BindingFixed, GoType: "[]byte"},
		"TSIP":    {Formats: []string{"B"}, Binding: BindingFixed, GoType: "byte"},
		"SVID":    {Formats: []string{"A", "U1", "U2", "U4", "U8"}, Binding: BindingOpen},
		"ERRCODE": {Formats: []string{"U1", "U2", "U4", "U8"}, Binding: BindingOpen},
	}

	// One body carrying every shape class the emitter branches on.
	structure := &StructureNode{Type: "list", Items: []StructureNode{
		{Item: "COMMACK"},
		{Item: "RMACK"},
		{Item: "SFCD"},
		{Item: "LINKID"},
		{Item: "SIGNED"},
		{Item: "REAL"},
		{Item: "ACKA"},
		{Item: "MHEAD"},
		{Item: "ABS"},
		{Item: "SVID"},
		{Type: "list", Items: []StructureNode{{Item: "MDLN"}}},            // nested fixed list
		{Type: "list", Repeat: "svids", Of: &StructureNode{Item: "SVID"}}, // repeated group
		{Type: "list", Packed: "tsips", Of: &StructureNode{Item: "TSIP"}}, // packed group
		{Type: "list", MinItems: intp(0), MaxItems: intp(2), Items: []StructureNode{ // optional group
			{Item: "ERRCODE"},
			{Item: "ERRTEXT"},
		}},
	}}

	mf := MessageFile{Stream: 2, Messages: []Message{
		{
			Function: 42, Name: "Shape Sample", Mnemonic: "SS",
			Direction: "equipment-to-host", Description: "Carries one leaf of every decodable shape class.",
			Exception: "None", Source: "e5",
			Bodies: []Body{{Actor: "both", ReplyExpected: false, Structure: structure}},
		},
		{
			// An opaque body is its own shape class: it is the whole body, not a list member.
			Function: 44, Name: "Opaque Sample", Mnemonic: "OS",
			Direction: "equipment-to-host", Description: "Carries an equipment-defined body.",
			Exception: "None", Source: "e5",
			Bodies: []Body{{Actor: "both", ReplyExpected: false, Structure: &StructureNode{Type: "opaque"}}},
		},
		{
			// A header-only body must contribute no decoder and no result type.
			Function: 46, Name: "Header Only Sample", Mnemonic: "HOS",
			Direction: "equipment-to-host", Description: "Carries no body.",
			Exception: "None", Source: "e5",
			Bodies: []Body{{Actor: "both", ReplyExpected: false, Structure: nil}},
		},
	}}

	msgOut, err := renderMessages(mf, items)
	require.NoError(t, err)
	decodeOut, err := renderDecoders(mf, items)
	require.NoError(t, err)
	testOut, err := renderDecodeTests(mf, items)
	require.NoError(t, err)
	supportOut, err := renderDecodeSupport()
	require.NoError(t, err)
	itemsOut, err := renderItems(items)
	require.NoError(t, err)

	// Sanity-check the branches that share a Go type actually reached different accessors before
	// spending a build on the output.
	require.Contains(t, string(decodeOut), "decodeByteAs[COMMACK]")
	require.Contains(t, string(decodeOut), "decodeUintAs[RMACK]")
	require.NotContains(t, string(decodeOut), "func DecodeS2F46")

	// Locate the root go-secs module (two levels up from tools/gemgen), which provides the real
	// secs2 package the generated code imports.
	wd, err := os.Getwd()
	require.NoError(t, err)
	rootDir := filepath.Clean(filepath.Join(wd, "..", ".."))
	rootMod, err := os.ReadFile(filepath.Join(rootDir, "go.mod"))
	require.NoError(t, err, "expected root go.mod at %s", rootDir)
	require.True(t, strings.Contains(string(rootMod), "module github.com/arloliu/go-secs/v2"),
		"root module at %s is not the go-secs root module", rootDir)

	// Create a throwaway package directory inside the root module so `go test` resolves the real
	// secs2 import.
	// Rewrite `package gem` to a unique name and `package gem_test` to match, so the generated
	// test becomes an internal test of the generated builders and decoders.
	checkDir, err := os.MkdirTemp(rootDir, "gemdecodercheck-")
	require.NoError(t, err)
	defer os.RemoveAll(checkDir)

	pkg := strings.ReplaceAll(filepath.Base(checkDir), "-", "")

	msgSrc := strings.Replace(string(msgOut), "package gem\n", "package "+pkg+"\n", 1)
	require.NotEqual(t, string(msgOut), msgSrc, "generated builder did not declare 'package gem'")

	decodeSrc := strings.Replace(string(decodeOut), "package gem\n", "package "+pkg+"\n", 1)
	require.NotEqual(t, string(decodeOut), decodeSrc, "generated decoder did not declare 'package gem'")

	supportSrc := strings.Replace(string(supportOut), "package gem\n", "package "+pkg+"\n", 1)
	require.NotEqual(t, string(supportOut), supportSrc, "decode support did not declare 'package gem'")

	itemsSrc := strings.Replace(string(itemsOut), "package gem\n", "package "+pkg+"\n", 1)
	require.NotEqual(t, string(itemsOut), itemsSrc, "generated items did not declare 'package gem'")

	testSrc := strings.Replace(string(testOut), "package gem_test\n", "package "+pkg+"\n", 1)
	require.NotEqual(t, string(testOut), testSrc, "generated test did not declare 'package gem_test'")
	testSrc = strings.ReplaceAll(testSrc, "\t\"github.com/arloliu/go-secs/v2/gem\"\n", "")
	testSrc = strings.ReplaceAll(testSrc, "gem.", "")

	for name, src := range map[string]string{
		"s2.go": msgSrc, "s2_decode.go": decodeSrc, "decode.go": supportSrc,
		"items.go": itemsSrc, "s2_decode_test.go": testSrc,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(checkDir, name), []byte(src), 0o600))
	}

	// Real build + test against real secs2 -- the whole point: prove the emitted accessors, generic
	// instantiations, and field types type-check, and that the round-trip assertions pass.
	cmd := exec.Command("go", "test", "./"+filepath.Base(checkDir)+"/")
	cmd.Dir = rootDir
	buildOut, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "generated decoders failed against real secs2:\n%s\n--- decoders ---\n%s\n--- test ---\n%s",
		string(buildOut), decodeSrc, testSrc)
}

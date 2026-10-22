package gem

// The tools/gemgen generator writes every stream file in this package from YAML under tools/gemgen/data:
// items.go from data/items.yaml, groups.go from data/groups.yaml,
// and for each stream N the four files sN.go, sN_test.go, sN_decode.go and sN_decode_test.go
// from data/messages/sN.yaml.
// The generated streams are 1 to 10 and 12 to 19
// (s1.yaml … s10.yaml and s12.yaml … s19.yaml; SEMI E5 defines no stream 11).
// decode.go holds the shared helpers every generated decoder calls.
// It carries no per-message data,
// but the generator writes it too, so the decoders and the helpers they need never drift apart.
// catalog.go holds the catalog of every stream's messages, behind Lookup and AcceptReply,
// and catalog_lookup_test.go its test over every form.
// Everything else here is hand-written:
// doc.go, this file, alcd.go, and the test files outside the per-stream set and catalog_lookup_test.go.
// Run `go generate ./...` from the repository root (or `go generate .` from this directory)
// to regenerate the files after editing the YAML.
//
// The `-C ../tools/gemgen` flag performs a real process chdir into tools/gemgen
// before `go run` builds and executes the gemgen binary,
// so the binary's own relative flag values (-items, -messages, -out) resolve relative to tools/gemgen,
// not to this package's directory.
// This was verified empirically:
// running the directive lands items.go, s1.go and s1_test.go in gem/ with no stray gem/data/ tree,
// confirming -C sets the child process's working directory as expected on this toolchain (go1.26.1).
//
//go:generate go run -C ../tools/gemgen . -items data/items.yaml -groups data/groups.yaml -messages data/messages -out ../../gem

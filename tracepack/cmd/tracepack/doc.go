// Command tracepack reads tracepack files (.tpk) on local paths.
//
// Install it with
//
//	go install github.com/arloliu/go-secs/tracepack/cmd/tracepack@<version>
//
// Usage:
//
//	tracepack dump --jsonl [-o <file>] <pack>
//	tracepack verify <pack>...
//	tracepack find --capture <uuid> --seq <n> (--hour <n> | --at <time>) <path>...
//
// Flags come before the paths.
//
// dump --jsonl writes the canonical JSONL export of a pack (the tracepack format specification §15) to stdout,
// and the read's incomplete reasons, footer errors and redaction defects to stderr.
// With -o, the export is written to a temporary file beside <file> and renamed over it only when the export succeeds,
// an incomplete export included; an export that fails leaves an existing <file> as it was.
// The file is created with mode 0600, as a temporary file is, whatever the mode of the file it replaces.
// Without -o, an export that fails has already written part of its output to stdout,
// and stderr says that output is not an export.
//
// verify checks each pack (the tracepack format specification §13) and reports its outcome and evidence,
// with a complete line that is true exactly when no block failed and no validated block disagrees with the index.
// complete leaves out what the pack metadata declares lost, its coverage entries and crash gaps,
// which make a read of the pack incomplete, so verify and dump can exit differently on one pack.
//
// find looks up the reply to the primary record that --capture, --seq and the primary's hour name
// (the tracepack semantics specification §7.2):
// --hour is the UTC hour since 1970-01-01T00:00Z, and --at the primary's timestamp in RFC 3339.
// Each path is a pack, or a directory searched for regular files named *.tpk without following symbolic links;
// the hidden directories below it are skipped, such as the .partial directory of a directory sink's unpublished segments.
// The packs are of one tool, each inside one UTC hour, and every one stays open during the lookup,
// with a file descriptor and its index in memory each, so a lookup over many packs needs as many.
// It reads the primary's hour and the next, with the library's default limits.
//
// Every command runs with the library's default reader budgets.
// The text of the verify and find reports is not a contract before tracepack v1.0.0.
//
// Exit codes:
//   - 0: the command ran and its report is clean.
//   - 1: the command ran and its report is not clean:
//     dump's read is incomplete, a pack's verify outcome is not finalized-consistent, or the lookup is incomplete.
//     A footer error alone does not make dump or find exit 1.
//     The lookup's outcome does not change the exit code.
//   - 2: the command could not run: a usage error, a path that cannot be read, a pack rejected at bootstrap,
//     an error of the read or the lookup, a write error, its diagnostics' included, or an interrupt.
package main

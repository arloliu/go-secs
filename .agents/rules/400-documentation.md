# 400 — Documentation

- All exported symbols have Godoc.
  The first line starts with the symbol name and is a one-sentence summary.
- Each public package has a `doc.go` with a package-level overview.
- Keep `README.md` and `sml/README.md` in sync with the current exported API.

## Line breaking (semantic linefeeds)

**Break lines by meaning, not by column.**
A line ends where a thought ends.
This is the semantic linefeeds convention: default to one sentence per line, and break a long sentence only at a real clause boundary.

**Why it matters.**
Column-wrapped prose reflows the entire paragraph when one clause changes.
The diff then shows five changed lines where one sentence changed, and a reviewer cannot see what actually moved.
Semantic linefeeds keep the diff to the line that changed, and every line stays independently readable.

This file is written the way it prescribes.
Everything below follows from the principle.

### One sentence per line

Never put two independent sentences on the same line, not even a short one trailing the period of another.
Never break mid-clause to hit a column.

```go
// Package hsms provides immutable HSMS message types and factories per SEMI E37.
//
// HSMS defines a TCP/IP framing layer and handshake protocol for SECS-II data messages.
// This package implements the message layer: construction, encode, decode, and read-only accessors.
// Connection and transport concerns live in sibling packages such as hsmsss and secs1.
package hsms
```

Not this — adding one word to the first sentence reflows all five lines:

```go
// Package hsms provides immutable HSMS message types and factories per SEMI
// E37. HSMS defines a TCP/IP framing layer and handshake protocol for SECS-II
// data messages. This package implements the message layer: construction,
// encode, decode, and read-only accessors. Connection and transport concerns
// live in sibling packages such as hsmsss and secs1.
package hsms
```

### Long sentences

Past roughly 120 characters, start looking for a clause boundary.
The length is only the trigger to look; the boundary decides where the break lands.
If the sentence has no boundary near there, let the line run — an over-long line beats a sentence severed mid-clause.

A clause boundary is a semicolon, a colon, an em dash, a coordinating conjunction (`and`, `but`, `so`), or the start of a relative clause (`which`, `that`, `where`).
A comma between items of a list is not one.

```go
// Free returns the message's pooled items to the pool and is safe to call more than once;
// callers must not retain the message, or any item obtained from it, after Free returns.
func (m *DataMessage) Free() { }
```

120 is a readability guide, not a gate.
**No linter checks line length in this repo** — `lll` is not enabled, and revive's `line-length-limit` is disabled in `.golangci.yaml`.
There is no tool to satisfy, so never wrap defensively.

### Never break these

Keep on one line whatever their length: URLs, compiler and lint directives (`//go:generate`, `//nolint:...`), generated file headers, and code inside indented Godoc examples.
Breaking a directive across lines silently stops it from applying.

### Markdown

Markdown gets the same treatment.
Renderers join consecutive lines back into one paragraph, so source line breaks cost the reader nothing and buy the same clean diffs.

```markdown
The passive session listens on the configured port and accepts a single peer.
A second connection attempt is refused while a session is already active.
```

Not this:

```markdown
The passive session listens on the configured port and accepts a single
peer. A second connection attempt is refused while a session is already
active.
```

Inside a list item the rule is unchanged — one sentence per line, continuation lines indented to the item's text.
Leave tables alone, since a row is one line by construction.

### Existing hard-wrapped text

Much of the current Godoc — `hsms/doc.go` and others — is column-wrapped mid-sentence at around 90 characters.
Some markdown under `docs/` and in `CHANGELOG.md` is too.
That text predates this rule.
It is legacy, not evidence against the principle: **do not copy its wrapping when writing new prose in the same file.**

Do not rewrap stable text either.
Reflow only the paragraph or comment block whose content you are already changing, never the rest of the file.
A pure rewrap produces a large, unreviewable diff that changes nothing but whitespace.

The single exception is a deliberate migration pass the user asks for, run through the `doc-sync` skill in reflow mode.
It lands in its own commit that changes line breaks and nothing else, which is what keeps a diff that large reviewable.

### Scope

Go comments, Godoc and internal alike, and markdown prose: `README.md`, `CHANGELOG.md`, `docs/`, specs, and the rule files in `.agents/rules/`.

## Template

```go
// FunctionName one-line summary.
//
// Longer description (optional).
//
// Parameters:
//   - param1: constraints
//   - param2: expected values
//
// Returns:
//   - Type: meaning
//   - error: failure conditions
//
// Example:
//
//	result, err := FunctionName(input)
func FunctionName(param1 T1, param2 T2) (Result, error) { }
```

Omit `Parameters` / `Returns` sections when there are none.
Simple getters need only a one-liner.

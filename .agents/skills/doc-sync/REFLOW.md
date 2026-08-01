# doc-sync — reflow mode

Rewrite exported Godoc to semantic linefeeds across a package, changing line breaks and nothing else.

The line-breaking rule lives in [`400-documentation.md`](../../rules/400-documentation.md#line-breaking-semantic-linefeeds) and is the source of truth.
Read it before starting; this file covers only how to run the pass.

Reflow is the sanctioned exception to "do not rewrap stable text".
It earns that exception two ways: it runs only when the user asks for it, and it lands in its own commit that changes nothing else.
A reflow mixed into a behavior change produces a diff no one can review.

## Scope

Every exported symbol of each package in scope, and its Godoc:

- functions and methods
- interfaces, including the comment on each interface method
- types, constants, and variables
- the package comment in `doc.go`

Unexported comments stay out unless the user asks for them.
Test files stay out.

## Procedure

1. **Inventory.** List every exported symbol in the package, and whether it carries a Godoc comment.
   A symbol with no comment is a drift finding, not a reflow target — report it and move on, since inventing prose is a different job.
2. **Reflow each comment.** One sentence per line.
   Break a sentence past roughly 120 characters at a clause boundary, and leave it long when there is no boundary near there.
3. **Keep these on one line** whatever their length: URLs, `//go:` and `//nolint:` directives, generated headers, and indented example code inside a comment.
   Splitting a directive silently stops it applying.
4. **Preserve Godoc structure.** Blank `//` paragraph separators, list items (`//   - …`), headings (`// # …`), and indented code blocks keep the shape they already have.
5. **Verify wording survived.** Run the check below, then `make lint` and `go build ./...`.

## Verifying wording survived

Reflow must move line breaks without touching a single word.
Prove it rather than asserting it — normalise every comment in the file to one whitespace-collapsed stream, before and after, and require the two to be identical:

```bash
norm() { grep -oE '^[[:space:]]*//.*' | sed 's|^[[:space:]]*//[[:space:]]\?||' | tr -s '[:space:]' '\n' | grep -v '^$'; }

for f in $(git diff --name-only -- '*.go'); do
  if diff <(git show "HEAD:$f" | norm) <(norm < "$f") >/dev/null; then
    echo "OK            $f"
  else
    echo "WORD CHANGED  $f"
    diff <(git show "HEAD:$f" | norm) <(norm < "$f")
  fi
done
```

`norm` reduces a file's comments to one word per line, so two files agree only when every word survived in the same order.
A file reporting `WORD CHANGED` prints the offending words — revert that edit before committing.

## Completion criterion

Every exported symbol in scope appears in the report as reflowed, already conforming, or skipped with a reason.
The package is not done while one exported symbol is unaccounted for.
The wording check passes for every changed file, `make lint` is clean, and `go build ./...` succeeds.

## Commit

One commit, reflow only.

```
docs(hsms): reflow exported Godoc to semantic linefeeds

Line breaks only; no wording, signature, or behavior change.
Verified with the comment-text normalisation check in
.agents/skills/doc-sync/REFLOW.md.
```

## Report

```
## doc-sync reflow report — hsms

### Reflowed
- DataMessage.Free, DataMessage.Derive, NewDataMessage, Message (interface, 6 method comments), doc.go package comment — 24 symbols.

### Already conforming
- ControlMessage.RejectReasonCode, NewSelectReq, NewSelectRsp — 9 symbols.

### Skipped
- MsgType constants L40-58: single-line comments, nothing to break.

### Missing Godoc (drift, not reflowed)
- Session.SendDataMessageAsync — exported, no comment.

### Verification
- Wording check: OK on all 7 changed files.
- make lint: 0 issues. go build ./...: ok.
```

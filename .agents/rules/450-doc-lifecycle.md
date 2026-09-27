# 450 — Document lifecycle

Every Markdown document under `docs/` is **living** or **finite**, and its class decides how it ends.
Decide the class when you create the file.

- **Living**: in force until replaced — specs, guides, changelogs, decision logs.
  Edit it in place; its changelog or git history carries the past.
- **Finite**: exists to get one piece of work done — plans, proposals, reports (research, audit, root cause).
  It ends by **closing**, and closing deletes the file.

## Where documents live

- `docs/specs/<subject>/` — one directory per subject:
  its living documents, its finite documents, and a `README.md` index.
- `docs/plans/<slug>.md` — a finite plan that belongs to no subject directory (a fix, a refactor, a performance pass).
  When the work also needs a report (a root-cause analysis, a measurement), the report is a section of the plan.
- `docs/guides/<slug>.md` — living how-to documents.

## Names

- File and directory names are kebab-case subjects and carry no dates; the date lives on the Status line.
  `docs/plans/sml-w-round-trip.md`, not `docs/plans/2026-09-27-sml-plan.md`.
- A series takes a stable ID: proposals are `proposals/p<N>-<slug>.md`.
  IDs are never reused; the subject `README.md` records the next free one, because closed proposals leave no file behind.

## Status line

Line 3 of every document, after the title and one blank line:

```
Status: <value> (<YYYY-MM-DD>) — <optional clause>
```

| Class | Values |
|---|---|
| living | `draft` (written, not yet in force), `current` (in force) |
| finite | `draft` (awaiting the owner), `active` (approved; work under way), `deferred` (parked; kept as the record a later design starts from) |

The date is the day the value last changed.
A plan also carries a phase table with `pending`, `in-progress` or `done` per phase.
A subject `README.md` is the index and carries no Status line.

## Closing a finite document

A finite document closes when its work is done, rejected, withdrawn or superseded.
In the commit that closes it:

1. Record the outcome in the living document that owns it:
   the subject's changelog or decision log, or, for a `docs/plans/` file, the commit message.
   Name the document and where its text lives, e.g. "P3 (object lifecycle) applied as v2.7; last text at `<commit>`".
2. Delete the file.
3. Remove its row from the subject `README.md`.

A document is closed when its file is gone and its outcome line exists.

## Subject README

`docs/specs/<subject>/README.md` is the subject's single index:
one row per file with its class, Status value and a one-line purpose, plus the next free proposal ID.
Change it in the same commit as any Status change, addition or deletion.
Other documents in the subject point at the README instead of keeping their own list of files.

## Links and decision logs

- Link only to files in the repository.
  Name material kept outside it — review reports, research notes, archived revisions — in plain text, without a path:
  "P3 review round 3 (kept outside the repository)".
- `<subject>-decisions.md` is append-only.
  A superseded entry keeps its text and gains "(superseded by <ID>)".
  Repository-wide architecture decisions go to `docs/adr/` through `/domain-modeling`; a subject's owner decisions go to its decision log.

## Check

`make docs-check` covers every `docs/specs/<subject>/` that has a `README.md` and every `docs/plans/*.md` whose name starts with no date;
documents that predate this rule join the check when they are migrated.
It fails when:

- a document other than `README.md` lacks the line-3 Status line, or its value is outside its class (living: `draft`, `current`; finite: `draft`, `active`, `deferred`);
- the README's rows and the directory's files differ, a row's Status differs from the file's, or a row's Class does not allow that value;
  a `docs/plans/` file is finite;
- a relative Markdown link resolves to no file.

It runs in `make ci` and in the lint job of `.github/workflows/ci.yaml`.

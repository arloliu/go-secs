# 550 - Git Conventions

Apply these rules when crafting commits, branches, or pull-request titles and descriptions.

## Branches
- Prefixes: `feat/`, `fix/`, `docs/`, `chore/`, `test/`, `refactor/`, `perf/`.
- Optional package scope after the prefix: `feat/hsmsss/linktest-threshold`, `fix/secs1/block-transport`.

## Commit Messages

### Format
- Follow [Conventional Commits](https://www.conventionalcommits.org/).
  A type prefix is required: `feat`, `fix`, `docs`, `chore`, `test`, `refactor`, `perf`, etc.
  An optional scope goes in parentheses, usually the package: `fix(hsms): ...`. Present tense.
- First line ≤ 50 characters when possible; hard cap 100.

### Body — short and clear
The body explains WHY the change is needed and WHAT its PURPOSE is,
then summarises the MAIN CHANGES at a high level. Aim for 3–8 short
paragraphs — readable in under a minute.

Skip low-level details that belong in the code, the PR description, or the spec:
- Per-function or per-file diffs (the code already shows them).
- Line-by-line walk-throughs.
- Review-iteration counts and rationale (e.g. how many rounds of reviewer feedback shaped the design).
- Exhaustive test enumerations.

Bias toward the reader who finds this commit via `git log` or `git blame` months later
and wants to understand the change quickly.

### No plan / review jargon
Future readers of `git log` and `git blame` have no access to in-progress plan documents or review reports.
Do NOT reference:
- Sequencing labels: `PR-1`, `PR-2`, `Phase 4`, ...
- Work-item IDs: `W12`, `W15`, `H2.C`, ...
- Review-iteration jargon: `plan-review v2 P0-B`, `post-impl v3.1`, `Codex xhigh`, ...
- References to specific `tmp/*_review.md` reports.

Bad: `fix(hsms): close W15+W16 per PR-2 spec`.

Good: `fix(hsms): serialize ConnStateMgr transitions with stale gate`.

Citing a discoverable spec FILE PATH is fine (e.g. `See docs/plans/.../02-spec.md`) —
the path is discoverable; the section IDs inside it are not.

### Attribution
Never add `Co-Authored-By` or any other attribution trailers.

## Pull Requests
- Title follows the same Conventional Commits format as the commit's first line.
- Body restates the WHY and PURPOSE for reviewers.
  Linking the spec and prior review history is acceptable here when it is useful context,
  but lead with domain language so a reviewer who hasn't read the plan can still understand the change.

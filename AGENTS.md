# go-secs — Rules & Skills Index

Rules are in `.agents/rules/`. Read any file whose topic matches the task before editing code.

| File | Topic |
|------|-------|
| `050-principles.md` | Working principles |
| `100-overview.md` | Package layout, architecture, prime directives |
| `200-coding-style.md` | Go idioms, error handling, file layout |
| `300-testing.md` | Test organization, async rules, make targets |
| `400-documentation.md` | Godoc format |
| `500-workflow.md` | Pre-commit checks, make targets |
| `550-git-conventions.md` | Branch, commit-message, and pull-request conventions |
| `600-perf-sec.md` | Hot paths, allocations, decode boundaries |
| `700-lint-after-write.md` | Lint workflow |

Skills (invoke by name):

| Skill | Purpose |
|-------|---------|
| `/go-api-review [pkg]` | Library-consumer DX review of a public package |
| `/qa-review [area]` | Correctness / concurrency / fault-tolerance review |
| `/doc-sync [scope]` | Sync `README.md`, `sml/README.md`, and `doc.go` comments against source |

Default scope for all skills: the top-level public packages. Narrow with an argument (`hsmsss`, `secs1`, `sml`, …).

## Agent skills

### Issue tracker

Issues live in GitHub Issues (github.com/arloliu/go-secs), via the `gh` CLI; external PRs are not pulled into the triage queue. See `docs/agents/issue-tracker.md`.

### Triage labels

Default label vocabulary (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`) — no existing repo label convention to map against. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `CONTEXT.md` + `docs/adr/` at the repo root (neither exists yet — created lazily by `/domain-modeling`). See `docs/agents/domain.md`.

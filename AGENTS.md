# go-secs — Rules & Skills Index

Rules are in `.agents/rules/`. Read any file whose topic matches the task before editing code.

| File | Topic |
|------|-------|
| `050-principles.md` | Working principles |
| `100-overview.md` | Package layout, architecture, prime directives |
| `200-coding-style.md` | Go idioms, error handling, file layout |
| `300-testing.md` | Test organization, async rules, make targets |
| `400-documentation.md` | Godoc format |
| `500-workflow.md` | Pre-commit checks, commit conventions, make targets |
| `600-perf-sec.md` | Hot paths, allocations, decode boundaries |
| `700-lint-after-write.md` | Lint workflow |

Skills (invoke by name):

| Skill | Purpose |
|-------|---------|
| `/go-api-review [pkg]` | Library-consumer DX review of a public package |
| `/qa-review [area]` | Correctness / concurrency / fault-tolerance review |
| `/doc-sync [scope]` | Sync `README.md`, `sml/README.md`, and `doc.go` comments against source |

Default scope for all skills: the top-level public packages. Narrow with an argument (`hsmsss`, `secs1`, `sml`, …).

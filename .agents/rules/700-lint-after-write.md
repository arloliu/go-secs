---
trigger: always_on
glob: "**/*.go"
description: Run linter after modifying Go files
---

# Lint After Write

After modifying any `.go` file:

1. **Run:** `make lint`
2. **Fix:** All reported issues before committing.
3. **Re-run:** Until clean.

## Common Fixes
| Lint Error | Fix |
|------------|-----|
| `goimports` | Run `goimports -w file.go` |
| `errcheck` | Handle or explicitly ignore with `_ =` |
| `unused` | Remove dead code |
| `govet` | Fix type/format mismatches |

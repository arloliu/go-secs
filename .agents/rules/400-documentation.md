# 400 — Documentation

- All exported symbols have Godoc. First line starts with the symbol name and is a one-sentence summary.
- For multi-line descriptions:
  - Do not wrap strictly by a hard character limit. Keep one sentence on one line if it is not too long.
  - Never mix two independent sentences on the same line (e.g., do not place the start of a second sentence immediately after the period of the first on the same line).
  - Break long sentences at natural syntactic boundaries (verbs, nouns, clauses) to preserve readability.
- Each public package has a `doc.go` with a package-level overview.
- Keep `README.md` and `sml/README.md` in sync with the current exported API.

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

Omit `Parameters` / `Returns` sections when there are none. Simple getters need only a one-liner.

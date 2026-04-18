# 400 — Documentation

- All exported symbols have Godoc. First line starts with the symbol name and is a one-sentence summary.
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

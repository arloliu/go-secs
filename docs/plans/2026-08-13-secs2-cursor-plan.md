# Implementation plan: typed path extraction — `secs2.Cursor` (v2.4)

Give consumers a one-expression, error-accumulating way to read a value out of a nested item,
replacing the current four-step dance
(`Get` → type assertion → `ToXxx` slice conversion → unchecked index).

## API

```go
// Constructor. A nil item yields a cursor whose accessors return a descriptive error.
func NewCursor(item Item) Cursor

// Cursor is a small value type; copying it is free and no method allocates on success.
type Cursor struct { /* item Item; err error; depth/index for error context */ }

func (c Cursor) At(indices ...int) Cursor      // navigate list indices; error-accumulating
func (c Cursor) Uint() (uint64, error)         // UintItem, any width
func (c Cursor) Int() (int64, error)           // IntItem, any width
func (c Cursor) Float() (float64, error)       // FloatItem, F4 or F8
func (c Cursor) Bool() (bool, error)           // BooleanItem
func (c Cursor) ASCII() (string, error)        // ASCIIItem
func (c Cursor) Binary() ([]byte, error)       // BinaryItem (cloned — immutability contract)
func (c Cursor) Size() (int, error)            // Size() of the item at the cursor
func (c Cursor) Item() (Item, error)           // unwrap: the item at the cursor
func (c Cursor) Err() error                    // first error encountered, nil if none
```

## Semantics

1. **Error accumulation**: `At` on an errored cursor is a no-op that carries the error forward;
   the terminal accessor returns the first error.
   One check per extraction instead of one per hop.
2. **Scalar accessors require size 1** and read the item's scalar storage directly —
   no slice clone (this is half the point; `ToUint` clones for a single-value read).
   Size ≠ 1 is an error naming the actual size.
3. **No cross-family coercion**: `Uint` on an `IntItem` is an error, not a conversion.
   Width widening inside a family (U1/U2/U4/U8 → uint64) follows the existing `ToUint` contract.
4. **Deferred construction errors** (`Item.Error() != nil`) surface from the accessor,
   consistent with the `ToXxx` methods.
5. **Error messages carry position context**: the failing hop's index and depth,
   plus got/want types — e.g. `secs2: cursor at index 3 (depth 2): got A, want U4`.
   Full-path recording is deliberately out:
   it would require a heap allocation on every `At`,
   and the failing hop plus depth locates the problem in practice.

## Zero-allocation contract

`Cursor` is a value struct; `At` returns a new value; errors are constructed only on the failure path.
Rule `600-perf-sec.md` applies:
add a benchmark asserting **0 allocs/op** for a representative happy-path extraction (`NewCursor(item).At(0, 1).Uint()`),
and wire it into the benchmark suite so a regression is visible.

## Steps

1. Read `secs2/item.go`, `secs2/list.go`, and one scalar item file (`secs2/uint.go`) first.
   Confirm the unexported scalar/values storage split the accessors will read.
2. Implement `cursor.go` with the API above.
3. Godoc: package-level example (`ExampleCursor`) showing an S1F14-style extraction,
   plus the doc.go "Parsing"/access section gains a pointer to the cursor.
4. Tests, benchmark, lint, CHANGELOG (`### Added`).

## Tests

- Happy path per accessor, including width variants (U1 vs U8 → `Uint`).
- Each failure class: index out of range, non-list intermediate, wrong type family,
  size ≠ 1, deferred construction error, nil item.
  Assert the error *message content* (index, depth, got/want) — the context is the feature.
- Error accumulation: the first failing hop wins; later hops do not mask it.
- `Binary` returns a clone (mutating the result does not affect the item).
- Benchmark: 0 allocs/op on the happy path.

## Acceptance

- `make test-all` green, lint clean, benchmark shows 0 allocs/op.
- The GEM decoder plan (same release) can consume the cursor —
  sequence this item first.

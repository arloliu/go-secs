# Sub-project 1 — `secs2` immutable Item core (design / spec)

**Status:** design approved 2026-06-28, ready for implementation plan.
**Depends on:** [`00-v2-proposal.md`](00-v2-proposal.md) (decisions D1–D8),
[`01-bakeoff-results.md`](01-bakeoff-results.md) (memoization + allocation verdicts).
**Module:** `github.com/arloliu/go-secs/v2`, branch `v2`, Go floor `1.26.0`.

---

## 1. Goal

Replace the v1 `secs2.Item` with a **deeply immutable** item model whose accessors
expose **no** mutable backing storage, removing the `Set*`/`Clone`/`Free`/pooling
machinery entirely. This is the leaf-level foundation the v2 message model
(sub-project 2) builds on.

The v1 audit (the motivating problem): *every* slice-returning accessor
(`ToInt`, `ToBinary`, `ToList`, `Values() any`, …) returns a direct reference to
the item's internal backing array, so v1 immutability is only a convention. v2
makes it real.

## 2. Scope

**In scope:** the immutable `secs2.Item` interface and its concrete leaf types;
the hybrid accessor surface; the construction surface (shortcuts + `New*`); the
deferred construction-error model; SECS-II item encode (`AppendTo`/`ToBytes`) and
SECS-II item decode (`secs2.Decode`); `ToSML`; removal of `Set*`/`Clone`/`Free`/
`Values() any`/pooling.

**Out of scope (later sub-projects):**
- Message-level body representation, the raw-frame view, and hybrid-by-provenance
  (sub-project 2). secs2 owns *item-level* encode/decode; the message layer owns
  frame parsing and the lazy invocation of `secs2.Decode`.
- Connection/transport/state (deferred, D4).
- `gem` high-level builder API and the v1→v2 codemod (sub-project 7).

## 3. Decisions adopted (from the brainstorm + proposal)

| Topic | Decision |
|-------|----------|
| Accessor API | **Hybrid**: zero-copy `iter.Seq` + indexed accessors as the hot path, **plus** copy-returning `To*()` for convenience and migration. |
| Construction errors | **Deferred** — constructors return `Item`, stash validation errors, surfaced via `Error()`; never panic. |
| Safety checkpoint | Message `Build()` (sub-project 2, D6) validates the body tree's `Error()` and refuses to build a message carrying an errored item. This is what makes deferred errors safe. |
| Error channels | `Error()` = construction/validation (this sub-project). `DecodeErr()` = lazy wire→item decode (D5, message level, sub-project 2). Kept distinct. |
| Type shape | `Item` stays an **interface** with concrete leaf types (D3 — keep `secs2.Item` simple). |
| `Values() any` | **Removed** (worst leak + type-unsafe; superseded by typed accessors). |
| `Clone()` | **Removed** — immutable values are safe to share; copies are pointless. |
| `Free()` + pooling | **Removed** (D7) — GC owns lifetime; no public `Free`, no `usePool`. |
| Item derivation | **None** in the core (no `WithChild`/`Append`); modify = rebuild. Revisit only on concrete need. |
| Encoding API | **`AppendTo(dst) []byte`** workhorse + `EncodedLen() int` + `ToBytes()=AppendTo(make([]byte, 0, EncodedLen()))` (exact-size prealloc → 1 alloc); none leak internal storage. Decoded items emit owned `raw` bytes. **No per-item `sync.Once` encode cache** — that bake-off verdict is for message-level lazy *decode* (sub-project 2), not item encode. |
| SML scope | `ToSML()` emits v1's **default canonical** format. v1's package-level SML formatting knobs are **not** ported here; configurable SML rendering is owned by the `sml` package (sub-project 3). |
| `Get()` semantics | List-navigation only: scalar `Get()` returns an error (v2 change from v1's self-return); `EmptyItem.Get()` returns self. |

## 4. The `Item` interface

```go
// Item is a deeply immutable SECS-II data item (SEMI E5 §9). All methods are
// safe for concurrent use. No method exposes mutable internal storage.
type Item interface {
    // --- identity / shape ---
    Type() string            // type-name constant (e.g. "ascii", "i4", "list")
    Size() int               // element count: list children / scalar count; byte length for string-backed types
    Error() error            // deferred construction/validation error (nil if valid)

    // --- predicates ---
    IsEmpty() bool
    IsList() bool
    IsBinary() bool
    IsBoolean() bool
    IsASCII() bool
    IsJIS8() bool
    IsLocalizedStr() bool
    IsInt8() bool; IsInt16() bool; IsInt32() bool; IsInt64() bool
    IsUint8() bool; IsUint16() bool; IsUint32() bool; IsUint64() bool
    IsFloat32() bool; IsFloat64() bool

    // --- nested navigation ---
    Get(indices ...int) (Item, error) // walk nested lists; error on bad path/type

    // --- copy-returning accessors (fresh copy each call; safe) ---
    ToList() ([]Item, error)          // shallow copy of children ([]Item; children immutable)
    ToBinary() ([]byte, error)
    ToBoolean() ([]bool, error)
    ToASCII() (string, error)
    ToJIS8() (string, error)
    ToLocalizedStr() (string, error)
    ToLocalizedStrHeader() (uint16, error)
    ToInt() ([]int64, error)
    ToUint() ([]uint64, error)
    ToFloat() ([]float64, error)

    // --- zero-copy indexed accessors (bounds [0,Size()); error-aware) ---
    ItemAt(i int) (Item, error)       // list child
    ByteAt(i int) (byte, error)       // binary
    BoolAt(i int) (bool, error)       // boolean
    IntAt(i int) (int64, error)
    UintAt(i int) (uint64, error)
    FloatAt(i int) (float64, error)

    // --- zero-copy iterators & bulk read (happy-path; see §6 on type/err handling) ---
    Items() iter.Seq[Item]            // list children
    Bools() iter.Seq[bool]
    Ints() iter.Seq[int64]
    Uints() iter.Seq[uint64]
    Floats() iter.Seq[float64]
    AppendBinaryTo(dst []byte) []byte // binary bulk read, zero internal exposure (see §6)

    // --- encoding ---
    EncodedLen() int                  // total SECS-II wire byte length (header + payload, recursive)
    AppendTo(dst []byte) []byte       // append SECS-II wire bytes into caller buffer (no internal exposure)
    ToBytes() []byte                  // = AppendTo(make([]byte, 0, EncodedLen())); a fresh, caller-owned slice (1 alloc)
    ToSML() string                    // SML text representation (canonical default format)
}
```

Notes:
- The full predicate / `To*()` set is carried verbatim from v1 so correct v1
  call sites compile unchanged (with the copy-vs-alias behavior change in §8).
- `Size()` is the element count and the bound for the `*At` accessors. There is
  no separate `Len()` (avoids a duplicate concept).
- **Removed vs v1:** `SetValues`, `Clone`, `Free`, `Values() any`.

## 5. Concrete leaf types

Carried from v1, made immutable (unexported fields, no setters). Backing storage
is never returned; only copies / iterators / index reads.

| Type | Backing | Accessor family |
|------|---------|-----------------|
| `ASCIIItem` | `string` | `ToASCII` (string — already leak-free) |
| `JIS8Item` | `string` | `ToJIS8` |
| `LocalizedStrItem` | `uint16` + `string` | `ToLocalizedStr`, `ToLocalizedStrHeader` |
| `BinaryItem` | `[]byte` | `ToBinary` (copy), `ByteAt`, `AppendBinaryTo` |
| `BooleanItem` | `[]bool` | `ToBoolean` (copy), `BoolAt`, `Bools` |
| `IntItem` | `byteSize` + `[]int64` | `ToInt`, `IntAt`, `Ints` |
| `UintItem` | `byteSize` + `[]uint64` | `ToUint`, `UintAt`, `Uints` |
| `FloatItem` | `byteSize` + `[]float64` | `ToFloat`, `FloatAt`, `Floats` |
| `ListItem` | `[]Item` | `ToList`, `ItemAt`, `Items`, `Get` |
| `EmptyItem` | — | singleton; `IsEmpty()==true`, `Size()==0`, `ToBytes()==[]byte{}` |

Each carries an immutable `err error` (the deferred construction error); decoded
items additionally carry immutable owned `raw []byte` (§7). String-backed types
need no copy on read because Go strings are immutable.

`ASCIIItem` accepts **any** input string/bytes without rejecting non-ASCII bytes,
matching v1's default (non-strict) mode. The v1 strict-validation knob is not
carried into `secs2` v2; validation/formatting strictness belongs to the `sml`
package (sub-project 3).

## 6. Accessor semantics (the leak fix)

- **Copy accessors** (`To*`) allocate and return a fresh slice/string every call.
  For `ToList`, the returned `[]Item` is a fresh slice; the `Item` elements are
  themselves immutable, so a shallow copy is sufficient and safe.
- **Indexed accessors** (`*At`) return a single value; `error` if `i` is out of
  `[0,Size())`, the item's `Error()` is non-nil, or the type does not match.
- **Iterators** (`iter.Seq[...]`) are the zero-allocation happy path. Because
  `iter.Seq` carries no error channel, the contract is: an iterator yields the
  element sequence **iff** the concrete type matches and `Error()==nil`;
  otherwise it yields nothing (empty sequence). Error-aware callers use `*At`,
  `To*`, or check `Error()`/`Is*()` first. This keeps `for v := range item.Ints()`
  clean for the common, type-correct case.
- **Binary** uses `AppendBinaryTo(dst) []byte` as its zero-copy primitive
  (append-into-caller-buffer, exposes nothing internal) alongside `ByteAt` and the
  `ToBinary` copy. (A `byte` iterator was rejected as unidiomatic.)

## 7. Encoding & internals

- **`AppendTo(dst []byte) []byte`** is the encoding workhorse: it appends the
  item's SECS-II wire bytes (header + payload, recursive for lists via children's
  `AppendTo`) into the caller's buffer, exposing no internal storage. **`EncodedLen()
  int`** returns the exact total wire byte length (header + payload, recursive;
  `len(raw)` for decoded items). `ToBytes()` is `AppendTo(make([]byte, 0,
  EncodedLen()))` — an exact-size prealloc, so it is exactly **one** allocation
  even for large/nested payloads. Neither aliases item state, so mutating the
  result cannot corrupt the item (resolves the shared-cache leak; the message
  layer sizes its send buffer via `EncodedLen` and fills it via `AppendTo` with no
  per-item copy).
- **Decoded items** carry an immutable, decoder-**owned** `raw []byte`; their
  `AppendTo` emits it directly (no re-encode). Because it is owned (a copy made by
  `Decode`, never the caller's input — §9) and never mutated, sharing it across a
  decoded subtree is safe.
- **No per-item encode memoization / `sync.Once`.** The bake-off's `sync.Once`
  verdict applies to *message-level lazy decode* (decode the body once — sub-project
  2), **not** to item encode. Constructed items encode on demand in `AppendTo`
  (a linear write; the message layer caches the assembled body, so repeated item
  encodes are not a hot path). Per-item encode caching is deliberately deferred
  (YAGNI) until a profile justifies it.
- `ToSML()` is computed on demand (no memoization required).
- **No pooling.** Items are allocated normally; GC owns lifetime. There are no
  public `*WithBytes` constructors; `secs2.Decode` constructs items in-package and
  sets `raw` directly.
- Items are safe for concurrent reads by construction (immutable; `AppendTo`/
  `ToBytes` only read immutable state and write to the caller's buffer).

## 8. Construction surface

Terse shortcuts and `New*` constructors are retained; all return `Item` and defer
errors (§3). v1's nested-builder ergonomics are preserved unchanged.

```go
// shortcuts (unchanged names): L A J W B BOOLEAN I1 I2 I4 I8 U1 U2 U4 U8 F4 F8
msg := L(
    A("MODEL"),
    I4(1, 2, 3),
    L(B(0x01, 0x02), BOOLEAN(true, false)),
)
if err := msg.Error(); err != nil { /* aggregated incl. children */ }
```

- `New*Item` constructors: `NewASCIIItem`, `NewJIS8Item`, `NewUTF8StrItem`,
  `NewLocalizedStrItem(lsh, s)`, `NewBinaryItem`, `NewBooleanItem`,
  `NewIntItem(byteSize, …)`, `NewUintItem(byteSize, …)`, `NewFloatItem(byteSize, …)`,
  `NewListItem(children…)`, `NewEmptyItem()`.
- Validation (byteSize ∈ valid set; value type/overflow; etc.) sets the deferred
  `err`; `ListItem` aggregates child errors via `errors.Join`. The item is
  immutable, so `Error()` is fixed at construction.
- **Safety checkpoint:** message construction (sub-project 2) rejects a body whose
  `Error()` is non-nil, so a deferred error cannot silently reach the wire.

## 9. Decoding

`secs2.Decode(data []byte) (Item, error)` owns SECS-II item decode (format byte +
length + payload, recursive for lists), moving it out of `hsms` (a v1 layering
smell). It is eager and idiomatic `(Item, error)`; the message layer (sub-project
2) invokes it lazily for decoded-from-wire bodies. Contract:

- **Owns its bytes.** `Decode` copies the input once into an owned buffer and sets
  each decoded item's `raw` to a sub-slice of that owned copy — never the caller's
  slice. Mutating the caller's input after `Decode` returns cannot affect the
  (immutable) result.
- **Empty / header-only input → `EmptyItem`**, not an error (matches v1
  `DecodeSECS2Item` and makes `Decode(EmptyItem.ToBytes())` round-trip).
- **Depth guard.** Nested lists are capped at `MaxListDepth = 64` (ported from v1);
  exceeding it is an error, preventing stack exhaustion on malicious input.
- **Errors** on malformed input: bad format code, truncated length/payload,
  oversized (> `MaxByteSize`).

## 10. Migration (v1 → v2, items only)

| v1 | v2 |
|----|----|
| `item.SetValues(…)` | rebuild the item (immutable; no setter) |
| `item.Clone()` | drop — share the immutable item directly |
| `item.Free()`, `secs2.UsePool(…)`, `IsUsePool()` | drop — GC-owned, no pooling API |
| `v := item.Values().([]int64)` | `item.ToInt()` (copy) or `for v := range item.Ints()` (zero-copy) |
| accessor result mutated in place | accessor now returns a **copy**; mutate your copy or rebuild (v1 in-place mutation was a latent bug) |

## 11. Success criteria

- **Round-trips:** for every type, `Decode(item.ToBytes())` reconstructs an equal
  item; SML round-trips where defined.
- **No leak (teeth test):** mutating any `To*` result, and exhausting every
  iterator, leaves the source item byte-identical (`ToBytes` unchanged). A test
  proves a returned slice does not alias internal storage.
- **Deferred errors:** invalid construction sets `Error()`; a list with an invalid
  child reports the aggregated error; valid construction reports `nil`.
- **Concurrency:** `-race` test with N goroutines calling accessors + `AppendTo`/
  `ToBytes` concurrently on a shared item passes.
- **Allocations:** `AppendTo` into a reused buffer is 0 allocs; `ToBytes()`
  allocates exactly the one result slice (exact-size prealloc via `EncodedLen`);
  `To*` copies allocate exactly one backing slice; direct `for range x.Ints()` is
  0 allocs **when `x` is the concrete item type** (the compiler inlines/stack-allocates
  the iterator closure). Called through the `Item` *interface*, an iterator costs
  ~3 allocs (interface dispatch blocks inlining) — still cheaper than copying a large
  slice. Benchmarks recorded.
- **`EncodedLen` invariant:** for every item, `EncodedLen() == len(ToBytes())`
  (and equals the length `AppendTo` adds). Tested across all types.
- Scoped to `./secs2/`: `go build ./secs2/`, `go vet ./secs2/`, scoped
  `golangci-lint run ./secs2/...` (0 issues), `go test -race ./secs2/` green. (The
  rest of the module is intentionally not building until later sub-projects.)

## 12. Open implementation details (resolve in the plan, not blocking design)

- Whether `LocalizedStrItem` shares the string-backed base or stays separate
  (v1: not pooled, separate) — moot now that pooling is gone; fold into the
  string-backed base if clean.
- `AppendBinaryTo` vs a future `io.Writer`-based sink (keep append-only per the
  proposal's no-`io.Writer`-leak stance, §5.E of the proposal).
```

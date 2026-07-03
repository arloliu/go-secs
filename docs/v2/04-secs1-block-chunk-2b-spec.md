# Sub-project 2b — SECS-I block chunk interface (design / spec)

**Status:** design approved 2026-06-29, ready for implementation plan.
**Depends on:** sub-project 1 (`docs/v2/02-secs2-immutable-item-spec.md`) — `secs2.Decode`/`AppendTo`;
sub-project 2a (`docs/v2/03-hsms-immutable-message-2a-spec.md`) — the frozen `internal/wire.Body`
(`Len`/`AppendTo`/`Buffers`/`Chunk`) and `wire.Chunk` stub, `wire.AdoptBody`/`wire.FromItem`.
Proposal decisions D1–D8 (`docs/v2/00-v2-proposal.md` §4–§5, esp. the SECS-I body/block paragraphs
lines 347–365); bake-off (`docs/v2/01-bakeoff-results.md`, which did NOT cover SECS-I).
**Module:** `github.com/arloliu/go-secs/v2`, branch `v2`, Go floor `1.26.0`.

---

## 1. Goal

Replace the v1 exported, mutable, per-block-copying SECS-I framing (`secs1.Block` +
`SplitMessage`/`AssembleMessage`/`ParseBlock`) with an **unexported, immutable** framing layer that
chunks the **shared `wire.Body`** into ≤244-byte SECS-I blocks **zero-copy** (via `wire.Chunk`
sub-views, eliminating v1's per-block `make`+`copy` at `secs1/message.go:48-49`) and reassembles
received blocks back into body bytes for lazy decode. This validates the 2a body/chunk interface
against SECS-I — in **both** directions — before `DataMessage` is frozen. The block-transfer
*protocol* (ENQ/EOT/ACK/NAK contention, T1–T4, the `openMessages` reassembly state machine) and the
SECS-I connection stay **deferred to sub-project 5** (D4).

## 2. Scope (sub-project 2b)

**In scope:**
- `internal/wire` additions (leaf, no new imports): a **bounds guard** on `Body.Chunk(off,n)` and a
  receive-side constructor `ChunkOf([]byte) Chunk` so the parser can wrap a read sub-slice zero-copy.
- `secs1` reduced to the **block-framing layer**: an unexported, immutable `block` value type
  (`[10]byte` header + `wire.Chunk` body); the **splitter** `splitBody(wire.Body, messageHeader)
  (iter.Seq[block], error)`; per-block wire serialization `block.appendTo` (length + header + body +
  checksum, SEMI E4 §7/§8); the wire parser `parseBlock` (checksum-validated); and the **stateless**
  reassembler `assembleBlocks([]block) (messageHeader, wire.Body, error)`.
- Deleting the SECS-I **connection/transport/state machinery** from v2 `secs1` (preserved on `main`,
  re-adapted in sub-project 5) — mirrors 2a's reduction of `hsms` to the message layer.

**Out of scope (later):**
- **Sub-project 5** — block-transfer protocol (ENQ/EOT/ACK/NAK, contention, RTY), T1–T4 timers, the
  stateful `messageAssembler`/`openMessages` map and inter-block ordering *over time*, and the SECS-I
  connection rewrite. 2b provides the stateless framing primitives those will drive.
- The **HSMS↔SECS-I relay accessor** (extracting a `wire.Body` out of an `hsms.DataMessage`): a
  connection concern, deferred to sub-project 5. **2b does NOT modify `hsms`** (Q2) — the splitter
  consumes a `wire.Body` directly. This consciously revises the 2a forward note ("2b will modify
  hsms"): the body interface is already shared, so validating it against SECS-I needs no `hsms` change.
- A v2 `secs1` *message* type and `gem`/codemod (later sub-projects).

## 3. Decisions adopted

| Topic | Decision | Source |
|-------|----------|--------|
| Block visibility | **unexported, immutable `block`** (v1's `Block` was exported + mutable, `block.go:45-159`). A block is a transport-framing detail, not a user object. | proposal lines 358-364 |
| Block representation | **value type**: `header [10]byte` + `body wire.Chunk` (zero-copy sub-view); read-only value accessors; **no setters, no `With*`** (blocks are born complete from splitter/parser); **no `sync.Once`/pointer fields** — must stay value-copyable for `iter.Seq`. | Q1; proposal |
| Splitter API | **`splitBody(body wire.Body, h messageHeader) (iter.Seq[block], error)`** — lazy, ~O(1) alloc (closure), **zero body copy**; single-pass fits SECS-I's strictly-sequential send-with-in-loop-retry. Up-front error guards oversize/range (§5). | Q1 |
| Body access | splitter consumes **`wire.Body`** directly (secs1 imports `internal/wire`; same module). **No `hsms` change in 2b.** Relay accessor → sub-project 5. | Q2 |
| Scope | **both directions, stateless** — split + serialize + parse + reassemble. Stateful protocol → sub-project 5. | Q3 |
| `wire` additions | `Body.Chunk` **bounds guard** (programming-error guard — panics with a descriptive message; callers compute valid offsets) + `ChunkOf([]byte) Chunk` (receive path). | 2a forward note |
| Reassembly output | `assembleBlocks` concatenates block bodies into **one owned buffer** → `wire.AdoptBody` (zero-copy retain) → returned `wire.Body` for lazy `secs2.Decode` later. Coalesce is 1 alloc + 1 copy (inherent — blocks arrive in separate reads). | §6 |
| Block buffer ownership | `parseBlock` is **zero-copy**: the block body aliases the caller's `rest`. Contract (mirrors 2a's internal zero-copy decode entry, §7): `rest` must be caller-**owned** and not mutated/reused while the block is live — until `assembleBlocks` coalesces it (or it is discarded). SP5's receiver allocates a fresh per-block buffer (v1 already does, `block_transport.go:182`), so this holds. `assembleBlocks` coalesces into a **fresh, independent** buffer, so after it returns the per-block buffers may be reused/freed. | I1; 2a §7 |
| Checksum computation | **No standalone `block.checksum()`** (it would force an `AppendTo(nil)` copy through `wire.Chunk` — the pure-waste pattern the 2a review flagged). `appendTo` sums the header+body region it just wrote into `dst` **in place**; `parseBlock` sums `rest[:lengthByte]` directly. Both allocation-free. | I2 |
| Block numbering | **SEMI E4 §8 strict: 1..N**, contiguous, reset to 1 per message — including a header-only single block (block 1). v1 emitted **0** for header-only (`message.go:35`); v2 is a clean break and E4 is authoritative. Flagged for SP5 conformance (§12). | E4 §8; map |
| Connection machinery | **DELETED** from v2 `secs1` (preserved on `main`; re-adapted in SP5). secs1 = framing layer only; imports `internal/wire` (+ `encoding/binary`, std) — **no `hsms` import**. | mirrors 2a |
| Checksum / header | **SEMI E4 §8 exactly** — see §9. | E4; `docs/secs1/01-block-structure.md` |

## 4. Package API (all unexported — secs1-internal, consumed by SP5 within the package)

```go
package secs1

import (
	"iter"

	"github.com/arloliu/go-secs/v2/internal/wire"
)

// messageHeader is the per-message (block-invariant) SECS-I header: the fields that are
// identical across every block and retransmission of one message (SEMI E4 §8). Block number
// and the E-bit are per-block and assigned by the splitter, not carried here.
type messageHeader struct {
	deviceID    uint16  // 15-bit (0..0x7FFF)
	rBit        bool    // direction: false = to equipment, true = to host (E4 §8, byte 0 bit 7)
	stream      uint8   // 0..127 (7-bit)
	function    uint8   // 0..255
	waitBit     bool    // reply expected (E4 §8, byte 2 bit 7)
	systemBytes [4]byte // transaction id (E4 §8, bytes 6..9)
}

// block is an immutable SECS-I transport block: a 10-byte header value plus a zero-copy body
// sub-view of the shared frame. Value type — copyable, no locks — so it can be yielded by iter.Seq.
type block struct {
	header [10]byte   // SEMI E4 §8 layout (see §9)
	body   wire.Chunk // 0..244 bytes; zero-copy sub-view of the shared body (send) or read buffer (recv)
}

// read-only accessors (decode the bit-packed header on demand):
func (b block) deviceID() uint16     // header[0:2] & 0x7FFF
func (b block) rBit() bool           // header[0] & 0x80
func (b block) stream() uint8        // header[2] & 0x7F
func (b block) function() uint8      // header[3]
func (b block) waitBit() bool        // header[2] & 0x80
func (b block) blockNumber() uint16  // header[4:6] & 0x7FFF
func (b block) eBit() bool           // header[4] & 0x80  (true = last block)
func (b block) systemBytes() [4]byte // header[6:10]

// appendTo emits [lengthByte][header(10)][body][checksum(2, big-endian)]. The checksum is summed
// IN PLACE over the header+body bytes just written to dst — there is no standalone checksum()
// method, which would force a copy through wire.Chunk (an AppendTo(nil), the pure-waste pattern the
// 2a review flagged). See §7.
func (b block) appendTo(dst []byte) []byte

// splitBody partitions body into ≤244-byte blocks, assigning block numbers 1..N and the E-bit on
// the last block. Empty body ⇒ exactly one header-only block (block 1, E-bit set). The returned
// blocks hold zero-copy wire.Chunk sub-views of body — no per-block copy. Returns an error (and a
// nil iterator) up front if deviceID > 0x7FFF, stream > 0x7F, or body.Len() exceeds the SECS-I
// maximum (244 * 32767). Each yielded block re-computes its checksum in appendTo (cheap, ≤244 bytes;
// blocks carry no memoization so they stay value-copyable).
func splitBody(body wire.Body, h messageHeader) (iter.Seq[block], error)

// parseBlock deserializes one wire block: lengthByte then rest = header(10)+body+checksum(2). It
// validates the length range (10..254) and length/data agreement, wraps the body zero-copy via
// wire.ChunkOf over rest, and verifies the checksum by summing rest[:lengthByte] (alloc-free,
// ErrChecksumMismatch on failure). OWNERSHIP: rest must be caller-owned and not mutated or reused
// while the returned block is live (zero-copy; the body aliases rest until assembleBlocks coalesces
// it or the block is discarded — see the §3 "Block buffer ownership" row).
func parseBlock(lengthByte byte, rest []byte) (block, error)

// assembleBlocks reassembles an ordered slice of received blocks into the message body. It is
// stateless (the openMessages map / T1-T4 / ordering-over-time live in SP5): it validates block
// numbers are 1..len contiguous, exactly the last block has the E-bit, and the message-level header
// fields (deviceID/rBit/stream/function/waitBit/systemBytes) are identical across all blocks; then
// concatenates the block bodies into one owned buffer and returns it as a wire.Body (via AdoptBody)
// for lazy secs2.Decode by the caller (SP5).
func assembleBlocks(blocks []block) (messageHeader, wire.Body, error)
```

```go
package wire // internal/wire — additions for 2b

// ChunkOf wraps an already-owned byte slice as a Chunk, zero-copy. Used by the SECS-I parser to
// view a received block body as a sub-slice of the read buffer without copying.
func ChunkOf(b []byte) Chunk { return Chunk{b: b} }

// Body.Chunk(off, n) gains a bounds guard: it panics with a descriptive message unless
// 0 <= off, 0 <= n, and off+n <= Len(). This guards a caller (the splitter) that must compute
// valid offsets; untrusted wire input is length-validated in parseBlock, not here.
```

## 5. Splitter — partition and validation

```
body bytes (encoded SECS-II item) = wire.Body, length L = body.Len()
guard: deviceID <= 0x7FFF, stream <= 0x7F, L <= MaxBlockBodySize*MaxBlockNumber (244*32767)
if L == 0:                       yield 1 header-only block: blockNum 1, E-bit set, empty body chunk
else for off := 0; off < L; off += 244:
    n      := min(244, L-off)
    isLast := off+n == L
    chunk  := body.Chunk(off, n)          // zero-copy sub-view (treeBody encodes once on first Chunk)
    yield block{ header: buildHeader(h, blockNum, isLast), body: chunk }
    blockNum++
```

- **Zero block-copy:** the per-block body is a `wire.Chunk` sub-view of the one shared body buffer;
  no `make`/`copy` per block (the v1 cost). For a `treeBody`, the first `Chunk` triggers the
  `sync.Once` encode; all blocks then sub-slice the one memoized buffer.
- **Allocations:** `splitBody` returns ~O(1) (the iterator closure); independent of block count.
- **Retry:** the SP5 sender ranges the iterator, and on NAK/timeout re-`appendTo`s the *current*
  block (re-checksumming) until ACK/abort before advancing — single-pass covers SECS-I exactly.

## 6. Reassembler — coalesce and hand off

```
validate: non-empty; block numbers 1..len contiguous; exactly blocks[len-1].eBit() (all earlier E=0);
          all blocks share identical deviceID/rBit/stream/function/waitBit/systemBytes
total := Σ block.body.Len(); buf := make([]byte, 0, total)
for each block: buf = block.body.AppendTo(buf)        // coalesce (wire.Chunk.AppendTo)
return messageHeader(of blocks[0]), wire.AdoptBody(buf), nil
```

- The coalesced `buf` is freshly owned and **independent of the input block buffers** (the coalesce
  copies), so `wire.AdoptBody(buf)` retains it zero-copy and the per-block `rest` buffers may be
  reused/freed once `assembleBlocks` returns. SP5 builds the message and `Item()` lazily
  `secs2.Decode`s it. The 1 alloc + 1 copy is inherent: received blocks arrive in separate TCP reads
  and must be made contiguous before decode.
- A header-only message (single block, empty body) yields a zero-length body ⇒ `secs2.Decode` →
  `EmptyItem` (the SP1/2a "validly empty" rule).

## 7. Block wire layout / encode-decode

- **Block wire frame** (SEMI E4 §7, `docs/secs1/01-block-structure.md`): `[lengthByte(1)]
  [header(10)][body(0..244)][checksum(2, big-endian)]`, total 13..257 bytes. `lengthByte =
  10 + len(body)` (range 10..254); it does **not** count itself or the checksum.
- **`block.appendTo`** appends `[lengthByte][header(10)]`, then the body via `wire.Chunk.AppendTo`,
  then sums the header+body region it just wrote into `dst` **in place** (alloc-free — no standalone
  `checksum()` over the `Chunk`, which would force an `AppendTo(nil)` copy), and appends the 2
  big-endian checksum bytes. The body is materialized exactly once (the `Chunk.AppendTo` that must
  happen anyway to put it on the wire), and summed where it already sits in `dst`.
- **`parseBlock`** reverses it. Convert `n := int(lengthByte)` **first** — `lengthByte` is a `byte`,
  so `lengthByte+2` would wrap at `lengthByte == 254`. Length-range check (`n` in 10..254) →
  `ErrInvalidLength`; `len(rest) == n+2` else mismatch error; body = `wire.ChunkOf(rest[10:n])`
  (zero-copy, see the §3 ownership row); checksum = sum of `rest[:n]` (header+body, alloc-free)
  compared against `binary.BigEndian.Uint16(rest[n:n+2])` → `ErrChecksumMismatch`.
- Size constants retained from v1 (`block.go:8-22`): `maxBlockBodySize = 244`, `minBlockLength = 10`,
  `maxBlockLength = 254`, `blockHeaderSize = 10`, `checksumSize = 2`, plus `maxBlockNumber = 32767`.

## 8. Error model (D5)

Idiomatic `(T, error)` throughout; sentinel errors for wire-validation failures:
- `splitBody` returns `(nil, error)` on the up-front guards (oversize body, deviceID/stream range).
- `parseBlock` returns `(block{}, error)` — `ErrInvalidLength`, length/data mismatch, `ErrChecksumMismatch`.
- `assembleBlocks` returns `(_, nil, error)` on empty input, block-number gap/duplicate, missing or
  misplaced E-bit, or cross-block header-field mismatch.
- No panics on untrusted input; the only panic is the `Body.Chunk` programming-error bounds guard.

## 9. SEMI ground truth (must satisfy) — SEMI E4 §8

**10-byte block header** (`docs/secs1/01-block-structure.md` §2):

| Byte | Bit 7 (MSB) | Bits 6..0 | Field |
|------|-------------|-----------|-------|
| 0 | **R-bit** (0=to equip, 1=to host) | DeviceID hi 7 | 15-bit DeviceID |
| 1 | DeviceID lo 8 | — | |
| 2 | **W-bit** (1=reply expected) | Stream (0..127) | |
| 3 | Function (0..255) | — | |
| 4 | **E-bit** (1=last block) | BlockNumber hi 7 | 15-bit BlockNumber (1..32767) |
| 5 | BlockNumber lo 8 | — | |
| 6..9 | System bytes (4) | — | transaction id |

- Multi-byte fields are **big-endian**. DeviceID and BlockNumber are 15-bit (top bit is R/E).
- **Checksum** (E4 §8.3 / doc §3): 16-bit unsigned **arithmetic sum of every byte of header + body**
  (NOT the length byte, NOT the checksum); written big-endian (hi byte first). Truncate via `& 0xFFFF`.
- Shared with HSMS: bytes 2-3 (W/stream, function) and 6-9 (system bytes) match the 2a HSMS header
  (E37 §8); only bytes 0-1 (R+deviceID vs session) and 4-5 (E+blockNum vs PType/SType) differ. **Body
  bytes are identical** (E5 is transport-neutral) — the premise that lets one `wire.Body` serve both.

## 10. Migration (v1 → v2, SECS-I framing layer)

| v1 | v2 |
|----|----|
| exported mutable `Block{Header [10]byte; Body []byte}` + setters | unexported immutable `block{header [10]byte; body wire.Chunk}`, accessors only |
| `SplitMessage(*hsms.DataMessage, deviceID, isEquip) []*Block`, `make`+`copy` per block | `splitBody(wire.Body, messageHeader) (iter.Seq[block], error)`, zero-copy `wire.Chunk` |
| `(*Block).Pack() []byte` | `block.appendTo(dst []byte) []byte` |
| `ParseBlock(lengthByte, data) (*Block, error)` (copies body) | `parseBlock(lengthByte, rest) (block, error)` (`wire.ChunkOf`, zero-copy) |
| `AssembleMessage([]*Block) (*hsms.DataMessage, error)` (decodes inline) | `assembleBlocks([]block) (messageHeader, wire.Body, error)` (coalesce only; decode is lazy/SP5) |
| stateful `messageAssembler` / `openMessages` / T4 | deferred to sub-project 5 |
| header-only block number = 0 | header-only block number = **1** (E4-strict; §12 conformance note) |

## 11. Success criteria

- **Round-trip (both directions):** for an item → `wire.FromItem` → `splitBody` → for each block
  `appendTo` → `parseBlock` → `assembleBlocks` → body bytes **equal** the original encoded body, and
  `secs2.Decode(body)` is **value-equal** to the original item. Cover: header-only (empty body),
  single block (1..244 B), exact boundary (244 B), multi-block (245 B, 488 B, 489 B), and a large
  body spanning many blocks.
- **Zero-copy split (behavioral — no public byte accessor):** build a body over an owned buffer via
  `wire.AdoptBody(buf)`, `splitBody`, then mutate `buf` in a block's body region and observe the
  change through that block's `appendTo` output — proving the block body aliases `buf`, not a copy.
  Strict pointer-identity alias checks for `Body.Chunk`/`ChunkOf` live in **`internal/wire`'s own
  white-box tests** (where `Chunk`'s backing is visible); **do not** add a public `Bytes` accessor to
  satisfy the wording. **No per-block body copy:** over a `wire.AdoptBody` raw-frame body (no encode
  step), `splitBody` + iterating all blocks performs zero body-copy allocations — alloc test. (A
  `*treeBody` allocates its memoized encode once on the first `Chunk`; that one-time cost is expected
  and excluded from this check.)
- **Parse ownership / assemble independence:** `parseBlock`'s block body aliases its `rest`
  (behavioral mutate-`rest`→observe-`appendTo` test). After `assembleBlocks` returns, mutating the
  input block buffers does **not** change the assembled `wire.Body` (proves the coalesce copied, so
  per-block buffers are free to reuse — the I1 ownership boundary).
- **Checksum / header bit-packing:** `appendTo` emits the SEMI E4 checksum (summed in place, no
  standalone `checksum()`); `parseBlock` rejects a one-byte-corrupted block (`ErrChecksumMismatch`),
  an out-of-range length byte (<10, >254), and a length/data-length mismatch. Every header field
  round-trips through `[10]byte` at boundary values: deviceID `0x7FFF` and `0`, blockNumber `0x7FFF`
  and `1`, stream `127`, all R/W/E bits set and clear.
- **Block numbering / E-bit:** `splitBody` sets the E-bit only on the last block and numbers blocks
  1..N; a header-only body yields one block (number 1, E-bit set).
- **Reassembler validation:** `assembleBlocks` rejects empty input, a block-number gap/duplicate, a
  missing E-bit (no terminal block), an E-bit before the last block, and a block whose message-level
  header fields differ from the first block's.
- **wire additions:** `Body.Chunk` with out-of-range `off`/`n` panics with a descriptive message;
  in-range returns the exact sub-view; `ChunkOf` wraps a slice zero-copy (alias assert).
- **Builds in isolation:** `go build ./secs1/ ./internal/wire/`, `go test -race ./secs1/
  ./internal/wire/`, scoped `golangci-lint` — green. (`hsmsss` and the SECS-I connection are rewritten
  in SP5; module-wide `make ci` stays red for the duration.)

## 12. Open implementation details (resolve in the plan)

- **Header-only block number (E4-strict vs v1):** the spec adopts E4-strict (block 1 for a header-only
  single block; v1 used 0). Decision stands for 2b; **flag for SP5 conformance testing** against real
  equipment / a SECS-I corpus — if peers require 0, revisit in the connection sub-project (framing is
  isolated, so the change would be local to `buildHeader`).
- **`messageHeader` placement / reuse:** whether the per-message header fields warrant a shared
  representation with the 2a HSMS header (they overlap on bytes 2-3, 6-9) or stay a secs1-local value.
  Default: secs1-local (the layouts differ on bytes 0-1, 4-5; sharing would over-couple).
- **Exact file operations:** which v1 `secs1/*.go` are rewritten (`block.go`, `message.go`) vs deleted
  (connection/transport/session/config/metric + their tests), enumerated in the plan — mirrors the 2a
  deletion list.
- **`Body.Chunk` guard form:** panic-with-message (chosen — programming-error guard) vs `(Chunk, bool)`.
  The plan locks the chosen form and its test.

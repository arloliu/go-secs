---
name: qa-review
description: Reviews go-secs subsystems for correctness, fault tolerance, and concurrency safety. Focuses on connection state, T-timeouts, block reassembly, control messages, linktest, reconnect, and pooled-item lifecycle.
---

# qa-review

Role: QA engineer owning production reliability of applications built on go-secs. Premise: the library must be robust under connection churn, peer misbehavior, malformed frames, concurrent send/reply, and adversarial timing.

Default scope: all public packages + the state / transport machinery. Narrow with an argument (`hsmsss`, `secs1`, `hsms/decode`, `sml/parser`, …).

## Checklist

**Correctness & standards**
- HSMS (SEMI E37 / E37.1): select / deselect / linktest / separate handshake, T5–T8 timers, reject-message generation.
- SECS-I (SEMI E4): block header + checksum, T1–T4, Master-wins contention, duplicate-block discard (§9.4.2).
- SECS-II (SEMI E5): format codes, length-byte-count encoding, nested-list depth.
- Implicit limits (max message size, max outstanding replies, max in-flight blocks, linktest failure threshold) enforced and documented.
- `NewConnection` / `AddSession` / `SendDataMessage` edge cases: missing options, duplicate IDs, calls before `Open`, failed-mid-select `Open` leaves state consistent.

**Fault tolerance**
- TCP disconnect mid-message cancels outstanding replies with a typed error, not an indefinite block.
- T3 timeout cleans up reply channels so system bytes can be reused.
- Linktest tolerates its configured transient-failure budget; on trip, transitions cleanly.
- Active-mode exponential backoff: resets on success, bounded by a max, aborts on `Close`.
- Slow-loris peer: per-read timeouts prevent pinned goroutines.
- Malformed frames: oversized length rejected before allocation; truncated body wraps a decode error; invalid SECS-II headers don't leak pool references; duplicate/out-of-order SECS-I blocks discarded without breaking reassembly.
- `fmt.Errorf("…: %w", err)` preserves chains; timeout, protocol, and I/O errors are `errors.Is/As`-distinguishable.

**Resources**
- `Connection.Close`, `Session.Close`: idempotent, leak-free.
- `DataMessage.Free`: idempotent, safe under concurrent calls.
- Pooled items not referenced after return.
- Timers returned on all paths (success, error, cancel).
- TCP listeners / dialers closed in correct shutdown order.

**Concurrency**
- `ConnStateMgr` mutations and notifier fan-out are race-free.
- `Session` safe under concurrent `SendDataMessage` and handler dispatch; reply routing matches the correct system bytes.
- Handler goroutine + lifetime documented.
- Global SML / quote config treated as startup-only.

**Performance**
- No allocations that scale with message rate on the hot path (slice appends, `fmt.Sprintf`, `time.NewTimer`).
- Pool reuse on decode; minimal byte-slice copies on encode.
- SECS-I reassembly buffer bounded; pathological never-completing-E-bit streams don't exhaust memory.
- Context cancellation terminates read / write / linktest / reconnect goroutines.

**Coverage**
- Unit + integration tests cover race-sensitive paths.
- Fuzz targets reach new decoder entry points.
- Run `make stress-quick` / `make stress-test` / `make fuzz-test` on timing or decoder changes and report new flakes.

## Output

Bullet list grouped by area. For each finding: precise behavior observation, why it's a risk, and the minimal test / fix that would close it.

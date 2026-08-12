---
type: Mechanic
title: Trailing-byte counting — where it's measured and how it reaches TrailingBytes()
description: The shared decodeFirstItem site behind all three secs2 decode entry points, and how hsms's copy-fallback path still routes through it.
tags: [secs2, hsms, decode, e5]
status: stable
generated: {by: "claude/sonnet-5", at: 2026-08-12T00:00:00Z}
verified:
  - {by: "claude/opus-5", at: 2026-08-12T08:33:19Z}
sources:
  - {resource: secs2/decode.go, digest: sha256:8ca1e530a8d03c4a, revision: 3660aa4}
  - {resource: hsms/data_msg.go, digest: sha256:f933e075422d98ed, revision: 3660aa4}
  - {resource: internal/framecodec/owned.go, digest: sha256:9d1b4b90d8205347, revision: 3660aa4}
---

# What it does

`secs2/doc.go` and `docs/specs/secs2-hsms-conformance-audit.md` (D3) already cover the WHAT and WHY
in depth: `Decode`/`DecodeOwned` stay lenient by contract (E5 §9.2 vs. the message-definition rule in
§10.3.1.3(2)), and `DecodeOwnedFrame`'s `int` return is how the transport surfaces the count instead
of rejecting the body.
Neither states WHERE the count is actually measured, that all three public entry points share exactly
ONE measurement site, or how `hsms.DataMessage`'s copy-fallback decode path — a body that is not
already the frame's own owned buffer — still gets counted through that same site rather than a
second, less-exercised one.

# How it works

`decodeFirstItem` (`secs2/decode.go`) is the shared body behind `Decode`, `DecodeOwned`, and
`DecodeOwnedFrame`.
It calls `decodeItem` once and, on success, computes the trailing count as `len(data) - endPos`,
where `endPos` is `decodeItem`'s returned end-of-item position.
The count is unconditionally 0 whenever `decodeItem` returns an error — a failed parse's returned
position is not a reliable "end of the first item" boundary to measure from.

`Decode` and `DecodeOwned` both discard the `int` `decodeFirstItem` returns and only return
`(Item, error)`: the lenient public contract is a thin wrapper that throws the count away, not a
separate code path that never computes it in the first place.
`DecodeOwnedFrame` is the only public entry point that keeps it:
`func DecodeOwnedFrame(body framecodec.OwnedSECS2Body) (Item, int, error) { return decodeFirstItem(body.Bytes()) }`.

`hsms.DataMessage.decode()` (`data_msg.go`) has TWO paths into `DecodeOwnedFrame`, not one.
The raw-frame path fires when `wire.OwnedBytes(msg.body)` succeeds — decoding in place over the
frame's own owned buffer.
The copy-fallback path ("should not occur — tree bodies pre-fire the once") copies the body via
`msg.body.AppendTo(nil)` first.
Both call `secs2.DecodeOwnedFrame(framecodec.AdoptSECS2Body(...))` — the fallback site's own comment
states the reason explicitly: routing both body shapes through the same `DecodeOwnedFrame` entry
point means trailing-byte counting has exactly one implementation covering both, not a well-tested
one for the common case and an untested second one for the rare fallback.

`DataMessage.TrailingBytes()` threads the count through the SAME `sync.Once` as `Item()`/`DecodeErr()`
(`decodeState.trailing`, set inside `decode()`): calling it fires the lazy decode if it has not run
yet, exactly like `DecodeErr`, and returns the cached value on every subsequent call.
It is meaningful only when `DecodeErr()` returns nil.

`framecodec.OwnedSECS2Body` (`internal/framecodec/owned.go`) is the capability token gating
`DecodeOwnedFrame` — a zero-cost wrapper (`AdoptSECS2Body`) around a `[]byte` that external code
cannot construct, so `DecodeOwnedFrame` stays uncallable outside the module regardless of its
return-arity.

# Invariants

- Exactly one function (`decodeFirstItem`) computes the trailing count.
  `Decode`, `DecodeOwned`, and `DecodeOwnedFrame` are all thin callers over it, so an accounting bug
  in the count can only live in one place.
- The count is 0 whenever the accompanying error is non-nil — a non-zero `TrailingBytes()` is
  meaningful only when `DecodeErr()` is nil.
- Both `DataMessage.decode()` paths (raw-frame and copy-fallback) call `DecodeOwnedFrame`, never
  `DecodeOwned` — routing the fallback through `DecodeOwned` would silently drop the count hsms needs.

# Failure modes

- A new decode entry point that calls `decodeItem` directly instead of going through
  `decodeFirstItem` computes no trailing count at all — `TrailingBytes()` would silently regress to 0
  for whatever path uses it.
- Routing the copy-fallback path through `DecodeOwned` instead of `DecodeOwnedFrame` would still
  compile but discard the trailing count: `TrailingBytes()` would read 0 for every message that hit
  the fallback, masking real padding on exactly the messages least likely to be covered by a test
  (the fallback is normally unreachable — tree-path bodies pre-fire the decode).
- Trusting `TrailingBytes()` when `DecodeErr()` is non-nil reads a hardcoded 0 that `decodeFirstItem`
  never actually measured, not "no trailing bytes observed".

# Where to look

- the shared measurement site: `secs2/decode.go` → `decodeFirstItem`
- the three public callers: `secs2/decode.go` → `Decode`, `DecodeOwned`, `DecodeOwnedFrame`
- both decode paths routing through `DecodeOwnedFrame`: `hsms/data_msg.go` → `(*DataMessage).decode`
- the lazy-decode threading of the count: `hsms/data_msg.go` → `(*DataMessage).TrailingBytes`, `decodeState`
- the capability token `DecodeOwnedFrame` requires: `internal/framecodec/owned.go` → `OwnedSECS2Body`, `AdoptSECS2Body`

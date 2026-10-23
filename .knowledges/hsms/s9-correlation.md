---
type: Mechanic
title: Where an inbound S9 is correlated to a waiting send
description: Where S9 correlation sits on the inbound path, what it reads from the S9 and from the waiter, how the MHEAD comparison is chosen per transport, and why the S9 is still delivered afterwards.
tags: [hsms, secs1, s9, reply-correlation, inbound]
status: stable
generated: {by: "claude/opus-5.5", at: 2026-10-23T18:00:00Z}
verified:
  - {by: "openai/gpt-6-astra", at: 2026-10-23T18:42:42Z}
sources:
  - {resource: hsms/connection_runtime.go, digest: sha256:7f242f475b188a1b, revision: 2a92a5e}
  - {resource: hsms/reply_registry.go, digest: sha256:8f6b52cc0e84f7db, revision: 2a92a5e}
  - {resource: hsms/connection.go, digest: sha256:42714e9581c0b14e, revision: 2a92a5e}
  - {resource: hsms/connection_send.go, digest: sha256:ea94ab4fc9d25bb9, revision: 2a92a5e}
  - {resource: secs1/transport.go, digest: sha256:8751c0d91c45f678, revision: 2a92a5e}
  - {resource: hsms/connection_config.go, digest: sha256:ce0ac428789edddd, revision: 2a92a5e}
  - {resource: hsms/data_msg.go, digest: sha256:a90443b564284b51, revision: 2a92a5e}
  - {resource: hsms/s9_correlation_test.go, digest: sha256:a3de9b08c23b6b18, revision: 2a92a5e}
---

# What it does

`WithS9Correlation`'s godoc states the contract:
which S9 functions match, the HSMS and SECS-I comparison rules,
that the S9 still reaches the handlers, and that it is correlated within the generation that admitted it.
What it leaves out is where on the inbound path this runs relative to the session-ID check and reply routing,
that the comparison is against the header the waiter stored at registration rather than anything re-derived,
how a connection learns its transport's MHEAD form,
and that the body decode it forces is skipped for every message it does not inspect.
That is the delta this entry records.
Which result wins once the S9 reaches the registry is [the reply-wait claim](/hsms/reply-wait-claim.md).

# How it works

**Position on the inbound path.**
`deliverOwnedFrameOn` runs, in order:
the origin stamp, `DataMsgRecvCount`, `checkSessionID` (when session-ID validation is on), the fan-out test hook,
then `correlateS9On(e, dm)`, then reply routing for a secondary, then `routeDataOn`.
So an S9 other than S9F1 that carries a foreign session ID is dropped before correlation,
and an S9F1 is always exempt from that drop.
The S9 functions correlation matches are odd, so `IsPrimary` reports them as primaries:
after correlation they never enter `routeReplyOn`
and always reach `routeDataOn`, which delivers them or diverts them to the decode-error handlers.
A Stream 9 message with an even function and the W-bit clear, S9F0 included, is a secondary and enters reply routing like any other.

**What `correlateS9On` reads.**
It returns at once unless `e` is non-nil, the stream is 9, and the connection's `s9Correlation` is set,
read from the config on every call, so a runtime config change applies to the next S9.
It then requires function 1, 3, 5, 7, or 11, and only then calls `dm.Item()`,
so every other message, S9F9 and S9F13 included, keeps its lazy body undecoded.
The body must be a 10-byte binary item; anything else, or a decode error, ends correlation silently,
with the decode error left cached on `dm` for `routeDataOn`.
It calls `e.replies.failS9` on the epoch passed to `deliverOwnedFrameOn`, resolved by that method's caller, never on a re-read of `c.cur`,
with an `*S9Error` that carries `dm` itself.

**What the S9 is compared against.**
`failS9` keys the lookup on the MHEAD's System Bytes (`mhead[6:10]`), never on the S9 envelope's own,
and compares the MHEAD against `w.header`,
the 10-byte header `registerData` stored from `dm.HeaderBytes()` when the primary was sent.
The registry key, the stream and function `route` compares, and the S9 header all come from that one stored header,
so the identity a reply is checked against and the identity an S9 is checked against cannot diverge.
A control waiter stores no header and is skipped on `w.isData`.

**The MHEAD form per transport.**
`NewConnection` sets `c.mhead` once with `transportMHEADPolicy(tr)`:
`mheadBlock` when the transport implements the unexported `secs1MHEADTransport` and reports true,
`mheadExact` otherwise.
`secs1`'s transport answers true through `UsesSECS1MHEAD`,
because a SECS-I peer's MHEAD is the block header it read, with the R-bit, E-bit, and block number of that block.
`mheadBlock` therefore masks the R-bit of byte 0 and skips bytes 4 and 5,
while `mheadExact` compares all 10 bytes, session ID and PType/SType included.

# Invariants

- Correlation runs after `checkSessionID` and before reply routing, on the epoch passed to `deliverOwnedFrameOn`.
- `correlateS9On` never consumes `dm`; routing proceeds identically whether or not a send was ended.
- No message other than S9F1, S9F3, S9F5, S9F7, or S9F11 has its body decoded by correlation,
  and none at all while the option is off.
- `c.mhead` is fixed at construction; it depends only on the transport, never on configuration.

# Failure modes

- **Correlating against `c.cur.Load()`** instead of the admitting epoch lets an S9 read by an old generation's receive goroutine end the successor's wait with the same System Bytes;
  `TestS9Correlation_OldGenerationPausedAcrossReconnect` catches it.
- **Decoding the body before the function switch** decodes S9 functions correlation never matches, S9F9 and S9F13 included;
  decoding before the epoch, stream, and option gates decodes every message that reaches correlation.
  `TestS9Correlation_BodyLeftUndecoded` catches it.
- **Keying the lookup on the S9 envelope's System Bytes** misses the intended waiter, so the send waits out T3.
  With the header comparison intact it cannot end a waiter whose stored System Bytes differ from the MHEAD's.
  `TestS9Correlation_NoMatch` does not detect a lookup-only change, although its test comment says its envelope row does:
  in that row the MHEAD differs from the sent header, so the comparison rejects it under either key.
- **Dropping `failS9`'s `isData` condition** lets an S9 end a Linktest or Select wait;
  `TestS9Correlation_ControlWaiterNotMatched` catches it.
- **A SECS-I transport that stops reporting `UsesSECS1MHEAD`** falls back to `mheadExact`,
  so an S9 from a SECS-I peer, whose MHEAD carries the E-bit and block number of the block it read, ends nothing,
  and the send waits out T3.

# Where to look

- the call site and its order: `hsms/connection_runtime.go` → `(*connection).deliverOwnedFrameOn`
- the gate, the decode, and the claim call: `hsms/connection_runtime.go` → `(*connection).correlateS9On`
- the session-ID drop ahead of it: `hsms/connection_runtime.go` → `(*connection).checkSessionID`
- the lookup, the header comparison, and the policies: `hsms/reply_registry.go` → `(replyRegistry).failS9`, `mheadPolicy`, `(mheadPolicy).matches`
- the stored header: `hsms/reply_registry.go` → `(replyRegistry).registerData`; `hsms/connection_send.go` → `(*connection).sendWaitReplyOn`
- the per-transport choice: `hsms/connection.go` → `transportMHEADPolicy`, `secs1MHEADTransport`, `NewConnection`
- SECS-I's capability: `secs1/transport.go` → `(*transport).UsesSECS1MHEAD`
- the contract: `hsms/connection_config.go` → `WithS9Correlation`
- primary classification: `hsms/data_msg.go` → `(*DataMessage).IsPrimary`
- the regression tests: `hsms/s9_correlation_test.go` → `TestS9Correlation_NoMatch`, `TestS9Correlation_BodyLeftUndecoded`

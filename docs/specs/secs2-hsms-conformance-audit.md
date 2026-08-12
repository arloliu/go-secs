# SECS-II / HSMS Conformance Audit — `secs2` vs E5, `hsms` vs E37

**Date:** 2026-08-11

**Scope:** the `secs2` package against SEMI E5-0813 (SECS-II),
and the shared `hsms` engine against SEMI E37-0413 (HSMS generic services).
The `hsmsss` / E37.1 axis is **not** re-derived here — see the companion audit and the re-verification section below.

**Sources:**
`~/semi_standards/markdowns/e005-00-0813/e005-00-0813.md` (E5-0813)
and `~/semi_standards/markdowns/e037-00-0413/e037-00-0413.md` (E37-0413).

**Verdicts are three-way:** *conforms*, *deviates*, *not implemented (by design)*.
A clause not traced to a symbol is recorded as **unchecked**, not as compliant.
See "Coverage" at the end for exactly what was and was not read.

## A note on the requested spec set

The request named E30, E37, and E37.1 for `secs2`, `hsms`, and `hsmsss`.
E30 is GEM — it governs the `gem` package, not `secs2`, and no copy of it exists under `~/semi_standards/`.
`secs2` implements SECS-II, which is **SEMI E5**, and E5-0813 *is* present.
So this audit reads `secs2` against E5.
A `gem`-vs-E30 audit is separate work.
**[R2]** Note that only *part* of it is blocked: `gem`'s message shapes can be checked against E5 §§9.6–10, which is on disk.
Only GEM's own requirements — capabilities, state models, scenarios — need the E30 standard added first.

## Summary

Nine deviation groups, ten numbered rows (D7 splits into D7a and D7b). **[R3][R4]**
Three reached the wire or the peer through ordinary public APIs (D5, D6, and D8 —
though D8's cost was local, not peer-visible **[R5]**),
a fourth (D7a) did so on misuse,
two are receive-path leniencies (D1, D3),
three were exported-API validation gaps (D2, D3b, D7),
and one was missing documentation the standard requires (D4).

**Revision note.**
An earlier draft of this audit claimed five deviations and that none touched the wire path.
Both statements were wrong, and an external review (Codex, `gpt-5.6-sol`, `xhigh`) caught them.
D5 and D6 were sitting in the "verified conformant" tables with arguments that ran backwards.
Every correction that review drove is marked **[R1]** below.

**Implementation note (this revision).** Every row but D5 has since been addressed on the branch recorded at
`docs/specs/e5-e37-conformance-fixes/`; see the Status column and each section's closing note below.
D1 and D3 ship as **observation**, not enforcement — delivery is unchanged by default, and conformant handling
is either opt-in (D1's `WithStrictReplyMatching`) or accessor-only (D3's `TrailingBytes()`) — so both rows keep
their *deviates* verdict even though the corresponding Action is ticked.
D5 stays an **open, deliberate** deviation: the nonconformant default reconnect backoff is a documented product
choice, not an oversight, and this revision records that choice rather than changing the default.

| # | Package | Clause | Deviation | Operational impact | Normative force | Status |
|---|---------|--------|-----------|--------------------|-----------------|--------|
| D1 | `hsms` | E37 §9.4.1 | Reply matching keys on System Bytes alone; Stream / Function / SessionID are not checked | Moderate — mis-delivers a reply; reachable against a **conformant** peer via `ForwardDataMessage` **[R2]** | "should" | **OBSERVED** — `ReplyMismatchCount` always on; `WithStrictReplyMatching` opt-in enforces (`4732ee6`) |
| D2 | `hsms` | E37 §8.3.21.3, Table 9 | `GetRejectReasonCode` rejects legal reason codes 5–255 | Low — public API only; the recv path is correct | Definitional | **FIXED** (`4aacca7`) |
| D3 | `secs2` | E5 §9.2, §10.3.1.3(2) | `Decode` silently ignores bytes trailing the first complete item | Low — reaches the HSMS body path, verified end-to-end | "shall" **[R1]** | **OBSERVED** — delivery unchanged; `DataMessage.TrailingBytes()` accessor (`bab6662`) |
| D3b | `hsms` | E37 §9.3.3.1 | Exported frame decoders accept a control frame carrying a body | Low — the live `hsmsss` recv path rejects it correctly | "must" | **FIXED** (`ffd4635`) |
| D4 | `hsms` / `hsmsss` | E37 §10.1, §9.2.4.1.2 | Five of the six required documentation items are unstated **[R1]** | Low — but "nothing misbehaves" holds only for the documentation gap itself; items 3, 5, and 6 each sat on top of a real behavioral gap (D6, the absent outbound size cap, and D8) **[R4]** | **"shall" / "is required"** | **FIXED** — items 1–5 closed in behavior and docs (`9c255d8`, `8a36c97`, `d761758`); item 6 documented only, the underlying unboundedness is unchanged |
| **D5** | `hsms` | E37 §9.2.1.1 | **Reconnect backoff starts at 100 ms, not T5 — the connect-separation floor is not honored [R1]** | Moderate — live wire behavior; §9.2.1 calls frequent connects "hostile to TCP/IP operations" | "should" | **OPEN, documented** (`d761758`) — deliberate default, see D5 below |
| **D6** | `hsmsss` | E37 §9.2.4.1, §9.2.4.1.1 | **A second dialer is accepted and then closed, which is none of the three permitted refusal options [R1]** | Moderate — live wire behavior; the peer's connect procedure completes, then the link drops | "will follow one of these three" | **FIXED** (`8a36c97`) |
| **D7a** | `hsms` | E5 §7.2 | **Primary-send APIs accept an even-function "primary" and put it on the wire [R1][R2]** | Low — misuse only, but it does reach the wire | "will" | **FIXED** (`ea2742f`) |
| **D7b** | `hsms` | E37 §8.3.21.3 | **Reject constructors accept reason code 0, the one value never permitted [R1]** | Low — inert; no library call site | "always nonzero" | **FIXED** (`4aacca7`) |
| **D8** | `hsms` | E37 §9.4.1.2 | **`SendDataMessageAsync(replyExpected: true)` sends a W-bit primary with no T3 timer [R4]** | Moderate — ordinary public API, no misuse required; the transaction can never time out. Cost is **local**, not peer-visible **[R5]** | **"must"** | **FIXED** (`ea2742f`) |

The last two audit-time columns pulled in opposite directions and both mattered when this table was first drawn.
D1 had the worst consequence when it fired but rested on a "should".
D4 broke a flat "shall" — §9.2.4.1.2 and §10.1's "is required to document" — while costing nothing at runtime.
(**[R2]** It was not the *only* one:
D3's E5 §10.3.1.3(2) is also a "shall not", though it sits in §10 territory this audit places outside `secs2`'s scope.
D5's and D1's clauses are "should"; D7a's is "will".)
**[R3][R4][R5]** Three were observable by a peer: D5 and D6 under ordinary use of the public API, and D7a on misuse.
D6 and D7a are now fixed; D5 remains open by deliberate choice, documented below.
**D8 was not peer-observable** — a missing local reply timer changed nothing the peer could see;
its cost was borne entirely on our side, as a transaction that could never time out.
It is fixed.
D7b was inert — no library call site reached it.
It is fixed too.
The Actions list at the end records what happened to each row.

---

# Part 1 — `secs2` against SEMI E5-0813

## Scope boundary

E5 is two documents in one.
§§5–9 define the message-transfer contract, the item/list data structures, and the wire encoding.
§10 and Tables 3–22 are the stream-and-function dictionary —
S1F1 through S21Fxx, the data-item dictionary, and the object attribute tables.

`secs2` implements the first part only:
it is the item model and the codec.
`secs2.Message` (`secs2/message.go`) carries a stream code, a function code, a wait bit, and an item,
and attaches no meaning to any particular SnFn pair.
The dictionary belongs to `gem`.
Every §10 clause is therefore recorded below as **not implemented (by design)** at the `secs2` level, not as unchecked.

## Clauses verified conformant

| Clause | Requirement | Where |
|---|---|---|
| §9.2 | Item header is format byte + 1–3 length bytes; length counts item-body bytes only | `secs2/item.go:441` `appendHeaderBytesFC`, `secs2/decode.go:119` |
| §9.2.1 | A zero length-byte count in the format byte is illegal and produces an error | Decode rejects it, `secs2/decode.go:125`; the encoder can never emit it — `lenByteCount` floors at 1, `secs2/item.go:449`–`454` **[R1]** |
| §9.2.1 / §9.3.1 | A zero *value* in the length bytes is legal | Verified: an empty list encodes `01 00`, an empty ASCII item `41 00`, and both round-trip |
| §9.2.2, Table 1 | Exactly 16 defined format codes, at the octal values listed | `secs2/item.go:41`–`57` — `0o00, 0o10, 0o11, 0o20, 0o21, 0o22, 0o30, 0o31, 0o32, 0o34, 0o40, 0o44, 0o50, 0o51, 0o52, 0o54`, all present and correct |
| §9.2.2 | The other 48 codes are undefined | Decode rejects them: `unknown format code`, `secs2/decode.go:285`. Verified against code `0o23` |
| §9.2.2 | Signed integers are two's complement | `secs2/decode.go:307`–`313` sign-extends via `int8`/`int16`/`int32` |
| §9.2.2 | Floating point conforms to IEEE 754 | `math.Float32frombits` / `math.Float64frombits`, `secs2/decode.go:432` |
| §9.2.2 | Boolean is a byte quantity; zero is false, non-zero is true | `secs2/decode.go:221` uses `!= 0`. Verified: payload byte `0x02` decodes to `true`; the encoder emits `0x01`/`0x00` |
| Table 1 #2 | Multibyte numerics are sent most-significant byte first | `binary.BigEndian` throughout `secs2/decode.go`; symmetric on the encode side |
| Table 1 #3 | For floats, the byte containing the sign bit is sent first | Same big-endian path |
| §9.3 | A list's length bytes count *elements*, not bytes | `secs2/decode.go:149`–`174` loops `length` times over child items; the encoder passes the direct-child count as the length field |
| §9.3 | A list element may itself be a list | Recursive `decodeItem`. **Capped at `MaxListDepth` = 64, which E5 does not authorize — see the accepted-deviations section [R1]** |
| §9.4.1 | The LSH is 2 bytes, precedes the string, and **is counted in the item length** | `secs2/localized_str.go:32` documents the layout; `AppendTo` writes `len(value)+2` as the length field (`:150`), and decode reads the LSH from inside the payload (`secs2/decode.go:252`) |
| §9.4.1 | A localized-string item is therefore never shorter than 2 payload bytes | `length < 2` is rejected, `secs2/decode.go:244` |
| §9.4.2, Table 2 | The 15 defined encoding-scheme codes | `secs2/localized_str.go:7`–`21` — all 15 present, values 0–14, matching Table 2 exactly |
| §9.4.3 | Codes 15–32767 reserved, 32768–65535 custom | Not constrained; the LSH is a plain `uint16`, which is what "available for custom purposes" requires |
| §6.4.2 | Stream is 7 bits (0–127), function is 8 bits (0–255) | `secs2/message.go:42` masks the stream with `0x7F`; `hsms.NewDataMessage` enforces the range on the transport side (see Part 2) |
| §6.4.3 | The protocol must be able to say whether a reply is requested | `Message.WaitBit()`, `secs2/message.go:48` |
| — | Payload length must be a whole multiple of the element width | `secs2/decode.go:294`, `:357`, `:420` — a 3-byte I2 payload is rejected |

## D3 — `Decode` ignores trailing bytes (deviates; OBSERVED, not rejected)

**Clause.**
E5 §9.2 defines an item as a header plus exactly *item length* body bytes.
A message body is one such item.
Bytes past the end of that item are not part of any structure the standard defines.

**[R1] A stronger hook exists one layer up.**
E5 §10.3.1.3(2): "The message **shall not** contain any Lists or Data Items not shown in the Message Definition,
unless the Message Definition specifically allows this."
That is a flat "shall", where §9.2 only defines the encoding and leaves the prohibition implicit.
The nuance worth keeping: §10.3.1.3 governs *standard message definitions*, which this audit places outside `secs2`'s scope,
so it does not change `secs2`'s verdict.
It does establish that the trailing bytes are a real protocol violation and not merely an undefined region —
and E5 §10.13 names S9F7 (Illegal Data) as the response, citing §10.3.1.3 by number.

**Behavior.**
`Decode` and `DecodeOwned` (`secs2/decode.go:31`, `:59`) call `decodeItem` and **discard the returned end position**:

```go
item, _, err := decodeItem(owned, 0, 0, slab)
return item, err
```

Verified empirically: `Decode([]byte{0x41, 0x01, 'A', 0xDE, 0xAD, 0xBE})` returns `<A[1] "A">` and a nil error.
Three garbage bytes vanish silently.

**Impact — verified end-to-end, not inferred.**
`hsms.decodeOwnedFrame` hands the whole body `owned[10:]` to the same decoder.
A full frame was built and pushed through `DecodeHSMSMessage`:
an S1F3 header whose declared Message Length covers a body of two concatenated `A[1]` items.
The frame decodes cleanly, `DataMessage.DecodeErr()` returns nil,
and `DataMessage.Item()` yields `<A[1] "A">` with size 1 and no error — the second item is gone.

The nil `DecodeErr()` is the part that matters.
`RouteData` (`hsms/connection_runtime.go:19`) gates its decode-error diversion on exactly that call,
so a truncated-interpretation body is **not** counted by `incBodyDecodeErr` and **not** sent to the decode-error handlers.
It reaches the application's data-message handlers as a well-formed single-item message.
A malformed or misframed peer body is accepted as valid rather than surfaced.

**Shipped as observation, not rejection (`bab6662`).**
`Decode` and `DecodeOwned` stay lenient by documented contract — first-item semantics, trailing bytes silently ignored —
because the nonconformant party here is field equipment this library cannot patch,
and a hard boundary check would reject frames from real peers that this project has no way to fix in the field.
`secs2.DecodeOwnedFrame` now returns the trailing byte count alongside the decoded item,
and `hsms.DataMessage.TrailingBytes()` surfaces that count through the transport,
for both the raw-frame decode path and the copy-fallback path.
Delivery is unchanged everywhere; the count is a diagnostic, not a gate.
See D1 below for the parallel receive-side leniency and the shared rationale for it.

## Deviations reviewed and accepted (no action)

**Table 1 #1 — ASCII items accept non-ASCII bytes.**
`NewASCIIItem` takes any Go string; `NewJIS8Item` likewise (`secs2/ascii.go:22`, `secs2/jis8.go:22`).
Both constructor doc comments say so explicitly.
E5's own note is that "nonprinting characters are equipment-specific",
and field equipment does put 8-bit bytes in format-20 items.
Validating would break interoperability for a rule E5 does not state as a constraint on the codec.
Deliberate, documented, keep.

**§6.3.1 — a single-block SECS-II message is at most 244 bytes.**
Not enforced.
This is a SECS-I framing constraint that E5 carries for compatibility; it is not a property of the item encoding.
`secs1` handles blocking; `secs2` correctly leaves it alone.
(The E37.1 audit records the parallel §9.1 "should not exceed 254 bytes" for HSMS-SS, likewise unenforced and likewise correct.)

**§9.2 — `MaxByteSize` caps an item payload at 2²⁴−1.**
Not a deviation: three length bytes is the maximum the E5 item header can express,
so this is the standard's own ceiling, named (`secs2/item.go:11`).

## Not implemented at the `secs2` level (by design)

| Clause | What it covers | Where it actually lives |
|---|---|---|
| §7.3, §7.3.1, Figure 1 | Stream/function allocation — which SnFn pairs are reserved vs. user-definable | Not modelled anywhere; any (stream ≤ 127, function) pair is constructible. Deliberate: the library is a transport, not a policy engine |
| §8.3(1) | Respond to S1F1 with S1F2 | Application layer; `gem` supplies the builders |
| §8.3(2) | Send the appropriate Stream 9 error for an unprocessable message | Partly implemented in the engine: an S9F1 goes out on a session-ID mismatch (`hsms/connection_runtime.go:133`) using `gem.S9F1`. The rest (S9F3/F5/F7/F11) is left to the application, with builders in `gem/s9.go` |
| §8.3(4) | Send S9F9 on a transaction timeout | **Implemented** in the engine, opt-in: `hsms.WithAutoS9F9` fires `gem.S9F9(SHEAD)` on a T3 expiry (`hsms/connection_config.go:471`). `hsmsss.WithEquipRole` turns it on |
| §8.3(5) | Terminate the transaction on receipt of function 0 | **Implemented**: `isSecondaryReply` admits F0 as a secondary so an SxF0 abort satisfies the waiting sender instead of stalling to T3 (`hsms/connection_runtime.go:159`) |
| §8.4, §8.4.1, §8.4.2 | Conversation protocols and conversation timeouts | Application layer, explicitly "not covered as part of this Standard" for the mechanism. `gem.S9F13` exists |
| §§9.6–9.8 and §10, Tables 3–22 | E5's own data-item, variable-item, and object dictionaries, and its stream/function message detail | `gem`. **[R3]** These are **E5** content, not E30 content — a `gem` message-shape audit can run against E5 today. E30 is needed only for GEM's own requirements (capabilities, state models, scenarios) |

---

# Part 2 — `hsms` against SEMI E37-0413 (generic services)

## Clauses verified conformant

### Message format (§8)

| Clause | Requirement | Where |
|---|---|---|
| §8.2.2 | 4-byte length prefix, 10-byte header, 0–n body | `hsms/decode.go:30`; `ControlMessage.ToBytes` emits exactly 14 bytes (`hsms/control_msg.go:74`) |
| §8.2.3 | Message Length is a 4-byte unsigned integer, MSB first | `binary.BigEndian.Uint32(data[0:4])`, `hsms/decode.go:37` |
| §8.2.4 | The minimum Message Length is 10 | Enforced, `hsms/decode.go:39`; and again at the frame reader |
| §8.2.5, Table 3 | Header field layout: SessionID 0–1, byte 2, byte 3, PType 4, SType 5, System Bytes 6–9 | Uniform across `hsms/control_msg.go` and `hsms/data_msg.go` |
| §8.2.6.1 | SessionID is a 16-bit unsigned integer, byte 0 MSB | `binary.BigEndian.Uint16(header[0:2])`, `hsms/control_msg.go:55` |
| §8.2.6.4, Table 4 | Only PType 0 is defined | Non-zero PType is refused at decode (`hsms/decode.go:105`) and answered with Reject reason 2 on the wire (`hsmsss/transport_recv.go:101`–`105`) |
| §8.2.6.6, Table 5 | STypes 0–7 and 9 are defined; 8 and 10–255 are not | `IsValidSType`, `hsms/message.go:35`. Undefined STypes decode to an error and draw Reject reason 1 |
| §8.2.6.8 | A .req's System Bytes must be unique among open transactions and vs. the most recent completed one | **Conforms for library-generated System Bytes only [R1].** `sysBytesGen` is a monotonic 32-bit counter per connection (`hsms/sysbytes.go:24`) and the wrap-vs-uniqueness argument is in the type doc. But `ForwardDataMessage` sends caller-supplied System Bytes verbatim and does **not** draw from the generator, so §8.2.6.8 becomes the caller's obligation there. That is inherent to a verbatim relay — a proxy must preserve upstream System Bytes — and the godoc states the duty and the "reply theft" consequence explicitly. Recorded as a delegated obligation, not an unqualified conformance. See D1 |
| §8.2.6.9 | A reply reuses the primary's System Bytes; each .rsp reuses its .req's | Every `New*Rsp` copies `req.header[6:10]` verbatim (`hsms/control_msg.go:208`, `:246`, `:279`) |
| §8.3.2 | A data message's length is 10 or greater | Same floor as §8.2.4; an empty body is permitted |
| §8.3.3.3 | Header byte 2 is W-bit \|\| 7-bit Stream; a reply must set W = 0 | `hsms/data_msg.go:327`. **`NewDataMessage` rejects W=1 on an even function** (`:319`) and a stream > 127 (`:304`) — both actively enforced, not merely encoded |
| §8.3.3.4 | Header byte 3 is the Function; bit 0 discriminates primary from reply | `hsms/data_msg.go:107`; `isSecondaryReply` uses `Function()%2 == 0` |
| §8.3.4.1, §8.3.6, §8.3.9, §8.3.12, §8.3.15, §8.3.18 | Every control message is header-only, Message Length exactly 10 | `ControlMessage.ToBytes` hard-codes length 10 (`hsms/control_msg.go:79`) |
| §8.3.7.1, §8.3.13.1 | Select.rsp / Deselect.rsp echo the request's SessionID | `hsms/control_msg.go:204`, `:241` |
| §8.3.7.2, Table 7 | SelectStatus in header byte 3 | `hsms/control_msg.go:206`; constants at `:137`–`:153` |
| §8.3.13.2, Table 8 | DeselectStatus in header byte 3 | `hsms/control_msg.go:243`; constants at `:155`–`:160` |
| §8.3.16, §8.3.19 | Linktest.req and Linktest.rsp carry SessionID 0xFFFF | Hard-coded, `hsms/control_msg.go:258`, `:276` — not inherited from config |
| §8.3.21.2 | Reject header byte 2 = the rejected PType when reason is 2, else the rejected SType | `hsms/control_msg.go:316`–`320`, and the raw variant at `:342` |
| §8.3.21.3 | Reject header byte 3 is a non-zero reason code | `hsms/control_msg.go:323`; constants 1–4 at `:164`–`:172` |
| §8.3.21.4 | Reject echoes the rejected message's System Bytes | `hsms/control_msg.go:325` |
| §9.3.3.1 | For a non-zero SType, PType must be 0 and no message text may be sent | Enforced on the live path: `hsmsss/transport_recv.go:111`–`115` rejects a control frame whose length is not exactly 10. **Now also enforced by the exported decoders** — `hsms.decodeOwnedFrame` rejects a control frame carrying a body with `ErrControlFrameWithBody` — see D3b, fixed |

### Procedures (§7) and timers (§9)

| Clause | Requirement | Where |
|---|---|---|
| §6.3.2 | An entity is either active or passive | `hsmsss.WithActive` / `WithPassive` |
| §7.4.1 | Select initiator: status 0 → SELECTED; non-zero → no transition; T6 expiry is a communications failure | `hsmsss/transport_active.go` `runSelectProcedure`; the commit is done synchronously on the recv goroutine at `hsmsss/transport_recv.go:161` |
| §7.4.2 | Select responder: status 0 and enter SELECTED, or non-zero and no transition | `hsmsss/transport_control.go:49` `handleSelectReq` |
| §7.5 | A data message received outside SELECTED triggers the reject procedure | `hsmsss/transport_recv.go:129`–`133` → `sendRejectNotSelected` (reason 4) |
| §7.7.2 | Deselect responder: status 0 + enter NOT SELECTED if SELECTED, else a non-zero status and no transition | `hsmsss/transport_control.go:276`–`278` returns `DeselectStatusNotEstablished` when not Selected; the success branch calls `SelectLost` and re-arms T7 |
| §7.8.2 | Linktest responder sends a Linktest.rsp | `hsmsss/transport_control.go:249` |
| §7.9 | Separate procedure | See the E37.1 audit, Gap 2 — E37.1 §7.6 narrows this |
| §7.10.3 | The reject procedure is required for a data message in NOT SELECTED and for an undefined SType or PType | Both, `hsmsss/transport_recv.go:101` and `:129` |
| §8.3.20 | A control **response** with no open transaction must draw a Reject | `hsmsss/transport_recv.go:176` → `sendRejectTransactionNotOpen` (reason 3). An orphan Reject.req is dropped rather than re-rejected, which is correct — it is not a response |
| §9.1.1 | On a communications failure the entity should terminate the connection | The supervisor drives teardown-and-reconnect on T6/T7/read failure |
| §9.2.1.1 | After an active connect terminates, wait T5 before the next attempt | **Deviates — see D5 [R1].** An earlier draft passed this row with the argument that a ramp topping out at T5 is "stricter early on". That is backwards |
| §9.2.2.1 | Disconnect if NOT SELECTED persists past T7 | `armT7` on entering NotSelected (`hsmsss/transport_procedures.go:236`, called from `transport_recv.go:55`), cancelled on the commit to Selected (`transport_recv.go:168`), re-armed after a successful Deselect (`transport_control.go:293`) |
| §9.2.3.1 | T8 bounds the gap between successive bytes **of a partial message**, not the idle link | `hsmsss/transport_recv.go:219`–`266`: no deadline before the first byte of a frame, then a T8 deadline on every subsequent read, spanning the length prefix and the header/body reads |
| §9.3.1.1 | T6 bounds an open control transaction | `hsms/connection_send.go:264`, yielding `ErrT6Timeout` |
| §9.4.1.2 | Start a reply timer at T3 on a W-bit primary | **Conforms.** Reply channel registered at `hsms/connection_send.go:227`; the T3 deadline is armed at `:262`–`:272`. The asynchronous path (`SendDataMessageAsync`) armed no timer — see D8, now fixed by refusing `replyExpected: true` there instead |
| §9.4.1.3 | Each open transaction needs its own reply timer | **[R1] citation corrected.** `hsms/reply_registry.go:35` proves the per-transaction *channel*, not the timer. The per-transaction timer is `hsms/connection_send.go:262`–`272`, which selects T3 or T6 per transaction kind and arms a fresh timer inside each send call |
| §9.4.2.1 | MHEAD / SHEAD carry the ten bytes of the **HSMS** header | `gem.S9F1(dm.HeaderBytes())` (`hsms/connection_runtime.go:139`) and the same for S9F9 |
| §10.2, Table 10 | Installation-time settings for T3, T5, T6, T7, T8, connect mode, addresses | **Conforms for the parameter set and ranges.** `WithT3`…`WithT8`, `WithActive`/`WithPassive`. Defaults are exactly Table 10's typical values: T3 45 s, T5 10 s, T6 5 s, T7 10 s, T8 5 s (`hsms/connection_config.go:71`–`76`). The accepted range is any positive duration — a superset of Table 10's required ranges |
| §10.2 (persistence sentence) | "All parameters must be stored in such a manner that the settings will be retained if the power fails or if the system software is reloaded" | **Not implemented — by design, and previously overclaimed [R1].** `hsmsss.NewConfig` builds an in-memory config (`hsmsss/config.go:45`–`67`); the library neither persists settings nor defines a persistence contract. This obligation lands on the embedding application, which is the same shape as E37.1 §10.1's declarations. It belongs in the §10.1 documentation section rather than being silently claimed as conformant |

## D1 — reply matching ignores Stream, Function, and SessionID (deviates; OBSERVED, strict opt-in)

**Clause.**
E37 §9.4.1 — when a sender sends a primary with W-Bit 1,
it should expect a reply whose header meets **four** requirements:
the SessionID matches the primary's,
the Stream matches the primary's,
the Function is one greater than the primary's *or* is 0,
and the System Bytes match the primary's.

**Behavior.**
Only the fourth is checked.
`RouteReply` (`hsms/connection_runtime.go:183`) looks the inbound message up in `replyRegistry` by System Bytes alone
(`hsms/reply_registry.go:52`),
and the send path returns whatever came back without inspection:

```go
// hsms/connection_send.go:276
return res.msg, res.err
```

The only additional filter is `isSecondaryReply` (`hsms/connection_runtime.go:159`),
which requires an even function code and a clear W-bit.
That is a guard against mis-consuming a *primary*, not a §9.4.1 check:
it admits any even function, from any stream, carrying any SessionID.

**Failure it allows.**
A peer that answers S1F3 with S2F4 — right System Bytes, wrong stream —
has its reply delivered to the waiting `SendDataMessage` caller as the correct reply.
So does a peer answering S1F3 with S1F6.
The caller receives a message it did not ask for, with no error.
The real reply, if it later arrives, misses the now-deregistered key and falls through to `RouteData`,
so it reaches the application's data-message handlers as an unsolicited message.

**[R1] Correction.** An earlier draft said the real reply is "dropped as an orphan".
It is not dropped — `DeliverOwnedFrame` routes a registry miss to `RouteData` (`hsms/connection_runtime.go:111`–`117`).
It arrives, just at the wrong door and out of band.

**[R1] The exposure is not only to nonconformant peers.**
An earlier draft said System-Bytes uniqueness makes collision unlikely and confined the risk to a buggy peer.
That understates it.
`ForwardDataMessage` sends caller-supplied System Bytes **verbatim**, deliberately bypassing the connection's generator,
and its own godoc names the consequence: they "may collide with an in-flight `SendDataMessage`/`SendSECS2Message` transaction
OR another forwarded message on the same connection … On a collision the peer's reply is routed to whichever waiter/handler
the registry resolves — **reply theft**" (`hsms/endpoint.go`, `ForwardDataMessage` doc).
So a relay or proxy — the exact use case `ForwardDataMessage` exists for — can manufacture the collision locally,
against a fully conformant peer.
A §9.4.1 stream/function check would not eliminate that (two forwarded S1F3s still collide),
but it would eliminate every cross-stream and wrong-function case, which is most of them.

**Note the asymmetry with `isSecondaryReply`.**
That function's own doc comment reasons carefully about System-Bytes collisions between the two symmetric peers.
The same reasoning applied one step further gives the §9.4.1 check for free:
the registry entry would carry the primary's stream, function, and SessionID,
and `route` would compare them before delivering.

**Shipped as observation, default unchanged; strict enforcement opt-in (`4732ee6`).**
The registry entry now carries the primary's Stream, Function, and an `isData` flag alongside the reply channel
(`hsms/reply_registry.go`).
`route` compares Stream and Function on every candidate that hits an open System-Bytes entry registered for a data
primary against a `*DataMessage` result — control responses (no `Stream()`/`Function()` to compare) and a
field-less `*RejectError` result are both excluded.
Function matches primary+1 **or** 0, admitting the SxF0 abort secondary per E5 §7.2/§10.4.1;
the F0 exception is function-only, so a wrong-stream F0 still mismatches.
SessionID stays outside this check — it is `WithSessionIDValidation`'s seam, upstream of the registry.

Under the **default**, a mismatch increments `ConnectionMetrics.ReplyMismatchCount` and the reply is still
delivered — nothing on the wire or in delivery changes.
That default is deliberate: the nonconformant party here is equipment the caller cannot patch,
and turning every mismatch into a miss would convert a sloppy peer's working (if wrong) reply into a T3 stall.

Under `WithStrictReplyMatching(true)` (`hsms/connection_config.go`), a mismatch is a **miss**:
the message falls through to `RouteData` as unsolicited, exactly like an orphan secondary today,
without consuming the registration, so a later conforming reply can still complete the transaction before T3.
`WithStrictReplyMatching`'s godoc states that full E37 §9.4.1 enforcement is itself **plus** `WithSessionIDValidation`.

The recommended adoption path is a soak under the default:
watch `ReplyMismatchCount`, and enable strict mode only once it holds at zero.
Enabling strict against a peer that is sloppy about stream or function turns its previously-working (if incorrect)
replies into transactions that time out at T3 — the first symptom is a hang, not an error.

## D2 — `GetRejectReasonCode` rejects legal reason codes (deviates, public API only; FIXED)

**Clause.**
E37 §8.3.21.3 and Table 9 define reason codes 1–4,
then reserve **4–127 for subsidiary standard-specific reasons** and **128–255 for local entity-specific reasons**.
All of 1–255 are legal on the wire; only 0 is not ("Reason code (always nonzero)").

**Behavior.**
`GetRejectReasonCode` (`hsms/control_msg.go:362`) returns `ErrInvalidRejectReason` for anything outside `[1, 4]`,
and the error text states the range as fact:

```go
// hsms/errors.go:28
ErrInvalidRejectReason = errors.New("hsms: invalid reject reason, should be in range of [1, 4]")
```

`ControlMessage.RejectReasonCode()` delegates to it,
so the method a library consumer would naturally reach for cannot read a legal subsidiary or local-entity reason code.

**The internal path is already right,** and knows it:
`RouteReply` reads header byte 3 directly and says why —
"we surface whatever reason the peer actually sent, including reserved codes (5..255) that `GetRejectReasonCode` would reject —
faithful reporting beats validation on this inbound path" (`hsms/connection_runtime.go:190`).
`RejectError.Reason` therefore carries the true code.

So this is a defect in the exported helper only, not in protocol behavior.
It is nonetheless a real one:
two public symbols document a wrong range, and the validating helper is the discoverable one.

**[R1] The same clause is under-enforced in the other direction.**
§8.3.21.3 says the reason code is "always nonzero" — zero is the one value that is *never* legal.
Neither exported Reject constructor checks it:
`NewRejectReq(rejected, 0)` and `NewRejectReqRaw(..., 0)` both write `header[3] = 0` unchallenged
(`hsms/control_msg.go:323`, `:348`).
So the validating reader rejects 251 legal codes and the constructors accept the single illegal one.
Recorded as part of D7 below.

**Fixed (`4aacca7`).**
`GetRejectReasonCode`'s bound now accepts `[1, 255]`, matching Table 9's full legal range,
and `ErrInvalidRejectReason`'s text is corrected to match.
`NewRejectReq` and `NewRejectReqRaw` now return `(*ControlMessage, error)` —
a second return value where there was previously one —
and refuse a zero reason code with `ErrInvalidRejectReason`, closing the send-direction gap this section and D7b
both describe.
The signature change is a **compile-time break**: every existing call site needed updating.

## D3b — exported decoders accept a control frame with a body (deviates; FIXED)

**Clause.**
E37 §9.3.3.1: "For STypes not equal to 0 but otherwise specified in this standard,
PType must = 0, and **no message text may be transmitted**."
Reinforced by §8.3.4.1 and its siblings: every control message's length is *always* 10.

**Behavior.**
`decodeOwnedFrame` (`hsms/decode.go:113`) builds a `ControlMessage` from `owned[0:10]` for any control SType
and never looks at `owned[10:]`.
A 14-byte-header-plus-body frame decodes as a valid control message; the body disappears.
`DecodeHSMSMessage`, `DecodeHSMSPayload`, and `DecodeOwnedHSMSPayload` all inherit this.

**The live path is correct.**
`hsmsss/transport_recv.go:110` refuses it before decode:

```go
if msgType != hsms.DataMsgType && len(frame) != 10 {
    t.sendReject(frame, pType, sType)
    return true
}
```

with the clause cited in the comment above it.
So an over-long control frame from a peer draws a Reject and keeps the link, exactly as §7.10.3 asks.

The gap is that the check lives in the transport rather than in the decoder,
so any consumer decoding frames through the public API — a proxy, a log replayer, a test harness — does not get it.

**Fixed (`ffd4635`).**
The E37 §9.3.3.1 body-length check now lives in `hsms.decodeOwnedFrame` itself,
so `DecodeHSMSMessage`, `DecodeHSMSPayload`, and `DecodeOwnedHSMSPayload` all reject a control frame carrying a
body, with the new `ErrControlFrameWithBody` sentinel.
The transport-layer check in `hsmsss/transport_recv.go` stays in place alongside it —
the live path needs to answer Reject and keep the link, which a decode error cannot do —
so both now enforce the clause, for different reasons.

## D4 — E37 §10.1 documentation is incomplete (deviates; FIXED) **[R1] recount: five of six, not four**

**Clause.**
E37 §10.1: "An HSMS implementation **is required** to document the following information", then six items.
§9.2.4.1.2 separately:
"The documentation of the passive local entity **shall** indicate which means it uses to refuse connections."

`hsmsss/doc.go:56` has an "E37.1 §10.1 implementation documentation" section
that discharges E37.1's **three** declarations (device IDs, termination mode, host/equipment).
E37 generic's six are a different list, and **five** of them are unstated anywhere.
(An earlier draft said "four" while its own table below marked five rows Missing.
The table was right.)

| E37 §10.1 item | Status |
|---|---|
| 1. Method for setting protocol parameters | **Covered** — the `WithT3`…`WithT8` godoc, plus `WithConnectionOption` in `hsmsss/doc.go:32` |
| 2. Range allowed and resolution for each parameter | **Fixed.** `hsms/doc.go`'s new "E37 §10.1 implementation documentation" section states that every timer accepts any positive `time.Duration` — nanosecond resolution, an effectively unbounded range, a superset of Table 10 — and flags T5's caveat: it bounds the reconnect-backoff *ceiling*, not the flat per-attempt separation §9.2.1.1 asks for (`d761758`) |
| 3. The option used for refusing incoming connection requests (also §9.2.4.1.2, a "shall") | **Fixed.** D6's behavior fix landed first (`8a36c97`), then `hsmsss/doc.go`'s §10.1 section named it: accept, answer a bare `Select.req` with status 1 (Communication Already Active) under an absolute T7 deadline, close (`d761758`) |
| 4. Maximum message size which can be received | **Fixed.** `hsms.MaxMessageSize` is now exported (was the unexported `maxHSMSMsgLen`) and documented, including the max-item limitation this section explains (`9c255d8`, `d761758`) |
| 5. Maximum expected size of messages sent | **Fixed [R1][R2].** `buildFrameBuffers` now rejects a data message whose frame (10-byte header + body) would exceed `MaxMessageSize`, returning `ErrMessageTooLarge` before the frame is written (`9c255d8`); the rejection is excluded from `DataMsgErrCount`, since it is a caller-side condition that never touches the wire, not a send failure (`9c255d8`); documented at `d761758`. See the v1-parity note below the table — neither the missing cap nor the list length-field behavior was a v2 regression |
| 6. Maximum number of supported concurrent open transactions | **Documented, still unbounded [R2][R3].** `hsms/doc.go`'s §10.1 section now states plainly that there is no cap (`d761758`): a **synchronous** send (`SendDataMessage`, `SendSECS2Message`) registers in the reply map and writes on the caller's own goroutine (`hsms/connection_send.go:204`), bounded only by caller goroutine count; an **asynchronous** reply-expecting send is refused outright as of D8 (see below), so it can no longer reach the wire with no correlation at all, but the synchronous path's transaction count is still unbounded. This is distinct from the outbound send queue (`WithSenderQueueSize`), which bounds buffered frames awaiting the wire, not open reply-wait transactions. The documentation obligation is discharged; the underlying unboundedness is an accepted, undecided trade-off, not fixed |

**v1-parity provenance for item 5.**
The missing outbound cap was inherited, not introduced.
v1 had **no** outbound size cap either, and v1's list check carries the identical hole:
`getDataByteLength(ListType, len(values))` passes the child *count*, not the byte total,
exactly as v2's `appendHeaderBytesFC` does.
That hole is **not** what item 5's fix closes — it is how E5 §9.3 defines a list's length field
(element count, not byte total) and is unenforceable at that layer in either version.
What the fix adds is a *frame*-level ceiling that neither version had before.

The item-vs-frame arithmetic is also worth naming, since v1 capped `msgLen` more strictly on its
primary receive path — `MaxByteSize − MinHSMSSize` (14 bytes lower, `MinHSMSSize` = 4-byte length field
+ 10-byte header) — while a second v1 receive path (`v1:hsmsss/message_reader.go`) used plain
`MaxByteSize`, making v1 internally inconsistent between its own two paths.
v2 unified on `MaxByteSize`, which **relaxed** the stricter of the two v1 receive paths by 14 bytes;
this predates the fixes recorded here and is not itself a deviation this revision addresses.

**[R2] The 32-bit length-prefix wrap in item 5 is now closed, not merely unchecked.**
An earlier revision of this note left it as an unconfirmed risk, reachable only by constructing a 4 GiB message
locally.
`buildFrameBuffers`'s new `MaxMessageSize` check (`9c255d8`) runs before the `uint32(10+n)` conversion on every
send path — synchronous, asynchronous, and enqueued alike, since all three funnel through `writeFrame` —
so `n` is now provably bounded well under 2³², and the wrap is unreachable through any exported send API.

Item 3 was the one with real weight — §9.2.4.1.2 is a "shall",
and a remote entity's operator needs to know whether a refused connection means "busy", "rejected", or "will time out".
**[R1]** It was also the one that could not be discharged by writing documentation alone while the behavior itself
was nonconformant (D6); D6's fix landed first, so the documentation added here (`d761758`) now describes real
behavior rather than papering over a gap.

## D5 — reconnect backoff does not honor the T5 connect-separation floor (deviates; OPEN, documented) **[R1]**

**Clause.**
E37 §9.2.1.1: "After an active connect procedure terminates by any means (successfully or unsuccessfully),
the Entity **should not** initiate another active connect procedure (for the same Remote Entity)
until the T5 Connect Separation Time has elapsed."
§9.2.1 gives the reason in plain terms:
"Frequent use of the active mode connect procedure to the IP Address and Port Number of an entity not yet ready to accept connections
can be **hostile to TCP/IP operations**."

**Behavior.**
`reconnectLoop` seeds its delay from `reconnectBackoffInitial` and multiplies up, clamping at T5
(`hsms/connection_lifecycle.go:407`–`424`).
The default seed is **100 ms** with a multiplier of 2.0 (`hsms/connection_config.go:86`–`87`), against a default T5 of 10 s.

So the separation after the first failed connect is 100 ms, then 200 ms, 400 ms, and so on.
With connect duration treated as negligible, new attempts begin at cumulative offsets
**0.1, 0.3, 0.7, 1.5, 3.1, 6.3, 12.7 s**.
**Six** of them land inside the first 10 s T5 window that §9.2.1.1 says should contain none,
and **three** inside the first second. **[R2]**

**Why the earlier draft passed it.**
The previous revision wrote that a ramp topping out at T5 "satisfies … from the T5-th attempt onward and is stricter early on."
That is exactly backwards.
A *shorter* wait is *less* restrictive, and the early attempts are precisely the ones §9.2.1 identifies as hostile.
**[R2]** An earlier revision of this section said "seven connect attempts in the first second".
That was wrong twice over — the correct figures are three in the first second and six inside the first T5 window.
The conformance conclusion is unchanged: §9.2.1.1 asks for the **whole** T5 interval after *each* terminated
active connect procedure, and six intervening attempts is six too many.

**Severity.**
Moderate, and it is live wire behavior a peer can observe.
It is a "should", not a "shall", and aggressive early reconnect is a deliberate and common product choice —
`WithReconnectBackoff` exists to tune it, and passing `WithReconnectBackoff(t5, 1.0)` gives flat-T5 conformant behavior today.
What made it a finding was that the **default** was nonconformant and nothing said so.

**Decided and documented, default unchanged (`d761758`).**
Two honest fixes were on the table, and they differ in kind:
default the seed to T5 (conformant out of the box, slower recovery),
or keep the fast seed and document the deviation on `WithReconnectBackoff` and `WithT5` with the conformant
setting spelled out.
The chosen path is the second.
`WithT5` and `WithReconnectBackoff`'s godoc now state the default's deviation from §9.2.1.1's flat T5 separation,
give the cumulative attempt offsets under the defaults —
0.1, 0.3, 0.7, 1.5, 3.1, 6.3 s, six attempts inside the first T5 window where the standard wants none —
and name `WithReconnectBackoff(t5, 1.0)` as the conformant setting.
**This remains the one deviation this revision leaves open by choice**, not by omission:
the default still does not honor T5, and no code changed to make it do so.

## D6 — passive refusal of a second connection matches none of E37's three options (deviates; FIXED) **[R1]**

**Clause.**
E37 §9.2.4.1.1: a passive entity unable to service more than one connection
"**will follow one of these three procedures**":

1. Accept the connection, but answer any subsequent Select with **Communication Already Active** (status 1).
   The connect procedure terminates successfully and the link stays in NOT SELECTED.
   E37 calls this "the preferred option".
2. **Actively reject the connection request** — `t_snddis` in TLI — so the remote's connect procedure terminates *unsuccessfully*.
3. Refuse to listen for or accept it at all, so the remote's connect procedure times out.

§9.2.4.1 also requires that each connection formed on the published port
"must exhibit the behavior defined in the HSMS state diagram as if it were completely independent of any other connection".

**Behavior.**
`acceptLoop` does neither of the three (`hsmsss/transport_passive.go:124`–`136`):

```go
extra, err := ln.Accept()
...
_ = extra.Close()
```

It **accepts** — so the second peer's connect procedure completes *successfully* and it enters CONNECTED / NOT SELECTED —
and then immediately closes, dropping it back to NOT CONNECTED with no Select attempted and no status returned.

**Why this is not option 2.**
Option 2 rejects the *connection request*.
By the time `Accept` returns, the TCP handshake is complete and the remote entity has already observed a successful connect.
The remote therefore sees connect-then-instant-disconnect, which the state diagram reaches only via transition 3
("breaking of TCP connection") with no intervening cause it can name.
Under option 1 it would learn *why* (status 1); under option 2 its connect would simply fail;
under option 3 it would time out.
Here it learns nothing, and a reconnecting peer will loop.

**Severity.**
Moderate, and live. This is the deviation with the clearest interoperability cost:
a second host that dials in gets no diagnostic and no back-pressure signal.

**The conformant fix, and a status-code trap. [R2]**
E37 names option 1 as preferred, and it names the response code literally:
"always respond to any subsequent HSMS select procedures with the **Communication Already Active** response code",
which E37 Table 7 assigns to status **1** (`hsms.SelectStatusAlreadyActive`).

An earlier revision of this section proposed `hsms.SelectStatusAlreadyUsed` (**3**, "Connect Exhaust") instead.
That is a trap worth naming, because 3's *description* is the better semantic fit —
"the connection was accepted, but the entity is already servicing a separate TCP/IP connection
and is unable to service more than one at any given time" describes this situation exactly.
But option 1 specifies status 1, so **status 3 does not literally implement option 1**.
Answering 1 is the defensible "preferred option" implementation;
answering 3 may communicate more but must then be described as a reasoned choice, not as option-1 conformance.

**The cost is not small, and an earlier revision understated it.**
`handleSelectReq` already emits status 1 for a duplicate Select, but it cannot simply be pointed at the extra socket:
its response goes out through the *current* connection runtime.
The extra socket needs an isolated, T7-bounded, one-shot Select exchange of its own —
enough machinery to read one frame, answer it, and close.

**Fixed: that machinery now exists (`8a36c97`).**
`acceptLoop`'s refuse path calls a new serial, per-connection `refuseExtraConn` helper that implements option 1:
accept the extra socket, arm one absolute deadline covering the whole exchange (`SetDeadline(now + T7)`),
read a fixed 4-byte length prefix and 10-byte header via `io.ReadFull` — never allocating from an untrusted
length — answer a bare `Select.req` with `Select.rsp` status **1** (`hsms.SelectStatusAlreadyActive`, not the
tempting-but-wrong status 3 named above), then close.
Extra connections are refused **serially**, one dialer at a time, so a slow or silent extra dialer delays refusing
the next one but never touches the live session's socket, FSM, or open transactions.
The exchange is interruptible at shutdown: `Stop` publishes a stop flag and closes whatever socket is already
in flight, so `Stop` never waits out the refusal deadline.
`TestPassive_RefusesSecondConnection` is rewritten to assert the new status-1-then-close answer.

## D7 — exported constructors accept values the standards forbid (deviates; FIXED) **[R1]**

Two validation gaps. They are **not** the same class, and an earlier revision wrongly said neither reaches the wire. **[R2]**

The Reject-constructor gap is inert: nothing in the library calls `NewRejectReq` with a zero reason.
The even-function-primary gap is **live, misuse-triggered wire behavior** —
`SendDataMessage`, `SendDataMessageAsync`, and `SendSECS2Message` all construct through `NewDataMessage`
and then write or enqueue the result (`hsms/session.go:68`, `:99`, `:111`),
so an even-function, W-clear "primary" goes out on the wire through ordinary public APIs.
Neither is reachable through *correct* use of the library.

**§8.3.21.3 — a Reject reason code is "always nonzero".**
`NewRejectReq(rejected, 0)` and `NewRejectReqRaw(..., 0)` write `header[3] = 0` with no check
(`hsms/control_msg.go:323`, `:348`).
Zero is the one reason code E37 never permits.
Pairs with D2, which rejects 251 codes that *are* permitted — the validation is inverted at both ends.

**E5 §7.2 — "All primary messages will be given an odd-numbered function code."**
(**[R2]** Note the verb: E5-0813 says "**will**", not "shall". The summary table previously mislabeled this as a "shall".)
`NewDataMessage` rejects an even function only when the W-bit is set (`hsms/data_msg.go:319`);
an even function with W clear passes, and `SendDataMessage` — documented as sending "a primary SECS-II data message" —
will send it as one.

The nuance that keeps this minor:
correct usage has a dedicated API. `ReplyDataMessage` (`hsms/endpoint.go:106`) is how a secondary is sent,
and it derives System Bytes from the primary.
The gap is only that the *primary* path does not refuse an even function.

**Do not fix this in `NewDataMessage`.**
That constructor is shared by `ReplyDataMessage` and `ForwardDataMessage`,
and a relay must be able to forward a secondary verbatim.
The check belongs on the primary-send entry points, not on the message type.

**Fixed exactly that way (`ea2742f`).**
`SendDataMessage`, `SendDataMessageAsync`, and `SendSECS2Message` now guard with the new `ErrEvenFunctionPrimary`
before `NewDataMessage` is called, so a refused send never burns a System Bytes value.
`NewDataMessage` itself is untouched — `ReplyDataMessage` and `ForwardDataMessage` still construct through it
and still legitimately carry even functions.
D7b's Reject reason-code gap is fixed too, as part of D2's `4aacca7` — see that section above.

## D8 — a W-bit primary sent asynchronously gets no T3 reply timer (deviates; FIXED) **[R4]**

**Clause.**
E37 §9.4.1.2: "After sending a Primary Message with W-Bit 1 (Reply Expected),
the sender **must** begin a reply timer, initialized to the T3 value."
Not "should" — this is one of the few flat "must" obligations on the sender in E37.

**Behavior.**
`SendDataMessageAsync` takes `replyExpected` as a parameter and preserves it into the header (`hsms/session.go:103`),
then hands the message to `SendAsync`, which registers no reply channel and arms no protocol timer
(`hsms/connection_send.go:417`).
So `SendDataMessageAsync(ctx, 1, 3, true, item)` puts a W-bit-1 primary on the wire with **no T3 timer running**.
No T3 Timeout Error can ever be raised for it, and any reply the peer sends lands in `DataMessageHandlers`
rather than closing a transaction.

**How this was missed twice.**
It is the propagated consequence of a round-3 finding that this audit accepted but did not chase to its end.
Round 3 corrected D4 item 6 to say the async path "reaches the wire with no local reply correlation" —
which is the same fact — without noticing that the §9.4.1.2 row in the conformant table asserts the opposite.
The row was written against the synchronous path and never re-examined.

**Is it a deviation or a delegated obligation?**
Both readings are available and the document should not pretend otherwise.
`SendDataMessageAsync`'s own godoc says "No reply is awaited", so a caller passing `replyExpected: true`
has arguably opted into owning correlation — the same delegation `ForwardDataMessage` makes explicit.
The difference is that `ForwardDataMessage` **documents the duty it delegates**, at length, including the failure mode.
`SendDataMessageAsync` does not mention T3, does not warn that W=1 produces an untimed transaction,
and does not say the caller must run its own timer.
So this is recorded as a deviation, not a delegation: an undocumented delegation is indistinguishable from an omission.

**Three honest fixes** were on the table, and they differ in kind:
reject `replyExpected: true` on the async entry point (narrowest, and arguably what the API name already implies);
arm a T3 timer and surface the timeout through a handler;
or document the delegation explicitly, as `ForwardDataMessage` already does.

**Fixed by the narrowest option (`ea2742f`).**
`SendDataMessageAsync` now refuses `replyExpected: true` with the new `ErrAsyncReplyExpected`:
`SendAsync` enqueues a message without beginning a reply timer, and SEMI E37 §9.4.1.2 requires a primary that
expects a reply to begin one, so a caller needing a reply-bearing transaction is directed to `SendDataMessage`
instead.
Four `hsmsss` integration test call sites that passed `replyExpected: true` to the async entry point were swept
to `false`; none of them read the primary's W-bit, so the flip is inert to what each test actually verifies.

## Deviations reviewed and accepted (no action)

**[R1] E5 §9.3 — nested lists are capped at depth 64.**
`MaxListDepth` (`secs2/decode.go:15`) rejects a list nested deeper than 64 levels (`:151`), and E5 authorizes no such limit:
§9.3 defines a list element as "either an item or a list" with no depth bound.
A message that is legal under E5 and inside every length limit is refused.
This is a deliberate stack-exhaustion guard on an attacker-reachable decode path, the depth is far beyond any real message,
and the alternative — unbounded recursion on hostile input — is worse.
Keep, but recorded here as an accepted deviation rather than left in the conformant table,
where an earlier draft had it as an unqualified pass.

**§7.4 / §7.7.3 — simultaneous Select and simultaneous Deselect.**
Simultaneous Select is handled: a duplicate `Select.req` is answered with status 1 and the link is kept.
The full rationale, and why status 1 is treated as success on both sides,
is in the E37.1 audit's "Deviations reviewed and accepted".
Simultaneous Deselect does not arise — we never initiate a Deselect.

**§9.2.4.1 — multiple connections to one published port.**
E37 permits but does not require servicing more than one.
`hsmsss` services exactly one, per E37.1's single-session profile.
**That choice is permitted and is accepted here.
The *mechanism* used to refuse the extras was not — see D6, now fixed. [R5]**
An earlier revision of this paragraph said "only the documentation of the refusal method is missing",
which was written before D6 existed and contradicted it.
Three separate things were true and should not have been collapsed:
servicing one connection is conformant (this section),
the accept-then-close refusal matched none of E37 §9.2.4.1.1's three options (D6, since fixed to option 1),
and whichever method was settled on still needed to be documented (D4 item 3, since done).

## Not implemented (by design)

| Clause | What | Why |
|---|---|---|
| §7.7.1 | Deselect **initiator** | `NewDeselectReq` exists (`hsms/control_msg.go:217`) but has **no non-test caller** anywhere in the repo — a full unfiltered sweep finds only unit tests and the `integration/` peer simulator. E37.1 §5.1.1 forbids using Deselect to end communications; teardown uses Separate. The constructor is kept because `hsms` implements generic E37 and a future HSMS-GS profile would need it |
| §5.5.2.2 "at least one session" | Multi-session operation | HSMS-GS territory; `hsmsss` is single-session by construction |
| §8.2.6.4, Table 4 | PType 1–127, reserved for subsidiary standards | No subsidiary presentation type is implemented; non-zero PType draws Reject reason 2, which is what §7.10.3 prescribes for an entity that does not support it |
| §8.2.6.6, Table 5 | SType 11–127, reserved for subsidiary standards | Same — no subsidiary STypes; undefined ones draw Reject reason 1 |
| §6.1 / Appendix 1 | TLI API bindings | Go's `net` package is the BSD-sockets equivalent; §6.1 explicitly permits any API |

---

# Part 3 — `hsmsss` against SEMI E37.1: re-verification, not re-derivation

`docs/specs/e37-1-hsms-ss-conformance-audit.md` (2026-08-10) already walks every normative E37.1 clause.
Rather than repeat it, its findings were re-checked against the current tree.

`git log --since=2026-08-08 -- hsms hsmsss` shows four commits, all of them the audit's own fixes and docs:
`1ec0dcd` (control-message SessionID), `f6cd783` (Separate in any substate), `44e8efe` and `fc33602` (the audit document and its citations).
**No code had landed on `hsms` or `hsmsss` since that audit — as of this section's original write-up, 2026-08-10.**
That is no longer current: Parts 1–2 above record nine commits' worth of `hsms`/`hsmsss` changes since,
and the addendum after the two corrections below records what happened to those corrections specifically.
This spot-check section is left as the historical re-verification it was, not rewritten to pretend otherwise.

Spot-checks confirming the audit still describes the tree:

- Gap 1 — all five outbound sites carry `hsms.ControlSessionID`:
  `hsmsss/transport_active.go:56` (Select.req),
  `hsms/connection_lifecycle.go:347` (farewell Separate),
  and the three Reject senders at `hsmsss/transport_control.go:160`, `:200`, `:220`.
- Gap 2 — `handleSeparateReq` takes a generation ctx and tears down in any connected substate
  (`hsmsss/transport_control.go:112`).
- Gap 3 — the lenient Linktest responder still carries its full rationale comment naming §7.4, §7.7, §7.8.2, and §9.1.1,
  and pointing at the audit and the pinning test (`hsmsss/transport_control.go:230`–`248`).
- §5.1.1 — no outbound `NewDeselectReq` call site exists.
- §10.1 — the three E37.1 declarations are in `hsmsss/doc.go:56`–`96`.

**Verdict: the E37.1 audit's *findings* hold; two of its *classifications* do not. [R1]**
Its one open item — the residual check-then-call race in the C1 straggler guard, deferred to its own branch —
is unchanged and still open.
D4 above adds the E37 *generic* §10.1 items that the E37.1-scoped audit did not cover.

Two corrections that belong to the E37.1 axis, surfaced by the external review of *this* document:

**E37.1 §7.3 is misfiled as conformant.**
The clause is absolute: "Deselect **shall not be used** in an HSMS-SS implementation.
Communication is ended using the Separate Procedure."
The prior audit's "Clauses verified conformant" table lists §7.3 with the note
"Responder-only (`handleDeselectReq`), never initiated — answering rather than failing is deliberate leniency
so a peer is not stranded on T6".
Those two halves contradict each other.
Answering a Deselect.req **is** use of the Deselect procedure, and §7.7 makes any §7 violation a communications failure.
The behavior is defensible for exactly the reason given, and should not change —
but it is the same shape as that audit's Gap 3 (the lenient Linktest responder), which was correctly filed as
a *deliberate deviation*, not as conformance.
It belongs in that audit's "Deviations reviewed and accepted" section with the same treatment:
a rationale comment at `handleDeselectReq`, and a test pinning it.
**[R2] But do not reuse the prior audit's justification unexamined.**
It argues that answering avoids stranding the peer on T6.
That is not a universal consequence: E37.1 §7.7 makes the forbidden Deselect a communications failure,
and E37 §9.1.1's remedy is that the entity "should terminate the TCP/IP connection" —
closing immediately **releases** the peer rather than leaving it waiting out T6.
So strictness is not obviously worse for the peer here, unlike Gap 3's Linktest case where it demonstrably drops a live link.
Keeping the lenient responder remains defensible as an interoperability choice,
but it needs a better-argued rationale than the T6 one when it is rewritten as a deviation.

**Inbound control-message SessionID is never checked against 0xFFFF.**
E37.1 §8.1: "In HSMS-SS Control Messages, Session ID will always assume the special value 0xFFFF."
The prior audit fixed every **outbound** site (its Gap 1) and verified that the receive path does not *reject* on session ID.
What neither audit recorded is that the receive path does not *check* it either.
An inbound Select.req carrying, say, `0x0042` is accepted and its session ID mirrored back in the Select.rsp —
the prior audit's own `TestPassive_SelectRspMirrorsRequestSessionID` probes with exactly that value and asserts the mirroring.
E37 §8.3.7.1 requires that mirroring ("must be equal to the value of the session ID in the corresponding Select.req").

**[R2] But calling the response simply "correct" is wrong, and this is sharper than an earlier revision allowed.**
The emitted `Select.rsp` is itself an HSMS-SS **control message**, and E37.1 §8.1 admits no exception:
"In HSMS-SS Control Messages, Session ID will always assume the special value 0xFFFF."
So echoing `0x0042` obeys generic E37 §8.3.7.1 and **violates** E37.1 §8.1 on the way out.

This is the same generic-vs-subsidiary collision the prior audit's Gap 1 was written to catch,
surviving in the one place Gap 1 did not look — the response to a *malformed* request.
The prior audit resolved the general case by noting that `.rsp` session IDs are "bound to the request, not free choices",
which is true under E37 but does not survive E37.1 §8.1's unconditional wording when the request is itself nonconformant.

So there are two unrecorded items, not one:
inbound, we never treat the nonconformant request as the §7.7 communications failure E37.1 makes it;
outbound, we answer it with a control frame that breaks §8.1.
Neither is a new bug — both are unlabelled acceptances — but the outbound half deserves an explicit decision:
echo per E37, or force `0xFFFF` per E37.1.
It cannot satisfy both.

**Both corrections carried out (`0cd8182`).**
§7.3 moved from `docs/specs/e37-1-hsms-ss-conformance-audit.md`'s "Clauses verified conformant" table into its
"Deviations reviewed and accepted" section, with the T6-avoidance rationale replaced per the **[R2]** correction
above: E37 §9.1.1's remedy releases the peer sooner than any dwell timer would, so the kept behavior is an
interoperability trade, not a stranding-avoidance necessity.
`TestDeselect_AnsweredDespiteE371Prohibition` pins it.
The §8.1 echo decision landed as "follow E37 generic, which binds the response to the request, rather than
second-guessing the request's own conformance" — a new "§8.1 — inbound control Session ID leniency (Select
responder)" entry in the same accepted-deviations section, pinned by
`TestSelectRsp_EchoesNonConformantSessionID`.
Both are documentation-and-test changes; neither altered `handleDeselectReq`'s or `handleSelectReq`'s observable
behavior.

---

# Coverage

**[R1] Two coverage claims in the earlier draft were false and are corrected here.**
Tables 3–22 sit under E5 **§§9.6–9.8** (Data Item Dictionary at §9.6, line 387; Variable Item Dictionary §9.7; Object Dictionary §9.8),
**not** under §10 — so "§§5–9 read in full" and "Tables 3–22 out of scope" could not both be true as written.
The corrected boundary is below.
And E37 Appendix 1 is **not** non-normative: it opens
"NOTICE: The material in this Appendix is an official part of SEMI E37 and was approved by full letter ballot procedures."

**Read clause-by-clause and traced to symbols:**

- E5-0813 §§5, 6, 7, 8, **9.1–9.5** in full
  (terminology, message transfer protocol, streams and functions, transaction protocols, and the item/list data structures),
  including Tables 1 and 2.
  **[R1]** §§9.6–9.8 — the Data Item, Variable Item, and Object dictionaries, carrying Tables 3–22 — were **not** read: they are dictionary content, not encoding rules.
- E37-0413 §§4, 5, 6, 7, 8, 9, 10, including Tables 3–10 and the state-transition table,
  with the exceptions listed immediately below.
- E37.1-0702: not re-read; covered by the prior audit, re-verified against the tree as above,
  plus the two classification corrections in Part 3.

**[R1] Clauses inside those ranges that were NOT traced to a symbol** — listed rather than left to look like passes:

| Clause | Why untraced |
|---|---|
| E37 §6.2.1, §6.2.4 | IP addressing and TCP port-range conventions. Delegated wholesale to Go's `net` package; no library code constrains either. Should be confirmed against `hsmsss` address handling before anyone calls this axis complete |
| E37 §6.3.4, §6.3.6 | The step-by-step passive and active connect procedures. The *outcomes* are traced (§6.3.2 connect modes, D6), but the numbered steps were not walked against `transport_passive.go` / `transport_active.go` |
| E37 §7.2 | "Urgent data is not supported." Never used, but not verified — no `MSG_OOB` / urgent-data path was searched for |
| E37 §7.6, §7.6.1 | The data-transaction taxonomy (primary-with-reply, primary-without-reply). Exercised throughout but not traced clause-by-clause |
| E37 §9.3.2.1 | "Stateless" transactions — an initiator awaiting a response may receive any message valid for its state. Plausibly satisfied by the single sequential recv loop, but not demonstrated |
| E37 Appendix 1 | Normative (see above), but its content is TLI/BSD API construct tables. Go's `net` package is the BSD-sockets equivalent, and §6.1 permits any API. Declared out of scope on that basis rather than dismissed as non-normative |

**[R2] Further clause groups, absent from the earlier revision of this table.**
The "traced in full" claim is narrowed here to **normative behavioral requirements**;
terminology definitions and informative examples are deliberately excluded rather than counted.
Even so, these behavioral clauses were neither traced nor previously declared:

| Clause | Status |
|---|---|
| E5 §6.2, §6.4.1, §6.5, §6.6 | Message-transfer-protocol obligations (device ID identification, transaction timeout notification, multiple open transactions). Satisfied in spirit by the `hsms` engine, but not traced from `secs2` |
| E5 §7.2, remaining rules | Reply function = primary + 1; the even function after a no-reply primary is *reserved*; F0 is reserved for abort. Only the odd/even primary rule was traced (D7a). The "reserved" rules are unenforced and unexamined |
| E5 §8.2, §8.4.3 | Transaction definition and conversation naming conventions |
| E5 §9.5 | Worked encoding examples. Not walked byte-for-byte; the equivalent checks were done by probe instead |
| E37 §6.4, §6.5 | Connection termination — E37 permits termination only from NOT SELECTED. **Worth a real trace**: our teardown path closes from Selected after a farewell Separate, which §7.9.1 sanctions, but the interaction was not verified |
| E37 §7.8.1 | Linktest *initiator* procedure. The responder was traced; the initiator's T6 handling was not |
| E37 §7.10.1, §7.10.2 | The Reject procedure from both sides as a *procedure*. Individual Reject emissions were traced; the initiator's "takes appropriate action" was not |
| E37 §§8.3.4–8.3.22 fixed header fields | Each control message fixes header bytes 2 and 3 (mostly to 0). **Emission conforms; validation does not exist** — see the leniency below |

**[R2] One of those groups exposes a real inbound leniency, recorded here rather than left implicit.**
E37 §8.3.4.3 and its siblings fix header bytes 2 and 3 for every control message
(0 for Select.req / Deselect.req / Linktest.req / Separate.req; status codes for the responses).
`decodeOwnedFrame` validates **PType and SType only** and accepts arbitrary bytes 2 and 3 (`hsms/decode.go:97`–`119`);
the live dispatcher adds only PType/SType and body-length checks (`hsmsss/transport_recv.go:97`).
So a `Select.req` carrying garbage in bytes 2–3 is accepted and processed.
The practical cost is low — those bytes are ignored for `.req` frames, and `selectStatus` reads byte 3 only for `.rsp` —
but it is unvalidated input on the decode path and belongs in the accepted-leniency inventory, not unmentioned.
This is **unchecked** as to consequence: no probe was run against a malformed-header control frame.

These are **unchecked**, not compliant.

**Read but declared out of scope, with reasons given in-line:**

- E5 §§9.6–9.8 and §10, carrying Tables 3–22 — the data-item, variable-item, and object dictionaries plus the SnFn message detail.
  This is `gem` territory, not `secs2`. **[R3]** It is **E5** territory, not E30's.
  **Unchecked against `gem`.**
- E37 Appendix 1 (TLI/BSD API tables) — **normative [R1]**, but satisfied by any TCP API; see the untraced-clause table above.

**Not read at all:**

- SEMI E30 (GEM).
  No copy exists under `~/semi_standards/`.
  **[R3]** The `gem` package is **entirely unaudited** by this document because it was outside the requested scope —
  *not* because E30 is missing. Those are different reasons and an earlier revision ran them together.

  **[R2] Correction to an earlier framing.** This audit twice implied the §§9.6–10 dictionaries are "E30 message definitions".
  They are not — **E5 owns them**: §§9.6–9.8 are E5's data-item, variable-item, and object dictionaries,
  and §10 is E5's own stream/function message detail.
  E30 layers GEM *requirements* on top (which messages an implementation must support, and in what state model).
  The practical consequence is that **the missing E30 copy does not block a `gem` audit**:
  `gem`'s builders can be checked against E5 §§9.6–10 for message *shape* today,
  using the copy already on disk.
  Only the GEM-conformance half — capability requirements, state models, scenarios — needs E30.
- SEMI E4 (SECS-I).
  Present at `~/semi_standards/markdowns/e004-00-0699-0612r/`, but `secs1` was outside the requested scope.
  **Unaudited.**
- The `sml` package.
  Not governed by any of these standards.

**Verification performed:**

Format-code coverage, length-byte handling, boolean normalization, zero-length legality, reserved-format-code rejection,
and the trailing-byte behavior of D3
were each confirmed by running a decode probe against the current tree, not by reading alone.
Everything else is a source read traced to the cited `file:line`.

# Actions

**All items but D5 have since been taken**, on the branch recorded at `docs/specs/e5-e37-conformance-fixes/`.
The original suggested order (most valuable first) is preserved below, unedited except for the checkbox and a
closing note recording what shipped; each note names the implementing commit(s), verified against
`git log --oneline` before this revision was written.
D1 and D3 are ticked because their Action — ship the receive-side posture decided for them — is discharged;
neither ships as a behavior change, and both sections above explain why.

- [x] **D6** — make passive refusal one of E37 §9.2.4.1.1's three options.
  **[R3]** Option 1 is the standard's stated preference and requires status **1**, `hsms.SelectStatusAlreadyActive` ("Communication Already Active") —
  **not** `SelectStatusAlreadyUsed` (3, "Connect Exhaust"), which an earlier revision of this line recommended and which the D6 body now rejects.
  Live wire behavior, clearest interop cost.
  **Done (`8a36c97`)** — see D6 above for the shipped `refuseExtraConn` mechanism.
- [ ] **D5** — decide the T5 question: default `reconnectBackoffInitial` to T5 (conformant by default), or keep the fast seed and document the deviation plus the conformant setting on `WithReconnectBackoff` and `WithT5`.
  A product decision, not a correctness one.
  **Decided, not fixed (`d761758`):** the fast seed stays the default, and the deviation plus the
  conformant `WithReconnectBackoff(t5, 1.0)` setting are now documented on `WithT5`/`WithReconnectBackoff`.
  This is the one row this revision leaves genuinely open — the checkbox stays unticked because no code changed.
- [x] **[R4] D8** — decide what `SendDataMessageAsync(replyExpected: true)` means: reject W=1 there,
  arm a T3 timer and surface the timeout, or document the delegation the way `ForwardDataMessage` already does.
  E37 §9.4.1.2 is a "must", and this needs no misuse to reach.
  **Done, narrowest option (`ea2742f`)** — `replyExpected: true` is refused with `ErrAsyncReplyExpected`;
  see D8 above.
- [x] **D1** — add the §9.4.1 stream/function/SessionID check to the reply registry.
  Carry the primary's three fields in the registry entry and compare in `route`;
  a mismatch is a miss, so the message falls through to the session handlers as unsolicited rather than being mis-delivered.
  **Done as observation, not as a default miss (`4732ee6`)** — Stream/Function are now carried and
  compared; SessionID stays outside this registry, owned by `WithSessionIDValidation`.
  A mismatch always counts (`ReplyMismatchCount`); it is only a miss under the opt-in `WithStrictReplyMatching`.
  Delivery under the default is unchanged — see D1 above for why the "mismatch = miss" behavior this line
  originally asked for is opt-in rather than the new default.
- [x] **D4 item 3** — state the passive-mode connection-refusal method in `hsmsss/doc.go`.
  **[R3][R4]** §9.2.4.1.2 is a "shall" — not the only one in the set (D3's is a "shall not", D8's is a "must").
  The *documentation* obligation is discharged by one paragraph; **overall conformance still needs the D6 behavioral fix**,
  since there is currently no conformant refusal option to document.
  **Done (`8a36c97` for the behavior, `d761758` for the documentation)** — both halves this line called out are closed.
- [x] **D4 items 2, 4, 5, 6** — add an "E37 §10.1 implementation documentation" section to `hsms/doc.go`.
  Consider exporting the maximum received message size, which item 4 asks implementations to publish.
  **Done (`9c255d8` exports `hsms.MaxMessageSize`; `d761758` add the documentation section)** —
  item 6 is documented as still unbounded, which is accurate; see D4 above.
- [x] **D2** — widen `GetRejectReasonCode` to accept `[1, 255]` and reject only 0,
  and correct `ErrInvalidRejectReason`'s text.
  Check callers first: the strict range may be load-bearing somewhere as an "is this one of the four we handle" test,
  in which case that test wants its own predicate.
  **Done (`4aacca7`)** — no call site depended on the strict `[1, 4]` range.
- [x] **D3** — reject trailing bytes on the **message-body path only** (`secs2.DecodeOwnedFrame`), not in the public parser.
  **[R6] Scope narrowed during implementation planning, correcting this section's own framing.**
  §9.2 defines the item encoding and never says a buffer holds exactly one item;
  the clause that forbids extra items is §10.3.1.3(2), a message-definition rule this audit places outside `secs2`'s scope.
  There is also no safety motive — `DecodeHSMSMessage` enforces `len(data) == 4+msgLen`, so the decoder cannot over-read.
  What remains is silent data loss on a correctly-framed body carrying extra items,
  which belongs at the capability-gated body seam.
  `Decode`/`DecodeOwned` stay lenient, with godoc stating first-item semantics.
  See `docs/specs/e5-e37-conformance-fixes/01-implementation-plan.md`, Task 5.
  **Shipped as observation, not rejection (`bab6662`)** — the "reject" framing this line was written against was
  superseded during planning by the observed posture: no path rejects trailing bytes; `TrailingBytes()` counts
  them instead.
  See D3 above.
- [x] **D3b** — move the control-frame-length check from `hsmsss/transport_recv.go` into `hsms.decodeOwnedFrame`
  so every decode path gets it.
  The transport check can stay as-is, since it must Reject rather than error.
  **Done exactly as described (`ffd4635`)**.
- [x] **D7a / D7b** — reject an even function on the *primary-send* entry points (`SendDataMessage`, `SendDataMessageAsync`, `SendSECS2Message`);
  reject reason code 0 in both Reject constructors.
  Not in `NewDataMessage` — `ReplyDataMessage` and `ForwardDataMessage` share it and legitimately carry even functions.
  **Done (`ea2742f` for D7a, `4aacca7` for D7b)** — both exactly as scoped, `NewDataMessage` untouched.
- [x] **[R2] Outbound size bound** — `buildFrameBuffers` has no `maxHSMSMsgLen` check and its `uint32(10+n)` conversion
  carries a `nolint` comment asserting a bound nothing enforces.
  Add the send-side cap, and decide whether the 32-bit wrap is reachable enough to warrant a test.
  **Done (`9c255d8`)** — the cap is added, the `nolint` comment is removed as no longer needed, and the
  32-bit wrap is closed as a consequence rather than tested as a separate risk; see the note above the D5 heading.
- [x] **[R2] E37.1 §8.1 vs E37 §8.3.7.1** — decide whether a `Select.rsp` answering a nonconformant request
  echoes the request's session ID (E37) or forces `0xFFFF` (E37.1). The two cannot both be satisfied; pick one and record why.
  **Decided (`0cd8182`)** — follow E37 generic and echo the request; recorded as an accepted deviation in
  `docs/specs/e37-1-hsms-ss-conformance-audit.md`.
  See the Part 3 addendum above.
- [x] **E37.1 follow-up** — reclassify §7.3 (Deselect responder) from conformant to an accepted deviation in
  `docs/specs/e37-1-hsms-ss-conformance-audit.md`, matching Gap 3's treatment, and record the unchecked inbound
  control-message SessionID as a fourth accepted leniency.
  Documentation and a pinning test; no behavior change.
  **Done (`0cd8182`)** — see the Part 3 addendum above.

## Review

This document was reviewed to consensus by an external reviewer (Codex, `gpt-5.6-sol`) over six rounds.
Corrections are marked inline **[R1]**–**[R5]** by the round that drove them.

| Round | Findings | Substantive | What it caught |
|---|---|---|---|
| 1 | 9 | 9 | Two deviations (D5, D6) were sitting in the *conformant* tables with arguments that ran backwards; `ForwardDataMessage`'s System-Bytes delegation; the D4 miscount; Tables 3–22 misplaced under §10; E37 Appendix 1 wrongly called non-normative |
| 2 | 7 | 4 | D4 item 5 still false after its first fix — there is **no** outbound size cap at all; D5's arithmetic wrong in both figures; D6 proposed status 3 while claiming option 1; D7 reaches the wire; inbound control header bytes 2–3 unvalidated |
| 3 | 5 | 1 | `SendDataMessageAsync` also carries the W-bit; four stale-text contradictions introduced by editing sections independently; E5 owns §§9.6–10, so a `gem` shape audit is not blocked by the missing E30 |
| 4 | 3 | 1 | The §9.4.1.2 conformant row contradicted the corrected D4 item 6 → promoted to **D8** |
| 5 | 1 | 1 | The last surviving pre-D6 sentence, still saying only documentation was missing; D8 mislabeled as peer-observable |
| 6 | 0 | 0 | Consensus |

**The recurring failure mode, recorded because it is the useful part.**
Every round after the first found contradictions created by the previous round's fixes.
Correcting a cited line without chasing the same claim to every other site that asserted it
produced four of round 3's five findings, two of round 4's three, and the whole of round 5's.
D8 exists only because round 4 noticed that round 3's correction to D4 item 6 had silently falsified
a "conforms" row two hundred lines away.
A conformance table is a set of claims that reference each other; editing one is editing several.

**What the loop did not do.**
It did not verify the unchecked items.
The coverage section names them — six E37 clause groups, several E5 groups, the 32-bit length wrap,
the malformed-control-header consequence — and they remain **unchecked**, not passed.
Consensus here means the document's claims are true and consistent,
not that the implementation has been exhaustively audited.

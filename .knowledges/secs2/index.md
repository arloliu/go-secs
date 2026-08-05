---
type: Unit
title: secs2
description: Deeply immutable SECS-II items (SEMI E5 §9), wire encoding, and the decode paths.
---

# Responsibility

The item model and its wire format: construction with deferred errors, three accessor families (copy, zero-copy iterator, indexed), `AppendTo`/`EncodedLen` encoding, and the decode entry points with their differing ownership contracts.

# Boundary

No transport, no message envelope beyond `SECS2Message`. HSMS headers, framing, and session state belong to `hsms`; SML text belongs to `sml`.

# Entries

* [What a decoded item aliases](/secs2/decode-aliasing.md) - which leaves point into the wire buffer, which copy, and where the doc comment is imprecise.
* [Item slab carving and its retention model](/secs2/item-slab-retention.md) - what retaining one decoded leaf actually pins.

# Entry points

- decode: `secs2/decode.go` → `Decode`, `DecodeOwned`, `DecodeOwnedFrame`
- shortcut constructors: `secs2/shortcut.go` → `L`, `A`, `B`, `BOOLEAN`, `I1`–`I8`, `U1`–`U8`, `F4`, `F8`
- encoding: `secs2/item.go` → `Item.AppendTo`, `Item.EncodedLen`, `Item.ToBytes`
- message wrapper: `secs2/message.go` → `NewMessage`

# Read first

`secs2/doc.go` documents the immutability contract, the three accessor families and their allocation behaviour, and the `Decode` vs `DecodeOwned` distinction at contract level. The entry above covers what happens beneath it.

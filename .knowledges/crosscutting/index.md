---
type: Unit
title: crosscutting
description: Mechanics that span more than one unit and answer to neither alone.
---

# Responsibility

Mechanics whose call graph or invariant genuinely crosses a unit boundary — e.g. a `secs2` decode
entry point whose only external caller is `hsms`, where the mechanic is the routing between them, not
either side alone.

# Boundary

Not a dumping ground for anything hard to place.
An entry belongs here only when splitting it into two unit-scoped entries would either duplicate the
shared part or leave the actual mechanic (the crossing itself) undescribed by either half.

# Entries

* [Trailing-byte counting — where it's measured and how it reaches TrailingBytes()](/crosscutting/trailing-bytes-counting.md) - the shared secs2 measurement site, and how hsms's copy-fallback decode path still routes through it.

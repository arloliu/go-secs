---
okf_version: "0.2"
swept_at: bc97919
---

# go-secs memex

Durable notes on how this repo works, one entry per mechanic. Read the unit index before exploring a package; see [CONVENTIONS.md](CONVENTIONS.md) for what earns an entry here.

# Units

* [hsms](hsms/) - Immutable HSMS message model and the shared connection engine both transports run on.
* [hsmsss](hsmsss/) - HSMS-SS transport: TCP role, E37.1 control procedures, linktest, reconnect.
* [secs1](secs1/) - SECS-I over TCP: block framing and the half-duplex line engine.
* [secs2](secs2/) - Deeply immutable SECS-II items, wire encoding, and the decode paths.
* [sml](sml/) - SML parsing and encoding, strict and non-strict.
* [gem](gem/) - SEMI E30 message builders.
* [logger](logger/) - Logging interface every package logs through.

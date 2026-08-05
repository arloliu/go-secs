---
type: Unit
title: logger
description: The logging interface every go-secs package logs through, with an slog default.
---

# Responsibility

Defines `Logger` (Debug/Info/Warn/Error/Fatal at `LogLevel` severities, structured key-value fields, `With` for child loggers) and ships `NewSlog` as the default implementation, plus package-level functions over a default instance.

# Boundary

No SECS/HSMS knowledge. Test doubles live in `logger/loggertest` specifically to keep testify out of this package's import graph — an entry here must not reintroduce that coupling in examples.

# Entries

None yet — this unit fills on miss via `/memex capture`.

# Entry points

- interface: `logger/` → `Logger`
- default impl: `logger/` → `NewSlog`, `Default`, `SetLevel`

# Read first

`logger/doc.go`. At 344 LOC this unit is small enough that reading it whole costs about what an entry would, so entries here need a high bar.

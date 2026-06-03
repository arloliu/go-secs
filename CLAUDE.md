# go-secs — Claude Code Configuration

## Identity

**go-secs** (`github.com/arloliu/go-secs`) is a Go library implementing SECS-II (SEMI E5), HSMS / HSMS-SS (SEMI E37 / E37.1), and SECS-I over TCP/IP (SEMI E4) for semiconductor-equipment communication. Consumed as a dependency; no binary.

Public packages: `hsms`, `hsmsss`, `secs1`, `secs2`, `sml`, `gem`, `logger`. Private: `internal/{pool,queue,throttle,util}` — never expose in public signatures, Godoc, or READMEs.

## Working Principles

- **Surface uncertainty before coding.** If multiple interpretations exist, present them; if unclear, ask.
- **Minimum change that solves the problem.** No speculative features or unasked-for flexibility.
- **Don't guess — verify.** Write a small test or benchmark; don't refactor on intuition.
- **Define verifiable success criteria.** Transform vague tasks into concrete checks.

## Git

Never add `Co-Authored-By` or any attribution trailer.

## Rules & Skills

@AGENTS.md

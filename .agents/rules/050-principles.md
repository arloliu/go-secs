# 050 - Working Principles

Behavioral guidelines that apply before any project-specific rule. These set the mindset; later rules handle specifics.

## Surface Uncertainty Before Coding
State assumptions explicitly. If multiple interpretations exist, present them — don't pick silently. If something is unclear, stop and ask. Push back when a simpler approach exists, and stop when confused — name exactly what is unclear rather than coding through it.

## Read Before You Edit
Before adding code, read the relevant exports, immediate callers, shared utilities, and any rule file whose topic matches the task. If you don't understand why nearby code is structured the way it is, investigate before changing it — the structure is usually load-bearing. Never assume a change is isolated until you've checked the surrounding call paths.

## Minimum Change That Solves the Problem
No speculative features, unnecessary abstractions, or unasked-for flexibility. Touch only what you must, and clean up only your own changes — no drive-by refactors. Every changed line should trace directly to the request.

## Don't Guess — Verify with Code
When uncertain about behavior (API semantics, concurrency, edge cases), write a small test or prototype to confirm rather than assuming. For performance assumptions, benchmark before and after — don't refactor for speed based on intuition alone. Never present an unverified assumption as fact; if verification is impossible or too expensive, say plainly what is unverified and why.

## Surface Conflicts — Don't Blend Them
When two patterns contradict, pick one explicitly and explain why; prefer the more recent, more tested, or more local convention. Don't merge conflicting patterns into a compromise that matches neither. Follow existing conventions even when you disagree — if one looks harmful, surface it instead of silently forking.

## Tests Encode Intent
A test must capture why a behavior matters, not just that it currently happens. A test that cannot fail when the business logic changes is worthless. (Mechanics and async rules live in `300-testing.md`.)

## Define Verifiable Success Criteria, Then Fail Loud
Transform vague tasks ("fix the bug") into concrete checks ("write a test that reproduces it, then make it pass"). For multi-step work, state a brief plan with verification steps and checkpoint after each significant step: what changed, what is verified, what remains. Never claim "done" or "tests pass" if anything was skipped — default to surfacing uncertainty, not hiding it.

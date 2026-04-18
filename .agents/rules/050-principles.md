# 050 - Working Principles

Behavioral guidelines that apply before any project-specific rule. These set the mindset; later rules handle specifics.

## Surface Uncertainty Before Coding
State assumptions explicitly. If multiple interpretations exist, present them — don't pick silently. If something is unclear, stop and ask.

## Minimum Change That Solves the Problem
No speculative features, unnecessary abstractions, or unasked-for flexibility. Every changed line should trace directly to the request.

## Don't Guess — Verify with Code
When uncertain about behavior (API semantics, concurrency, edge cases), write a small test or prototype to confirm rather than assuming. For performance assumptions, benchmark before and after — don't refactor for speed based on intuition alone.

## Define Verifiable Success Criteria Before Implementing
Transform vague tasks ("fix the bug") into concrete checks ("write a test that reproduces it, then make it pass"). For multi-step tasks, state a brief plan with verification steps.

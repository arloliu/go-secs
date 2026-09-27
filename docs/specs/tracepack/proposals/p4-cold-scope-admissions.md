# Proposal P4 — admissions for scopes outside the catalog window (issue record, not yet designed)

Status: deferred (2026-09-26) — open issue record; split out of proposal P3 by owner decision.
Source: P3 review round 1 (the report and the P3 text are kept outside the repository).
Nothing here is a decided rule.

## 1. The gap

Under P3, the catalog indexes only a window of scopes and rejects every admission for a scope outside it:
segment and patch registration and merge claims.
Consequences until P4 is designed:
- corrections and repairs of hours older than the window are impossible;
- a late segment of such an hour stays unregistered in `staging/` (read by listing views, never merged);
- a scope that left the index unsettled, or that a window-only rebuild dropped, keeps its pending merge and deletion work until retention.

## 2. Findings carried over from P3 round 1

| Finding | What a design must handle |
|---|---|
| P1 — listing views racing a concurrent admission | a reader that lists a scope while an admission, or the recovery of an uncertain one, changes it can combine packs and commit objects from different committed states (e.g. a late commit of a cancelled patch without the cancelling generation); needs a consistency contract between listing reads and re-import |
| P1 — staging visibility | a segment upload completing after eviction and before registration; ordering relative to upload visibility, not registration |
| P1 — rebuild and unsettled old scopes | a rebuild must rediscover scopes outside the window that still need merges, deletions or interrupted-admission recovery before admissions reopen |
| P1 — readiness with disjoint indexed scopes | re-importing an old scope must not break readiness for the capture's recent scopes |
| P2 — generation numbering after re-import | the claimed-generation high-water mark and claim identity; reconcile with the epoch-on-state-loss rule of [STO §5] |

## 3. Candidate direction (unreviewed)

Treat re-import as recovery of a scope in an unknown state:
acquire a new publisher epoch, re-import the scope within its serialization,
commit a cancellation generation built by an ordinary merge of the listed committed view before accepting any other admission
(the existing [STO §5] interrupted-admission recovery), and only then register the segment, patch or claim.
This touches [STO §5] recovery, so it needs its own review rounds.

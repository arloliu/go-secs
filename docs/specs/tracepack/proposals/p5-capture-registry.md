# Proposal P5 — capture end evidence beyond the retained packs (issue record, not yet designed)

Status: deferred (2026-09-26) — open issue record; split out of proposal P3 by owner decision Q10 (G5-65).
Source: P3 review rounds 3–5 (the reports and the P3 text are kept outside the repository).
Nothing here is a decided rule; G5-61..G5-64 are recorded owner choices for this design, deferred, not superseded.

## 1. The gap

Under P3 revision 6, the catalog records every capture's boundaries and epoch closures from every segment or converter archive presented for registration,
never removes them, and a rebuild recomputes them from the footers of every retained pack.
Two cases remain open:
- **Retention-removed evidence.**
  Once retention removes the pack holding a capture's boundary, a rebuild cannot recover it, and the capture reads as `open`.
  A barrier whose gap reaches retained hours (a clock step, or an absent `gap_end`) is then lost.
  v2.6 has the same property.
- **Rebuilds that read only the window.**
  P3 requires a rebuild to bootstrap every retained pack (≈ 6.5 million for 1500 tools over 6 months).
  A cheaper rebuild that reads only the window needs another source of end evidence for captures whose boundary lies outside it.

## 2. Design explored in P3 revisions 3–5

- G5-61 (Q6 a): a capture with a later linked capture but no established end is treated as ended `stop-unclean` with an unbounded gap.
- G5-62 (Q7 a): a capture registry object `captures/<tool_id>/<capture_id>/<previous_capture_id>`, never deleted, so successors are known in absolute terms.
- G5-63 (Q8 a): a capture end object `captures/<tool_id>/<capture_id>/end/<kind>/<gap_start>/<gap_end>`, never deleted, written before any successor's registry object.
- G5-64 (Q9 a): registry and end objects written and registered with the catalog before the packs depending on them; a rebuild combines a complete registry listing with the registrations it receives.

The texts are in P3 revision 5 (§4.1, §4.5 Registry, §4.6), quoted in the appendix below.

## 3. Findings a design must handle

| Source | Finding |
|---|---|
| r3 finding 1 (P1) | a successor visible only in cold archives hides the trigger of the conservative barrier; "no successor" must mean actual absence |
| r3 finding 3 (P2) | retained end evidence must take precedence over any synthetic fallback; an explicit evaluation order |
| r4 finding 1 (P1) | a crash between the boundary pack and its end object, followed by a rebuild, hides the barrier; write-ahead publication needed |
| r4 finding 2 (P2) | registry cache: observation, refresh, rebuild handoff, interrupted enumeration |
| r4 finding 3 (P2) | end-object barriers: time effect vs epoch effect; closure evidence lost with evicted footers |
| r5 finding 1 (P1) | a write-ahead clean end closes epochs before the final records are uploaded: an end object needs the boundary's seq, and closure only once coverage reaches it |
| r5 finding 2 (P2) | a query's registry observation must share a snapshot with scope evidence; writers that crash before registering behind a rebuild's cursor |
| r5 finding 3 (P2) | `close_seq` derived from a disputed clean `stop` must not count as closure; an interrupted clean stop yields a disagreement |

## 4. Lesson from the P3 rounds

Every fix in revisions 3–5 moved the window in which end evidence is missing or premature rather than closing it:
evidence published after the data (round 4) or before it (round 5).
A design should state, for each end-evidence object, which data it proves present, not only that the capture ended.

## Appendix — texts from P3 revision 5

Quoted verbatim, because P3 revision 5 is kept outside the repository.
Section numbers, `STO §n` references and tags such as `(A, B)` refer to that revision and to tracepack v2.6.

### A.1 — P3 revision 5 §4.1: STO §3 — lifecycle of objects and the capture registry (A, B)

STO §3, after the commit-object bullet, add:

> - Capture registry object: `<prefix>/captures/<tool_id>/<capture_id>/<previous>`, where `<previous>` is the capture's `previous_capture_id`, or `none` when the capture has none; its content is empty.
>   The writer that uploads a capture's first pack — a recorder, recovery (§4) or a log converter (§7) — first writes it and then registers it with the catalog (§5),
>   retrying each step until it succeeds; no pack of the capture is uploaded before both have.
> - Capture end object: `<prefix>/captures/<tool_id>/<capture_id>/end/<kind>/<gap_start>/<gap_end>`, where `<kind>` is `stop` or `stop-unclean`
>   and each bound is the boundary's `gap_start` / `gap_end` in signed decimal nanoseconds, or `none` when the boundary has no such bound (unbounded, §4) or the kind is `stop`;
>   its content is empty.
>   A writer that ends a capture with a `stop` or `stop-unclean` boundary writes the end object, copying that boundary, and registers it with the catalog,
>   retrying each step until it succeeds, before it uploads the pack holding the boundary.
>   The boundary is fixed before that: recovery computes it from the spool and the liveness anchor (§4), and a recorder stopping cleanly makes its `stop` boundary durable in the spool first.
>   A retried recovery computes the same boundary from the same spool and anchor; two different end objects of one capture are a disagreement (§5 Completeness).
>   A writer that never writes such a boundary — e.g. a log converter reaching the end of its input without one — writes no end object.
> - Every writer that writes a registry object naming a previous capture first makes sure that capture's end object is written and registered,
>   if necessary by recovering it (§4), and only then writes and registers the new registry object.
>   Hence every capture that has a registered successor has a registered end object, unless a writer did not conform.
> - The registry of a tool is the objects under `<prefix>/captures/<tool_id>/`: the captures of the tool, their end objects,
>   and the successors of capture C, which are the registry objects whose last key segment is C.

STO §3, "Every key contains the `pack_id`, so every upload has a unique key and no object is ever overwritten."
→ "Every pack key contains the `pack_id`, so every pack upload has a unique key and no pack is ever overwritten;
a registry or end object is only ever rewritten with its own key and empty content, so a retry is harmless."

STO §3, "Retried and duplicate uploads are harmless: the catalog registers each `pack_id` once and records are deduplicated by (`capture_id`, `seq`)"
→ "Retried and duplicate uploads are harmless: the catalog registers each `pack_id` of an indexed scope once (§5), and records are deduplicated by (`capture_id`, `seq`) in every view".

After the last bullet of STO §3, add:

> - Packs, commit objects and fence objects are removed only by §4 Deletion and §5 Retention; registry and end objects are never removed.
>   Storage lifecycle rules (e.g. S3 or MinIO ILM) MAY transition objects under `archive/` to another storage class, provided they stay readable and listable;
>   they MUST NOT expire any object, and MUST NOT apply to `staging/`, `commit/`, `fence/` or `captures/`.
> - The highest-numbered fence object is never deleted; lower-numbered fence objects MAY be deleted.

STO §4 Flush, after "whenever the next record's UTC hour differs from the current segment's, so that every segment has one scope", add:
"Before the first segment of a capture is uploaded, its registry object (§3) is written and registered;
before the segment holding a `stop` boundary is uploaded, the capture's end object is."

STO §4 Recovery, after "appends a `stop-unclean` capture-boundary to it, uploads it, and only then starts a new capture ([FMT I-7])", add:
"; before the upload it writes and registers the old capture's registry object if that is missing, and then the end object copying the boundary (§3)".
Recovery already computes the boundary, gap bounds included, before uploading; the delta only orders two writes before the upload.
A clean stop interrupted after its end object and before its upload is recovered like any crash:
recovery appends a `stop-unclean` boundary and writes a second end object, a disagreement that §4.6 resolves conservatively.

STO §7, item 9, append: "Before its first archive of a capture, the converter writes and registers the capture's registry object (§3);
it writes an end object only for a `stop` boundary it actually writes."

### A.2 — P3 revision 5 §4.5: STO §5 — Registry, and the rebuild addition

From the additions after the catalog's opening paragraph:

> - **Registry**: the catalog holds every registry and end object (§3) registered with it, for every capture, indexed or not; registering an object twice changes nothing.
>   A writer registers each object before the step that depends on it (§3), so an object missing from the catalog has not yet been followed by that step:
>   the capture has uploaded no pack yet, or has not uploaded the pack holding its end, and treating the object as absent gives the state the capture actually has.
>   A rebuild starts only after the lost catalog has stopped acknowledging registrations.
>   It accepts registrations from its start and lists `captures/<tool_id>/` completely, all pages, for every tool;
>   its registry is that listing plus every registration it received.
>   An object the lost catalog acknowledged was written before the listing began, so the listing returns it; an object the lost catalog did not acknowledge is registered again by its writer.
>   Until the listing of a tool is complete, every result for that tool is `incomplete`; an interrupted listing is restarted.
>   A reader without a catalog lists the registry completely itself; its results are `cold` in any case.

From the edits to STO §5:

STO §5 Restart and rebuild, append:
"A rebuild takes over registry and end objects as in Registry (above).
The service's deleting component stops with the catalog that directed it; a rebuild starts only after it has stopped, so no deletion decided by the lost catalog runs afterwards.
A scope outside the rebuilt window is read through listing views; merge and deletion work it still had pending waits for the retention of its hour."

### A.3 — P3 revision 5 §4.6: STO §5 — completeness with a window (B, Q6, Q8)

STO §5 Completeness, append:

> - A result that touches a scope that is not indexed is `incomplete` with reason `cold` and the searched scope;
>   a transaction lookup whose eligibility window touches such a scope never returns `unmatched` ([SEM §7.2]).
> - The **end state** of a capture is established by the first of these that applies:
>   1. its registered end object (§3): `stop`, or `stop-unclean` with the gap bounds it carries;
>   2. a `stop` or `stop-unclean` boundary in the view of an indexed scope of the capture;
>   3. a successor — a registered registry object naming the capture, or an indexed pack whose `previous_capture_id` names it —
>      makes the capture ended `stop-unclean` with an unbounded gap interval: a barrier for every epoch of the capture and for all time;
>   4. otherwise the capture is `open`.
>   Step 3 applies only when a writer did not conform (§3) or an end object was lost, and is reported as such.
>   Two end objects, or an end object and an indexed boundary, that disagree are reported as a disagreement —
>   a writer that did not conform, or a clean stop interrupted before its upload (§3) — and every barrier any of them states applies;
>   none of them is then evidence of a clean `stop`.
>   A `stop-unclean` end established by step 1 is a barrier for its gap interval and for every epoch of the capture that no indexed view shows closed ([FMT §10] `close_seq`):
>   closure evidence in an evicted scope no longer counts, so such an epoch keeps a transaction lookup from `unmatched` even when the primary lies outside the gap interval.
>   By step 2, the epochs follow from the boundary's footer as before.
>   A `stop` end established by step 1 or 2 is evidence that every epoch of the capture ended ([SEM §7.2]).
> - Barriers apply by their own intervals whether or not any scope of their capture is indexed.
>   Steps 1 and 3 use the catalog's registry (Registry, above), so they survive eviction, and a rebuild recovers them for every capture, including captures outside the window.

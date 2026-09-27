# Proposal P7 — closing a crashed producer's capture over a durable bus (issue record, not yet designed)

Status: deferred (2026-09-27) — open issue record; split out of the v2.9 amendment by owner decision after three review rounds.
Source: the v2.9 review rounds 2 and 3 (reports kept outside the repository).
Nothing here is a decided rule.

## 1. The gap

Under [STO §4] "Recorder over a durable bus", a producer that stops without publishing a `stop` boundary leaves its capture without end evidence.
The successor capture of the same `recorder_instance_id` names it in `previous_capture_id` ([FMT I-7]),
but v2.9 defines no rule by which the service turns that into a `stop-unclean` boundary.
Consequences until P7 is designed:
- the capture remains `open`: the successor's `start` alone closes neither the capture nor an epoch;
  completeness and `unmatched` for bounded windows still follow [STO §5] and [SEM §7.2]
  (an epoch with a `close_seq`, a next same-key primary, or contiguous coverage over a bounded window keep their meaning);
- the recorder downtime between the two captures is visible only through the seq coverage and the successor's `start`,
  not as a barrier with `gap_start` / `gap_end`;
- every record the bus accepted is staged as long as the stream retains it for redelivery, and the listing and catalog views are unaffected.

A local recorder with a spool is not affected; its recovery rule in [STO §4] stands.

## 2. What the review rounds established

Each attempt to write the boundary from the service side met one of these:
1. Publish fence:
   "zero messages on the capture's subject" proves that every message the bus still holds was acknowledged, hence staged,
   but not that no further message can be accepted (a publish request in flight when the producer died).
   [STO §4] excludes limit-based removal of accepted messages from the traffic stream;
   a sound closure rule still needs a bus-enforced fence, or a settle period justified by the bus's publish semantics,
   to handle a publish request still in flight.
2. Interrupted closure:
   a closure that reserves the boundary's seq in the catalog and then writes the boundary segment has two crash windows
   (reserved but unwritten; written but unregistered),
   and a rebuild reads the staged segment while the live catalog shows only the reservation.
   The rule needs idempotent recovery for both windows, in the style of [STO §5] interrupted-admission recovery.
3. Metadata of the boundary pack:
   a capture whose `start` never reached the bus has no capture descriptor,
   so the boundary pack cannot carry the always-required pack metadata;
   the empty-capture case needs its own rule or must stay `open`.
4. Post-closure arrivals:
   a segment of a closed capture offered afterwards can only come from a nonconforming component,
   but moving it out of `staging/` conflicts with [STO §3] (packs are removed only by Deletion and Retention),
   and a listing view may observe it between upload and any exclusion.
   The rule needs a reader-side exclusion or an upload path that never lands in `staging/`.

## 3. Directions not yet designed

- A producer-side end object written by the successor before its first publish, in the spirit of proposal P5's registry objects,
  which would give the service the fence of point 1 without inspecting the bus.
- A closure protocol modelled on the commit protocol of [STO §5]:
  reserve, write, commit object, install, with the same recovery rule.
- Treating a crashed producer's capture as ended by the successor's `start` for barrier purposes only, without a boundary record,
  which changes [STO §5] Completeness rather than the format.

## 4. Consequences for the first release

The log service ships without P7: a crashed producer's capture stays `open`.
The producer SHOULD publish a `stop` boundary on every orderly shutdown, so the case is limited to crashes.

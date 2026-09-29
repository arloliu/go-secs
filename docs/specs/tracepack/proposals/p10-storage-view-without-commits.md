# Proposal P10 — a storage view without commit objects

Status: draft (2026-09-29) — redesign pending after review round 1; not decided.
Source: split out of proposal P9 (its former Part B) by owner decision G5-95, after P9 review round 1 (the report is kept outside the repository).
Nothing here is a decided rule.

## 1. Question and status

The owner asked (2026-09-29): if the catalog were authoritative and were lost entirely,
could it be rebuilt from the packs alone, without commit and fence objects?

- **For a rebuild, yes, under the premise of §3.**
  A rebuild starts only after the deleting component has stopped ([STO §5] Restart and rebuild),
  so no pack disappears while it lists the bucket;
  when every copy of a record is byte-identical, the union of the retained packs is the view.
- **For live operation, not yet.**
  The commit protocol also gives readers a coherent observation point while merges publish and packs are deleted,
  and it keeps deletion from removing the last good copy.
  Retiring it needs a mechanism of equal strength; review round 1 found four P0 gaps in the first design (§9).

The text below is that first design, kept as the starting point.
P10 does not block phase 4 `Verify`;
phase 4 `Repair` waits for P10's outcome, because its metadata (`patch_base`) depends on it (G5-95).
P10 closes when it is applied or rejected: the outcome goes to the changelog and the decision log, and this file is deleted.

## 2. Why commit objects exist today

[STO §5] Commit protocol:
"A rejected admission never gets a commit object, so a rebuild — which admits only committed generations and patches — never activates it."
A rejected or interrupted admission matters
because two packs of a scope may hold **different** bytes for one (`capture_id`, `seq`):
a correction re-emits records with new bits,
a stale patch or merge was built from an older view,
and a partial replacement set lacks members.
Commit objects, fence objects, publisher epochs, generation ranking and `patch_base` decide which version is in the view,
and the recovery of interrupted admissions exists to keep that decision consistent across crashes and rebuilds.

## 3. Premise: every copy of a record is byte-identical

If every pack that holds a record holds the same bytes for it,
no admission can make the view wrong by being present, and none of that machinery decides anything.
The premise needs four rules:

1. No correction patches (G5-94).
2. A repair re-emits the recoverable records of the packs it names byte for byte;
   [STO §6] "possibly with new stored bits or classification" is removed for repairs.
3. **Classification is owned by the producer** for a capture over a durable bus.
   Today [STO §4] gives `classifier` to the consumer,
   so during a rolling upgrade two consumers with different classifier versions disagree:
   they write different `decode_status`, `trailing_bytes` and `quality.decode-failed` for one redelivered record
   (P8 §5 point 5 records this).
   The producer computes them once, the capture descriptor names the `classifier`, and consumers store them as received;
   P8 §2.1's body then carries them instead of zeros.
4. Every other byte a writer derives (the quality bits of [FMT §7.2] it owns, the copy fields)
   is a function of the record fixed by the format version, not by the writer's version.
   This holds for format 1.0 and is stated as a rule.

Under the premise an I-12 `conflict` can come only from a defect, as today.

## 4. The view of a scope

The **view** of a scope is every finalized pack of the scope in the bucket —
segments, converter packs, repairs and archives, `staging/` and `archive/` alike —
except packs named in the `supersedes` of a finalized repair of the scope.
Records are deduplicated by (`capture_id`, `seq`) ([FMT I-12]);
the `coverage` of every pack in the view applies.

Coverage stays conservative, as G5-89 chose:
a stale repair's coverage over seqs that another pack still holds reports `incomplete` although the records exist.
That over-reports only in the rare case of a repair racing a duplicate delivery,
and never presents lost records as absent.

Which packs a reader actually reads is an efficiency choice, not a correctness one, because every copy is identical.
The **read set**: the merged packs (`compaction_level` ≥ 1) that no other merged pack of the scope dominates (§5),
plus every original pack (`compaction_level` = 0) that no read-set merged pack lists in `compacted_from`,
minus superseded packs.
Its records equal the view's.

## 5. Deletion by containment

Deletion must never remove the last copy of a record or of a `coverage` entry,
and must have one direction, so two packs never justify deleting each other.

- **Original packs** (`compaction_level` = 0: segments, converter packs, repairs)
  are deletable when a finalized merged pack of the scope lists them in `compacted_from`.
  A damaged pack and its repair are both listed by the merge that folds the repair,
  so both become deletable at once, as today.
- **Merged packs.**
  M1 is dominated by M2 when `compacted_from`(M2) ⊋ `compacted_from`(M1),
  or when the two lists are equal and M2's `pack_id` is greater (UUIDv7, so the later pack).
  This is a strict order, so the undominated merged packs are never deletable on these grounds.
  A dominated merged pack is deletable.
  Every record of M1 came from an original pack in its list, so M2 holds it;
  M2 also carries every `coverage` entry those packs carried, because merges union coverage ([STO §4]).
- **Grounds are checked, not trusted.**
  A pack justifies a deletion only when it is finalized and attested (`blocks_validated`),
  and its F-5 seq ranges and `coverage` entries contain those of the pack to be deleted:
  a footer comparison, no block read.
- The deletion delay of [STO §4] and Retention of [STO §5] are unchanged, less commit objects.
- A reader that finds a listed pack missing relists the scope once before reporting anything:
  the pack was deleted because a covering pack exists.

Worked cases:
- Segment S; merge M1 lists S; a second merge M2 also lists S.
  S is deletable.
  M1 and M2 have equal lists, so the one with the smaller `pack_id` is deletable; never both.
- Concurrent merges M1 {S1, S2} and M2 {S2, S3}: neither dominates;
  S1, S2 and S3 are deletable;
  the view is M1 ∪ M2 with S2's records deduplicated;
  the next merge {S1, S2, S3} dominates both.
- Damaged segment D and repair R naming D: readers skip D;
  the next merge lists D and R, carries R's records and coverage, and makes both deletable.

## 6. What retires

- **Objects:** commit objects and fence objects ([STO §3] keys).
- **Tags:** `publisher_epoch`, `patch_base`,
  `replacement_set_id`, `replacement_set_size`, `replacement_set_index`, `scope_generation`;
  `supersedes` on archives (lineage only today; `compacted_from` carries what deletion needs).
  `supersedes` stays for repairs.
- **Rules:** publisher epochs and fences ([STO §2], §3),
  the commit protocol and the recovery of interrupted admissions ([STO §5]),
  generation claims and the serialization of admissions as correctness rules, and the coherent observation of cold scopes:
  a listing view becomes "list the scope's packs, bootstrap them, compute the view", with the relist rule of §5.
  Tool leases (G5-87) may stay to avoid duplicate merge work; they no longer protect correctness.
  "No admissions outside the index" goes: a late pack of a cold scope is part of its view like any other.
- **Vectors:** the commit, fence, cancellation, uncertain-admission and set-member vectors of [STO §8];
  new vectors for §4–§5 replace them (§9).
- **Proposals:** P4's findings (listing views racing admissions, generation numbering after re-import) lose their cause;
  P4 would close as superseded.
  P5 is unchanged: retention still removes end evidence a rebuild cannot recover (G5-83).
  P7 is re-examined, because its closure used catalog transactions to fence late segments.

## 7. The catalog after this change

The catalog becomes an index: per-pack entries, each scope's read set, per-capture evidence, the window.
A catalog rebuilt by listing the bucket and reading footers is exact,
except for per-capture evidence whose packs retention removed — the residual G5-83 already accepts.
Registering a capture descriptor before acknowledging the `start` (G5-86) is unchanged.

## 8. Risks for review

- A merger that lists in `compacted_from` a pack it did not fully copy would make deletion lose data.
  The footer containment check of §5 catches a missing seq range,
  not a missing record inside a range that another input also covered.
- A listing reader bootstraps every pack of a scope to find the read set, as a listing view does today.
- Moving classification to the producer changes the bus body of P8 and the producer API of phase 8.
- Coverage conservatism (§4).

## 9. Review round 1: open findings

A redesign must resolve these before P10 can be decided.

| Severity | Finding | Direction |
|---|---|---|
| P0 | A merged pack damaged later and superseded by a repair still dominates by `compacted_from` and still witnesses deletion, so the last healthy copy (an older merged pack or segment) can be deleted | define witness eligibility first: a superseded, unreadable or unverified pack neither dominates a read-set member nor justifies a deletion; keep older copies when a repair cannot recover a record |
| P0 | `compacted_from` and footer seq ranges do not prove that a merged pack holds the same record bytes and every inherited `coverage` entry; the read set would hide an older copy before a `conflict` could be seen | read-set dominance and deletion depend on verified record-byte and coverage containment, failing closed; define how a merger establishes and keeps that proof. Today's deletion rule trusts `compacted_from` the same way, so the proof also closes an existing gap |
| P0 | A reader lists `archive/` before a merged pack is uploaded; the pack appears and its source segment is deleted before the reader lists `staging/`; the reader sees neither and no listed key went missing | a coherent observation, or a deletion and read protocol under which a reader sees a surviving witness or reports `incomplete`, across prefixes and pages; define when the deletion delay starts and what reader lifetime it covers |
| P0 | A bus consumer makes a segment durable and acknowledges its records before the catalog registers it; an indexed query that reads only the catalog returns nothing for them | define the handoff from durable upload to catalog visibility and query completeness, crash and rebuild included; until a query proves it saw every relevant finalized pack it reports `incomplete` |
| P1 | Byte identity at the bus boundary: P8 still gives the copy fields and `field_validity` to consumers, and `field_validity` of a log conversion is not derivable from the payload | a field-by-field producer and consumer contract for the bus body and the capture descriptor, with a rule for mixed consumer versions |
| P1 | `Repair` cannot re-emit byte for byte through the public record path: `Record` has no field for header bytes beyond 56, and the header encoder zeroes reserved bytes | a raw-header preservation path for `Repair`, or `Repair` declines such a pack and reports `incomplete` |
| P1 | Seq coverage, capture-boundary barriers, epoch closure and the "never `unmatched` across a cold scope" rule rely on a consistent catalog snapshot and on rebuilding per-capture evidence from every retained pack | state which completeness rules remain, define the rebuild and registration handoff and the barrier snapshot, and keep `cold` / `incomplete` until another proof covers the whole eligibility window |

Tests the review asked for:
a merged pack damaged and repaired, with deletion in either order and a rebuild;
uploads, paginated `archive/` and `staging/` listings, deletions, registrations, rebuilds and retention interleaved, every result either holding the record or reporting `incomplete` or `removed`;
raw-byte comparison across duplicate deliveries, repairs and merges, with longer headers, reserved bytes, different payloads at one seq and missing inherited coverage;
transaction lookups whose eligibility window reaches an unseen segment, a cold scope, an unclosed epoch or a per-capture barrier.

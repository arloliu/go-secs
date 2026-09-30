# Proposal P2 — defective merge inputs (issue record, not yet designed)

Status: deferred (2026-09-26) — open issue record; split out of proposal P1 §6 by owner decision after review round 6.
Source: P1 review rounds 3–6 (the reports and the P1 text are kept outside the repository).
Nothing here is a decided rule; it records what the P1 review rounds established so a later design starts from it.
Outcome (2026-09-30): owner decision G5-109 (spec v2.17) settles the first two bullets of §1:
a merger reads every block of every input in full and fails, publishing nothing, when an input is not `finalized-consistent`,
and the damaged input is repaired first ([STO §4]).
P2 stays deferred for its recovery items:
any input the cancellation merge of an interrupted-admission recovery refuses — defective, breaching its scope or its own metadata, with differing capture-level tags, or holding a conflicting record —
which leaves the scope closed until an operator acts ([STO §5]; §2 r6 P1, the recovery orderings of §3).
The two r5 P1 rows of §2 and the first two vectors of §3 assumed a defective block could be published in a member;
under G5-109 it cannot, so they are moot unless a recovery design reintroduces such publication; §4's inventory stays open for that design.

## 1. The gap

[STO §4] copies non-overlapping blocks without decoding them and decodes overlapping ones for record-level resolution.
It does not say what happens when an input block fails [FMT I-2]:
- a non-overlapping block is copied as is, so a CRC-valid but undecodable block can enter a generation;
- an overlapping block cannot be resolved record by record at all.
[FMT §13] recovers only the validated prefix of a pack; recovery beyond a corrupt middle block is deferred ([OVW §6]).

## 2. Findings carried over

| Origin | Finding | State |
|---|---|---|
| r5 P1 | An undecodable block copied into a member, followed by healthy blocks of other inputs: once the inputs are deleted, prefix repair of the member also loses the later healthy records | remedy verified in r6: an undecodable copied block ends its member and later blocks start the next member; open: fixing member boundaries and `replacement_set_size` relative to the claim (r6 P2), and a sub-256-MiB multi-member vector |
| r5 P2 | An undecodable block in an overlapping seq range cannot be resolved; the merge must publish nothing and keep its inputs | safe-failure rule verified in r6 |
| r6 P1 | The same situation during [STO §5] interrupted-admission recovery: the cancellation merge cannot run, and registering repair patches before the uncertain admission is fenced lets a late uncertain patch and the repair both enter the view after a crash and rebuild | open; needs a recovery whose intermediate repair work is never an independent admission — e.g. repair material staged privately and consumed by the fenced cancellation generation as its only visible commit — or durable recovery state with matching rebuild rules |
| r5 P1 | If membership ever depends on a member being finalized, a committed member damaged later must still count as a member, so its siblings stay visible and it stays repairable | relevant whenever a design makes current finalization a membership condition, for any reason; rule verified in r6: finalization at admission, membership fixed at commit |
| r4 P1 | A merger's own failed new encoding must never be published in a generation that makes its inputs deletable | covered in P1 for coalescing and record-level resolution (validated in memory, discarded on failure) |

## 3. Vectors already specified in the review rounds

- A pre-existing I-2 defect in the middle of segment A followed by a healthy segment B: publication, predecessor deletion, repair, folding merge and rebuild keep every record of B.
- A committed two-member generation, predecessor deletion, later footer or trailer damage, rebuild, repair and folding merge: the healthy sibling stays visible and the repair is admissible.
- An undecodable block inside an overlapping seq range: nothing published, no input deletable.
- Recovery orderings (r6): an uncertain correction and an uncertain generation; late commit arrival before and after repair preparation and commit; a crash before the cancellation commits; an uncertain repair commit; another interruption during cancellation.

## 4. Outstanding inventory (from round 6)

When P2 is designed, its edit inventory must also cover:
- implementation phase 4: repair verification for an isolated defective final block and for a later-damaged committed member;
- phases 5 and 6: the sibling-visibility, rebuild and folding-merge checks of §3;
- STO §5: serialization, commit protocol, interrupted-admission recovery and rebuild, as affected by the chosen recovery design;
- STO §4: active-view and lineage rules, if the chosen design changes them.

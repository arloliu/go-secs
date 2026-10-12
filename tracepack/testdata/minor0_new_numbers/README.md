# minor0_new_numbers

A format 1.0 (minor 0) extract holding the numbers format 1.1 assigns,
and its canonical export written by tracepack v0.4.0 under schema `tracepack-jsonl/1`.
A reader of any later version keeps those numbers unknown in this pack,
since a format 1.1 addition is known only in a pack whose `format_minor` is 1 or more
(the tracepack format specification §5 and §14),
so the export of `pack.tpk` must equal `export.jsonl` byte for byte.

The pack holds three data records in one block:

- seq 0, an S7F3 carrying `quality.redacted`, named by the one `redaction` entry;
- seq 1, an S1F1 whose stored `quality` has bit 7 set (`bit(7)` in the export);
- seq 2, an S1F2.

Its pack metadata holds:

- top-level tag 0x0033 twice, value type bytes, exported under `unknown` in stored order;
- in `redaction_policy`, nested tags 0x0005 and 0x0006, value type bytes;
- in the `redaction` entry, nested tag 0x0006 twice, value type bytes, 16 bytes each.

The pack was written by the tracepack v0.4.0 Writer.
Then the nested tags and bit 7 were added by byte surgery,
with every checksum, the block's F-3 `quality_union` and the pack's F-5 `quality_union` recomputed;
`Verify` reports it finalized-consistent.
The files are not regenerated: they pin what v0.4.0 wrote.

| File | SHA-256 |
|---|---|
| `pack.tpk` (1516 bytes) | `76c2b74671d79c62f50999edea4f129d56c339dd51b80bfdcdb0c7b04765c5e4` |
| `export.jsonl` (2536 bytes) | `16acce01bcd886e4fdb7cfc666abe5fe134f8279563636a1662c19610db84ce9` |

# merge_minor0_v040

A merge of two format 1.0 (minor 0) segments by tracepack v0.4.0,
and the canonical export of its archive under schema `tracepack-jsonl/1`.
A merge of later versions given the same inputs and options writes the same archive,
so its export must equal `export.jsonl` byte for byte
(the tracepack format specification §14 and §15).

- `a.tpk` and `b.tpk` are segments of one capture in one UTC hour, under a classifier and a `max_frame_len`:
  `a.tpk` holds seqs 0 to 5, `b.tpk` seqs 4 to 9, in blocks of two records.
  The records are a start boundary at seq 0, an annotation note at seq 7, and S1F1 and S6F11 data frames.
- `archive.tpk` is their merge, input `a.tpk` first, with the options `TestMergeMinor0V040` uses:
  writer `merger/golden`, generation 1 of publisher epoch 1, fixed `pack_id`, `replacement_set_id` and clock,
  default block threshold and coalescing.
- `export.jsonl` is the export of `archive.tpk`.

`generator_test.go.txt` wrote the four files inside the released tracepack v0.4.0 module, offline;
its header comment gives the command.
The files are not regenerated: they pin what v0.4.0 wrote.

| File | SHA-256 |
|---|---|
| `a.tpk` (1181 bytes) | `4ccda7b4bc40e5fe524d21f1a8bd2252574223d5b230e0d6d0e6d27424f977bf` |
| `b.tpk` (1179 bytes) | `377acaa85b2bd00441204d436a9e9dab55b2a8d2be40fb8d57ed1066542adbf2` |
| `archive.tpk` (1084 bytes) | `f8394eee1095070df53c4391aac8264c7c2743006fb41266dac866a50c6cdcb1` |
| `export.jsonl` (4774 bytes) | `625efda443ce31151953b3bbd56e88a819bf5d0267e6e30efb56a79c04f98878` |

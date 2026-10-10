# tracepack conformance corpus

This directory is the conformance corpus of tracepack format 1.0:
packs, and the results a conforming implementation produces from them.
An implementation in any language proves conformance against the committed files;
it never needs to regenerate them, and it needs no Go.

The corpus is defined by the [tracepack corpus specification](../../../docs/specs/tracepack/tracepack-corpus.md) [CORPUS]:
its layout, the manifest, every JSON schema with its projection table, the item-validity checklist and the vector catalogue.
This README is informative.
Where it and [CORPUS] differ, [CORPUS] wins.
Section references below are `[CORPUS §n]`, `[FMT §n]` for the [format specification](../../../docs/specs/tracepack/tracepack-format.md),
`[JSONL §n]` for the [JSONL export](../../../docs/specs/tracepack/tracepack-jsonl.md), `[SEM §n]` for the [record semantics](../../../docs/specs/tracepack/tracepack-semantics.md)
and `[STO §n]` for the [storage profile](../../../docs/specs/tracepack/tracepack-storage.md).

The corpus holds the vectors of [FMT §16] and [SEM §9]:
single packs, and sets of packs given to one read over several packs or searched for a transaction.
Store vectors and redaction vectors are not in it yet;
[CORPUS §9] states where each clause stands.

## Identification

`manifest.json` ([CORPUS §3]) identifies the corpus and lists its vectors:

| Key | Value |
|---|---|
| `corpus` | `tracepack-corpus/2`, the schema of every JSON file here |
| `spec_version` | the spec version the goldens follow, `2.31` |
| `format_version` | `1.0` |
| `jsonl_schema` | `tracepack-jsonl/1`, the schema of every `.jsonl` file |
| `zstd_encoder` | the zstd encoder that produced the corpus's encoder-made zstd blocks and footers (see [zstd](#zstd)) |
| `vectors` | one entry per vector, ascending by `id` |

The schema `tracepack-corpus/2` is a draft until tracepack v1.0.0.
A change to a schema, or to what an existing file states, takes a new `corpus` value;
adding a vector, a query or a case keeps it ([CORPUS §10]).

## Layout

```text
manifest.json       corpus identity and one entry per vector
primitives.json     CRC and UUID byte-order values to check before reading a pack ([CORPUS §5.1])
README.md           this file
.gitattributes      keeps every file's bytes as committed
<vector id>/        one directory per vector
```

A vector entry gives the vector's `id`, a one-line `title`, the clauses it `cites`, its `class`, its `labels`, the `codec` its packs use, its `source`, the `files` its directory holds,
and, for a vector of several single-record sub-vectors, its `cases` (`{id, seq, cites}`).
Its `id` starts with its group:
`basic-`, `bootstrap-`, `footer-`, `framing-`, `hsms-`, `repair-`, `sample-`, `sem-`, `validation-` or `verify-`,
and for class `multi-pack` `multi-` (reads over several packs) or `tx-` (transaction lookups).
Ids are stable and never reused.

A vector directory holds only files with these names ([CORPUS §2]):

| File | Present | Content |
|---|---|---|
| `pack.tpk` | every class but `multi-pack` | the pack; for a `truncation` vector, the base pack every cut is taken from |
| `export.jsonl` | class `read`, `repair`, `truncation` | the expected export ([JSONL]) |
| `verify.json` | class `read`, `repair`, `truncation` | the expected verification report ([CORPUS §5.2]) |
| `rejection.json` | class `rejection` | the expected bootstrap rejection code ([CORPUS §5.3]) |
| `queries.json` | optional | queries over the pack and their expected records and `incomplete` reasons ([CORPUS §5.4]) |
| `fields.json` | optional | the HSMS header fields of each data and control record ([CORPUS §5.5]) |
| `classify.json` | optional | frames and their expected `decode_status` and `trailing_bytes` ([CORPUS §5.9]) |
| `footer.json` | optional | footer values as stored, whether the footer is accepted, and as computed from the records ([CORPUS §5.8]) |
| `repair.json` | class `repair` | the repair options and the expected result ([CORPUS §5.6]) |
| `patch.tpk`, `patch.jsonl`, `patch.verify.json` | class `repair`, result `patched` | the reference patch, its export and its verification report |
| `truncation.json` | class `truncation` | the expectation of every cut of the pack ([CORPUS §5.7]) |
| `pack-<n>.tpk` | class `multi-pack` | the packs, numbered n = 0, 1, … without a gap |
| `pack-<n>.verify.json` | class `multi-pack` | the expected verification report of each pack ([CORPUS §5.2]) |
| `reads.json` | class `multi-pack`, optional | reads over several packs and their expected results ([CORPUS §5.10]) |
| `lookups.json` | class `multi-pack`, optional | transaction lookups and their expected results ([CORPUS §5.11]) |

The optional files before `pack-<n>.tpk` never appear in class `multi-pack`.
A `multi-pack` vector holds `reads.json`, `lookups.json` or both, and no `.jsonl` file:
the other classes cover the export, and each pack's verification report pins the state of the packs its results name.

Labels mark packs that a conforming writer of format 1.0 does not write:
`damaged` (damage after writing: a failed CRC, a cut, a broken codec stream),
`nonconforming-writer` (bytes a conforming writer never writes, every CRC valid)
and `pre-v2.13-sample` (`sample-v010-rows`, a pack of the format definition before v2.13, [FMT §14]);
a `multi-pack` vector carries the labels of any of its packs.

## Using the corpus

Read `manifest.json`, check `corpus`, `spec_version`, `format_version` and `jsonl_schema` against what your implementation follows,
then take each vector in turn and the files its entry lists.

- **Compare JSON files as JSON values**, never as text ([CORPUS §4]):
  key order and whitespace do not matter, array order does, since every array is in the order its schema states.
  64-bit integers are decimal strings, bytes base64, UUIDs lowercase, enum values and bits named as in [JSONL §3];
  an optional key without a value is omitted, never `null`.
- **Compare `.jsonl` and `.tpk` files byte for byte.**
- **Classes** ([CORPUS §1]):
  - `read`, `repair`, `truncation`: open `pack.tpk`; its export equals `export.jsonl`, and its verification projects to `verify.json`.
  - `rejection`: `pack.tpk` is rejected at bootstrap for the reason `rejection.json` names.
  - `repair`: repair `pack.tpk` with the options in `repair.json`; the result is the one it names.
    A patch has the properties [FMT §16] lists for repair vectors and holds the expected blocks, records and `coverage` entries.
    `patch.tpk` is the reference implementation's patch:
    yours may differ from it in the order of its pack metadata entries, and so in its footer offsets.
    Read `patch.tpk` as you read `pack.tpk`, against `patch.jsonl` and `patch.verify.json`.
  - `truncation`: for every length `L` from 0 to the pack's size − 1,
    the cut `pack.tpk[:L]` meets the row of `truncation.json` whose `from`..`to` holds `L`;
    a repair table also gives the repair result of each cut.
  - `multi-pack`: open every `pack-<n>.tpk`; its verification projects to `pack-<n>.verify.json`.
    Then run each read of `reads.json` and each lookup of `lookups.json`, below.
- **Optional files**: each query in `queries.json` yields its `seqs` and `incomplete` reasons;
  `fields.json` lists the header fields as read;
  `footer.json` states whether the footer is accepted,
  and, for an implementation that reads footer values, what it stores and what the records give;
  `classify.json` binds a classifier.
- A **writer** conforms when a conforming reader round-trips its output.
  One writer behaviour is held as bytes:
  `validation-i2-injection` is a validating writer's output stopped before a block that fails I-2, without a trailer.
- Skip the files of an operation you do not offer:
  repair, classification, reading footer values, a read over several packs, a transaction lookup.
  The other files still bind you.
- No expectation depends on error text, a reader's own limits, an I/O error or a cancellation:
  results are codes, offsets, seqs and counts.
  The one exception is the conflict bound a read states;
  an implementation that offers no conflict bound skips the reads that state one.

## Reads over several packs

Each read of `reads.json` ([CORPUS §5.10]) names its packs by number.
Give one read exactly those packs, in the order `packs` lists them:
they stand for the active views of the scopes a query selects, and no view is computed from them ([CORPUS §8]).
Read in its `order`, `capture` or `time`, with its `filter` (as in `queries.json`),
returning payloads when `payloads` is true, and with the conflict bound `max_conflicts` when present.

- **Success** `{items, incomplete, conflicts, footer_errors}`:
  `items`, one per version yielded, in the order yielded,
  each its seq, the pack and block of its representative and whether its copies conflict;
  the `incomplete` reasons of each pack, in the order of `packs`;
  `conflicts` in discovery order, each version as the packs holding it;
  and the packs whose footer the read did not use.
  In capture order a conflict is discovered per seq, as a capture advances to its next selected record.
- **Error** `{error: "conflict-limit", conflicts}`: the read found one conflict more than its bound;
  `conflicts` holds the first `max_conflicts`, and nothing the read yielded before is compared.
- A filter never lets summaries exclude a cluster that decides an expected value, so a reader's pruning changes no result.

## Transaction lookups

Each lookup of `lookups.json` ([CORPUS §5.11]) finds the transaction of its primary, `key` (`capture_id`, `seq` and `hour`, used as given),
over `max_scopes` hours from `key.hour`.

- **Source** ([CORPUS §8]): build it from the packs `view` and `evidence` list, and no other pack of the vector.
  The `view` packs form scopes by `capture_id` and the hour of `period_start`;
  each scope's view is its active view ([STO §4]), every archive's replacement set and every repair pack taken as committed,
  and a scope whose highest rank holds two complete sets is conflicted and not read.
  View order: the generation's member first, unless a patch based on the generation replaced it,
  then the other view packs in the order `view` lists them.
  With `complete` true every scope is indexed but those of the `evidence` packs; with `complete` false none is.
  The per-capture evidence folds the F-5 statistics of every source pack;
  it is partial when `complete` is false or a source pack has no valid footer.
- **Result** `{outcome, key, window_end, records, gaps, searched, conflicts, footer_errors}`:
  `key` only when the lookup kept a version of the primary, and `window_end` only when a record read bounds the window;
  every kept version with its pack and block, its role labels and window flags, and its candidate flags when it is a candidate;
  the scopes read to their end; the conflicts and unused footers of each scope read.
  A key that names no primary gives the error form `{error: "not-primary"}`.
- **Facts**: `gaps` holds one fact per distinct condition that keeps the lookup from establishing absence,
  sorted by the comparator of [CORPUS §5.11].
  A fact's keys locate its condition:
  the hours, a pack and block, a seq, a read defect, a `coverage` entry, a boundary with its barrier bases.
  An `index` fact's `offset` is the file offset of the block's envelope in its `pack-<n>.tpk`,
  and a `read` fact's the offset its `queries.json` reason gives;
  no other fact has one.
  Project your own report of what a lookup could not establish onto these facts.

Every record a read or lookup returns is the record of its seq in the pack and block the result names:
every record header field and, when returned, the payload, as stored.

## zstd

Every vector uses codec `none` unless a codec is its subject ([CORPUS §6]).
zstd fixes what a frame decodes to, not the bytes an encoder writes, so two encoder versions can encode the same input differently ([CORPUS §6.1]):

- **Reading never depends on an encoder.**
  Every check above runs against the committed files, strictly, whatever zstd library you read them with.
- **Only regeneration does.**
  The manifest's `zstd_encoder` names the encoder that produced the corpus's encoder-made zstd blocks and footers.
  A generator running with that encoder reproduces every file byte for byte.
  Running with another encoder, or one it cannot establish, it compares each vector that depends on the encoder by decoded equivalence
  (the same header and metadata bytes, each block equal once decoded, an identical export),
  skips that vector's offset-bearing files and footer, compares every other file byte for byte,
  and reports that the corpus is due for regeneration.
- The damaged zstd streams are frames built by hand ([CORPUS §6.2]), never encoder output:
  a Raw first block holding the whole header section, then a malformed compressed block or an early end.
  They, `basic-unknown-codec`, every codec-`none` vector and the fixed sample are always compared byte for byte.

## Regenerating the corpus

The Go reference implementation generates the corpus in its internal package `internal/corpus`.
Each vector's recipe writes its packs with the public `Writer` under a fixed clock and identifiers derived from the vector's id and each pack's number,
then edits their bytes where the vector needs what a `Writer` never writes.
The generator reads every pack as a reader of the corpus does, runs every read with `MergeIterate` and every lookup with `FindTransaction` over `NewReaderSource`,
and checks each result against the recipe's expectation, written by hand from the specification,
before it writes any file.

From the repository root:

```sh
make corpus-tracepack
```

or, from `tracepack/`:

```sh
go test ./internal/corpus -run '^TestCorpus$' -update
```

`-update` writes every generated file, removes the files no vector lists, and keeps `README.md` and `.gitattributes`.
It refuses to run when it cannot establish the zstd encoder,
which it reads with `go list -m github.com/klauspost/compress` from the package directory.
A second run must leave `git diff --exit-code tracepack/testdata/corpus` clean.
Without `-update`, `TestCorpus` regenerates the corpus in memory, compares it with the committed files as [CORPUS §6.1] states,
reads every committed vector again, strictly, and checks the layout and the size below.

`sample-v010-rows` (`source: fixed`) is never regenerated:
its pack is the one tracepack v0.1.0 wrote, copied from `tracepack/testdata/v0.1.0-rows.tpk`, and is always compared byte for byte.

Two tests elsewhere in the module read the committed files too (paths from `tracepack/`):

- `classify/corpus_test.go` (`TestCorpusClassification`) runs `classify.Frame`, the go-secs-backed classifier,
  on every frame of every `classify.json` under its `max_frame_len`, and requires the listed `decode_status` and `trailing_bytes`;
  where they disagree, the corpus decides and the classifier is fixed.
  The same frames seed `FuzzFrame`.
- `corpus_writer_test.go` (`TestCorpusValidatingWriter`) runs the validating `Writer` on the inputs of `validation-i2-injection`,
  with the defect injected into its last block,
  and requires `ErrValidation` and output equal to that vector's `pack.tpk`.

Every committed pack, each `pack-<n>.tpk` included, is also a seed of `FuzzExportJSONL`.

## Size budget

`TestCorpus` fails when the files under this directory, this README included, sum to more than 2,000,000 bytes.
To leave room for the vectors still to come, each group and root file keeps within its budget below.
`TestGroupBudgets` enforces every budget as a ceiling and fails when this table differs from the ceilings it checks.
Measured when the last vector was added:

| Group | Vectors | Files | Bytes | Budget |
|---|---:|---:|---:|---:|
| `tx-` | 19 | 133 | 192,141 | 330,000 |
| `footer-` | 28 | 94 | 160,798 | 240,000 |
| `repair-` | 18 | 97 | 114,176 | 180,000 |
| `basic-` | 16 | 53 | 87,426 | 130,000 |
| `multi-` | 13 | 83 | 79,053 | 120,000 |
| `verify-` | 13 | 43 | 72,739 | 110,000 |
| `framing-` | 10 | 30 | 58,163 | 90,000 |
| `sem-` | 4 | 14 | 51,398 | 75,000 |
| `hsms-` | 5 | 25 | 33,796 | 50,000 |
| `bootstrap-` | 16 | 33 | 8,113 | 15,000 |
| `validation-` | 1 | 4 | 3,419 | 5,000 |
| `sample-` | 1 | 3 | 2,661 | 3,000 |
| `manifest.json` | | 1 | 82,830 | 120,000 |
| `primitives.json` | | 1 | 718 | 1,000 |
| `README.md`, `.gitattributes` | | 2 | 17,000 (about) | 20,000 |
| **Total** | 144 | 616 | 964,000 (about) | 1,489,000 |

To keep a vector small:
codec `none` unless the codec is the subject, small payloads, a block threshold of a few hundred bytes,
truncation base packs of two or three blocks, and single-record sub-vectors as `cases` of one vector instead of separate vectors.

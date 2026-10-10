# tracepack conformance corpus

This directory is the conformance corpus of tracepack format 1.0:
packs, and the results a conforming reader produces from them.
An implementation in any language proves conformance against the committed files;
it never needs to regenerate them, and it needs no Go.

The corpus is defined by the [tracepack corpus specification](../../../docs/specs/tracepack/tracepack-corpus.md) [CORPUS]:
its layout, the manifest, every JSON schema, the item-validity checklist and the vector catalogue.
This README is informative.
Where it and [CORPUS] differ, [CORPUS] wins.
Section references below are `[CORPUS §n]`, `[FMT §n]` for the [format specification](../../../docs/specs/tracepack/tracepack-format.md),
`[JSONL §n]` for the [JSONL export](../../../docs/specs/tracepack/tracepack-jsonl.md), `[SEM §n]` for the [record semantics](../../../docs/specs/tracepack/tracepack-semantics.md)
and `[STO §n]` for the [storage profile](../../../docs/specs/tracepack/tracepack-storage.md).

The corpus holds the vectors of [FMT §16] and [SEM §9] that format-level reading can show:
single packs, each with its export, its bootstrap rejection or the expectation of every cut.
Verification reports, query results, classifications, reads over several packs and transaction lookups are tests of the Go module,
not files of the corpus ([CORPUS §1]).
Redaction vectors are not in it yet;
[CORPUS §9] states where each clause stands.

## Identification

`manifest.json` ([CORPUS §3]) identifies the corpus and lists its vectors:

| Key | Value |
|---|---|
| `corpus` | `tracepack-corpus/3`, the schema of every JSON file here |
| `spec_version` | the spec version the goldens follow, `2.32` |
| `format_version` | `1.0` |
| `jsonl_schema` | `tracepack-jsonl/1`, the schema of every `.jsonl` file |
| `zstd_encoder` | the zstd encoder that produced the corpus's encoder-made zstd blocks and footers (see [zstd](#zstd)) |
| `vectors` | one entry per vector, ascending by `id` |

The schema `tracepack-corpus/3` is a draft until tracepack v1.0.0.
A change to a schema, or to what an existing file states, takes a new `corpus` value;
adding a vector or a case keeps it ([CORPUS §10]).

`tracepack-corpus/3` removes from `tracepack-corpus/2` the classes `repair` and `multi-pack`,
the files `verify.json`, `queries.json`, `classify.json`, `repair.json`, `patch.*`, `pack-<n>.*`, `reads.json` and `lookups.json`,
and the `outcome`, `verify` and `repair` keys of the rows of `truncation.json`, whose rows with equal remaining keys are now one.
The kept packs, exports, rejections, `fields.json` and `footer.json` files are byte for byte those of `tracepack-corpus/2`.

## Layout

```text
manifest.json       corpus identity and one entry per vector
primitives.json     CRC and UUID byte-order values to check before reading a pack ([CORPUS §5.1])
README.md           this file
.gitattributes      keeps every file's bytes as committed
<vector id>/        one directory per vector
```

A vector entry gives the vector's `id`, a one-line `title`, the clauses it `cites`, its `class`, its `labels`, the `codec` its pack uses, its `source`, the `files` its directory holds,
and, for a vector of several single-record sub-vectors, its `cases` (`{id, seq, cites}`).
Its `id` starts with its group:
`basic-`, `bootstrap-`, `footer-`, `framing-`, `hsms-`, `repair-`, `sample-`, `sem-`, `validation-` or `verify-`.
The `repair-` vectors are the three repair vectors of `tracepack-corpus/2` whose packs serve a format clause no other vector serves,
read like any pack ([CORPUS §9.4]).
Ids are stable and never reused.

A vector directory holds only files with these names ([CORPUS §2]):

| File | Present | Content |
|---|---|---|
| `pack.tpk` | every class | the pack; for a `truncation` vector, the base pack every cut is taken from |
| `export.jsonl` | class `read`, `truncation` | the expected export ([JSONL]) |
| `rejection.json` | class `rejection` | the expected bootstrap rejection code ([CORPUS §5.3]) |
| `fields.json` | optional | the HSMS header fields of each data and control record ([CORPUS §5.5]) |
| `footer.json` | optional | footer values as stored, whether the footer is accepted, and as computed from the records ([CORPUS §5.8]) |
| `truncation.json` | class `truncation` | the expectation of every cut of the pack ([CORPUS §5.7]) |

Labels mark packs that a conforming writer of format 1.0 does not write:
`damaged` (damage after writing: a failed CRC, a cut, a broken codec stream),
`nonconforming-writer` (bytes a conforming writer never writes, every CRC valid)
and `pre-v2.13-sample` (`sample-v010-rows`, a pack of the format definition before v2.13, [FMT §14]).

## Using the corpus

Read `manifest.json`, check `corpus`, `spec_version`, `format_version` and `jsonl_schema` against what your implementation follows,
then take each vector in turn and the files its entry lists.

- **Compare JSON files as JSON values**, never as text ([CORPUS §4]):
  key order and whitespace do not matter, array order does, since every array is in the order its schema states.
  64-bit integers are decimal strings, bytes base64, UUIDs lowercase, enum values and bits named as in [JSONL §3];
  an optional key without a value is omitted, never `null`.
- **Compare `.jsonl` and `.tpk` files byte for byte.**
- **Classes** ([CORPUS §1]):
  - `read`, `truncation`: open `pack.tpk`; its export equals `export.jsonl`.
  - `rejection`: `pack.tpk` is rejected at bootstrap for the reason `rejection.json` names.
  - `truncation`: for every length `L` from 0 to the pack's size − 1,
    the cut `pack.tpk[:L]` meets the row of `truncation.json` whose `from`..`to` holds `L`:
    it is rejected at bootstrap for the reason the row names,
    or it opens and its export is the header line of `export.jsonl` followed by the row's `exported_records` first record lines.
- **Optional files**: `fields.json` lists the header fields as read;
  `footer.json` states whether the footer is accepted,
  and, for an implementation that reads footer values, what it stores and what the records give.
- A **writer** conforms when a conforming reader round-trips its output.
  One writer behaviour is held as bytes:
  `validation-i2-injection` is a validating writer's output stopped before a block that fails I-2, without a trailer.
- Skip the parts of `footer.json` you do not read, if you do not read footer values.
  The other files still bind you.
- No expectation depends on error text, a reader's own limits, an I/O error or a cancellation:
  results are codes, offsets, seqs and counts.

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
  skips that vector's `truncation.json`, whose cut lengths follow the compressed sizes, and its footer,
  compares every other file byte for byte,
  and reports that the corpus is due for regeneration.
- The damaged zstd streams are frames built by hand ([CORPUS §6.2]), never encoder output:
  a Raw first block holding the whole header section, then a malformed compressed block or an early end.
  They, `basic-unknown-codec`, every codec-`none` vector and the fixed packs are always compared byte for byte.

## Regenerating the corpus

The Go reference implementation generates the corpus in its internal package `internal/corpus`.
Each vector's recipe writes its pack with the public `Writer` under a fixed clock and identifiers derived from the vector's id,
then edits its bytes where the vector needs what a `Writer` never writes.
The generator reads every pack as a reader of the corpus does
and checks each read against the recipe's expectation, written by hand from the specification,
before it writes any file.
The expectation also states the pack's verification report, its query results and the verification of each cut,
which the generator checks in memory and writes to no file.

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

Two vectors are `source: fixed`, never regenerated, and always compared byte for byte:

- `sample-v010-rows`: its pack is the one tracepack v0.1.0 wrote, copied from `tracepack/testdata/v0.1.0-rows.tpk`.
- `repair-of-patch`: its pack is the patch an earlier tracepack's repair wrote, damaged after writing;
  tracepack no longer writes such a pack, so the generator copies the committed `repair-of-patch/pack.tpk` as it is.

Two tests elsewhere in the module read the committed files too (paths from `tracepack/`):

- `classify/corpus_test.go` (`TestClassifierCases`) takes the classifier expectations of the `sem-` recipes
  (each case's `decode_status` and `trailing_bytes`, written by hand from [SEM §3] and the checklist of [CORPUS §7]),
  checks that the vector's committed pack stores them, with every other data and control record listed as format-only,
  and runs `classify.New`, the go-secs-backed classifier, on each case's payload under the vector's `max_frame_len`;
  where they disagree, the specification decides and the classifier is fixed.
  The same frames seed `FuzzFrame`.
- `corpus_writer_test.go` (`TestCorpusValidatingWriter`) runs the validating `Writer` on the inputs of `validation-i2-injection`,
  with the defect injected into its last block,
  and requires `ErrValidation` and output equal to that vector's `pack.tpk`.

Every committed pack is also a seed of `FuzzExportJSONL`.

## Size budget

`TestCorpus` fails when the files under this directory, this README included, sum to more than 2,000,000 bytes.
To leave room for the vectors still to come, each group and root file keeps within its budget below.
`TestGroupBudgets` enforces every budget as a ceiling and fails when this table differs from the ceilings it checks.
Measured when the corpus moved to `tracepack-corpus/3`:

| Group | Vectors | Files | Bytes | Budget |
|---|---:|---:|---:|---:|
| `footer-` | 28 | 64 | 151,505 | 240,000 |
| `basic-` | 16 | 32 | 78,265 | 130,000 |
| `verify-` | 13 | 28 | 64,144 | 110,000 |
| `framing-` | 10 | 20 | 53,443 | 90,000 |
| `sem-` | 4 | 8 | 41,529 | 75,000 |
| `hsms-` | 5 | 15 | 25,510 | 50,000 |
| `repair-` | 3 | 6 | 11,117 | 20,000 |
| `bootstrap-` | 16 | 32 | 7,814 | 15,000 |
| `validation-` | 1 | 2 | 2,811 | 5,000 |
| `sample-` | 1 | 2 | 2,021 | 3,000 |
| `manifest.json` | | 1 | 53,377 | 80,000 |
| `primitives.json` | | 1 | 718 | 1,000 |
| `README.md`, `.gitattributes` | | 2 | 12,000 (about) | 20,000 |
| **Total** | 97 | 213 | 504,000 (about) | 839,000 |

To keep a vector small:
codec `none` unless the codec is the subject, small payloads, a block threshold of a few hundred bytes,
truncation base packs of two or three blocks, and single-record sub-vectors as `cases` of one vector instead of separate vectors.

# HSMS payload compression — algorithm research

**Date:** 2026-08-11

**Question:** what is the most suitable compression algorithm for HSMS message payloads — fast and efficient?

**Scope:** SEMI E37 (HSMS generic) and E37.1 (HSMS-SS) wire legality,
pure-Go no-cgo block compressors,
and the deployment shape a library consumed as a dependency can actually ship.

**Sources:**
`~/semi_standards/markdowns/e037-00-0413/e037-00-0413.md` (E37-0413, generic services),
`~/semi_standards/markdowns/e037-01-0702/e037-01-0702.md` (E37.1-0702, HSMS-SS),
`~/semi_standards/markdowns/e005-00-0813/e005-00-0813.md` (E5-0813, SECS-II),
the `klauspost/compress@v1.19.0` source in the local module cache,
`pierrec/lz4/v4@v4.1.28`,
`golang/snappy@v1.0.0`,
and this repo's `hsms/`, `hsmsss/`, `secs2/`.

**Method:** every claim is traced to the artifact that owns it.
Nothing measured here; the repo-corpus numbers land in [Measured results](#8-measured-results-repo-corpus).

**Tagging:** each substantive claim is marked **EVIDENCE** (quoted spec text, quoted source, or a published measurement),
**INFERENCE** (reasoned from evidence),
or **ASSUMPTION** (stated belief, not yet checked).

---

## Verdict

**Compressing the HSMS Message Text on the wire is not conformant for an HSMS-SS implementation, under any signalling scheme.**
E37 reserves the presentation-type extension point for *subsidiary standards*, not implementations,
and E37.1 closes it outright: "All HSMS-SS messages are PType 0 (SECS II encoded) as defined in HSMS" (E37.1 §8.2).
Under E37 §8.3.3.7 a PType-0 Message Text "will be formatted as SECS-II messages",
so a compressed frame in the body is a violation whether or not any header bit advertises it.
Full argument in [§1](#1-wire-level-legality).

**The only conformant path is compression above the SECS-II layer: the compressed blob carried as a SECS-II binary item, inside a user-defined stream/function.**
E5 §7.3.1 reserves "Streams 64 to 127, Functions 1 to 255" for user definition,
so such a message is a legal SECS-II message, in a legal PType-0 HSMS Data Message, on a conformant HSMS-SS link.
Full argument in [§6](#6-negotiation--deployment-options-ranked).

**Given that path, the algorithm is zstd via `github.com/klauspost/compress/zstd`, using `EncodeAll`/`DecodeAll` with a trained dictionary.**
The discriminator is not throughput — it is whether the *block* API accepts a dictionary on the **encode** side:

| Library | Block encode API | Dictionary on encode? | Dictionary ID in the output? |
|---|---|---|---|
| `klauspost/compress/zstd` | `(*Encoder).EncodeAll` | **Yes** — `WithEncoderDict` / `WithEncoderDictRaw` | **Yes** — Dictionary_ID field in the frame header |
| `klauspost/compress/s2` | `(*Dict).Encode` / `EncodeBetter` / `EncodeBest` | Yes — `MakeDict` / `NewDict` | **No** |
| `pierrec/lz4/v4` | `(*Compressor).CompressBlock` | **No** — dictionary is decode-side only | n/a |
| `golang/snappy` | `Encode` | **No** — the format has none | n/a |

Sources for each cell in [§2](#2-candidate-algorithms--pure-go-no-cgo).
Without an encode-side dictionary a 200 B – 4 KB SECS-II body has almost no exploitable history,
and per-message compression is close to net-negative on it;
that is the whole reason the dictionary column decides this ([§3](#3-dictionary-trained-zstd)).

**Do not put the dependency in the core module.**
`go.mod` today carries three small direct dependencies;
`klauspost/compress` is large and would be imposed on every consumer of `go-secs`.
Ship a codec interface in core and the zstd implementation as a separate module, or accept a caller-supplied codec ([§6.4](#64-dependency-placement)).

---

## 1 Wire-level legality

### 1.1 What PType is, under E37 generic

**EVIDENCE.** The Message Text's encoding is defined by PType, not fixed.
E37 §8.2.2, message-format table: the last row is "0–n Bytes | Message Text. Format is further specified by PType field of message header."

**EVIDENCE.** E37 §8.2.6.4:

> *PType* PType (Presentation Type) is an 8-bit unsigned integer value which occupies byte 4 of the header.
> PType is intended as an enumerated type defining the presentation layer message type: how the Message Header and Message Text are encoded.
> Only PType = 0 is defined by HSMS to mean SECS-II message encoding.
> For non-zero PType values, see "Special Considerations."

**EVIDENCE.** E37 Table 4 (PType):

| Value | Description |
|---|---|
| 0 | SECS-II Encoding |
| 1–127 | Reserved for subsidiary standards |
| 128–255 | Reserved, not used |

So PType *is* an extension point for "the body is not plain SECS-II" — that is precisely what it was designed to express.
The question is who is allowed to use it.

### 1.2 Who may define a non-zero PType

**EVIDENCE.** E37 §9.3.3.1 answers it, and the answer is "subsidiary standards", not implementations:

> Subsidiary standards must be consistent with this convention.
> In particular, for SType = 0, subsidiary standards defining PType values not equal to 0 may specify both the message text encoding and the interpretation of header bytes 2 and 3.
> For STypes not equal to 0 but otherwise specified in this standard, PType must = 0, and no message text may be transmitted.

Two consequences.
First, a *subsidiary standard* — a SEMI-balloted document such as E37.1 or E37.2 — is the only thing that may assign meaning to PType 1–127.
No such standard defines a compression PType, and this repo is not one.
Second, the last sentence forecloses a side channel: a control message (SType ≠ 0) may not carry any Message Text,
so compression parameters cannot be negotiated by attaching a body to a Linktest or Select.

**EVIDENCE.** The only sanction for a private, implementation-defined value is E37 Appendix A1-10.1, and it is explicitly bounded:

> User-defined extensions through new message types are permissible as long as they are confined to intra-vendor communication interfaces:
> any inter-vendor communications interface which requires the use of such extensions is considered to be noncompliant with the HSMS standard.

A1-10.2 adds that such extensions should live in the "reserved, not used" ranges (PType 128–255, SType 128–255),
so that future subsidiary standards do not collide with them.

**INFERENCE.** `go-secs` is a general-purpose library published for arbitrary consumers.
It cannot warrant that any given link is intra-vendor.
Shipping a PType-signalled compression mode as a supported feature therefore ships a documented route to a noncompliant inter-vendor interface,
which is the exact case A1-10.1 names.

### 1.3 What E37.1 narrows

**EVIDENCE.** E37.1 §5.1 states the profile's whole purpose:
"The purpose of this standard is to explicitly limit the capabilities of the HSMS Generic Services to the minimum necessary for this type of application."

**EVIDENCE.** E37.1 §8.2, in full:
"PType — All HSMS-SS messages are PType 0 (SECS II encoded) as defined in HSMS."

**EVIDENCE.** E37.1 §8.3, in full:
"SType — Only HSMS-defined STypes are permitted in HSMS-SS. User-defined SType messages are not permitted."

**EVIDENCE.** E37.1 §7.7 supplies the teeth:
"Communications Failures — As defined by HSMS.
Note that, in addition to the communications failures defined under HSMS, any violation of the restrictions defined in prior sections of this document are also to be treated as communication failures."

So under HSMS-SS a non-zero PType is not merely undefined — emitting or receiving one is a *communications failure*,
and E37 §9.1.1 says the entity "should terminate the TCP/IP connection" on a communications failure.

### 1.4 What the receiving peer does about it

**EVIDENCE.** E37 §7.10.3 requires the Reject procedure for "the receipt of a message whose SType or PType (see next section: Message Format) is not defined for the entity receiving the message."
E37 Table 9 assigns ReasonCode 2 = "PType Not Supported",
and §8.3.21.2 says header byte 2 of the Reject.req echoes the offending PType.

**EVIDENCE.** This repo already implements exactly that, on both sides:

- `hsms/decode.go:105-107` — every inbound frame with `h[4] != 0` is rejected with `ErrInvalidPType` before any body handling.
- `hsms/data_msg.go:353-359` — the raw-header data-message constructor requires PType and SType both 0.
- `hsmsss/transport_control.go:142-157` — `sendReject` reports a non-zero PType as `hsms.RejectPTypeNotSupported`.
- `hsms/errors.go:14-17` — "ErrInvalidPType indicates that an invalid PType was provided.
  The PType should be 0 for SECS-II message."

**INFERENCE.** A wire-level compression mode would therefore have to be implemented *by weakening these checks*, on both peers, in a library whose current behavior is a documented E37.1 conformance property (`docs/specs/e37-1-hsms-ss-conformance-audit.md`).
That is a conformance regression, not a feature flag.

### 1.5 Verdict for §1

| Scheme | Conformant under E37 generic? | Conformant under E37.1 (HSMS-SS)? |
|---|---|---|
| PType 1–127 for a compressed body | **No** — only a subsidiary standard may assign these (§9.3.3.1); none does | **No** — §8.2 fixes PType 0 |
| PType 128–255 for a compressed body | Intra-vendor only (A1-10.1/A1-10.2); noncompliant on any inter-vendor link | **No** — §8.2 fixes PType 0 |
| Non-zero SType carrying compression metadata | **No** — §9.3.3.1: non-zero STypes defined by this standard "must = 0 [PType], and no message text may be transmitted" | **No** — §8.3 bars user-defined STypes |
| Compressed bytes in a PType-0 body, unsignalled | **No** — §8.3.3.7: for PType 0 "the text will be formatted as SECS-II messages" | **No** — plus §7.7 makes it a communications failure |
| Compressed blob as a SECS-II **binary item** inside a valid SECS-II body | **Yes** — it is just data; the body remains well-formed SECS-II | **Yes** |

---

## 2 Candidate algorithms — pure Go, no cgo

cgo is disqualifying: it breaks consumers' cross-compilation, and `go-secs` is consumed as a dependency.
All four candidates below are pure Go.
Facts are read from the source in the local module cache, and release dates from `proxy.golang.org`.

### 2.1 `github.com/klauspost/compress/zstd`

**EVIDENCE — API shape.** Block API, one shot in each direction:

- `func (e *Encoder) EncodeAll(src, dst []byte) []byte` (`zstd/encoder.go:722`)
- `func (d *Decoder) DecodeAll(input, dst []byte) ([]byte, error)` (`zstd/decoder.go:319`)

Constructed by `NewWriter(nil, opts...)` and `NewReader(nil, opts...)`;
`zstd/encoder.go:69-70` — "NewWriter will create a new Zstandard encoder.
If the encoder will be used for encoding blocks a nil writer can be used."

**EVIDENCE — dictionaries.**
Encode side: `WithEncoderDict(dict []byte)` (`zstd/encoder_options.go:382`) and `WithEncoderDictRaw(id uint32, content []byte)` (`:398`).
Decode side: `WithDecoderDicts(dicts ...[]byte)` (`zstd/decoder_options.go:112`), which registers several at once, keyed by ID.
Detail in [§3](#3-dictionary-trained-zstd).

**EVIDENCE — allocation behavior.** `zstd/README.md`:

> Especially when encoding blocks you should take special care to reuse the encoder.
> This will effectively make it run without allocations after a warmup period.
> To make it run completely without allocations, supply a destination buffer with space for all content.

The source backs this: `EncodeAll` pulls a warm encoder out of a buffered channel rather than building one (`zstd/encoder.go:723-728`),
and appends into the caller's `dst`.

**EVIDENCE — maintenance and Go version.**
`v1.19.2` released 2026-08-05 (`proxy.golang.org/github.com/klauspost/compress/@v/v1.19.2.info`).
`go.mod` of v1.19.0 declares `go 1.24`.
Top-level `README.md`: "This package will support the current Go version and 2 versions back."
Also relevant to a dependency-conscious library: the module publishes `retract` directives for v1.18.1, v1.14.1–v1.14.3.
Status per `zstd/README.md`: "STABLE — there may always be subtle bugs, a wide variety of content has been tested and the library is actively used by several projects."

**EVIDENCE — build-tag escape hatches.** `zstd/README.md`: "This package is pure Go. Use `noasm` and `nounsafe` to disable relevant features."

### 2.2 `github.com/klauspost/compress/s2`

**EVIDENCE — API shape.** Block API with an explicit dictionary receiver:
`func (d *Dict) Encode(dst, src []byte) []byte` (`s2/dict.go:142`), plus `EncodeBetter` (`:186`), `EncodeBest` (`:228`), and `func (d *Dict) Decode(dst, src []byte) ([]byte, error)` (`:260`).
Dictionaries are built in-process: `MakeDict(data, searchStart []byte) *Dict` (`:83`), `MakeDictManual` (`:115`), `NewDict(dict []byte) *Dict` (`:41`), serialized by `(*Dict).Bytes()` (`:70`).

**EVIDENCE — dictionary size bounds.** `s2/dict.go:13-22` — `MinDictSize = 16`, `MaxDictSize = 65536`, `MaxDictSrcOffset = 65535`.
`s2/README.md`: "S2 further limits the dictionary to only be enabled on the first 64KB of a block."

**EVIDENCE — the disqualifier.** `s2/README.md`:

> The same dictionary *must* be used for both encoding and decoding.
> S2 does not keep track of whether the same dictionary is used,
> and using the wrong dictionary will most often not result in an error when decompressing.

and

> *Note: S2 dictionary compression is currently at an early implementation stage, with no assembly for neither encoding nor decoding.
> Performance improvements can be expected in the future.*

**INFERENCE.** For a protocol library, "the wrong dictionary silently produces wrong bytes" is a worse failure mode than any ratio deficit.
A SECS-II body decoded from a mismatched dictionary would be handed to `secs2.Decode` as plausible-looking garbage;
most of the time it errors, but nothing guarantees it.
zstd's frame-embedded Dictionary_ID turns the same mistake into a typed error ([§3.3](#33-dictionary-distribution-and-versioning)).

### 2.3 `github.com/pierrec/lz4/v4`

**EVIDENCE — API shape.** Block API:
`func (c *Compressor) CompressBlock(src, dst []byte) (int, error)` (`lz4.go:71`),
`func (c *CompressorHC) CompressBlock(src, dst []byte) (int, error)` (`lz4.go:121`),
`func UncompressBlock(src, dst []byte) (int, error)` (`lz4.go:37`),
`func CompressBlockBound(n int) int` (`lz4.go:27`).
The package-level `CompressBlock`/`CompressBlockHC` functions are marked "This function is deprecated.
Use a Compressor instead."

**EVIDENCE — the disqualifier.** The block API has `UncompressBlockWithDict(src, dst, dict []byte)` (`lz4.go:47`) and **no** compress-side counterpart.
`grep -n "Dict" options.go` returns nothing, so the frame `Writer` has no dictionary option either.
Dictionary support is decode-only.

**EVIDENCE — concurrency.** `lz4.go:53`: "A Compressor is not safe for concurrent use by multiple goroutines."
A pool would be required.

**EVIDENCE — maintenance and Go version.**
`v4.1.28` released 2026-08-05 (`proxy.golang.org`) — actively maintained.
`go.mod` declares `go 1.17`.

**INFERENCE.** lz4 is out on the dictionary column alone, not on quality or maintenance.
(A note on stale local caches: this machine's module cache holds `pierrec/lz4@v2.6.1+incompatible`, two majors behind.
Any review of the v2 API would be reviewing dead code — v4 is the current line.)

### 2.4 `github.com/golang/snappy` and `klauspost/compress/snappy`

**EVIDENCE — API shape.** `func Encode(dst, src []byte) []byte` (`encode.go:20`), `func Decode(dst, src []byte) ([]byte, error)` (`decode.go:57`), `func MaxEncodedLen(srcLen int) int` (`encode.go:78`).
Stateless package-level functions, so trivially concurrency-safe, and no encoder object to pool.

**EVIDENCE — the disqualifier.** The Snappy block format has no dictionary mechanism.
`snappy.go:26-44` documents the complete block format — a varint decoded-length prefix followed by literal and copy tags — and there is no dictionary or preset-history construct in it.
`grep -i dict` across the package returns nothing.

**EVIDENCE — maintenance.** `golang/snappy` `v1.0.0` released 2023-12-25 (`proxy.golang.org`); no newer version is published.
Its `go.mod` is a single `module` line with no `go` directive.
`klauspost/compress`'s top-level `README.md` describes `klauspost/compress/snappy` as "a drop-in replacement for `github.com/golang/snappy` offering better compression and concurrent streams" — same format, same absence of dictionaries.

**INFERENCE.** Snappy is the right shape (stateless, allocation-light, one-shot) and the wrong capability.
On SECS-II bodies of a few hundred bytes without a dictionary it is the candidate most likely to expand the payload.

### 2.5 Summary table

| | zstd (klauspost) | s2 (klauspost) | lz4/v4 (pierrec) | snappy (golang) |
|---|---|---|---|---|
| Pure Go, no cgo | Yes | Yes | Yes | Yes |
| One-shot block API | `EncodeAll`/`DecodeAll` | `Dict.Encode`/`Dict.Decode`, `s2.Encode*`/`s2.Decode` | `Compressor.CompressBlock`/`UncompressBlock` | `Encode`/`Decode` |
| Encode-side dictionary | **Yes** | Yes | **No** | **No** |
| Dictionary identified in output | **Yes** (frame Dictionary_ID) | **No** | n/a | n/a |
| Encoder safe for concurrent one-shot calls | Yes, pool-bounded | Yes (`*Dict` is read-mostly, `sync.Once`-guarded tables) | **No** (`Compressor` not concurrency-safe) | Yes (stateless) |
| Decoder bomb guard | `WithDecoderMaxMemory` | caller-enforced | caller-sized `dst` | caller-enforced |
| Latest release | v1.19.2, 2026-08-05 | (same module) | v4.1.28, 2026-08-05 | v1.0.0, 2023-12-25 |
| `go` directive | 1.24 | 1.24 | 1.17 | none |

**ASSUMPTION.** The `*s2.Dict` concurrency claim above is read off the struct's `sync.Once`-guarded lazy tables (`s2/dict.go:29-36`) rather than from a documented guarantee.
`s2` does not state concurrency-safety for `*Dict` in prose; treat it as unverified until exercised under `-race`.

---

## 3 Dictionary-trained zstd

This is the decisive section.

### 3.1 Why a dictionary, specifically, for SECS-II

**EVIDENCE — the shape of the data.** SECS-II bodies are TLV-framed.
E5 §9.2: "An item is an information packet which has a length and format defined by the first 2, 3, or 4 bytes of the item.
These first bytes are called the item header (IH).
The item header consists of the format byte and the length byte(s)…"
Every item on the wire therefore carries a 2–4 byte header from a 16-value format vocabulary (E5 §9.2.2).

**EVIDENCE — the shape of the traffic.** The heavy messages are structurally identical from one send to the next:

- S1F4 (Selected Equipment Status Data), E5 §S1,F4: structure `L,n / 1. <SV1> / … n. <SVn>`, and "The equipment reports the value of each SVID requested **in the order requested**."
- S6F11 (Event Report Send), E5 §S6,F11: `L,3 { <DATAID>, <CEID>, L,a { L,2 { <RPTIDi>, L,b { <V1>…<Vb> } } } }` — the same report skeleton repeated per report, per event, forever.
- S7F23 (Formatted Process Program Send), E5 §S7,F23: `L,4 { <PPID>, <MDLN>, <SOFTREV>, L,c { L,2 { <CCODE>, L,p { <PPARM1>…<PPARMp> } } } }`.

**INFERENCE.** Almost all of the redundancy in this traffic is *cross-message*: the item skeleton, the SVID/RPTID/CEID identifier sets, the format bytes, the recurring ASCII names.
Within a single 200 B – 4 KB body there is very little history for an LZ matcher to find.
A per-message compressor therefore pays frame overhead and entropy-coding startup cost against a nearly incompressible input.
A dictionary moves the cross-message redundancy into the matcher's initial history, which is what makes these payloads compressible at all.

**EVIDENCE — upstream says the same thing.** facebook/zstd, "The case for Small Data compression":
compression algorithms "learn from past data how to compress future data", so small data is harder;
dictionaries let a user "tune the algorithm for a selected type of data" by supplying "a few samples (one file per sample)";
"Dictionary gains are mostly effective in the first few KB";
and "there is no *universal dictionary*", so "deploying one dictionary per type of data will provide the greatest benefits."

### 3.2 How klauspost/compress supports dictionaries

**EVIDENCE — encode side.** `zstd/encoder_options.go:373-382`:

> WithEncoderDict allows to register a dictionary that will be used for the encode.
> The slice dict must be in the [dictionary format] produced by "zstd --train" from the Zstandard reference implementation.
> The encoder *may* choose to use no dictionary instead for certain payloads.
> Can be changed with ResetWithOptions.

`zstd/encoder_options.go:393-398` offers a lighter variant:

> WithEncoderDictRaw registers a dictionary that may be used by the encoder.
> The slice content may contain arbitrary data.
> It will be used as an initial history.

**INFERENCE.** `WithEncoderDictRaw` is the cheap on-ramp: any concatenation of representative message bodies works as raw history, with no training tool and no dictionary file format.
It gives back-reference matching but not the trained entropy tables (Huffman literal table, FSE tables, initial offsets) that a real dictionary carries.
Expect it to capture some of the gain, not all of it.

**EVIDENCE — decode side.** `zstd/decoder_options.go:103-112`:

> WithDecoderDicts allows to register one or more dictionaries for the decoder.
> Each slice in dict must be in the [dictionary format] produced by "zstd --train" from the Zstandard reference implementation.
> If several dictionaries with the same ID are provided, the last one will be used.
> Can be changed with ResetWithOptions.

`zstd/README.md` adds: "The dictionary will be used automatically for the data that specifies them.
A re-used Decoder will still contain the dictionaries registered."

**EVIDENCE — one dictionary per encoder, many per decoder.** `zstd/README.md`: "To enable a dictionary use `WithEncoderDict(dict []byte)`. Here only one dictionary will be used".

**EVIDENCE — the cost.** `zstd/README.md`:

> For any real gains, the dictionary should be built with similar data.
> If an unsuitable dictionary is used the output may be slightly larger than using no dictionary.

and

> For now there is a fixed startup performance penalty for compressing content with dictionaries.

**EVIDENCE — incompatibility with parallel-block streaming.** `zstd/README.md`, in the `WithConcurrentBlocks` notes: "Not compatible with dictionary encoding."
Irrelevant here — that option applies only to the streaming path, which §4 rules out anyway.

### 3.3 Dictionary distribution and versioning

**EVIDENCE — zstd self-describes which dictionary was used.**
The encoder writes a Dictionary_ID into the frame header:
`zstd/frameenc.go:33-49` emits 1, 2, or 4 bytes of `f.DictID` when it is non-zero, with the size flagged in the low two bits of the Frame_Header_Descriptor.
`zstd/encoder.go:763` sets `DictID: e.o.dict.ID()` on every `EncodeAll` frame.
The decoder reads it back at `zstd/framedec.go:158-183`.

**EVIDENCE — mismatch is a typed error, not silent corruption.**
`zstd/zstd.go:74-75` declares it — "ErrUnknownDictionary is returned if the dictionary ID is unknown" —
and `zstd/decoder.go:942-956` (`setDict`) is the single site that returns it,
looking the frame's `DictionaryID` up in the registered map and erroring when it is absent.

**EVIDENCE — and it fires on the block path, not only on streams.**
`(*Decoder).DecodeAll` calls `d.setDict(frame)` immediately after `frame.reset` and returns its error before any decoding happens (`zstd/decoder.go:352-354`).
This was checked because the whole zstd-over-s2 preference in §2.2 rests on it.

**INFERENCE.** This gives the versioning story for free, and it is the single strongest reason to prefer zstd over s2 here.
Bump the dictionary → bump its ID → an older peer that lacks the new dictionary fails loudly with `ErrUnknownDictionary` on the first message instead of decoding rubbish.
A rolling upgrade is then: register the new dictionary on both peers' *decoders* first (many may be registered at once), then switch the encoders over.

**INFERENCE.** Operational obligations that follow:

- The dictionary is a versioned artifact with its own lifecycle, shipped and pinned alongside the code — not generated at runtime, not derived from live traffic, because both peers must hold byte-identical content for a given ID.
- Dictionary ID 0 must be reserved, and the reason is sharper than "0 means no dictionary".
  `frameenc.go:34` emits the field only when `f.DictID > 0`,
  and `setDict` deliberately declines to error on ID 0: "A zero or missing dictionary id is ambiguous: either dictionary zero, or no dictionary… so only return an error if the dictionary id is not zero" (`zstd/decoder.go:948-955`).
  A dictionary registered under ID 0 therefore loses the loud-failure property that makes this scheme safe.
- A dictionary trained on one fab's SVID/CEID/recipe vocabulary is worth little at another.
  Per-deployment training is the norm, per E5's own point that there is no universal dictionary.

### 3.4 Training: what is available in pure Go, and what is not

**EVIDENCE — the documented route is the C CLI.** `zstd/README.md`: "Dictionaries are generated by the `zstd --train` command and contains an initial state for the decoder,"
and "Use the [zstd commandline tool](https://github.com/facebook/zstd/releases) to build a dictionary from sample data."
facebook/zstd documents `zstd --train FullPathToTrainingSet/* -o dictionaryName`.
`s2/README.md` shows the FASTCOVER variant: `zstd -r --train-fastcover training-set/* --maxdict=65536 -o name.dict`.

**EVIDENCE — a pure-Go builder exists, but it is not a trainer.**
`zstd/dict.go:192` — `func BuildDict(o BuildDictOptions) ([]byte, error)`.
Its options (`zstd/dict.go:165-190`) are:

```go
type BuildDictOptions struct {
	ID uint32          // Dictionary ID.
	Contents [][]byte  // Content to use to create dictionary tables.
	History []byte     // History to use for all blocks.
	Offsets [3]int     // Offsets to use.
	CompatV155 bool
	Level EncoderLevel
	DebugOut io.Writer
}
```

`History` — the dictionary *content* — is supplied by the caller, and is required (`dict.go:216-218` rejects `len(hist) < 8`).
`Contents` is the sample set used to fit the entropy tables *over that history*.

**INFERENCE — this is the important nuance.**
`BuildDict` performs the entropy-table and offset half of dictionary construction in pure Go, but not the content-selection half.
The reference `zstd --train` runs COVER/FASTCOVER to *choose* which substrings belong in the dictionary;
`BuildDict` expects you to have already chosen.
So:

- **Fully pure-Go, no external tooling:** pick the history heuristically (e.g. concatenate N representative bodies, or the most frequent message shapes), feed it as `History` with the sample set as `Contents`, get a proper dictionary with entropy tables and an ID. Strictly better than `WithEncoderDictRaw`, strictly worse than a COVER-selected dictionary.
- **Best ratio:** run `zstd --train` (or `--train-fastcover`) offline on a corpus dump, check the resulting `.dict` into the deployment artifact, load it with `WithEncoderDict` / `WithDecoderDicts`. The build-time dependency on a C tool never reaches consumers — the shipped artifact is a byte slice.

**EVIDENCE — a bridge between the two.** `zstd/dict.go:152` exports `InspectDictionary(b []byte)`, which parses a `zstd --train` dictionary and exposes `ID()`, `Content()`, `ContentSize()`, `Offsets()`.
`s2/README.md` uses exactly this to convert a zstd-trained dictionary into an s2 one;
the same call lets Go code inspect or re-key a dictionary produced offline without shelling out.

### 3.5 Published evidence on small-payload dictionary gains

Everything below is **EVIDENCE**, measured by upstream authors on the corpora named.
None of it is SECS-II.
Extrapolating it to SECS-II bodies is **INFERENCE** and is exactly what [Measured results](#8-measured-results-repo-corpus) is for.

**facebook/zstd**, "The case for Small Data compression":
demonstrated on a github-users dataset of roughly 10,000 records at roughly 1 KB each,
reporting that "compression gains are achieved while simultaneously providing *faster* compression and decompression speeds."

**`s2/README.md`**, using `github_users_sample_set` (from the zstd v1.1.3 release) with a 64 KB zstd-trained dictionary — s2, not zstd, but the same corpus and the same dictionary:

| | Default | Better | Best |
|---|---|---|---|
| Without dictionary | 3362023 (44.92%) | 3083163 (41.19%) | 3057944 (40.86%) |
| With dictionary | 921524 (12.31%) | 873154 (11.67%) | 785503 (10.49%) |

The README's own summary: "So for highly repetitive content, this case provides an almost 3x reduction in size."

**`s2/README.md`**, less uniform corpus — first 64 KB of every `.go` file in `go/src`, Go 1.19.5, 8912 files, 51253563 bytes input:

| | Default | Better | Best |
|---|---|---|---|
| Without dictionary | 22955767 (44.79%) | 20189613 (39.39%) | 19482828 (38.01%) |
| With dictionary | 19654568 (38.35%) | 16289357 (31.78%) | 15184589 (29.63%) |
| Saving/file | 362 bytes | 428 bytes | 472 bytes |

**INFERENCE.** The two tables bracket the expected outcome.
`github_users_sample_set` — many small records of one rigid schema — is the structural analogue of S1F4 / S6F11 traffic, and is where the near-3x figure comes from.
The Go-source table is the analogue of PPBody recipe text: real but modest gains, a few hundred bytes per message.
The gap between them is a warning that the answer for SECS-II is corpus-dependent and must be measured, not assumed.

**EVIDENCE — a cheap fallback worth testing.** `s2/README.md`: "in the `github_users_sample_set` above, the average compression only goes up from 10.49% to 11.48% by using the first file as dictionary compared to using a dedicated dictionary."

**INFERENCE.** If that transfers, a single representative captured message used as raw history via `WithEncoderDictRaw` may recover most of the benefit of a trained dictionary, at zero tooling cost.
Cheap to test on the repo corpus and worth testing first.

---

## 4 Block versus stream

### 4.1 The block API is the right shape

**EVIDENCE.** HSMS messages are length-prefixed and fully materialized before the write.
E37 §8.2.2: the frame is a 4-byte Message Length, then a 10-byte header, then 0–n bytes of Message Text;
§8.2.3: Message Length "specifies the length in bytes of the Message Header plus the Message Text."
The length must be known before the first byte goes out.

**EVIDENCE.** This repo builds the whole frame in one shot.
`hsms/data_msg.go:77-94` — `ToBytes` documents "Performs exactly one allocation" and does
`dst := make([]byte, 0, 4+10+n)`, writes the big-endian length from `msg.body.Len()`, appends the header, then `msg.body.AppendTo(dst)`.

**INFERENCE.** There is no point in the send path where bytes are produced incrementally without their total length being known,
so the streaming `io.WriteCloser` form has nothing to offer and costs a goroutine handshake per message.
`zstd/README.md` agrees on the general principle: "Smaller encodes are encouraged to use the EncodeAll function."

### 4.2 Concurrency guarantees, quoted

**EVIDENCE — encoder.** `zstd/encoder.go:716-721`:

> EncodeAll will encode all input in src and append it to dst.
> This function can be called concurrently, but each call will only run on a single goroutine.
> If empty input is given, nothing is returned, unless WithZeroFrames is specified.
> Encoded blocks can be concatenated and the result will be the combined input stream.
> Data compressed with EncodeAll can be decoded with the Decoder, using either a stream or DecodeAll.

`zstd/README.md` adds: "Using the Encoder for both a stream and individual blocks concurrently is safe."

**EVIDENCE — the mechanism, and its limit.** `zstd/encoder.go:722-729`:

```go
func (e *Encoder) EncodeAll(src, dst []byte) []byte {
	e.init.Do(e.initialize)
	enc := <-e.encoders
	defer func() {
		e.encoders <- enc
	}()
	return e.encodeAll(enc, src, dst)
}
```

So a single `*zstd.Encoder` **is** safe for concurrent `EncodeAll`,
but it is an encoder *pool* behind a buffered channel, sized by `WithEncoderConcurrency(n)` — default `runtime.GOMAXPROCS(0)` (`zstd/encoder_options.go:39`).
A caller beyond `n` in flight blocks on the channel receive.

**INFERENCE.** Two things follow for a connection-oriented library.
First, one shared `*Encoder` per process is correct — do not build a `sync.Pool` of encoders on top of one that already pools.
Second, `n` is a memory knob as much as a parallelism knob: each pooled encoder holds match tables sized by the window.
For per-message bodies bounded by a few MB, `WithWindowSize` should be pinned low (see §5.3) rather than left at the 8 MB default (`zstd/encoder_options.go:40`).

**EVIDENCE — decoder.** `zstd/decoder.go:314-318`:

> DecodeAll allows stateless decoding of a blob of bytes.
> Output will be appended to dst, so if the destination size is known you can pre-allocate the destination slice to avoid allocations.
> DecodeAll can be used concurrently.
> The Decoder concurrency limits will be respected.

`zstd/decoder.go:78-80`: "Only a single stream can be decoded concurrently,
but the same decoder can run multiple concurrent stateless decodes.
It is even possible to use stateless decodes while a stream is being decoded."

`WithDecoderConcurrency(n)` (`zstd/decoder_options.go:68`): "When decoding block with DecodeAll, this will limit the number of possible concurrently running decodes… By default this will be set to 4 or GOMAXPROCS, whatever is lower" (`:34-40`).

### 4.3 Required decoder hardening

**EVIDENCE — the repo's existing frame cap.**
`hsms/decode.go:22` — `const maxHSMSMsgLen = secs2.MaxByteSize`, and `secs2/item.go:11` — `const MaxByteSize = 1<<24 - 1`.
The comment at `hsms/decode.go:11-15` is explicit that this is "a whole-frame DoS cap… a value chosen to reject an attacker-controlled length before allocating the frame buffer".

**EVIDENCE — compression reopens that hole.**
`zstd`'s default decoded-size limit is 64 GiB: `zstd/decoder_options.go:41` — `o.maxDecodedSize = 64 << 30`.
`WithDecoderMaxMemory(n uint64)` (`:90`) "allows to set a maximum decoded size for in-memory non-streaming operations… This can be used to control memory usage of potentially hostile content."
Exceeding it yields `ErrDecoderSizeExceeded` (`zstd/zstd.go:71-72`).

**EVIDENCE — the cap is enforced before decoding, when the frame declares its size.**
`zstd/decoder.go:359-365`: if `frame.FrameContentSize != fcsUnknown` and it exceeds the remaining budget, `DecodeAll` returns `ErrDecoderSizeExceeded` without decoding a byte.
Both `WithDecoderMaxWindow` → `ErrWindowSizeExceeded` (`:355-358`) and this check run ahead of `runDecoder`.

**EVIDENCE — a tighter guard exists for the block path.**
`zstd/decoder_options.go:163-168`:

> WithDecodeAllCapLimit will limit DecodeAll to decoding cap(dst)-len(dst) bytes, or any size set in WithDecoderMaxMemory.
> This can be used to limit decoding to a specific maximum output size.
> Disabled by default.

**INFERENCE — this is a requirement, not a tuning option.**
A compressed blob that passes the 16 MiB frame cap can expand to gigabytes.
`WithDecoderMaxMemory` must be set to the same ceiling the framing layer already enforces (`secs2.MaxByteSize`), or lower.
Pair it with `WithDecodeAllCapLimit(true)` and a `dst` pre-sized from the envelope's declared uncompressed length:
that turns the per-message bound into the caller's own number rather than a global one,
and it also removes the reallocation that `zstd/decoder.go:315-316` says a pre-sized `dst` avoids.

**EVIDENCE — do not use the convenience wrappers.**
`zstd/simple_go124.go` exports package-level `EncodeTo` / `DecodeTo` (Go 1.24+, weak-pointer-cached encoder/decoder).
They hard-code `WithWindowSize(1<<20)`, `WithDecoderMaxMemory(1<<30)`, and no dictionary.
Unsuitable: a 1 GiB decode cap and no dictionary defeat both §3 and §4.3.

---

## 5 Size threshold — where compression starts paying

### 5.1 zstd frame overhead, derived from source

**EVIDENCE.** A zstd frame emitted by `EncodeAll` costs, before any compressed content:

| Component | Bytes | Source |
|---|---|---|
| Magic number | 4 | `zstd/frameenc.go:25` |
| Frame_Header_Descriptor | 1 | `zstd/frameenc.go:63` |
| Window_Descriptor | 1, omitted when SingleSegment | `zstd/frameenc.go:64-68` |
| Dictionary_ID | 1 (ID < 256), 2 (< 65536), else 4; omitted when 0 | `zstd/frameenc.go:34-49`, `:69-71` |
| Frame_Content_Size | 0 when not SingleSegment and size < 256; 1 when SingleSegment and size < 256; 2 for 256 ≤ size < 65792; 4; or 8 | `zstd/frameenc.go:50-60`, `:72-86` |
| Block header | 3 | `zstd/blockenc.go:134-136` |
| Content checksum | 4, enabled by default | `zstd/encoder_options.go:38` (`crc: true`), `zstd/encoder.go:825-827` |

**EVIDENCE — SingleSegment is chosen automatically.** `zstd/encoder.go:755-758`:

```go
// Use single segments when above minimum window and below window size.
single := len(src) <= e.o.windowSize && len(src) > MinWindowSize
if e.o.single != nil {
	single = *e.o.single
}
```

with `MinWindowSize = 1 << 10` (`zstd/framedec.go:39`).
So a body **under 1024 bytes** is not single-segment by default — it pays the Window_Descriptor byte.

**INFERENCE — the arithmetic, for a dictionary with ID < 256.**

- Default options, body < 256 B: 4 + 1 + 1 (window) + 1 (dictID) + 0 (FCS) + 3 (block header) + 4 (CRC) = **14 bytes**.
- `WithSingleSegment(true)` and `WithEncoderCRC(false)`, body < 256 B: 4 + 1 + 0 + 1 + 1 + 3 = **10 bytes**.
- Same settings, body 256 B – 64 KB: 4 + 1 + 0 + 1 + 2 + 3 = **11 bytes**.

**INFERENCE.** Turning the CRC off is defensible here and worth 4 bytes on every message:
TCP already checksums, and E37 §8.2.3's length field plus this repo's strict frame validation already bound the damage.
It is a deliberate trade, not free — record it as such if adopted.

### 5.2 SECS-II wrapping overhead, on top

**EVIDENCE.** E5 §9.2: "the actual number of bytes in the message for one item is the item length plus 2, 3, or 4 bytes for the item header."
This repo encodes the same way — `secs2/binary.go:128` calls `appendHeaderBytesFC(dst, BinaryFormatCode, len(item.values))`.

**INFERENCE.** The envelope must carry the original message's identity, not only its body — see [§6.1](#61-rank-1--secs-ii-binary-item-at-the-application-layer-task-option-c) for why.
The cheapest shape that does is `L,2 { <U4 uncompressedLen>, <B compressedBlob> }`,
where the blob is the compressed **10-byte HSMS header plus the original body**.
That costs roughly 2 (list header) + 6 (U4 item) + 2–4 (binary item header) ≈ **10–12 bytes**,
on top of the 10–14 bytes of zstd frame — a total fixed cost of roughly **20–26 bytes** per compressed message.

A `dictID` field is deliberately *not* in the envelope: it would duplicate zstd's own frame Dictionary_ID (§3.3), which the decoder already validates.
An s2-based codec would have to add one back, at +6 bytes.

The alternative — spelling out stream, function, W-bit and System Bytes as separate envelope items — costs roughly 10 more bytes and gains nothing,
because those 10 header bytes compress to near-nothing inside the blob once the dictionary has seen them.

**INFERENCE.** The 10 carried header bytes are *not* free of the on-wire header:
the envelope message needs its own HSMS header too, so the outer 10 bytes are paid regardless and the inner 10 are what compresses.
That is already counted above.

### 5.3 The threshold

**INFERENCE.** Below roughly 100 bytes of SECS-II body, compression cannot win: the fixed cost alone exceeds any plausible saving,
and the result is a strictly larger message plus two CPU round-trips.
Between roughly 100 bytes and 1 KB the answer is entirely a function of dictionary quality — with a good dictionary it can be a large win, without one it is usually a loss.
Above a few KB, ordinary within-message redundancy starts carrying its own weight and the dictionary matters less (which is what facebook/zstd means by "Dictionary gains are mostly effective in the first few KB").

The exact break-even is a measurement, not a derivation.
It belongs in [Measured results](#8-measured-results-repo-corpus).

**INFERENCE — implementation shape.** Whatever the number turns out to be, the codec must:

1. Skip compression entirely below a configured `MinCompressSize`.
2. Fall back to the uncompressed form whenever the compressed envelope is not smaller than the plain body — never ship a negative result.
   This must be an unconditional check,
   because `zstd/README.md` warns "If an unsuitable dictionary is used the output may be slightly larger than using no dictionary."
3. Pin `WithWindowSize` near the largest expected body rather than the 8 MB default, so pooled encoders do not each hold 8 MB of match tables for 500-byte messages.

**INFERENCE — `WithSingleSegment(true)` and a pinned `WithWindowSize` are coupled, and must be measured together.**
SingleSegment omits the Window_Descriptor (`zstd/frameenc.go:64-68`) and is worth a byte on the encode side,
but it shifts an obligation onto the decoder: `zstd/encoder_options.go:304-311` warns that with the flag set "the decoder must allocate a memory segment of size equal or larger than size of your content."
So SingleSegment moves decode-side allocation from window-bounded to content-bounded, partly cancelling the memory saving the pinned window was chosen for.
For SECS-II bodies both are small, so the trade is probably fine — but treat them as one knob with two settings, not two independent recommendations.

### 5.5 Latency: T3, not just bytes

**EVIDENCE.** Every reply-expected transaction sits inside the T3 reply timeout.
E37 §9.4.1.1: "The T3 reply timeout is a limit on the length of time that the HSMS message protocol is willing to wait for a Reply message."
§9.4.1.2: after sending a primary with W-Bit 1 the sender starts a T3 timer, and expiry closes the transaction as a T3 Timeout Error.

**INFERENCE.** Compression adds a compress on the sender and a decompress on the receiver to each leg, so a reply-expected transaction pays four codec operations inside one T3 budget.
T3 is conventionally seconds, so this is not close on any realistic body — but it is the budget the CPU cost is spent against, and it is what makes `SpeedBestCompression` (`zstd/encoder_options.go:181-183`) unattractive on a live link no matter how good the ratio.

**EVIDENCE — T8 is not the relevant timer.** E37 §9.2.3: T8 is the Network Intercharacter Timeout, defined against the receiver, for the gap between the parts of a *partially received* message.
Codec time happens after a complete frame has been read, so it does not run against T8.

### 5.4 Where the payoff actually lives

**EVIDENCE — the large-body messages.**

- **S7F3 Process Program Send.** E5 structure `L,2 { <PPID>, <PPBODY> }`; PPBODY's formats are "10, 20, 3(), 5()" (binary, ASCII, integer) and it "describes to the equipment, in its own language, the actions to be taken" (E5 data-item table, PPBODY). E5 §S7,F3 notes "If S7,F3 is multi-block, it must be preceded by the S7,F1/S7,F2 Inquire/Grant transaction" — multi-block is the expected case.
- **S7F23 Formatted Process Program Send.** `L,4 { <PPID>, <MDLN>, <SOFTREV>, L,c { L,2 { <CCODE>, L,p { <PPARM…> } } } }`, likewise multi-block by design.
- **S7F37 / S7F39 Large (Formatted) Process Program Send.** E5 §S7,F37 and §S7,F39 — named "Large" in the standard.
- **S6F11 Event Report Send.** `L,3 { <DATAID>, <CEID>, L,a { L,2 { <RPTIDi>, L,b { <V…> } } } }`, and again "If S6,F11 is Multi-block, it must be preceded by the S6,F5/S6,F6 Inquire/Grant transaction."
- **S1F4 Selected Equipment Status Data.** `L,n` of SV values, in a caller-fixed order.

**EVIDENCE — the small-body traffic where compression is pure loss.**
S1F1/S1F2 (Are You There / On Line Data), S7F4 (`<ACKC7>`, one byte), S6F12 (`<ACKC6>`, one byte), S2F41-class host commands.
Control messages are not even candidates: E37 §8.3.4.1, §8.3.18.1 and the Table 6 summary give Select.req, Linktest.req/rsp, Separate.req and Reject.req "Message Length is always 10 (Header only)" with Message Text "none".

**EVIDENCE — a hard reason not to compress small messages.** E37.1 §9.1:

> Multiblock Messages — For each SECS-II message, the SECS-II standard defines whether that message should be transmitted in SECS-I as a singleblock message or as a multiblock message.
> This distinction becomes unimportant with HSMS, which transmits all messages in the same fashion.
> However, to be compatible with older SECS-I applications, when an HSMS application sends a SECS-II message defined as single block, the HSMS Message Length should not exceed 254 bytes (10 byte header plus 244 text bytes).

**INFERENCE.** A message the standard defines as single-block has at most 244 text bytes,
so it is below any sane compression threshold *and* subject to a byte budget that a failed compression attempt could blow.
Single-block-defined messages should be excluded categorically, not merely by size.

**INFERENCE — ranking of the opportunity.**

1. **PPBody-bearing recipe traffic (S7F3 / S7F23 / S7F37 / S7F39).**
   Largest absolute payloads; often ASCII or vendor text.
   Highest raw byte savings per message, lowest message rate.
   A dictionary helps but matters least here.
2. **S1F4 / S6F1 trace and status polling.**
   Small per message, enormous in aggregate, and structurally identical every time.
   The dictionary case is strongest here and the per-message case is weakest — this is the band that only a dictionary can serve.
3. **S6F11 event reports.**
   Between the two: moderate size, highly repetitive skeleton, high rate during processing.
4. **Everything else.** Leave alone.

---

## 6 Negotiation / deployment options, ranked

The conformance verdict from §1 is applied to each.

### 6.1 Rank 1 — SECS-II binary item at the application layer (task option **c**)

**Conformance: clean.**

**EVIDENCE.** The message stays a PType-0 HSMS Data Message whose Message Text is well-formed SECS-II (E37 §8.3.3.7),
so nothing in E37 or E37.1 is touched.
And the message itself is sanctioned by E5 §7.3.1, which reserves for user definition:

> In Streams 1 to 63, Functions 64 to 255.
> In Streams 64 to 127, Functions 1 to 255.

E5 §7.3.3: "It is recognized that there will be user needs beyond the specific definitions given in this Standard. In these situations, the streams and functions reserved for user definition should be used…"

**INFERENCE — two sub-shapes, and they are not equivalent.**

- **c1 — envelope message in a user-defined stream/function.**
  A primary in S64–S127 carrying `L,2 { <U4 uncompressedLen>, <B compressedBlob> }`,
  where the blob decompresses to the original message's **10-byte HSMS header followed by its SECS-II body**,
  which the receiver then dispatches exactly as if that frame had arrived on the wire.
  Spec-clean at both layers.
  Transparent to application code above the codec.
  Requires both peers to implement the envelope,
  and requires the stream/function pair to be configurable, since E5 leaves the user range unallocated and two vendors will collide.

  **The inner header is not optional, and this is the correctness constraint that fixes the envelope's shape.**
  E37 §9.4.1 binds a reply to its primary by SessionID, Stream, Function (primary + 1, or 0), and System Bytes;
  §8.2.6.9 requires a Reply Data Message's System Bytes to equal the primary's.
  Those four fields live in the *outer* envelope's header, which is S64Fn with its own System Bytes — so they are lost unless the blob carries the original header.
  An envelope whose blob is the body alone cannot be dispatched, cannot be replied to, and cannot satisfy §9.4.1.

  **INFERENCE.** Carrying the inner header also settles the reply direction cleanly:
  the reply is its own envelope message, whose blob holds the *reply's* original header (with the primary's System Bytes, per §8.2.6.9), while the outer envelope pair does its own E37 §9.4.1 matching independently.
  Two transaction layers, each internally consistent.
  T3 accounting then applies to the outer transaction — see [§5.5](#55-latency-t3-not-just-bytes).
- **c2 — compressing one item in a standard message** (e.g. replacing PPBODY with a compressed blob).
  Structurally legal — PPBODY's declared formats include binary (E5 data-item table, PPBODY: "10, 20, 3(), 5()") — but it silently changes what the item *means* to a receiver that has not agreed.
  A conformant peer that does not know about it will hand compressed bytes to a recipe interpreter.
  Do not do this in a library.

**INFERENCE — the honest cost of c1.**
This is not transparent compression.
`ForwardDataMessage`-style verbatim relay, `Equal`, SML round-tripping, and any middlebox all see the envelope, not the original message.
The outer envelope's own W-bit must be set to match the inner message's, or a primary expecting a reply will not get one.
The T3 reply timer runs on the outer transaction, so the inner message's timeout semantics are the envelope's to preserve.
And the whole scheme is an out-of-band agreement between two applications — the wire says only "here is a user-defined message with a binary item in it".

### 6.2 Rank 2 — out-of-band identical config, unsignalled compressed body (task option **a**)

**Conformance: not conformant.**
**Ship only behind an explicit opt-in, off by default, documented as non-conformant.**

**EVIDENCE.** E37 §8.3.3.7: "The HSMS Message Text contains the text of the Data Message (if any), formatted as specified by the PType field.
For PType = 0, the text will be formatted as SECS-II messages."
A zstd frame is not a SECS-II message.
Setting PType 0 and putting non-SECS-II bytes behind it does not make it legal — it makes the header a lie about the body.

**EVIDENCE.** On an HSMS-SS link this is worse than merely irregular: E37.1 §7.7 classifies "any violation of the restrictions defined in prior sections of this document" as a communications failure,
and E37 §9.1.1 says the entity "should terminate the TCP/IP connection" on a communications failure.

**INFERENCE.** The thin sanction is E37 A1-10.1's intra-vendor carve-out, and it is thin in two ways:
it speaks of "user-defined extensions through new message types" (which this is not — it changes no message type),
and it declares any inter-vendor use noncompliant.
A library cannot police who its consumers connect to.

**INFERENCE.** If it ships at all, it must be gated behind an explicitly-named option, default off,
with godoc that states it is non-conformant with E37 §8.3.3.7 and E37.1 §8.2 and is intra-vendor only.
It has exactly one advantage over c1: it is transparent — every existing message type keeps its identity, and no envelope scheme has to be designed.
That advantage does not outweigh shipping a conformance violation as a supported feature.

### 6.3 Rank 3 — PType-signalled (task option **b**)

**Conformance: dead.**

**EVIDENCE.** E37.1 §8.2: "All HSMS-SS messages are PType 0 (SECS II encoded) as defined in HSMS."
E37 §9.3.3.1 reserves non-zero PType definition to subsidiary standards.
E37 A1-10.1 bars inter-vendor use of private extensions.
E37 §7.10.3 and Table 9 ReasonCode 2 require a conformant peer to answer with Reject.req — which this repo already does at `hsmsss/transport_control.go:157`.

**INFERENCE.** Implementing it means deleting the PType-0 check at `hsms/decode.go:105-107` and the Reject path,
both of which are audited conformance properties (`docs/specs/e37-1-hsms-ss-conformance-audit.md`).
Not a trade worth making for a compression feature.
Were a SEMI subsidiary standard for compressed HSMS ever balloted, this would become rank 1 overnight — it is the architecturally correct answer, just not one an implementation gets to make.

### 6.4 Dependency placement

**EVIDENCE.** `go.mod` declares `module github.com/arloliu/go-secs/v2`, `go 1.26.0`,
with three direct dependencies: `phsym/console-slog`, `puzpuzpuz/xsync/v3`, `stretchr/testify`.
`klauspost/compress` is a large module (its `s2` package alone carries a 525 KB amd64 assembly file, `s2/encodeblock_amd64.s`).

**INFERENCE.** `go-secs` is consumed as a dependency, so any import it adds is imposed on every consumer's build and module graph, compressing or not.
Three shapes, in preference order:

1. **Caller-supplied codec.**
   Core defines a two-method interface (`Compress(dst, src []byte) []byte`, `Decompress(dst, src []byte) ([]byte, error)`) and a size threshold;
   the consumer wires in whatever they like.
   Zero new dependencies in core.
   Least convenient, most honest.
2. **Separate module.**
   `github.com/arloliu/go-secs/codec/zstd` with its own `go.mod`, depending on `klauspost/compress`, implementing the core interface.
   Consumers who want it opt in by importing it.
   Costs a second release cadence.
3. **Direct dependency in core.**
   Simplest to use, and imposes a large module on everyone.
   Rejected.

**INFERENCE.** Shape 1 or 2 also keeps the *dictionary* out of the library.
The dictionary is deployment data, and belongs to the deployment.

---

## 7 Recommendation

**INFERENCE**, on the evidence above.

1. **Algorithm: zstd**, via `github.com/klauspost/compress/zstd`, one-shot `EncodeAll` / `DecodeAll`.
   It is the only pure-Go candidate whose *block* API accepts a dictionary on the encode side, and the only one that records which dictionary was used.
2. **Dictionary: required, not optional.** Without one, SECS-II bodies in the 200 B – 4 KB band are close to incompressible and the feature is not worth its complexity.
   Start with `WithEncoderDictRaw` over concatenated representative bodies (zero tooling), measure, then move to a `zstd --train --train-fastcover` dictionary loaded via `WithEncoderDict` / `WithDecoderDicts` if the gap justifies it.
   `zstd.BuildDict` is the pure-Go middle option and needs the history content chosen by the caller.
3. **Level: `SpeedDefault` as the starting point** (`zstd/README.md`: roughly zstd level 3), with `SpeedFastest` (roughly level 1) as the comparison.
   `SpeedBetterCompression` costs "~ 2x-3x the default CPU usage" per `zstd/encoder_options.go:176-178`, which is hard to justify on a latency-sensitive equipment link.
4. **Deployment: application layer, SECS-II binary item, user-defined stream/function** (§6.1, sub-shape c1).
   The only path that is conformant with E37, E37.1, and E5 simultaneously.
5. **Threshold: skip below a configured minimum; never ship a larger result; exclude single-block-defined messages categorically** (§5.3, §5.4, E37.1 §9.1).
6. **Hardening: `WithDecoderMaxMemory` capped at `secs2.MaxByteSize` or lower**, plus a pre-decompression check of the declared uncompressed length (§4.3).
7. **Placement: caller-supplied codec interface in core, or a separate module** — not a new direct dependency of `go-secs` (§6.4).

**Runner-up.** `klauspost/compress/s2` with a dictionary, if measurement shows zstd's CPU cost is the binding constraint.
Blocked today by the absence of a dictionary identifier in the output (`s2/README.md`: "using the wrong dictionary will most often not result in an error when decompressing")
and by the author's own "early implementation stage" caveat.
Adopting it would require carrying a dictionary ID in the SECS-II envelope and validating it before decode — recoverable, but it is work zstd does for free.

**Rejected.** `pierrec/lz4/v4` — no encode-side dictionary, and `Compressor` is not concurrency-safe.
`golang/snappy` — no dictionary mechanism in the format, and no release since 2023-12-25.

---

## 8 Measured results (repo corpus)

All figures in this section are **EVIDENCE** — measured on this machine, roundtrip-asserted per codec/sample pair.
Anything not measured is marked as an open gap in [§8.7](#87-what-these-measurements-do-not-settle) rather than inferred.

**Environment.**
`klauspost/compress v1.19.2`, `pierrec/lz4/v4 v4.1.28`, Go 1.24, single-core (`-cpu 1`);
dictionaries trained with the reference `zstd` CLI v1.5.5 (COVER).

**Reproducing these numbers.**
The corpus generator, harness and raw output live in `benchmarks/compression/`
(inside the separate `benchmarks` module, so the codec dependencies never reach consumers of the core module):

```
cd benchmarks
make bench-compression     # ~60s; writes results/compression_{tables,bench}.txt
```

Or directly, for a single table:

```
go test ./compression/ -run 'TestRatios|TestHoldout|TestItemLevel' -v   # ratio tables
go test ./compression/ -run XXX -bench BenchmarkDictCost -cpu 1         # dictionary cost
```

The dictionary cases shell out to the reference `zstd` CLI to train a COVER dictionary.
Without it on `PATH` they degrade to the no-dictionary codecs —
the two `+dict` columns drop out and the run still passes,
so a green run is not by itself evidence that the dictionary figures were reproduced.
`benchmarks/compression/RESULTS.txt` holds the raw output the tables below were built from.
The gaps listed in [§8.7](#87-what-these-measurements-do-not-settle) are closable by extending this harness.

### 8.1 Corpus caveat — read before quoting any number

The corpus is **modeled, not captured from a real link**.
Shapes were written to match real SECS-II traffic
(seeded pseudo-random sensor values, realistic SVID naming, clustered wafer-map defects, `key=value` step-structured recipe bodies),
specifically because this repo's existing benchmark shapes are pathologically compressible —
`benchmarks/hsmsssdata/v2/shapes_test.go` fills a 1 MiB recipe with `byte('A'+i%26)` and a wafer map with `byte(i%4)` —
and would drive every ratio toward zero.

The recipe figures assume PPBody is structured text.
Real PPBody is frequently opaque vendor binary and will compress substantially worse.
**Treat the recipe rows as an upper bound, not a promise.**

One deliberate difference from §5.2:
these numbers compress the **body alone**, not body + the inner 10-byte HSMS header.
The header adds 10 highly-repetitive bytes that a dictionary absorbs almost entirely,
so the real envelope is marginally better than shown — the gap is well under the rounding in these tables.

### 8.2 Compressed size, by message shape

Percentages are compressed size as a fraction of raw body — lower is better;
above 100% means the codec *inflated* the message.

| Sample | Raw B | zstd-fastest | zstd-default | s2 | lz4-block | zstd-fastest+dict | zstd-default+dict |
|---|---:|---:|---:|---:|---:|---:|---:|
| S1F2_online | 21 | 161.9% | 161.9% | 109.5% | 109.5% | 181.0% | 181.0% |
| S2F41_hostcmd | 75 | 117.3% | 117.3% | 104.0% | 102.7% | **45.3%** | **45.3%** |
| S1F4_svid_20 | 146 | 108.9% | 108.9% | 102.7% | 101.4% | 94.5% | 89.0% |
| S6F11_event_10 | 82 | 115.9% | 115.9% | 103.7% | 102.4% | **79.3%** | **79.3%** |
| S6F11_event_100 | 592 | 80.1% | 80.1% | 83.3% | 89.4% | 68.2% | 71.1% |
| S1F4_svid_200 | 1 434 | 67.6% | 60.7% | 72.4% | 81.7% | 65.5% | **57.4%** |
| S6F11_trace_1k (F8) | 8 017 | 100.2% | 100.2% | 100.1% | 100.4% | 100.2% | 100.2% |
| S6F11_trace_10k (F8) | 80 018 | 100.0% | 100.0% | 100.0% | 100.4% | 100.0% | 100.0% |
| S6F11_traceF4_10k | 40 017 | 100.0% | 100.0% | 100.0% | 100.4% | 100.0% | 100.0% |
| S6F11_traceQ3_10k | 80 018 | 31.0% | 31.4% | 42.1% | 42.4% | 31.1% | 31.4% |
| S6F11_traceQ1_10k | 80 018 | 8.1% | **7.8%** | 16.1% | 20.5% | 8.1% | **7.8%** |
| S6F11_traceU2_10k | 20 017 | 100.1% | 56.6% | 100.0% | 94.1% | 100.1% | 56.6% |
| S7F3_recipe_20step | 2 350 | 26.8% | 27.5% | 46.1% | 44.9% | **26.5%** | 26.9% |
| S7F3_recipe_500step | 56 916 | **20.2%** | 20.5% | 32.5% | 30.3% | 20.1% | 20.5% |
| WaferMap_100k | 100 012 | 2.4% | **2.3%** | 3.8% | 4.0% | 2.4% | 2.3% |

### 8.3 Answers to the eight open questions

**Q1 — Break-even size.**
Two distinct break-evens, and the second is the one that matters.
On *ratio alone*, every dictionary-less codec inflates any body below roughly 500 bytes.
Once the ~20–26 byte envelope overhead from §5.2 is charged,
dictionary-zstd needs to save more than ~26 bytes to pay for itself:

| Sample | Raw B | +dict B | Bytes saved | Net after ~26 B envelope |
|---|---:|---:|---:|---:|
| S6F11_event_10 | 82 | 65 | 17 | **−9 (loss)** |
| S1F4_svid_20 | 146 | 130 | 16 | **−10 (loss)** |
| S2F41_hostcmd | 75 | 34 | 41 | +15 (marginal) |
| S6F11_event_100 | 592 | 404 | 188 | **+162** |
| S1F4_svid_200 | 1 434 | 823 | 611 | **+585** |

So the practical threshold for structured messages is **~300–600 bytes**, not the ~82–146 B at which the raw ratio first dips below 100%.
Below it, the envelope costs more than the codec saves.

**Q2 — Dictionary lift.** Large, and it is the single most important variable.
Measured over 200 **holdout** messages whose seeds are disjoint from the training set:

| Codec | Total B | % of raw |
|---|---:|---:|
| raw | 36 531 | 100.0% |
| zstd-fastest | 33 822 | 92.6% |
| zstd-default | 33 563 | 91.9% |
| s2 | 33 407 | 91.4% |
| lz4-block | 34 317 | 93.9% |
| **zstd-fastest+dict** | **27 228** | **74.5%** |
| zstd-default+dict | 27 519 | 75.3% |

The dictionary is the difference between ~8% saved and ~25% saved.
This confirms §3's central claim on this repo's own data.

⚠️ **Methodology warning worth carrying forward.**
An earlier run of this harness reported a far better figure (S2F41 at 34.7%) because two message builders were unseeded,
so their exact bytes appeared 300× in the dictionary training set *and* in the measurement set.
Seeding them so only LOTID/PPID vary moved it to the 45.3% above.
Dictionary numbers measured on training data are meaningless — and the error is easy to make silently.

Note also that the dictionary makes the 21-byte S1F2 *worse* (161.9% → 181.0%):
the frame carries a Dictionary_ID reference that a tiny body cannot amortize.

**Q3 — `BuildDict` vs `zstd --train`.** **Not measured** — see [§8.7](#87-what-these-measurements-do-not-settle).
What is confirmed is the mechanism §3 describes:
`zstd.BuildDict` errors with `dictionary of size 0 < 8` when `History` is empty
(`zstd/dict.go:192-218`), so it fits entropy tables over caller-chosen content and does not perform COVER selection.
All dictionary figures above therefore come from the reference CLI.

**Q4 — Level curve.** `SpeedFastest` vs `SpeedDefault` is a wash on the shapes that matter, so take the cheaper one.
Default wins by 0.1 pp on the wafer map and 1.8 pp on dictionary-less S1F4_svid_200; fastest wins on the recipe.
Encode cost differs materially: 105 vs 183 ns on a 75 B body, 6.0 vs 6.6 µs on a 2.4 KB recipe.
`SpeedBetterCompression` was not measured.

**Q5 — Allocations.**
**0 B/op, 0 allocs/op** for both `EncodeAll` and `DecodeAll`, across every shape, with a warm encoder and reused `dst`.
The codec adds no allocation pressure to the existing `ToBytes` path.

**Q6 — zstd vs s2-with-dictionary at matched dictionaries.**
**Not measured** — see [§8.7](#87-what-these-measurements-do-not-settle).
s2 above runs dictionary-less.
The §2 finding that s2 records no dictionary ID is unaffected by this gap and remains the deciding argument against it.

**Q7 — Window size and `SingleSegment`.** **Not measured** — see [§8.7](#87-what-these-measurements-do-not-settle).

**Q8 — Codec latency against T3.** Not a constraint, by three orders of magnitude.
Worst measured round trip is the 100 KB wafer map at 33.5 µs encode + 23.6 µs decode ≈ **57 µs**.
Against a typical T3 of 45 s that is ~0.00013% of the reply budget.
Even the flat ~4.3 µs dictionary charge on the smallest messages is irrelevant at T3 scale;
it matters only as aggregate CPU, addressed in §8.5.

### 8.4 Three findings the research pass did not predict

**1. Raw IEEE-754 trace data is incompressible — the codec is not the lever, the *encoding* is.**
An F8 sensor random-walk sits at 100.0–100.2% for *every* codec, dictionary or not; F4 is identical at 100.0%.
Mantissa low bits are effectively white noise.
Quantizing the same trace to 3 decimals drops it to 31.0%, to 1 decimal **7.8%**;
re-encoding as U2 counts gives 56.6%.
Any expectation that high-rate trace bursts are the big win is **wrong** unless the values are quantized first —
and quantization is a decision for the application that builds the item, not for this library.

**2. The dictionary's cost is a flat per-call charge, independent of dictionary size.**
Encode cost for a fixed 146-byte payload, varying only the dictionary:

| Dictionary | none | 1 KiB | 4 KiB | 8 KiB | 32 KiB | 110 KiB |
|---|---:|---:|---:|---:|---:|---:|
| ns/op | 144 | 4 356 | 4 941 | 4 373 | 4 437 | 4 937 |

Flat in dictionary size — so it is fixed per-`EncodeAll` setup, not dictionary scanning.
**Choosing a smaller dictionary to save CPU buys nothing;** size it purely for ratio.

Critically, the 144 ns no-dictionary figure is **not** fast compression —
it is zstd detecting incompressibility and storing a raw block.
Real compression work on a small body costs ~4–5 µs whether or not a dictionary is present.

**3. Decode is cheap, and the dictionary penalty is asymmetric.**
S2F41 decodes in 67.6 ns plain / 140.0 ns with dictionary; the 100 KB wafer map in 23.6 µs / 29.1 µs.
The dictionary costs ~30× on encode but only ~2× on decode.
For a host decoding from many tools this asymmetry favours the dictionary far more than the encode figures alone suggest.
(lz4 at 6.5 ns and s2 at 9.4 ns decode small blocks an order of magnitude faster than zstd,
but neither can encode with a dictionary — §2.)

### 8.5 CPU per byte saved — the metric that sets the threshold

| Shape | Bytes saved | Encode ns | ns per byte saved |
|---|---:|---:|---:|
| WaferMap_100k (zstd-fastest) | 97 606 | 33 534 | **0.34** |
| S7F3_recipe_20step (zstd-fastest) | 1 721 | 6 011 | **3.5** |
| Holdout small msgs (zstd-fastest+dict) | 46.5 /msg | ~4 300 | **~92** |

A ~270× spread.
This is the quantitative argument for gating by size and shape rather than compressing every message.
In absolute terms the dictionary charge is still modest — at 1 000 msg/s it is ~0.4% of one core —
so it is a tradeoff to expose, not a reason to avoid dictionaries.

### 8.6 The application-layer path costs nothing — measured

§6 concludes that the only conformant path is a compressed blob inside a user-defined stream/function.
Measured directly: compress only the inner payload, then re-wrap as `L{A(ppid), B(compressed)}`:

| Sample | Raw B | Whole-body | Item-level | Delta |
|---|---:|---:|---:|---:|
| S7F3_recipe_500step | 56 916 | 11 487 (20.2%) | 11 470 (20.2%) | **−17 B** |
| WaferMap_100k | 100 012 | 2 406 (2.4%) | 2 405 (2.4%) | **−1 B** |

Item-level is not merely close — it is fractionally *smaller*,
because the surviving SECS-II framing is a few bytes the whole-body pass also had to encode.
For these shapes the payload already *is* a single `A`/`B` item,
so there is no cross-item redundancy a whole-body pass could exploit that item-level compression misses.

**The spec constraint and the measurements point the same way**, which is the strongest result in this document:
the conformant path is also the one that gives up nothing on the shapes worth compressing.

The converse bounds the approach.
For small structured messages the redundancy lives in the item *framing* —
repeated format bytes, length prefixes, SVID names —
which item-level compression cannot reach at all.
That is exactly the traffic where only a trained dictionary helps,
and exactly where wire-level compression is forbidden.

### 8.7 What these measurements do not settle

Stated explicitly so no reader mistakes silence for a result:

- **Q3** — heuristic pure-Go `History` selection vs COVER-trained dictionaries.
  Unmeasured; affects whether a consumer can skip the offline `zstd --train` step.
- **Q6** — s2 with a matched dictionary.
  Unmeasured; §2's "no dictionary ID in the output" finding is the independent reason s2 loses,
  so this gap does not change the recommendation.
- **Q7** — `WithWindowSize` / `WithSingleSegment` ratio and memory tradeoffs.
  Entirely unmeasured.
- **`SpeedBetterCompression`** — not benchmarked at any level.
- **Concurrent throughput** — all figures are single-core (`-cpu 1`).
  `WithEncoderConcurrency` bounds parallel `EncodeAll` calls and was not varied.
- **Real captured traffic** — the corpus is modeled (§8.1). The recipe ratio is the number most likely to be optimistic.

### 8.8 Library mechanics confirmed from upstream source

- `Encoder.EncodeAll` is documented "can be called concurrently" and `Decoder.DecodeAll` "can be used concurrently"
  (`zstd/encoder.go:716-721`, `zstd/decoder.go:314-318`, klauspost/compress v1.19.2).
  A single shared encoder/decoder pair is safe; no `sync.Pool` is required,
  though `WithEncoderConcurrency` bounds how many calls proceed in parallel.
- Both append into a caller-supplied `dst`, measured at 0 allocs/op when `dst` is reused —
  they fit the repo's existing buffer-reuse discipline.

---

## Source index

**SEMI standards** (local markdown, `~/semi_standards/markdowns/`)

| Clause | Subject |
|---|---|
| E37-0413 §7.10.3 | Reject required for undefined SType or PType |
| E37-0413 §8.2.2, §8.2.3 | Frame layout; Message Length semantics |
| E37-0413 §8.2.6.4, Table 4 | PType definition and value allocation |
| E37-0413 §8.2.6.5–6, Table 5 | SType definition and value allocation |
| E37-0413 §8.2.6.9 | Reply Data Message System Bytes must equal the primary's |
| E37-0413 §8.3.3.7 | PType 0 Message Text "formatted as SECS-II messages" |
| E37-0413 §9.2.3 | T8 applies to partial-frame receipt, not codec time |
| E37-0413 §9.4.1, §9.4.1.1–9.4.1.2 | Reply matching fields; T3 reply timeout |
| E37-0413 §8.3.21.2, Table 9 | Reject.req ReasonCode 2, PType Not Supported |
| E37-0413 §9.1.1 | Communications failure ⇒ terminate the connection |
| E37-0413 §9.3.3, §9.3.3.1 | Only subsidiary standards may define non-zero PType |
| E37-0413 A1-10.1, A1-10.2 | User-defined extensions: intra-vendor only |
| E37.1-0702 §5.1 | Profile's purpose is to limit generic HSMS |
| E37.1-0702 §7.7 | Any profile violation is a communications failure |
| E37.1-0702 §8.2 | All HSMS-SS messages are PType 0 |
| E37.1-0702 §8.3 | Only HSMS-defined STypes permitted |
| E37.1-0702 §9.1 | Single-block messages: ≤ 254 bytes for SECS-I compatibility |
| E5-0813 §7.3, §7.3.1, §7.3.3 | Stream/function allocation; user-defined ranges |
| E5-0813 §9.2, §9.2.2 | Item header: format byte + 1–3 length bytes |
| E5-0813 S1F4, S6F11, S7F3, S7F23, S7F37, S7F39 | Message structures cited in §5.4 |

**Upstream libraries** (module cache and `proxy.golang.org`)

| Artifact | Used for |
|---|---|
| `klauspost/compress@v1.19.0 zstd/encoder.go:716-729, :730-830` | `EncodeAll` contract, encoder pool, frame-header construction |
| `klauspost/compress@v1.19.0 zstd/decoder.go:70-84, :314-400, :942-956` | `DecodeAll` contract and body, pre-decode caps, `setDict` / `ErrUnknownDictionary` |
| `klauspost/compress@v1.19.0 zstd/encoder_options.go:36-47, :82-103, :105-111, :168-187, :304-320, :373-398` | Defaults, concurrency, window, levels, dictionaries |
| `klauspost/compress@v1.19.0 zstd/decoder_options.go:30-42, :57-101, :103-112, :144-168` | Decoder defaults, `WithDecoderMaxMemory`, `WithDecoderDicts`, `WithDecodeAllCapLimit` |
| `klauspost/compress@v1.19.0 zstd/dict.go:152-163, :165-230` | `InspectDictionary`, `BuildDict` / `BuildDictOptions` |
| `klauspost/compress@v1.19.0 zstd/frameenc.go:15-88`, `zstd/framedec.go:28-35, :158-186`, `zstd/blockenc.go:134-136` | Frame/block overhead; Dictionary_ID round trip |
| `klauspost/compress@v1.19.0 zstd/zstd.go:71-83` | `ErrDecoderSizeExceeded`, `ErrUnknownDictionary` |
| `klauspost/compress@v1.19.0 zstd/simple_go124.go` | Why the convenience wrappers are unsuitable |
| `klauspost/compress@v1.19.0 zstd/README.md` | Status, block-API guidance, dictionary caveats |
| `klauspost/compress@v1.19.0 s2/dict.go:13-128, :142-268`, `s2/README.md` | s2 dictionary API, bounds, published measurements, caveats |
| `klauspost/compress@v1.19.0 README.md`, `go.mod` | Support policy, Go version, retractions |
| `pierrec/lz4/v4@v4.1.28 lz4.go:26-129`, `options.go`, `go.mod` | Block API, absent encode dictionary, concurrency, Go version |
| `golang/snappy@v1.0.0 snappy.go:1-44`, `encode.go:13-78`, `decode.go:50-57`, `go.mod` | Block API, format has no dictionary |
| `facebook/zstd` README, "The case for Small Data compression" | Upstream rationale and training command |
| `proxy.golang.org/…/@v/*.info` | Release dates for v1.19.2, v4.1.28, snappy v1.0.0 |

**This repo**

| Location | Used for |
|---|---|
| `hsms/decode.go:11-22, :97-120` | Frame cap; PType-0 enforcement on the inbound path |
| `hsms/data_msg.go:77-94, :353-359` | Single-allocation frame build; header validation |
| `hsms/errors.go:14-17` | `ErrInvalidPType` contract |
| `hsms/control_msg.go:167-168, :304-342` | `RejectPTypeNotSupported` |
| `hsmsss/transport_control.go:124-193`, `hsmsss/transport_recv.go:101-111` | Reject-on-bad-PType path |
| `secs2/item.go:11` | `MaxByteSize = 1<<24 - 1` |
| `secs2/binary.go:119-138` | Binary item wire encoding |
| `go.mod` | Current dependency surface, Go 1.26.0 |
| `docs/specs/e37-1-hsms-ss-conformance-audit.md` | Existing E37.1 conformance posture |
| `.knowledges/hsmsss/e37-1-narrows-e37-generic.md` | Where E37.1 narrows E37 in this codebase |

// Package tracepack is the Go reference implementation of the tracepack container format (.tpk):
// an immutable, compressed, self-indexed archive of recorded SECS-II traffic between one tool and its host,
// including HSMS control messages and transport lifecycle events.
//
// The format is specified in docs/specs/tracepack/ of the go-secs repository.
// The specification, not this package, defines every field;
// the conformance corpus is the executable arbiter where the two disagree.
// This module is versioned independently of go-secs, with tags of the form tracepack/vX.Y.Z.
//
// # Concepts
//
// A capture is one run of one recorder over one tool, named by its capture_id; its records are numbered by seq.
// A pack is one immutable file of records with its pack metadata and a footer that indexes them.
// A scope is one capture in one UTC hour, and every pack but an extract holds the records of one scope.
// A recorder stages segments, the packs of one flush interval;
// a merge folds the packs of a scope its caller gives into one archive (the tracepack storage specification §2 and §4).
// Format 1.1, a pack's format_minor 1, adds packs masked at write under a redaction policy
// that encrypts the originals of data records (the tracepack semantics specification §8),
// and data records unobserved by design, of the streams and functions the pack's thinning entries declare;
// a Writer writes minor 0 for a pack holding neither, as for every pack before format 1.1.
// A pack of minor 1 that holds no such feature is still read as minor 1, and a merge of it writes minor 1
// (the tracepack format specification §14).
// Format 1.2, minor 2, adds recorder_instance_name, the human-readable name of the deployment recorder_instance_id identifies;
// a Writer writes minor 2 exactly when the pack metadata carries it.
//
// # Recording
//
// A CaptureDescriptor holds the pack metadata the producer of a capture owns.
// SegmentWriter writes one capture as segments through a SegmentSink,
// owning the capture's start and stop boundaries, its seq counter and the segments' rolls;
// NewDirSink stores the segments as files laid out as a bucket's keys.
// A Classifier, such as the one tracepack/classify returns, sets the decode_status of each frame AppendFrame takes.
// The SegmentWriter Godoc states which errors refuse a record and leave the writer usable, and which fail it;
// an error wrapping ErrPublishUncertain reports a segment that is published but may not survive a system crash.
// NewWriter writes one pack from pack metadata the caller assembles, the layer under SegmentWriter and Merge;
// it and Merge write block bodies and the footer with zstd unless their options set Uncompressed,
// at the options' CompressionLevel, CompressionDefault unless set.
// A Writer writes format_minor 2 when the pack metadata holds a feature of format 1.2,
// else 1 when it holds a feature of format 1.1, else 0.
// Writer.Close returns a Continuation, whose Next gives the seq the capture's next pack starts at,
// and reports no next pack after a pack whose last record has seq 2^63-1, the last seq of a capture.
//
// # Durable-bus capture
//
// A producer that publishes each record of a capture as one bus message (the tracepack storage specification §4)
// encodes the capture's descriptor with EncodeDescriptor, for the start message's descriptor header,
// and each record with the bus codec of the subpackage tracepack/bus,
// which also spells the subject and header values and preflights a message's size.
// The module imports no NATS client.
//
// The log service, the consumer that writes the capture's packs, checks each message with the same subpackage,
// which gives the producer's values of a start through DecodeDescriptor
// and derives the values the consumer sets from the payload as received,
// classified by a Classifier such as classify.NewNamed returns, before any masking.
// Under a redaction policy the package tracepack/redact masks the records of a flush batch and computes their entries;
// the consumer collects them into PackMeta.Redaction and only then constructs the Writer,
// which writes the pack metadata first.
// A pack masked at write, one that is not an extract and carries redaction_policy,
// carries both encryption tags, exactly one classifier and at most one max_frame_len, and is written with given seqs;
// NewWriter, Writer.Append and Writer.Close check the Marking of every record against its entries.
// A record carrying QualityUnobserved needs the thinning entry of its stream and function.
// WriteClose writes the close of a capture its producer left open, one stop-unclean record at seq 2^63-1.
// This package never imports tracepack/redact, so a program that only reads packs links no masking or encryption code.
//
// # Reading
//
// Open bootstraps a Reader over one pack, and Reader.Iterate reads its records.
// A Reader keeps the bytes of Open's first round that hold blocks and serves block reads from them,
// so a pack up to ReaderOptions.WholeReadThreshold, 4 MiB by default, currently costs one ReadAt for Open and none afterwards;
// it keeps at most max(WholeReadThreshold, HeadWindow + TailWindow) bytes,
// outside the budgets of MergeIterateOptions.MaxHeldBytes, TxOptions.MaxHeldBytes and TxOptions.MaxStateBytes.
// MergeIterate is one read over several packs, typically the packs of the scopes a query touches
// (the tracepack semantics specification §7.4):
// it compares the copies of each record across the packs, yields identical copies once,
// and yields the records in capture order or time order, as MergeIterateOptions.Order selects.
// Records of one seq with different bytes are reported, not hidden:
// every version the query selects is yielded, marked as a conflict,
// and the conflict is listed in Result.Conflicts beside the defects in Result.Incomplete.
// MergeIterate reads ahead of the block it resolves, within MaxHeldBytes,
// in time order, and in capture order when at most one capture has a cluster to read,
// so it may issue several ReadAt calls at once on a Reader's io.ReaderAt, as Open does,
// and it waits for all of them before it returns.
// Every read reports, as an IncompleteReasonCrashGap defect, each stop-unclean capture boundary whose gap meets the query's time range
// (the tracepack semantics specification §7.4, Crash gaps).
//
// Readers decode the pack metadata under the pack's format_minor, as UnmarshalPackMetaMinor does,
// so a number of format 1.1 keeps its format 1.0 status in a pack of minor 0;
// Reader.Unobserved says whether a record's bit 7 means unobserved in its pack.
// A record that carries quality.redacted or that a redaction entry names has its Marking in Item.Redaction,
// and PackHeader.RedactionDefects lists the defects of the entries found at bootstrap
// (the tracepack semantics specification §8);
// a defect never fails Open or a read and never makes it incomplete, and the records it concerns read as wholly masked.
//
// FindTransaction looks up the reply to a primary record (the tracepack semantics specification §7.2)
// over the packs a PackSource gives for each hour it reads, of any capture.
// It reports TxMatched, TxAmbiguous or TxNotFound,
// with the records it kept and every reason the lookup is incomplete, each a TxGap in TxResult.Incomplete, beside any outcome;
// TxNotFound states nothing about records outside the packs given.
// FindTransactions pairs every primary of one capture in a time range the same way, reading each hour once,
// and passes each primary's result, equal to FindTransaction's for it, to a callback in ascending hour, then seq.
// NewReaderSource returns a PackSource over packs the caller opened, all of one tool, such as the files of a NewDirSink,
// grouped by the UTC hour of their period;
// the caller owns the readers and keeps them open while the source is used.
//
// ExportJSONL writes the canonical JSONL export of one pack (the tracepack JSONL specification):
// a header line, then one line per record, the same bytes any conforming exporter writes for the pack.
// A pack of format minor 0 exports schema tracepack-jsonl/1, as earlier versions wrote it,
// a pack of minor 1 tracepack-jsonl/2, which names the additions of format 1.1,
// and any other pack tracepack-jsonl/3, which also names recorder_instance_name.
// A damaged pack exports the records of its validated blocks, with the Result Iterate returns for it:
// a failed block or a crash gap makes it incomplete, while a footer that is not used only adds an entry to FooterErrs.
// A reader budget that keeps part of the pack out fails the export with an error wrapping ErrReadLimit instead.
// A line may be written in several pieces, currently of at most 64 KiB each, so one line may take several Write calls.
// Verify reads every block of a pack and reports its outcome, from finalized-consistent to corrupt.
//
// # Merging and storage
//
// Merge writes one archive of a scope from the segments and archives its caller gives,
// with the scope_generation and publisher_epoch the caller supplies;
// it writes the archive's lineage, compacted_from and supersedes, from the inputs.
// It carries the masks and thinning of format 1.1 packs and the recorder_instance_name of format 1.2 packs,
// and writes the largest format_minor of its inputs;
// it refuses inputs carrying redaction_policy that differ in that policy or in their classifier configuration,
// or a masked input beside an unmasked one,
// while inputs without a policy merge with their classifier and max_frame_len values united, as before;
// differing redaction entries for one seq are conflicts (the tracepack storage specification §4 Merge).
// Packs of format 1.2 are merged only by tracepack v0.8.0 or later, which an earlier merger enforces by refusing them.
// The caller decides which packs a merge takes and when a pack may be deleted,
// and publishes the archive only after Merge returns nil (the tracepack storage specification §4).
// SegmentKey and ArchiveKey build the keys of the tracepack storage specification §3, and EscapeToolID spells a tool as a key does.
//
// # Compatibility
//
// From tracepack v1.0.0 on, every v1 release keeps what this documentation states as contract:
//   - the exported identifiers and their signatures;
//   - which sentinel error an error wraps, ErrRefused included;
//   - the outcomes, orders and determinism the documentation states, and its byte-identity promises:
//     the close WriteClose writes, the export ExportJSONL writes, and the output of Mask in tracepack/redact;
//   - reading every pack of format 1.x;
//   - the JSONL schema values tracepack-jsonl/1 to tracepack-jsonl/3, each naming fixed bytes:
//     any change to the bytes of an export takes a new schema value;
//   - the flags and exit codes of the tracepack command, its split of output between stdout and stderr, and the bytes of its dump --jsonl.
//
// These are not guaranteed, and may change in any release:
//   - the text of error messages;
//   - how often a call checks ctx, how far a read reads ahead, how many goroutines a call runs and how many ReadAt calls it issues,
//     the size of the pieces a write is made of, and how much larger than needed a reused buffer may be;
//     where this documentation gives such a number, it says what the implementation currently does;
//   - the default values of the budgets, which may be tuned;
//   - the bytes zstd compresses to, across versions of this module and of the zstd library it uses;
//   - the text of the verify and find reports of the tracepack command.
//
// Every enum may gain values in a later format minor, so a switch over one needs a default case.
//
// A pack's redaction entries are part of its pack metadata,
// which a Reader opens only within ReaderOptions.MaxPackMetadataLen, DefaultMaxPackMetadataLen (64 MiB) by default.
// So 64 MiB is the default interoperability budget, since every reader of a deployment's packs must agree on the budget;
// Merge refuses an archive whose pack metadata would exceed the budget of MergeOptions.Reader, with ErrMergeLimit.
// The limit is in bytes, since an entry's size varies with its item path, domain and ranges:
// 64 MiB holds about 450,000 entries of 147 bytes, as an example.
package tracepack

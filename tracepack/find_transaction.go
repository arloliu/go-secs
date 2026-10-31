package tracepack

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// Default TxOptions values, which a zero field takes.
const (
	// DefaultTxMaxHours is the default TxOptions.MaxHours: the primary's hour and the next.
	DefaultTxMaxHours = 2
	// DefaultTxMaxStateBytes is the default TxOptions.MaxStateBytes: 64 MiB.
	DefaultTxMaxStateBytes = 64 << 20
)

// The range of TxKey.Hour: the UTC hours h for which h·3600·10⁹ and (h + 1)·3600·10⁹ nanoseconds both fit an int64,
// so that the time range of every hour a lookup reads can be stated in nanoseconds.
const (
	// MinTxHour is the smallest valid TxKey.Hour, -2562047.
	MinTxHour int64 = math.MinInt64 / hourNs
	// MaxTxHour is the largest valid TxKey.Hour, 2562046.
	MaxTxHour int64 = math.MaxInt64/hourNs - 1
)

// Outcomes of a transaction lookup (the tracepack semantics specification §7.2).
const (
	// TxMatched means exactly one valid match was found.
	TxMatched TxOutcome = iota + 1
	// TxAmbiguous means several valid matches were found.
	TxAmbiguous
	// TxNotFound means no valid match was found among the records the lookup read;
	// it states nothing about records outside the packs it was given.
	TxNotFound
)

// Roles of a version kept by a transaction lookup (the tracepack semantics specification §7.2), one bit each.
// A version holds the bit of every role it plays:
// a data record in unknown direction can be both a possible reply and a possible same-key primary.
const (
	// TxPrimary marks a version of the primary.
	TxPrimary TxClass = 1 << iota
	// TxCandidate marks a candidate: a data record above the primary in its capture and epoch,
	// in the opposite direction, with SessionID and System Bytes available and equal to the primary's.
	TxCandidate
	// TxPossibleReply marks a possible reply: a data record that is not a candidate but could be one,
	// because its direction is unknown or a SessionID or System Bytes it would need is unavailable.
	TxPossibleReply
	// TxSameKeyPrimary marks a same-key primary: a data record above the primary in its capture, epoch and direction,
	// with an available odd function and SessionID and System Bytes available and equal to the primary's.
	TxSameKeyPrimary
	// TxPossiblePrimary marks a possible same-key primary: a data record that is not a same-key primary but could be one,
	// because a direction, function, SessionID or System Bytes it would need is unknown or unavailable.
	TxPossiblePrimary
	// TxClosing marks a closing record: a socket-close transport event of the primary's epoch,
	// or a capture-boundary record of kind stop of any epoch.
	TxClosing
	// TxOutcomeRecord marks an outcome record: a Reject.req control record or a T3 timer-expiry transport event of the primary.
	TxOutcomeRecord
)

// txClassMask holds the defined TxClass bits.
const txClassMask = TxPrimary | TxCandidate | TxPossibleReply | TxSameKeyPrimary | TxPossiblePrimary | TxClosing | TxOutcomeRecord

// Reasons a transaction lookup is incomplete (the tracepack semantics specification §7.2), each a TxGap.
// Any of them stands beside any outcome, none hiding another.
const (
	// TxGapNoKey reports a primary without a transaction key:
	// missing where something in its hour's read explains it, conflicting,
	// in a direction other than host-to-equipment or equipment-to-host,
	// or with its SessionID or System Bytes unavailable.
	TxGapNoKey TxGapReason = iota + 1
	// TxGapConflict reports a conflict on a seq of the primary's capture at or above the primary's,
	// within one hour's read or across hour reads.
	TxGapConflict
	// TxGapIndex reports a disagreement an hour's read detected (IncompleteReasonIndexMismatch):
	// a block whose records disagree with its F-2 entry, or another footer value that disagrees with the records it describes.
	// The lookup then guarantees only that it is incomplete with this gap; every other fact it reports is best-effort.
	TxGapIndex
	// TxGapRead reports a defect of an hour's read other than a coverage entry, a crash gap and an index mismatch:
	// a failed block, an unknown codec, a truncation or a reader limit.
	TxGapRead
	// TxGapCoverage reports a coverage entry of a pack read, of the primary's capture,
	// that meets the window's seqs and the time range of the hours scheduled.
	TxGapCoverage
	// TxGapUnevaluated reports a pack read whose pack metadata has quality_evaluated false.
	TxGapUnevaluated
	// TxGapSeqGap reports a seq of a bounded window that no hour's read yielded.
	TxGapSeqGap
	// TxGapOpenWindow reports a window that no record read bounds after the last hour read.
	TxGapOpenWindow
	// TxGapCrashGap reports a crash gap: a stop-unclean boundary of any capture, from a pack read,
	// whose gap meets the time range of the hours scheduled, or of the primary's hour on an early return
	// (the tracepack semantics specification §7.4, Crash gaps).
	TxGapCrashGap
	// TxGapCaptureBoundary reports a capture-boundary record of the primary's epoch above the primary
	// other than the clean stop that bounds the window.
	TxGapCaptureBoundary
	// TxGapOrderingUncertain reports a record of the primary's epoch with ordering-uncertain in an hour's read.
	TxGapOrderingUncertain
	// TxGapCorrelation reports a primary with correlation-incomplete.
	TxGapCorrelation
	// TxGapUnavailable reports, in the window, a possible reply, an undecidable candidate,
	// or a candidate at or after a possible same-key primary.
	TxGapUnavailable
	// TxGapScopeBreach reports a version an hour's read yielded whose ts_utc_ns lies outside that hour
	// (the tracepack format specification I-13): a pack that does not keep to its scope.
	// It names the hour, the version's pack, block and seq;
	// a version of the primary's capture is still classified, and kept as any other version is.
	TxGapScopeBreach
)

// ErrNotPrimary reports a TxKey that names no primary:
// a record that is not a data record with an available odd function,
// or no record where nothing in the read of its hour explains its absence (the tracepack semantics specification §7.2).
var ErrNotPrimary = errors.New("tracepack: not a primary")

// TxKey names the primary record of a transaction lookup.
type TxKey struct {
	// CaptureID is the primary's capture_id; it is not zero.
	CaptureID UUID
	// Seq is the primary's seq; it is not above 2^63-1, the largest seq the tracepack format allows.
	Seq uint64
	// Hour is the UTC hour of the primary's ts_utc_ns, its scope, numbered in whole hours from 1970-01-01T00:00Z;
	// it lies in [MinTxHour, MaxTxHour].
	Hour int64
}

// TxKeyOf returns the TxKey of a primary from its record:
// the capture_id of its pack, its seq, and its ts_utc_ns as a time, whose UTC hour, HourOf(ts), is the key's Hour.
// For a record r of a pack whose capture_id is captureID, it is TxKeyOf(captureID, r.Seq, time.Unix(0, r.TSUTCNs)).
func TxKeyOf(captureID UUID, seq uint64, ts time.Time) TxKey {
	return TxKey{CaptureID: captureID, Seq: seq, Hour: HourOf(ts)}
}

// TxOptions configures FindTransaction.
type TxOptions struct {
	// MaxHours is the number of hours scheduled from TxKey.Hour on, empty hours included, each read once;
	// zero means DefaultTxMaxHours; a negative value is refused.
	MaxHours int
	// MaxHeldBytes bounds the block buffers each hour's read holds at once, as MergeIterateOptions.MaxHeldBytes does,
	// outside of which the bootstrap bytes each Reader retains lie;
	// zero means DefaultMaxHeldBytes; a negative value is refused.
	MaxHeldBytes int64
	// MaxConflicts bounds the conflicts of the whole lookup:
	// each conflict an hour's read lists counts one, and so does each seq yielded by more than one hour's read, once however many,
	// and a conflict at the primary among copies its hour's read did not compare;
	// zero means DefaultMaxConflicts; a negative value is refused.
	MaxConflicts int
	// MaxStateBytes bounds the lookup's own state: the records it keeps, with their payloads and header extensions,
	// and what it records of the seqs it visited, the conflicts, the gaps, the crash gaps and the hours read;
	// zero means DefaultTxMaxStateBytes; a negative value is refused.
	// Each item is charged a fixed size as the state grows,
	// a record also its payload and header-extension bytes, a copied coverage entry also the values of its unknown entries,
	// and the charge of a record copied but not kept is released;
	// a charge that would pass MaxStateBytes fails the lookup with an error wrapping ErrReadLimit.
	// The readers and what each MergeIterate builds lie outside it,
	// the bootstrap bytes each Reader retains included,
	// at most max(WholeReadThreshold, HeadWindow + TailWindow) of its ReaderOptions per Reader.
	MaxStateBytes int64
}

// TxOutcome is the outcome of a transaction lookup.
//
// The zero value is no outcome, which a TxResult holds only beside an error; String prints it as "none",
// and any undefined value as "unknown(<n>)".
type TxOutcome uint8

var _ fmt.Stringer = TxOutcome(0)

// TxClass is the set of roles one kept version plays in a transaction lookup (the tracepack semantics specification §7.2).
//
// Zero means none: the version was kept only because another version of its seq qualified,
// the other side of a conflict.
// Unlike Quality, TxClass names undefined bits in Names and String, so no set bit goes unseen.
type TxClass uint16

var _ fmt.Stringer = TxClass(0)

// TxRecord is one version of a record a transaction lookup kept.
//
// Its Record, Payload included, and its HeaderExtra are copies, valid after FindTransaction returns.
type TxRecord struct {
	// Record is the version as stored, its Payload cloned.
	Record Record
	// HeaderExtra is the record header's extension area, cloned; nil for a 44-byte record header.
	HeaderExtra []byte
	// Hour is the hour whose read yielded this version.
	Hour int64
	// Pack is the pack_id of the version's representative in that read, and Block the index of its block in the pack.
	Pack  UUID
	Block int
	// Conflict reports that the lookup found the seq in conflict, within an hour's read or across hour reads,
	// and is set for every kept version of that seq, not only those Item.Conflict marks:
	// a copy that a block disagreeing with its F-2 entry let arrive uncompared is marked as well.
	Conflict bool
	// Unobserved reports that the version is unobserved by design:
	// Reader.Unobserved of the record, asked of the Reader of the version's representative pack, Pack.
	Unobserved bool
	// Class is the set of roles the version plays.
	Class TxClass
	// InWindow reports that the seq lies inside the window:
	// above the primary's and below WindowEnd, or above the primary's while WindowEnd is nil.
	InWindow bool
	// Bound reports that this version is the closing record or same-key primary at WindowEnd.
	Bound bool
	// Decidable, Eligible and Valid are the candidate flags, set only when Class has TxCandidate.
	// Decidable: the version's stream and function, and the primary's stream, are available.
	// Eligible: the seq lies inside the window and below the first possible same-key primary inside it.
	// Valid: the version is a valid match, eligible and decidable,
	// with the primary's stream and the primary's function + 1, or function 0.
	Decidable, Eligible, Valid bool
}

// TxGapReason classifies why a transaction lookup is incomplete.
//
// The zero value is not a reason; String prints it and any undefined value as "unknown(<n>)".
type TxGapReason uint8

var _ fmt.Stringer = TxGapReason(0)

// TxGap is one reason a transaction lookup is incomplete, reported beside its outcome.
//
// Every pointer field is a copy, valid after FindTransaction returns.
type TxGap struct {
	// Reason classifies the gap.
	Reason TxGapReason
	// Hours holds the hours the gap concerns: one, or, for a conflict across hour reads, every hour whose read yielded the seq;
	// empty when it concerns no hour read, as for a seq gap or an open window.
	Hours []int64
	// Pack is the pack_id of the pack the gap concerns; nil when it concerns none.
	Pack *UUID
	// Block is the index of the block the gap concerns in that pack; -1 when none.
	Block int
	// Offset is the file offset of the defect the gap reports; -1 when none.
	Offset int64
	// Seq is the seq the gap concerns: the conflicting seq, the first missing seq of a seq gap, a capture-boundary's seq,
	// the seq of the first ordering-uncertain record read, the seq of a version outside its hour;
	// nil when none, as for a crash gap, whose seq Boundary holds.
	Seq *uint64
	// Defect is the reason of the read defect a TxGapRead or TxGapIndex reports; zero otherwise.
	Defect IncompleteReason
	// Coverage is the coverage entry a TxGapCoverage reports,
	// or, for a TxGapNoKey gap of a missing primary, the first coverage entry that meets the primary's seq and hour;
	// nil otherwise.
	Coverage *Coverage
	// Boundary is the stop-unclean boundary a TxGapCrashGap reports, or the capture-boundary record a TxGapCaptureBoundary reports;
	// nil otherwise.
	Boundary *Boundary
	// Err describes the gap; nil when the other fields say all.
	Err error
}

// TxHour is one hour a transaction lookup read to its end.
type TxHour struct {
	// Hour is the UTC hour.
	Hour int64
	// Packs holds the pack_ids of the packs the source gave for the hour, of any capture, in the order given.
	Packs []UUID
}

// TxPackError is a footer error of a pack a transaction lookup read, which does not make the lookup incomplete by itself.
type TxPackError struct {
	// Hour is the hour whose read took the pack.
	Hour int64
	// Pack is the pack's pack_id.
	Pack UUID
	// Err is the pack's PackHeader.FooterErr.
	Err error
}

var _ error = TxPackError{}

// TxResult is the result of a transaction lookup (the tracepack semantics specification §7.2).
//
// Every record, coverage entry and boundary it holds is a copy, valid after FindTransaction returns.
type TxResult struct {
	// Outcome is the outcome; zero only beside an error.
	Outcome TxOutcome
	// Epoch, Dir, SessionID and SystemBytes are the association key derived from the primary,
	// and Stream and Function its match fields, Stream valid iff StreamAvailable.
	// They are zero when the primary is missing or the lookup fails with ErrNotPrimary, and an unavailable field stays zero;
	// when the primary has several versions, in its own hour or a later one, they are those of the first version, as diagnostics.
	Epoch           uint32
	Dir             Dir
	SessionID       uint16
	SystemBytes     [4]byte
	Stream          uint8
	Function        uint8
	StreamAvailable bool
	// W is the primary's W bit, valid iff WAvailable; it takes no part in the lookup.
	W, WAvailable bool
	// WindowEnd is the seq of the window's bound:
	// the smallest seq of a same-key primary, or of a closing record without a conflict, that the lookup read;
	// nil while none was read.
	WindowEnd *uint64
	// Records holds every kept version, in ascending seq, then hour of the read, then version order within the read:
	// the primary's versions, then every seq that qualified, each with every version the read that kept it yielded.
	// Beside an error, it holds the versions kept until then,
	// in the order and with the flags the lookup had reached when it failed.
	Records []TxRecord
	// Incomplete holds every reason the lookup is incomplete, in discovery order;
	// the lookup is complete when it is empty.
	Incomplete []TxGap
	// Searched holds the hours read to their end, in the order read.
	Searched []TxHour
	// Conflicts holds the conflicts each hour's read listed, of any capture, in read order;
	// only those on a seq of the primary's capture at or above the primary's make the lookup incomplete.
	Conflicts []Conflict
	// FooterErrs holds the footer error of each pack read whose footer was not used.
	FooterErrs []TxPackError
}

// FindTransaction looks up the reply to the primary record key names, over the packs src gives,
// and reports the outcome of the tracepack semantics specification §7.2:
// TxMatched, TxAmbiguous or TxNotFound,
// with the records it kept, the conflicts, and every reason it is incomplete, beside the hours it searched.
//
// It reads hour key.Hour and each following hour of [key.Hour, key.Hour + MaxHours), the hours scheduled, once each,
// every one of them even after the window is closed,
// since a later hour can hold another version of a window seq or a smaller bound.
// Each hour's read is one MergeIterate in capture order, without a filter, over every pack src gives for the hour, of any capture.
// The lookup decides on the primary's capture only:
// its primary, window, gaps, versions and the conflicts that make it incomplete are of that capture, seqs compared as (capture_id, seq);
// read defects, scope breaches, packs with quality_evaluated false and crash gaps are reported for every pack read.
// From the primary it derives the transaction key:
// the epoch, direction, SessionID and System Bytes, and the stream and function to match.
// It searches only the seqs above the primary's.
// The window ends at the smallest seq of a same-key primary or closing record the lookup read;
// a footer's close_seq or boundary entry never bounds it.
//
// A primary that is missing where something in its hour's read explains it, conflicting within that read, or without a key
// ends the lookup after that one hour: such an early return is TxNotFound and incomplete.
// A missing primary is explained by a read defect other than a coverage entry or a crash gap, a scope breach,
// a coverage entry that meets its seq and hour, or a crash gap of its capture that meets its hour;
// a primary missing where nothing explains it fails the lookup with ErrNotPrimary.
//
// The comparison of a record's copies covers the hours the lookup reads, as MergeIterate's covers the packs it is given:
// another version of a record in an hour the lookup did not read is not compared,
// whether it lies after the last hour read or before key.Hour after a backward clock step.
// Such a version is not found by verifying one pack or merging one scope either,
// since neither compares versions that lie in different hours.
// A seq of the window with no version in the hours read is a seq gap.
// An ordering-uncertain record of the primary's epoch is likewise detected only in the hours read.
// A stop-unclean boundary of any pack read is a crash gap when its gap meets the time range of the hours scheduled
// (the tracepack semantics specification §7.4, Crash gaps).
//
// One valid match is TxMatched, several TxAmbiguous, none TxNotFound;
// every reason the lookup is incomplete is listed in TxResult.Incomplete beside the outcome, none hiding another.
// When an hour's read detects a disagreement between a block and its F-2 entry, or another footer value and its records,
// only the TxGapIndex gap and the lookup's being incomplete are guaranteed:
// the outcome, the other reasons, the versions and every other fact are best-effort.
//
// Parameters:
//   - ctx: cancels the lookup; it is checked before each call to the source, during each hour's read,
//     periodically in the lookup's own loops, currently every 4096 iterations, a period that may change, and before returning.
//   - src: the source of the packs; not nil.
//   - key: the primary; CaptureID not zero, Hour in [MinTxHour, MaxTxHour].
//   - opts: the limits; see TxOptions.
//
// Returns:
//   - TxResult: the result; on an error it holds what was found before the error, Outcome zero.
//     Searched lists only hours read to their end.
//   - error: an error wrapping ErrInvalidQuery, before anything is read:
//     for a nil src, a zero key.CaptureID, a key.Seq above 2^63-1, a key.Hour outside [MinTxHour, MaxTxHour],
//     a negative field of opts, or a last hour key.Hour + MaxHours - 1 above MaxTxHour;
//     ErrNotPrimary;
//     an error of src, wrapped with the hour; an error for a nil reader src gives;
//     the error of an hour's MergeIterate, an error wrapping ErrInvalidQuery for a Reader or pack_id src gives twice among them;
//     a ReadAt error; an error wrapping ErrReadLimit from MaxHeldBytes, MaxConflicts or MaxStateBytes;
//     ctx's error, wrapped.
//     A block over its Reader's MaxBlockLen is not an error: it is a TxGapRead gap.
func FindTransaction(ctx context.Context, src PackSource, key TxKey, opts TxOptions) (TxResult, error) {
	return findTransactionWith(ctx, src, key, opts, nil)
}

// findTransactionWith is FindTransaction; prep, set only by tests, runs on the lookup's state before the lookup starts.
func findTransactionWith(ctx context.Context, src PackSource, key TxKey, opts TxOptions, prep func(l *txLookup)) (TxResult, error) {
	opts, err := checkFindTransaction(src, key, opts)
	if err != nil {
		return TxResult{}, err
	}

	return lookupHours(ctx, liveHours{src: src}, key, opts, prep)
}

// lookupHours runs the lookup of key, with opts checked and defaulted (checkFindTransaction), over the hours of hours:
// FindTransaction's over its source, or FindTransactions's replay of the hours it read once.
func lookupHours(ctx context.Context, hours txHourSource, key TxKey, opts TxOptions, prep func(l *txLookup)) (TxResult, error) {
	if err := ctx.Err(); err != nil {
		return TxResult{}, fmt.Errorf("tracepack: find transaction: %w", err)
	}

	l := newTxLookup(key, opts, hours)
	if prep != nil {
		prep(l)
	}
	err := l.run(ctx)
	if err == nil {
		if cerr := ctx.Err(); cerr != nil {
			err = fmt.Errorf("tracepack: find transaction: %w", cerr)
		}
	}
	if err != nil {
		l.res.Outcome = 0
	}

	return l.res, err
}

// checkFindTransaction validates the arguments of FindTransaction before anything is read and applies the defaults of opts.
//
// It checks, in this order: src is not nil; key.CaptureID is not zero; key.Seq is a seq the format allows;
// key.Hour lies in [MinTxHour, MaxTxHour]; no field of opts is negative;
// then, MaxHours defaulted, the last hour scheduled, key.Hour + MaxHours - 1, is not above MaxTxHour,
// compared without overflow as MaxHours - 1 > MaxTxHour - key.Hour.
//
// Returns:
//   - TxOptions: opts with every zero field defaulted, MaxHeldBytes and MaxConflicts as MergeIterate defaults them.
//   - error: an error wrapping ErrInvalidQuery.
func checkFindTransaction(src PackSource, key TxKey, opts TxOptions) (TxOptions, error) {
	if src == nil {
		return opts, fmt.Errorf("%w: src is nil", ErrInvalidQuery)
	}
	if key.CaptureID == (UUID{}) {
		return opts, fmt.Errorf("%w: the capture_id is zero", ErrInvalidQuery)
	}
	if key.Seq > format.MaxU64 {
		return opts, fmt.Errorf("%w: seq %d is above %d", ErrInvalidQuery, key.Seq, format.MaxU64)
	}
	if key.Hour < MinTxHour || key.Hour > MaxTxHour {
		return opts, fmt.Errorf("%w: hour %d is outside [%d, %d]", ErrInvalidQuery, key.Hour, MinTxHour, MaxTxHour)
	}
	if msg := negativeOption("TxOptions", opts.budgets()...); msg != "" {
		return opts, fmt.Errorf("%w: %s", ErrInvalidQuery, msg)
	}
	if opts.MaxHours <= 0 {
		opts.MaxHours = DefaultTxMaxHours
	}
	// key.Hour <= MaxTxHour, so MaxTxHour - key.Hour cannot overflow, and MaxHours - 1 >= 0 fits an int64.
	if int64(opts.MaxHours-1) > MaxTxHour-key.Hour {
		return opts, fmt.Errorf("%w: %d scopes from hour %d pass hour %d", ErrInvalidQuery, opts.MaxHours, key.Hour, MaxTxHour)
	}
	if opts.MaxHeldBytes <= 0 {
		opts.MaxHeldBytes = DefaultMaxHeldBytes
	}
	if opts.MaxConflicts <= 0 {
		opts.MaxConflicts = DefaultMaxConflicts
	}
	if opts.MaxStateBytes <= 0 {
		opts.MaxStateBytes = DefaultTxMaxStateBytes
	}

	return opts, nil
}

// String returns the outcome's name in the tracepack semantics specification, "none" for the zero value,
// or "unknown(<n>)" for a value this package does not define.
func (o TxOutcome) String() string {
	switch o {
	case 0:
		return "none"
	case TxMatched:
		return "matched"
	case TxAmbiguous:
		return "ambiguous"
	case TxNotFound:
		return "not-found"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(o))
	}
}

// HasAny reports whether any bit set in bits is also set in c.
// HasAny(0) is false.
func (c TxClass) HasAny(bits TxClass) bool {
	return c&bits != 0
}

// HasAll reports whether every bit set in bits is also set in c.
// HasAll(0) is true.
func (c TxClass) HasAll(bits TxClass) bool {
	return c&bits == bits
}

// Names returns the names of c's defined bits, in ascending bit order:
// "primary", "candidate", "possible-reply", "same-key-primary", "possible-primary", "closing", "outcome";
// then, when any undefined bit is set, one "unknown(0x<hex>)" naming them all.
// It returns nil for zero.
func (c TxClass) Names() []string {
	var names []string
	for _, b := range []struct {
		bit  TxClass
		name string
	}{
		{TxPrimary, "primary"},
		{TxCandidate, "candidate"},
		{TxPossibleReply, "possible-reply"},
		{TxSameKeyPrimary, "same-key-primary"},
		{TxPossiblePrimary, "possible-primary"},
		{TxClosing, "closing"},
		{TxOutcomeRecord, "outcome"},
	} {
		if c&b.bit != 0 {
			names = append(names, b.name)
		}
	}
	if u := c &^ txClassMask; u != 0 {
		names = append(names, fmt.Sprintf("unknown(%#x)", uint16(u)))
	}

	return names
}

// String returns Names joined by "|", or "none" for zero.
func (c TxClass) String() string {
	if c == 0 {
		return "none"
	}

	return strings.Join(c.Names(), "|")
}

// String returns the reason's name in the tracepack semantics specification, or "unknown(<n>)" for a value this package does not define.
func (r TxGapReason) String() string {
	switch r {
	case TxGapNoKey:
		return "no-key"
	case TxGapConflict:
		return "conflict"
	case TxGapIndex:
		return "index"
	case TxGapRead:
		return "read"
	case TxGapCoverage:
		return "coverage"
	case TxGapUnevaluated:
		return "unevaluated"
	case TxGapSeqGap:
		return "seq-gap"
	case TxGapOpenWindow:
		return "open-window"
	case TxGapCrashGap:
		return "crash-gap"
	case TxGapCaptureBoundary:
		return "capture-boundary"
	case TxGapOrderingUncertain:
		return "ordering-uncertain"
	case TxGapCorrelation:
		return "correlation"
	case TxGapUnavailable:
		return "unavailable"
	case TxGapScopeBreach:
		return "scope-breach"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(r))
	}
}

// Error returns Err's text, prefixed with the hour and pack_id.
func (e TxPackError) Error() string {
	return fmt.Sprintf("tracepack: hour %d pack %s: %v", e.Hour, e.Pack, e.Err)
}

// Unwrap returns Err, so errors.Is and errors.As see the error about the pack.
func (e TxPackError) Unwrap() error {
	return e.Err
}

// Complete reports whether the lookup is complete: whether Incomplete is empty.
// A conflict that bears on the lookup is reported in Incomplete as a TxGapConflict gap
// (the tracepack semantics specification §7.2), while a conflict unrelated to it is listed in Conflicts only;
// FooterErrs alone never make the lookup incomplete.
func (r *TxResult) Complete() bool {
	return len(r.Incomplete) == 0
}

// Matches returns every kept version that is a valid match, one with Valid set, in Records order;
// nil when there is none.
//
// Records still holds every version the lookup kept, the diagnostic ones included.
// The pointers alias elements of the backing array of r.Records:
// writing through one changes that element,
// and replacing or reallocating r.Records detaches them from it.
func (r *TxResult) Matches() []*TxRecord {
	var out []*TxRecord
	for i := range r.Records {
		if r.Records[i].Valid {
			out = append(out, &r.Records[i])
		}
	}

	return out
}

// PrimaryVersions returns every kept version of the primary, one whose Class has TxPrimary, in Records order,
// those a later hour's read yielded included;
// nil when there is none.
//
// Records still holds every version the lookup kept, the diagnostic ones included.
// The pointers alias elements of the backing array of r.Records:
// writing through one changes that element,
// and replacing or reallocating r.Records detaches them from it.
func (r *TxResult) PrimaryVersions() []*TxRecord {
	var out []*TxRecord
	for i := range r.Records {
		if r.Records[i].Class.HasAny(TxPrimary) {
			out = append(out, &r.Records[i])
		}
	}

	return out
}

// budgets returns the budget fields of o, each refused when negative, in declaration order.
func (o TxOptions) budgets() []optionField {
	return []optionField{
		{"MaxHours", int64(o.MaxHours)},
		{"MaxHeldBytes", o.MaxHeldBytes},
		{"MaxConflicts", int64(o.MaxConflicts)},
		{"MaxStateBytes", o.MaxStateBytes},
	}
}

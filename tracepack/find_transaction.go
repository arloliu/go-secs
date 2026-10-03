package tracepack

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// Default TxOptions values, which a zero or negative field takes.
const (
	// DefaultTxMaxScopes is the default TxOptions.MaxScopes: the primary's hour and the next.
	DefaultTxMaxScopes = 2
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
	// TxUnmatched means the lookup established that the primary has no valid match in the scopes it read.
	TxUnmatched
	// TxIncomplete means the lookup could establish neither a match nor its absence.
	TxIncomplete
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

// Reasons a transaction lookup is not complete (the tracepack semantics specification §7.2), each a TxGap.
// TxGapNoKey, TxGapConflict, TxGapIndex and TxGapScopeBreach make the outcome TxIncomplete;
// every other reason only prevents TxUnmatched.
const (
	// TxGapNoKey reports a primary without a transaction key:
	// missing where a gap explains it, conflicting, in a direction other than host-to-equipment or equipment-to-host,
	// or with its SessionID or System Bytes unavailable.
	TxGapNoKey TxGapReason = iota + 1
	// TxGapConflict reports a conflict on a seq at or above the primary's, within one scope read or across scope reads.
	TxGapConflict
	// TxGapIndex reports a block of a scope read whose records disagree with its F-2 entry (ReasonIndexMismatch).
	TxGapIndex
	// TxGapCold reports a scope that the catalog does not index, read or not.
	TxGapCold
	// TxGapRead reports a defect of a scope read other than a coverage entry and an index mismatch.
	TxGapRead
	// TxGapCoverage reports a coverage entry of a pack read, for the primary's capture, that meets the lookup's query.
	TxGapCoverage
	// TxGapUnevaluated reports a pack read whose pack metadata has quality_evaluated false.
	TxGapUnevaluated
	// TxGapEvidence reports per-capture evidence that its source marks partial.
	TxGapEvidence
	// TxGapSeqGap reports a seq of a bounded window that no scope read yielded.
	TxGapSeqGap
	// TxGapOpenWindow reports a window that no record read bounds after the last scope.
	TxGapOpenWindow
	// TxGapBarrier reports a completeness barrier: a stop-unclean boundary (the tracepack storage specification §5).
	TxGapBarrier
	// TxGapCaptureBoundary reports a capture-boundary of the primary's epoch other than the clean stop that bounds the window.
	TxGapCaptureBoundary
	// TxGapOrderingUncertain reports a record of the primary's epoch with ordering-uncertain in a scope read.
	TxGapOrderingUncertain
	// TxGapCorrelation reports a primary with correlation-incomplete.
	TxGapCorrelation
	// TxGapUnavailable reports, in the window, a possible reply, an undecidable candidate,
	// or a candidate at or after a possible same-key primary.
	TxGapUnavailable
	// TxGapContradiction reports an evidence closure at or below the primary,
	// or one at a seq the lookup read where no version is the closing record it claims.
	TxGapContradiction
	// TxGapConflicted reports a scope whose view is conflicted (the tracepack storage specification §4),
	// which the lookup does not read; a scope not indexed also has its TxGapCold gap, listed first.
	TxGapConflicted
	// TxGapScopeBreach reports a version a scope read yielded whose ts_utc_ns lies outside the scope's hour
	// (the tracepack format specification I-13): a pack that does not keep to its scope.
	// It names the scope's hour, the version's pack, block and seq;
	// the version is still classified, and kept as any other version is.
	TxGapScopeBreach
)

// ErrNotPrimary reports a TxKey that names no primary:
// a record that is not a data record with an available odd function,
// or no record where a complete read of an indexed scope shows none (the tracepack semantics specification §7.2).
var ErrNotPrimary = errors.New("tracepack: not a primary")

// TxKey names the primary record of a transaction lookup.
type TxKey struct {
	// Capture is the primary's capture_id; it is not zero.
	Capture UUID
	// Seq is the primary's seq; it is not above 2^63-1, the largest seq the tracepack format allows.
	Seq uint64
	// Hour is the UTC hour of the primary's ts_utc_ns, its scope, numbered in whole hours from 1970-01-01T00:00Z;
	// it lies in [MinTxHour, MaxTxHour].
	Hour int64
}

// TxOptions configures FindTransaction.
type TxOptions struct {
	// MaxScopes is the number of hours scheduled from TxKey.Hour on, empty and conflicted hours included,
	// each read but a conflicted one;
	// zero or negative means DefaultTxMaxScopes.
	MaxScopes int
	// MaxHeldBytes bounds the block buffers each scope read holds at once, as MergeIterateOptions.MaxHeldBytes does;
	// zero or negative means DefaultMaxHeldBytes.
	MaxHeldBytes int64
	// MaxConflicts bounds the conflicts of the whole lookup:
	// each conflict a scope read lists counts one, and so does each seq yielded by more than one scope read, once however many,
	// and a conflict at the primary among copies its scope's read did not compare;
	// zero or negative means DefaultMaxConflicts.
	MaxConflicts int
	// MaxStateBytes bounds the lookup's own state: the records it keeps, with their payloads and header extensions,
	// and what it records of the seqs it visited, the conflicts, the gaps and the scopes read;
	// zero or negative means DefaultTxMaxStateBytes.
	// Each item is charged a fixed size as the state grows,
	// a record also its payload and header-extension bytes, a copied coverage entry also the values of its unknown entries,
	// and the charge of a record copied but not kept is released;
	// a charge that would pass MaxStateBytes fails the lookup with an error wrapping ErrReadLimit.
	// The readers, the evidence, the barriers and what each MergeIterate builds lie outside it.
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
	// Hour is the hour of the scope read that yielded this version.
	Hour int64
	// Pack is the pack_id of the version's representative in that read, and Block the index of its block in the pack.
	Pack  UUID
	Block int
	// Conflict reports that the lookup found the seq in conflict, within a scope read or across scope reads,
	// and is set for every kept version of that seq, not only those Item.Conflict marks:
	// a copy that a block disagreeing with its F-2 entry let arrive uncompared is marked as well.
	Conflict bool
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

// TxGapReason classifies why a transaction lookup is not complete.
//
// The zero value is not a reason; String prints it and any undefined value as "unknown(<n>)".
type TxGapReason uint8

var _ fmt.Stringer = TxGapReason(0)

// TxGap is one reason a transaction lookup is not complete, reported beside its outcome.
//
// Every pointer field is a copy, valid after FindTransaction returns.
type TxGap struct {
	// Reason classifies the gap.
	Reason TxGapReason
	// Hours holds the hours the gap concerns: one, or, for a conflict across scope reads, every hour whose read yielded the seq.
	Hours []int64
	// Pack is the pack_id of the pack the gap concerns; nil when it concerns none.
	Pack *UUID
	// Block is the index of the block the gap concerns in that pack; -1 when none.
	Block int
	// Offset is the file offset of the defect the gap reports; -1 when none.
	Offset int64
	// Seq is the seq the gap concerns: the conflicting seq, the first missing seq of a seq gap, a capture-boundary's seq,
	// the seq of the first ordering-uncertain record read, a contradicted closure's seq, the seq of a version outside its scope's hour;
	// nil when none, as for a barrier, whose seq Barrier holds.
	Seq *uint64
	// Defect is the reason of the read defect a TxGapRead or TxGapIndex reports; zero otherwise.
	Defect IncompleteReason
	// Coverage is the coverage entry a TxGapCoverage reports,
	// or, for a TxGapNoKey gap of a missing primary, the first coverage entry that meets the primary's seq and hour;
	// nil otherwise.
	Coverage *Coverage
	// Barrier is the boundary a TxGapBarrier or TxGapCaptureBoundary reports; nil otherwise.
	Barrier *Boundary
	// Err describes the gap; nil when the other fields say all.
	Err error
}

// TxScope is one scope a transaction lookup read to its end.
type TxScope struct {
	// Hour is the scope's UTC hour.
	Hour int64
	// Indexed reports whether the catalog indexes the scope; a scope not indexed was read through its listing view.
	Indexed bool
	// Packs holds the pack_ids of the scope's view, in view order.
	Packs []UUID
}

// TxPackError is a footer error of a pack a transaction lookup read, which does not make the lookup incomplete by itself.
type TxPackError struct {
	// Hour is the hour of the scope read the pack belongs to.
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
	// when the primary has several versions, in its own scope or a later one, they are those of the first version, as diagnostics.
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
	// Gaps holds every reason the lookup is not complete, in discovery order.
	Gaps []TxGap
	// Searched holds the scopes read to their end, in the order read.
	Searched []TxScope
	// Conflicts holds the conflicts each scope read listed, in read order.
	Conflicts []Conflict
	// FooterErrs holds the footer error of each pack read whose footer was not used.
	FooterErrs []TxPackError
}

// FindTransaction looks up the reply to the primary record key names, in one observation src supplies,
// and reports the outcome of the tracepack semantics specification §7.2:
// TxMatched, TxAmbiguous, TxUnmatched or TxIncomplete,
// with the records it kept, the conflicts, and every reason the lookup is not complete, beside the scopes it searched.
//
// It opens one Observation of key.Capture over the hours [key.Hour, key.Hour + MaxScopes), and closes it before returning.
// It reads the scope of key.Hour and each following hour of that range once, each with one MergeIterate in capture order,
// every one of them even after the window is closed,
// since a later scope can hold another version of a window seq or a smaller bound.
// A scope whose view is conflicted is not read: it is a TxGapConflicted gap,
// and in key.Hour it explains a missing primary, as a scope the catalog does not index does.
// From the primary it derives the transaction key:
// the epoch, direction, SessionID and System Bytes, and the stream and function to match.
// It searches only the seqs above the primary's.
// The window ends at the smallest seq of a same-key primary or closing record the lookup read;
// a footer's or the per-capture evidence's claim that an epoch closed never bounds it,
// and a claim the records contradict is a gap.
// A primary missing where a gap explains it, conflicting within its own scope's read, or without a key ends the reads after that scope.
//
// The comparison of a record's copies covers the scopes the lookup reads, as MergeIterate's covers the packs it is given:
// another version of a record in an hour the lookup did not read is not compared,
// whether it lies after the last hour read or before key.Hour after a backward clock step.
// Such a version is not found by verifying one pack or merging one scope either,
// since neither compares versions that lie in different hours.
// A seq of the window with no version in the scopes read is a seq gap, so it never yields TxUnmatched.
// An ordering-uncertain record of the primary's epoch is likewise detected only in the scopes read.
//
// A conflict on a seq at or above the primary's, a block whose records disagree with its F-2 entry,
// a version whose ts_utc_ns lies outside the hour of the scope that yielded it, or a primary without a key
// makes the outcome TxIncomplete;
// such a version in the scope of key.Hour also explains a missing primary, as a read defect does.
// Otherwise one valid match is TxMatched and several TxAmbiguous, each with every gap found listed beside it;
// with none, the outcome is TxUnmatched when no gap is listed, TxIncomplete otherwise.
//
// Parameters:
//   - ctx: cancels the lookup; it is checked before each call to the source, during each scope read,
//     every 4096 iterations of the lookup's own loops, and before returning.
//   - src: the source of the observation; not nil.
//   - key: the primary; Capture not zero, Hour in [MinTxHour, MaxTxHour].
//   - opts: the limits; see TxOptions.
//
// Returns:
//   - TxResult: the result; on an error it holds what was found before the error, Outcome zero.
//     Searched lists only scopes read to their end.
//   - error: an error wrapping ErrInvalidQuery, before anything is read:
//     for a nil src, a zero key.Capture, a key.Seq above 2^63-1, a key.Hour outside [MinTxHour, MaxTxHour],
//     or a last hour key.Hour + MaxScopes - 1 above MaxTxHour;
//     ErrNotPrimary;
//     an error of src or of the Observation, wrapped with the hour, a Close error only when nothing else failed;
//     an error for a scope the Observation reports conflicted beside readers;
//     a ReadAt error; an error wrapping ErrReadLimit from MaxHeldBytes, MaxConflicts or MaxStateBytes;
//     ctx's error, wrapped.
//     A block over its Reader's MaxBlockLen is not an error: it is a TxGapRead gap.
func FindTransaction(ctx context.Context, src PackSource, key TxKey, opts TxOptions) (TxResult, error) {
	return findTransactionWith(ctx, src, key, opts, nil)
}

// findTransactionWith is FindTransaction; prep, set only by tests, runs on the lookup's state before the lookup starts.
func findTransactionWith(ctx context.Context, src PackSource, key TxKey, opts TxOptions, prep func(l *txLookup)) (res TxResult, err error) {
	opts, err = checkFindTransaction(src, key, opts)
	if err != nil {
		return TxResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return TxResult{}, fmt.Errorf("tracepack: find transaction: %w", err)
	}
	// checkFindTransaction bounded the last hour by MaxTxHour, so the end of the range fits.
	to := key.Hour + int64(opts.MaxScopes)
	obs, err := src.Observe(ctx, key.Capture, key.Hour, to)
	if err != nil {
		return TxResult{}, fmt.Errorf("tracepack: find transaction: observe hours [%d, %d): %w", key.Hour, to, err)
	}

	// Close runs once on every path, a panic included; its error is returned only when nothing else failed.
	defer func() {
		if cerr := obs.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("tracepack: find transaction: close hours [%d, %d): %w", key.Hour, to, cerr)
		}
		if err != nil {
			res.Outcome = 0
		}
	}()

	l := newTxLookup(key, opts, obs)
	if prep != nil {
		prep(l)
	}
	err = l.run(ctx)
	if err == nil {
		if cerr := ctx.Err(); cerr != nil {
			err = fmt.Errorf("tracepack: find transaction: %w", cerr)
		}
	}

	return l.res, err
}

// checkFindTransaction validates the arguments of FindTransaction before anything is read and applies the defaults of opts.
//
// It checks, in this order: src is not nil; key.Capture is not zero; key.Seq is a seq the format allows;
// key.Hour lies in [MinTxHour, MaxTxHour];
// then, MaxScopes defaulted, the last hour scheduled, key.Hour + MaxScopes - 1, is not above MaxTxHour,
// compared without overflow as MaxScopes - 1 > MaxTxHour - key.Hour.
//
// Returns:
//   - TxOptions: opts with every zero or negative field defaulted, MaxHeldBytes and MaxConflicts as MergeIterate defaults them.
//   - error: an error wrapping ErrInvalidQuery.
func checkFindTransaction(src PackSource, key TxKey, opts TxOptions) (TxOptions, error) {
	if src == nil {
		return opts, fmt.Errorf("%w: src is nil", ErrInvalidQuery)
	}
	if key.Capture == (UUID{}) {
		return opts, fmt.Errorf("%w: the capture_id is zero", ErrInvalidQuery)
	}
	if key.Seq > format.MaxU64 {
		return opts, fmt.Errorf("%w: seq %d is above %d", ErrInvalidQuery, key.Seq, format.MaxU64)
	}
	if key.Hour < MinTxHour || key.Hour > MaxTxHour {
		return opts, fmt.Errorf("%w: hour %d is outside [%d, %d]", ErrInvalidQuery, key.Hour, MinTxHour, MaxTxHour)
	}
	if opts.MaxScopes <= 0 {
		opts.MaxScopes = DefaultTxMaxScopes
	}
	// key.Hour <= MaxTxHour, so MaxTxHour - key.Hour cannot overflow, and MaxScopes - 1 >= 0 fits an int64.
	if int64(opts.MaxScopes-1) > MaxTxHour-key.Hour {
		return opts, fmt.Errorf("%w: %d scopes from hour %d pass hour %d", ErrInvalidQuery, opts.MaxScopes, key.Hour, MaxTxHour)
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
	case TxUnmatched:
		return "unmatched"
	case TxIncomplete:
		return "incomplete"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(o))
	}
}

// Has reports whether any bit set in bit is also set in c.
func (c TxClass) Has(bit TxClass) bool {
	return c&bit != 0
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
	case TxGapCold:
		return "cold"
	case TxGapRead:
		return "read"
	case TxGapCoverage:
		return "coverage"
	case TxGapUnevaluated:
		return "unevaluated"
	case TxGapEvidence:
		return "evidence"
	case TxGapSeqGap:
		return "seq-gap"
	case TxGapOpenWindow:
		return "open-window"
	case TxGapBarrier:
		return "barrier"
	case TxGapCaptureBoundary:
		return "capture-boundary"
	case TxGapOrderingUncertain:
		return "ordering-uncertain"
	case TxGapCorrelation:
		return "correlation"
	case TxGapUnavailable:
		return "unavailable"
	case TxGapContradiction:
		return "contradiction"
	case TxGapConflicted:
		return "conflicted"
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

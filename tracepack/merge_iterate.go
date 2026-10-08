package tracepack

import (
	"context"
	"fmt"
)

// Default MergeIterateOptions values, which a zero or negative field takes.
const (
	// DefaultMaxHeldBytes is the default MergeIterateOptions.MaxHeldBytes: 512 MiB.
	DefaultMaxHeldBytes = 512 << 20
	// DefaultMaxConflicts is the default MergeIterateOptions.MaxConflicts: 65536 conflicts.
	DefaultMaxConflicts = 1 << 16
)

// Orders of a read over several packs (the tracepack semantics specification §7.4).
const (
	// OrderCapture yields each capture's records in ascending seq,
	// captures interleaved block by block.
	OrderCapture Order = iota + 1
	// OrderTime yields the records by (ts_utc_ns, capture_id, seq), then version.
	OrderTime
)

// Order selects the order of MergeIterate.
//
// The zero value is not an order; String prints it and any undefined value as "unknown(<n>)".
type Order uint8

var _ fmt.Stringer = Order(0)

// MergeIterateOptions configures MergeIterate.
type MergeIterateOptions struct {
	// Order is the order the records are yielded in; it is required.
	Order Order
	// MaxHeldBytes bounds the bytes of the block buffers MergeIterate holds at once,
	// reserved before each block is read;
	// zero or negative means DefaultMaxHeldBytes.
	// It does not bound the process heap:
	// the readers, the block descriptors and clusters MergeIterate builds before reading,
	// its queues, codec state, Result.Conflicts, and the hours the read covers for Query.Retention and Result.Removed lie outside it.
	// A read that would exceed it ends with an error wrapping ErrReadLimit, returned with the Result found so far.
	MaxHeldBytes int64
	// MaxConflicts bounds len(Result.Conflicts), not the size of one conflict;
	// zero or negative means DefaultMaxConflicts.
	// One conflict more ends the read with an error wrapping ErrReadLimit, returned with the Result found so far.
	MaxConflicts int
}

// mergeIterateOptions holds the options of a read over several packs that only this package sets.
type mergeIterateOptions struct {
	// reserveConflict, when set, is called before each conflict is added to Result.Conflicts,
	// in place of the check against MaxConflicts, so a caller can count the conflicts of several reads against one allowance.
	// An error ends the read, wrapped, the conflict not added.
	reserveConflict func() error
	// storedFlags, when set, receives the record_flags of each item's record header, as stored, before fn is called with the item:
	// Record keeps only mono_present of them, so a caller that compares stored records byte for byte needs the others.
	storedFlags *uint8
}

// PackError is an error about one pack of a read that does not make the read incomplete.
type PackError struct {
	// Pack is the index into the readers given to MergeIterate of the pack the error concerns; 0 for Reader.Iterate.
	Pack int
	// Err describes the error.
	Err error
}

var _ error = PackError{}

// MergeIterate reads the records of several packs selected by q and passes them to fn in opts.Order,
// each record, or each selected version of a conflicting record, once;
// it returns the status of the read (the tracepack semantics specification §7.4).
//
// Captures compare by capture_id as raw UUID bytes, which is also the order of their text form.
//
// It reads every pack given, typically the packs of the active views of the scopes a query touches,
// and leaves none out for its own time range or summaries:
// a pack outside the query's time range can hold another version of a record inside it.
// Its guarantees cover the packs given; a copy of a record in a pack not given is neither read nor compared.
// MergeIterate never modifies a Reader, so Readers can be shared with concurrent reads, Reader.Iterate and MergeIterate alike.
//
// The Result starts, in the order of readers, with the defects every read of each pack carries, those Open found,
// and the coverage defects of each pack for the query, as Reader.Iterate reports them, each with Pack set to the reader's index.
// FooterErrs lists each pack whose footer was not used; FooterErr stays nil.
// Before yielding anything, MergeIterate reads each walked block, one without an F-2 entry, once,
// to take from its records what an index would state: its seq range, time range, epoch range, and kind and dir counts;
// a block that fails that read adds its defect and takes no further part.
// The blocks of one capture whose seq ranges overlap, across every pack, form an overlap cluster.
// The footer, or the summaries of walked blocks, exclude a cluster only when they exclude every block of it:
// a cluster excluded whole is not read (a walked block is not read a second time), and a conflict inside it is not reported;
// every block of any other cluster is read, a walked one a second time.
//
// In a cluster read, the copies of each seq are compared byte for byte before the filter is applied:
// the record header as stored, all record_header_len bytes, and the payload, whatever q.Payloads.
// Identical copies are one record, yielded once.
// Each distinct version has a representative, the first block holding it in cluster order
// (ascending first seq, then the order of readers, then file order),
// from which it is yielded, and which Item.Pack and Item.Block name.
// When a seq has more than one version, it is a conflict:
// each version the filter selects is yielded with Item.Conflict set,
// and the conflict is added to Result.Conflicts, whether or not the filter selected a version.
// A read that returns nil lists every conflict of every cluster it compared, in discovery order:
// in OrderTime, clusters in the order they are read, seqs ascending within each;
// in OrderCapture, the captures are started in ascending capture_id, each up to its first selected record,
// then conflicts follow the order the read advances the captures in.
// One conflict over opts.MaxConflicts ends the read with ErrReadLimit, the conflicts found so far in the Result.
// The same readers in the same order, with unchanged bytes, the same q and opts, yield the same records and list the same conflicts.
//
// A record is selected as Reader.Iterate selects it, the filter testing each version's representative.
// In OrderCapture, each capture's records come in ascending seq,
// the next from the capture whose next selected record has the representative with the smallest ts_min,
// ties broken by capture_id, a block's ts_min being its F-2 value or that of its computed summary;
// the selected versions of a conflicting record come in version order, before the capture's next seq,
// with other captures' records possibly between them.
// In OrderTime, the records come in ascending (ts_utc_ns, capture_id, seq), then version:
// MergeIterate takes the clusters not excluded in ascending (ts_min, capture_id, first seq),
// and before reading each, whole, yields every selected record already read whose ts_utc_ns is strictly below that cluster's ts_min;
// once no cluster is left, it yields the rest.
// The version order is the order of the first block holding each version in cluster order, the order of Conflict.Versions.
//
// opts.MaxHeldBytes bounds the bytes of the block buffers held at once, each block's reserved before it is read.
// Capture order holds, per capture, the open blocks of its current cluster and the representatives of the versions it has yet to yield;
// time order holds the blocks of the cluster being read and the representative of every selected record not yet yielded,
// and a cluster can span hours.
// A reservation over the limit ends the read with ErrReadLimit.
// It does not bound the process heap: the readers, the block descriptors and clusters MergeIterate builds before reading,
// its queues, codec state, Result.Conflicts, and the hours the read covers for q.Retention (below) and Result.Removed lie outside it.
// A block over its Reader's ReaderOptions.MaxBlockLen, or whose stated dimensions cannot be consistent, is a defect and is not read;
// a block that fails a check of its read is a defect too.
// Neither contributes records, and the cluster's other blocks resolve its seqs.
// An indexed block whose records disagree with its F-2 entry adds a ReasonIndexMismatch defect and still yields its records.
// The defects of the reads follow those of the packs: first those of the walked blocks' first reads, in reader then file order,
// then those of the clusters' reads, in the order the blocks are read.
//
// MergeIterate trusts a valid footer's F-2 ranges and F-3 summaries,
// so its order and identity hold, for the packs given, only when every block read and every block excluded agrees with its index:
// a block whose records lie outside its F-2 seq range can place a seq in the wrong cluster,
// where a copy of it is neither compared nor deduplicated,
// and a record before its block's F-2 ts_min can be yielded out of time order.
//
// fn receives each selected record as an Item that is valid only during the call:
// MergeIterate reuses the Item, and Record.Payload and HeaderExtra alias the representative's block, held until fn returns.
// ctx is checked once after the arguments are validated, before each block read,
// after a seq group is gathered, whenever the count of copies gathered reaches or passes a multiple of 4096,
// before the 4096th call of fn, the 8192nd and so on,
// and once more before returning nil.
// A failed check returns ctx's error, and fn is not called again;
// when fn returns an error, that error is returned, whatever became of ctx during the call.
//
// With q.Retention set, MergeIterate applies the retention boundary (the tracepack storage specification §5, Retention).
// The read covers the scope hour of each pack, the hour of its period_start, for every role but extract,
// so a record whose timestamp lies outside that hour still belongs to it;
// an extract's record belongs to the hour of its own timestamp:
// the read covers the hours of the indexed blocks' F-2 ts_min and ts_max,
// then the hours of the records of every block it decodes, before the filter,
// the read of each walked block before anything is yielded included,
// all compared at once with the largest boundary seen.
// The covered hours number at most the packs other than extracts, plus two per indexed extract block,
// plus the distinct hours of the extract records decoded.
// MergeIterate asks q.Retention after the arguments are validated, before it reads any block;
// before the first call of fn, and before every call whose record's representative is in another block than the last call's,
// whichever capture, version or release of pending records it comes from;
// after a failed ReadAt, whatever its error wraps, ErrReadLimit included; and before it returns nil.
// The deleting component deletes an object only after the boundary passed its hour,
// so a check finding every covered hour retained proves that no read before it met an object retention deleted.
// When a covered hour is removed, MergeIterate ends at once with an error wrapping ErrRemoved, passing fn nothing more,
// and returns the Result found so far with Removed listing the removed hours:
// everything the read reported of those hours, records passed to fn, defects and conflicts included, is superseded,
// and the caller discards it.
// An error from fn, ctx's error and a limit of the read itself,
// a reservation over MaxHeldBytes or one conflict over MaxConflicts, end the read without asking q.Retention.
//
// Parameters:
//   - ctx: cancels the read at the checks above; passed to q.Retention.
//   - readers: the packs to read, distinct, none nil; an empty readers reads nothing.
//   - q: the query; see Query.
//   - opts: the order, required, and the limits; see MergeIterateOptions.
//   - fn: receives each selected record; a non-nil error stops the read.
//
// Returns:
//   - Result: the status of the read; complete when Incomplete and Removed are empty.
//     On an error it holds what was found before the error;
//     Result{} for an invalid argument, for ctx done when the arguments are validated, and for an empty readers.
//   - error: an error wrapping ErrInvalidQuery, before anything is read:
//     for a nil fn, an Order that is not one, an invalid q, a nil reader, a Reader given twice, or two readers of one pack_id;
//     ctx's error, wrapped;
//     an error wrapping ErrReadLimit when a reservation would exceed MaxHeldBytes or one more conflict would exceed MaxConflicts;
//     a ReadAt error, wrapped with the reader and block index;
//     the error fn returned, as is;
//     an error wrapping ErrRemoved, joined after the ReadAt error when it follows one;
//     or the error q.Retention returned, wrapped, joined after the ReadAt error when it follows one.
func MergeIterate(ctx context.Context, readers []*Reader, q Query, opts MergeIterateOptions, fn func(*Item) error) (Result, error) {
	return mergeIterateWith(ctx, readers, q, opts, mergeIterateOptions{}, fn)
}

// mergeIterateWith is MergeIterate with the options only this package sets, which the zero mergeIterateOptions leaves unused.
func mergeIterateWith(ctx context.Context, readers []*Reader, q Query, opts MergeIterateOptions, internal mergeIterateOptions,
	fn func(*Item) error,
) (Result, error) {
	o, err := checkMergeIterate(ctx, readers, &q, opts, fn)
	if err != nil {
		return Result{}, err
	}
	if len(readers) == 0 {
		return Result{}, nil
	}

	p := newMergeIteratePlan(o)
	p.reserveConflict, p.storedFlags = internal.reserveConflict, internal.storedFlags
	if err = p.build(ctx, readers, &q); err == nil {
		// checkMergeIterate admitted no order but these two.
		if o.Order == OrderCapture {
			err = p.runCapture(ctx, &q, fn)
		} else {
			err = p.runTime(ctx, &q, fn)
		}
	}

	return p.rs.end(p.res, err)
}

// String returns the order's name, or "unknown(<n>)" for a value this package does not define.
func (o Order) String() string {
	switch o {
	case OrderCapture:
		return "capture"
	case OrderTime:
		return "time"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(o))
	}
}

// Error returns Err's text, prefixed with the pack index.
func (e PackError) Error() string {
	return fmt.Sprintf("tracepack: pack %d: %v", e.Pack, e.Err)
}

// Unwrap returns Err, so errors.Is and errors.As see the error about the pack.
func (e PackError) Unwrap() error {
	return e.Err
}

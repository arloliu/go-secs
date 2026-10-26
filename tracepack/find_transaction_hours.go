package tracepack

import "context"

// txHourSource is where a transaction lookup takes its hours from:
// the packs of each hour and the read of them.
// FindTransaction reads the packs its PackSource gives (liveHours);
// FindTransactions replays the hours it read once.
type txHourSource interface {
	// readers returns the readers of hour, in a fresh slice, as PackSource.Readers does.
	readers(ctx context.Context, hour int64) ([]*Reader, error)
	// read reads hour over readers as one MergeIterate in capture order with payloads and no filter:
	// each item to fn, its record_flags as stored through flags before fn sees it,
	// and through header its record's HSMS header when the source holds it decoded, else nothing,
	// each conflict reserved through reserveConflict before it is listed, then the read's Result.
	// A lookup treats each Item and the Result as read-only and keeps no reference into them.
	read(ctx context.Context, hour int64, readers []*Reader, maxHeld int64,
		reserveConflict func() error, flags *uint8, header *txCachedHeader, fn func(*Item) error) (Result, error)
}

// txCachedHeader is the HSMS header of the record of the item a read passes to its callback,
// as Record.HSMSHeader returns it, when ok; a source that does not hold it decoded never sets it.
type txCachedHeader struct {
	h  HSMSHeader
	ok bool
}

// liveHours reads the hours of a PackSource as they are, for FindTransaction.
type liveHours struct {
	src PackSource
}

var _ txHourSource = liveHours{}

func (h liveHours) readers(ctx context.Context, hour int64) ([]*Reader, error) {
	return h.src.Readers(ctx, hour)
}

// read reads the packs as they are; it decodes no header, so it never sets header.
func (h liveHours) read(ctx context.Context, _ int64, readers []*Reader, maxHeld int64,
	reserveConflict func() error, flags *uint8, _ *txCachedHeader, fn func(*Item) error,
) (Result, error) {
	return mergeIterateWith(ctx, readers, Query{Payloads: true},
		MergeIterateOptions{Order: OrderCapture, MaxHeldBytes: maxHeld},
		mergeIterateOptions{reserveConflict: reserveConflict, storedFlags: flags}, fn)
}

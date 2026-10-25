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
	// each conflict reserved through reserveConflict before it is listed, then the read's Result.
	// A lookup treats the Result as read-only and keeps no reference into it.
	read(ctx context.Context, hour int64, readers []*Reader, maxHeld int64,
		reserveConflict func() error, flags *uint8, fn func(*Item) error) (Result, error)
}

// liveHours reads the hours of a PackSource as they are, for FindTransaction.
type liveHours struct {
	src PackSource
}

var _ txHourSource = liveHours{}

func (h liveHours) readers(ctx context.Context, hour int64) ([]*Reader, error) {
	return h.src.Readers(ctx, hour)
}

func (h liveHours) read(ctx context.Context, _ int64, readers []*Reader, maxHeld int64,
	reserveConflict func() error, flags *uint8, fn func(*Item) error,
) (Result, error) {
	return mergeIterateWith(ctx, readers, Query{Payloads: true},
		MergeIterateOptions{Order: OrderCapture, MaxHeldBytes: maxHeld},
		mergeIterateOptions{reserveConflict: reserveConflict, storedFlags: flags}, fn)
}

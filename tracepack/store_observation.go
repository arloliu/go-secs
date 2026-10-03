package tracepack

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
)

// storeObservation is the Observation a storeSource's Observe returns: what it fixed, and the objects it keeps open.
type storeObservation struct {
	from, to int64
	// scopes holds the fixed view of each hour of [from, to), hour from first.
	scopes   []storeScope
	evidence CaptureEvidence
	// barriers holds the tool's barriers of the catalog's snapshot.
	barriers []Boundary
	// charged is the charge of the observation's state when Observe returned,
	// and peak the largest charge Observe held.
	charged, peak int64

	mu sync.Mutex
	// objects holds every object the observation keeps open; nil once closed.
	objects  []Object
	closed   bool
	closeErr error
}

var _ Observation = (*storeObservation)(nil)

// storeScope is the fixed view of one hour of a storeObservation.
type storeScope struct {
	// readers holds a Reader per pack of the view, in view order.
	readers    []*Reader
	indexed    bool
	conflicted bool
}

// Scope returns the fixed view of hour's scope, its Readers in a fresh slice.
func (o *storeObservation) Scope(ctx context.Context, hour int64) (SourceScope, error) {
	if err := o.check(ctx); err != nil {
		return SourceScope{}, err
	}
	if hour < o.from || hour >= o.to {
		return SourceScope{}, fmt.Errorf("tracepack: store source: hour %d outside the observation's hours [%d, %d)", hour, o.from, o.to)
	}
	sc := &o.scopes[hour-o.from]

	return SourceScope{Readers: slices.Clone(sc.readers), Indexed: sc.indexed, Conflicted: sc.conflicted}, nil
}

// Evidence returns a deep copy of the capture's evidence.
func (o *storeObservation) Evidence(ctx context.Context) (CaptureEvidence, error) {
	if err := o.check(ctx); err != nil {
		return CaptureEvidence{}, err
	}
	e := o.evidence
	e.Boundaries = cloneBoundaries(e.Boundaries)
	e.Closures = slices.Clone(e.Closures)

	return e, nil
}

// Barriers returns deep copies of the barriers whose gap interval meets [from, to),
// which must be non-empty and lie within the observation's hours.
func (o *storeObservation) Barriers(ctx context.Context, from, to int64) ([]Boundary, error) {
	if err := o.check(ctx); err != nil {
		return nil, err
	}
	// Observe bounded the hours by [MinTxHour, MaxTxHour+1), whose nanoseconds fit in an int64.
	if from >= to || from < o.from*hourNs || to > o.to*hourNs {
		return nil, fmt.Errorf("tracepack: store source: barriers range [%d, %d) is empty or not within the hours' range [%d, %d)",
			from, to, o.from*hourNs, o.to*hourNs)
	}
	var out []Boundary
	for i := range o.barriers {
		if barrierMeets(&o.barriers[i], from, to) {
			out = append(out, cloneBoundary(o.barriers[i]))
		}
	}

	return out, nil
}

// Close closes every object the observation keeps open, each once, and returns the first close error.
// Later calls close nothing and return that error once the first call has finished, nil before.
func (o *storeObservation) Close() error {
	// The objects are taken under the lock and closed outside it, so a Close never waits on another caller.
	o.mu.Lock()
	if o.closed {
		err := o.closeErr
		o.mu.Unlock()

		return err
	}
	o.closed = true
	objects := o.objects
	o.objects = nil
	o.mu.Unlock()

	var first error
	for _, obj := range objects {
		if err := obj.Close(); err != nil && first == nil {
			first = fmt.Errorf("tracepack: store source: close: %w", err)
		}
	}
	o.mu.Lock()
	o.closeErr = first
	o.mu.Unlock()

	return first
}

// check returns ctx's error, or an error once the observation is closed.
func (o *storeObservation) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("tracepack: store source: %w", err)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return errors.New("tracepack: store source: the observation is closed")
	}

	return nil
}

// cloneBoundaries returns a deep copy of bs, nil for an empty bs.
func cloneBoundaries(bs []Boundary) []Boundary {
	if len(bs) == 0 {
		return nil
	}
	out := make([]Boundary, len(bs))
	for i := range bs {
		out[i] = cloneBoundary(bs[i])
	}

	return out
}

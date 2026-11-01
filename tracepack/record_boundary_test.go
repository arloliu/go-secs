package tracepack_test

import (
	"testing"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// eventPayload encodes ev as a transport event's payload.
func eventPayload(t *testing.T, ev tracepack.TransportEvent) []byte {
	t.Helper()

	b, err := ev.MarshalBinary()
	require.NoError(t, err)

	return b
}

func TestRecordCaptureBoundary(t *testing.T) {
	t.Parallel()

	boundary := func(k tracepack.BoundaryKind) []byte {
		return eventPayload(t, tracepack.TransportEvent{Event: tracepack.EventCaptureBoundary, BoundaryKind: &k})
	}
	tests := []struct {
		name     string
		kind     tracepack.Kind
		payload  []byte
		wantKind tracepack.BoundaryKind
		want     bool
	}{
		{name: "start", kind: tracepack.KindTransportEvent, payload: boundary(tracepack.BoundaryKindStart), wantKind: tracepack.BoundaryKindStart, want: true},
		{name: "stop", kind: tracepack.KindTransportEvent, payload: boundary(tracepack.BoundaryKindStop), wantKind: tracepack.BoundaryKindStop, want: true},
		{
			name: "stop unclean", kind: tracepack.KindTransportEvent, payload: boundary(tracepack.BoundaryKindStopUnclean),
			wantKind: tracepack.BoundaryKindStopUnclean, want: true,
		},
		{name: "gap", kind: tracepack.KindTransportEvent, payload: boundary(tracepack.BoundaryKindGap), wantKind: tracepack.BoundaryKindGap, want: true},
		{
			name: "no boundary_kind", kind: tracepack.KindTransportEvent,
			payload:  eventPayload(t, tracepack.TransportEvent{Event: tracepack.EventCaptureBoundary}),
			wantKind: tracepack.BoundaryKindUnknown, want: true,
		},
		{
			name: "another event", kind: tracepack.KindTransportEvent,
			payload:  eventPayload(t, tracepack.TransportEvent{Event: tracepack.EventSocketClose}),
			wantKind: tracepack.BoundaryKindUnknown,
		},
		{name: "a payload that does not decode", kind: tracepack.KindTransportEvent, payload: []byte{0xFF}, wantKind: tracepack.BoundaryKindUnknown},
		{name: "an empty payload", kind: tracepack.KindTransportEvent, wantKind: tracepack.BoundaryKindUnknown},
		{name: "a data record", kind: tracepack.KindData, payload: boundary(tracepack.BoundaryKindStart), wantKind: tracepack.BoundaryKindUnknown},
		{name: "a control record", kind: tracepack.KindControl, payload: boundary(tracepack.BoundaryKindStart), wantKind: tracepack.BoundaryKindUnknown},
		{name: "an annotation", kind: tracepack.KindAnnotation, payload: boundary(tracepack.BoundaryKindStart), wantKind: tracepack.BoundaryKindUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := tracepack.Record{Kind: tt.kind, Payload: tt.payload}
			kind, ok := r.CaptureBoundary()
			assert.Equal(t, tt.want, ok)
			assert.Equal(t, tt.wantKind, kind)
		})
	}
}

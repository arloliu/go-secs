package bus

import (
	"encoding/base64"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
)

// busDecision is what a consumer, or a producer's preflight, does with an error of this package, the first match deciding.
type busDecision string

const (
	// decideGap: the message is never written; terminate it, its seq a seq gap, and alert.
	decideGap busDecision = "seq gap"
	// decideDeferLayout: a layout this module does not know; defer the message and alert.
	decideDeferLayout busDecision = "defer the message"
	// decideDeferCapture: a start that cannot be accepted; defer the capture and alert.
	decideDeferCapture busDecision = "defer the capture"
	// decideRefetch: a claim-check object that does not match; defer the message or fetch the object again.
	decideRefetch busDecision = "refetch"
	// decideCallerBug: a call this package cannot take; fix the caller.
	decideCallerBug busDecision = "caller bug"
	// decideClassifierBug: a value DeriveRecord cannot set from the classifier; fix the classifier.
	decideClassifierBug busDecision = "classifier bug"
	// decideBusTransient: any other error; it may be transient.
	decideBusTransient busDecision = "transient"
)

// decideBus returns the decision for err, in the documented order:
// each sentinel of this package first, before any cause it wraps.
func decideBus(err error) busDecision {
	switch {
	case errors.Is(err, ErrMalformed):
		return decideGap
	case errors.Is(err, ErrUnsupportedLayout):
		return decideDeferLayout
	case errors.Is(err, ErrNotAccepted):
		return decideDeferCapture
	case errors.Is(err, ErrPayloadMismatch):
		return decideRefetch
	case errors.Is(err, tracepack.ErrInvalidArgument):
		return decideCallerBug
	case errors.Is(err, tracepack.ErrInvalidRecord):
		return decideClassifierBug
	default:
		return decideBusTransient
	}
}

// TestDecision drives each row of the bus decision table through the codec,
// including a bus sentinel over a decoder's cause, which the bus sentinel decides while the cause stays matchable.
func TestDecision(t *testing.T) {
	t.Parallel()

	data := tracepack.Record{Seq: 7, TSUTCNs: 1, Kind: tracepack.KindData, Fidelity: tracepack.FidelityWireExact, Payload: busFrame(1, 3, []byte{1, 2})}
	body := busEncode(t, &data)
	desc := descriptorBytes(t)

	tests := []struct {
		name string
		err  func(t *testing.T) error
		want busDecision
		// also holds the causes the error also matches, which the bus sentinel overrides.
		also []error
	}{
		{name: "CheckMessage: a kind layout 1 does not carry", want: decideGap, err: func(t *testing.T) error {
			_, err := CheckMessage(busEnvelope(t, data.Seq), withKind(body, tracepack.Kind(5)))
			return err
		}},
		{name: "CheckMessage: an annotation payload that does not decode", want: decideGap, also: []error{tracepack.ErrCorrupt}, err: func(t *testing.T) error {
			note := tracepack.Record{Seq: 8, TSUTCNs: 1, Kind: tracepack.KindAnnotation, Fidelity: tracepack.FidelityNotApplicable, Payload: []byte{0x01}}
			_, err := CheckMessage(busEnvelope(t, note.Seq), busEncode(t, &note))

			return err
		}},
		{name: "CheckStart: a message over max_payload", want: decideGap, err: func(*testing.T) error {
			return CheckStart(HeaderBlockLimit, 45, MinMaxPayload)
		}},
		{name: "EncodeBody: a record of kind 0", want: decideGap, err: func(*testing.T) error {
			br := NewRecord(&tracepack.Record{Seq: 1, TSUTCNs: 1})
			_, err := EncodeBody(&br)

			return err
		}},
		{name: "CheckMessage: an unknown layout", want: decideDeferLayout, err: func(t *testing.T) error {
			_, err := CheckMessage(withLayout(busEnvelope(t, data.Seq), "2"), body)
			return err
		}},
		{name: "CheckMessage: a start without a descriptor", want: decideDeferCapture, err: func(t *testing.T) error {
			_, err := CheckMessage(startEnvelope(t, ""), startBody(t))
			return err
		}},
		{name: "CheckMessage: a start whose descriptor does not decode", want: decideDeferCapture, also: []error{tracepack.ErrCorrupt}, err: func(t *testing.T) error {
			_, err := CheckMessage(startEnvelope(t, base64.StdEncoding.EncodeToString(desc[:len(desc)-1])), startBody(t))
			return err
		}},
		{name: "CheckPayload: an object that differs", want: decideRefetch, err: func(t *testing.T) error {
			ccBody, env := claimCheck(t, &data, "b")
			br, err := CheckMessage(env, ccBody)
			require.NoError(t, err)

			return CheckPayload(&br, data.Payload[:5])
		}},
		{name: "CheckPayload: an inline record", want: decideCallerBug, err: func(t *testing.T) error {
			br, err := CheckMessage(busEnvelope(t, data.Seq), body)
			require.NoError(t, err)

			return CheckPayload(&br, data.Payload)
		}},
		{name: "CheckStart: a max_payload below the minimum", want: decideCallerBug, err: func(*testing.T) error {
			return CheckStart(100, 44, MinMaxPayload-1)
		}},
		{name: "DeriveRecord: a classifier's trailing_bytes out of range", want: decideClassifierBug, err: func(*testing.T) error {
			r := tracepack.Record{Kind: tracepack.KindData, Payload: busFrame(1, 1, nil)}
			return DeriveRecord(&r, &fakeClassifier{status: tracepack.DecodeStatusOKWithTrailing, trailing: -1})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.err(t)
			require.Error(t, err)
			assert.Equal(t, tt.want, decideBus(err), "%v", err)
			for _, c := range tt.also {
				assert.ErrorIs(t, err, c, "the cause stays matchable")
			}
		})
	}
}

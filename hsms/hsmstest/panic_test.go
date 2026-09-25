package hsmstest

// A fake is for testing handlers, so a handler panic should surface to the test rather than being
// silently recovered the way a real connection recovers one — see Deliver and DeliverDecodeError.
// These tests pin that difference; the fake needs no code change to satisfy them.

import (
	"errors"
	"testing"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// TestFakeEndpoint_Deliver_HandlerPanicPropagates proves a panicking DataMessageHandler is NOT
// recovered by Deliver, unlike the real connection's fan-out.
func TestFakeEndpoint_Deliver_HandlerPanicPropagates(t *testing.T) {
	f := NewFakeEndpoint()
	f.AddDataMessageHandler(func(*hsms.DataMessage, hsms.SECS2Endpoint) {
		panic("handler blew up")
	})

	msg := mustDataMessage(t)
	require.Panics(t, func() { f.Deliver(msg) })
}

// TestFakeEndpoint_DeliverDecodeError_HandlerPanicPropagates mirrors
// TestFakeEndpoint_Deliver_HandlerPanicPropagates for DeliverDecodeError.
func TestFakeEndpoint_DeliverDecodeError_HandlerPanicPropagates(t *testing.T) {
	f := NewFakeEndpoint()
	f.AddDecodeErrorHandler(func(*hsms.DataMessage, error, hsms.SECS2Endpoint) {
		panic("decode handler blew up")
	})

	msg := mustDataMessage(t)
	require.Panics(t, func() { f.DeliverDecodeError(msg, errors.New("decode failed")) })
}

// mustDataMessage builds a minimal well-formed *hsms.DataMessage for use as Deliver /
// DeliverDecodeError input — the panic under test comes from the registered handler, not from
// anything about the message itself.
func mustDataMessage(t *testing.T) *hsms.DataMessage {
	t.Helper()

	msg, err := hsms.NewDataMessage(1, 1, false, 0, [4]byte{0, 0, 0, 1}, secs2.NewEmptyItem())
	require.NoError(t, err)

	return msg
}

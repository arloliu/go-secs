package secs1

// s9_correlation_test.go — hsms.WithS9Correlation over a real secs1 pair built by New.
// The active host sends a W-bit primary with SendDataMessage,
// and the passive equipment answers it with an S9F7 whose MHEAD is the block header it received.

import (
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// TestS9Correlation_EquipmentFaultsSend answers the host's S6F11 with an S9F7 instead of a reply.
// The MHEAD is the block header as SECS-I carries it, with the R-bit, the E-bit, and block number 1 set,
// none of which the host's own header has.
// The host's send still returns the *hsms.S9Error, and the host's data handlers receive the S9F7 too.
//
// TEETH: make secs1's transport report false from UsesSECS1MHEAD and the send waits out T3 instead.
func TestS9Correlation_EquipmentFaultsSend(t *testing.T) {
	t.Parallel()

	type faulted struct {
		mhead [10]byte
		err   error
	}
	equipment := make(chan faulted, 1)
	hostHandled := make(chan *hsms.DataMessage, 1)

	_, active := openAbortPair(t,
		func(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
			mhead := msg.HeaderBytes()
			mhead[0] |= 0x80                // R-bit
			mhead[4], mhead[5] = 0x80, 0x01 // E-bit, block number 1
			err := ep.SendDataMessageAsync(t.Context(), 9, 7, false, secs2.B(mhead[:]))
			equipment <- faulted{mhead: mhead, err: err}
		},
		func(msg *hsms.DataMessage, _ hsms.SECS2Endpoint) { hostHandled <- msg },
		WithConnectionOption(hsms.WithS9Correlation(true)),
	)

	out := sendS6F11(t, active)

	var f faulted
	select {
	case f = <-equipment:
	case <-time.After(10 * time.Second):
		t.Fatal("the primary never reached the equipment's data handler")
	}
	require.NoError(t, f.err)

	var r abortReply
	select {
	case r = <-out:
	case <-time.After(10 * time.Second):
		t.Fatal("the host's send never returned")
	}

	require.Nil(t, r.msg)

	var s9Err *hsms.S9Error
	require.ErrorAs(t, r.err, &s9Err)
	require.Equal(t, byte(7), s9Err.Function)
	require.Equal(t, f.mhead, s9Err.MHEAD)
	require.NotNil(t, s9Err.Msg)
	require.Equal(t, "hsms: peer answered S6F11 with S9F7 (illegal data)", s9Err.Error())

	select {
	case msg := <-hostHandled:
		require.Equal(t, byte(9), msg.Stream())
		require.Equal(t, byte(7), msg.Function())
	case <-time.After(10 * time.Second):
		t.Fatal("the S9F7 never reached the host's data handler")
	}

	m := active.conn.Metrics()
	require.Equal(t, uint64(0), m.DataMsgErrCount(), "an S9 is never a data-message error")
	require.Equal(t, int64(0), m.DataMsgInflightCount())
}

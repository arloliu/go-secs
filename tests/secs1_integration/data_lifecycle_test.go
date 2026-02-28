package secs1integration

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// TestSECS1_DataMessage_Roundtrip
//
// Simple request/reply between active host and passive equipment.
// ---------------------------------------------------------------------------
func TestSECS1_DataMessage_Roundtrip(t *testing.T) {
	require := require.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	port := getFreePort(t)

	eqp := newEndpoint(t, ctx, port, true, false, runEchoReply)
	require.NoError(eqp.conn.Open(false))
	defer closeEndpoint(t, eqp)

	host := newEndpoint(t, ctx, port, false, true, runEchoReply)
	require.NoError(host.conn.Open(true))
	defer closeEndpoint(t, host)

	waitSelected(t, host, 5*time.Second)
	waitSelected(t, eqp, 5*time.Second)

	// host -> eqp: S1F1 with ASCII item
	reply, err := host.session.SendDataMessage(1, 1, true, secs2.NewASCIIItem("hello-secs1"))
	require.NoError(err)
	require.NotNil(reply)

	asciiVal, err := reply.Item().ToASCII()
	require.NoError(err)
	require.Equal("hello-secs1", asciiVal)
	reply.Free()

	// eqp -> host: S1F3 with ASCII item
	reply2, err := eqp.session.SendDataMessage(1, 3, true, secs2.NewASCIIItem("eqp-msg"))
	require.NoError(err)
	require.NotNil(reply2)

	asciiVal2, err := reply2.Item().ToASCII()
	require.NoError(err)
	require.Equal("eqp-msg", asciiVal2)
	reply2.Free()
}

// ---------------------------------------------------------------------------
// TestSECS1_MultiBlockMessage
//
// Sends a message whose SECS-II body exceeds 244 bytes, triggering
// multi-block assembly on the wire. Verifies the full payload round-trips.
// ---------------------------------------------------------------------------
func TestSECS1_MultiBlockMessage(t *testing.T) {
	require := require.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	port := getFreePort(t)

	eqp := newEndpoint(t, ctx, port, true, false, runEchoReply)
	require.NoError(eqp.conn.Open(false))
	defer closeEndpoint(t, eqp)

	host := newEndpoint(t, ctx, port, false, true)
	require.NoError(host.conn.Open(true))
	defer closeEndpoint(t, host)

	waitSelected(t, host, 5*time.Second)
	waitSelected(t, eqp, 5*time.Second)

	// Build a binary item > 244 bytes to force multi-block.
	// SECS-I block payload max = 244 bytes.
	// With header overhead, a ~500 byte body should produce 3+ blocks.
	const bodySize = 500
	payload := make([]byte, bodySize)
	for i := range payload {
		payload[i] = byte(i % 256)
	}

	reply, err := host.session.SendDataMessage(6, 1, true, secs2.NewBinaryItem(payload))
	require.NoError(err)
	require.NotNil(reply)

	gotBytes, err := reply.Item().ToBinary()
	require.NoError(err)
	require.Len(gotBytes, bodySize, "multi-block round-trip body size mismatch")

	// Verify content
	require.Equal(byte(0), gotBytes[0])
	require.Equal(byte(255), gotBytes[255])
	require.Equal(byte(0), gotBytes[256])
	reply.Free()
}

// ---------------------------------------------------------------------------
// TestSECS1_OpenCloseReopen
//
// Verifies that a SECS-I Connection can be opened, used, closed, then
// re-opened and used again without error.
// ---------------------------------------------------------------------------
func TestSECS1_OpenCloseReopen(t *testing.T) {
	require := require.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	port := getFreePort(t)

	eqp := newEndpoint(t, ctx, port, true, false, runEchoReply)
	host := newEndpoint(t, ctx, port, false, true)
	defer closeEndpoint(t, eqp)
	defer closeEndpoint(t, host)

	// --- first open ---
	require.NoError(eqp.conn.Open(false))
	require.NoError(host.conn.Open(true))
	waitSelected(t, host, 5*time.Second)
	waitSelected(t, eqp, 5*time.Second)

	reply, err := host.session.SendDataMessage(1, 1, true, secs2.NewASCIIItem("first"))
	require.NoError(err)
	require.NotNil(reply)
	reply.Free()

	// --- close both ---
	require.NoError(host.conn.Close())
	require.NoError(eqp.conn.Close())
	time.Sleep(300 * time.Millisecond) // allow goroutines to settle

	// --- reopen ---
	require.NoError(eqp.conn.Open(false))
	require.NoError(host.conn.Open(true))
	waitSelected(t, host, 5*time.Second)
	waitSelected(t, eqp, 5*time.Second)

	reply2, err := host.session.SendDataMessage(1, 1, true, secs2.NewASCIIItem("second"))
	require.NoError(err)
	require.NotNil(reply2)

	asciiVal, err := reply2.Item().ToASCII()
	require.NoError(err)
	require.Equal("second", asciiVal)
	reply2.Free()
}

// ---------------------------------------------------------------------------
// TestSECS1_PassiveRecoveryAfterAbruptDrop
//
// Passive equipment is connected, the active host drops, then a new active
// host reconnects. The passive side should accept the new connection.
// ---------------------------------------------------------------------------
func TestSECS1_PassiveRecoveryAfterAbruptDrop(t *testing.T) {
	require := require.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	port := getFreePort(t)

	eqp := newEndpoint(t, ctx, port, true, false, runEchoReply)
	require.NoError(eqp.conn.Open(false))
	defer closeEndpoint(t, eqp)

	// --- first host connects ---
	host1 := newEndpoint(t, ctx, port, false, true, runEchoReply)
	require.NoError(host1.conn.Open(true))
	waitSelected(t, host1, 5*time.Second)
	waitSelected(t, eqp, 5*time.Second)

	reply, err := host1.session.SendDataMessage(1, 1, true, secs2.NewASCIIItem("first-host"))
	require.NoError(err)
	require.NotNil(reply)
	reply.Free()

	// --- abruptly close first host ---
	require.NoError(host1.conn.Close())
	time.Sleep(500 * time.Millisecond)

	// --- second host reconnects ---
	host2 := newEndpoint(t, ctx, port, false, true, runEchoReply)
	require.NoError(host2.conn.Open(true))
	defer closeEndpoint(t, host2)

	waitSelected(t, host2, 10*time.Second)

	reply2, err := host2.session.SendDataMessage(1, 1, true, secs2.NewASCIIItem("second-host"))
	require.NoError(err)
	require.NotNil(reply2)

	asciiVal, err := reply2.Item().ToASCII()
	require.NoError(err)
	require.Equal("second-host", asciiVal)
	reply2.Free()
}

// ---------------------------------------------------------------------------
// TestSECS1_T3ReplyTimeout
//
// Equipment never replies; verify T3 fires.
// ---------------------------------------------------------------------------
func TestSECS1_T3ReplyTimeout(t *testing.T) {
	require := require.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	port := getFreePort(t)

	// Equipment with no handler -> never replies
	eqp := newEndpoint(t, ctx, port, true, false)
	require.NoError(eqp.conn.Open(false))
	defer closeEndpoint(t, eqp)

	host := newEndpoint(t, ctx, port, false, true)
	require.NoError(host.conn.Open(true))
	defer closeEndpoint(t, host)

	waitSelected(t, host, 5*time.Second)

	start := time.Now()
	_, err := host.session.SendDataMessage(1, 1, true, secs2.NewASCIIItem("ping"))
	elapsed := time.Since(start)

	require.Error(err, "expected T3 timeout error")
	// T3 minimum is 1s
	require.GreaterOrEqual(elapsed, 900*time.Millisecond, "T3 fired too early")
	require.LessOrEqual(elapsed, 3*time.Second, "T3 fired too late")
}

// ---------------------------------------------------------------------------
// TestSECS1_LargeMultiBlockStability
//
// Sends multiple large messages concurrently from both sides to stress
// multi-block assembly under contention.
// ---------------------------------------------------------------------------
func TestSECS1_LargeMultiBlockStability(t *testing.T) {
	require := require.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	port := getFreePort(t)

	eqp := newEndpoint(t, ctx, port, true, false, runEchoReply)
	require.NoError(eqp.conn.Open(false))
	defer closeEndpoint(t, eqp)

	host := newEndpoint(t, ctx, port, false, true, runEchoReply)
	require.NoError(host.conn.Open(true))
	defer closeEndpoint(t, host)

	waitSelected(t, host, 5*time.Second)
	waitSelected(t, eqp, 5*time.Second)

	// ~600 byte payload -> multiple blocks
	const bodySize = 600
	payload := make([]byte, bodySize)
	for i := range payload {
		payload[i] = byte(i % 256)
	}

	const rounds = 10
	var wg sync.WaitGroup
	wg.Add(2 * rounds)

	var errCount atomic.Int32

	send := func(ep *endpoint, tag string) {
		for i := 0; i < rounds; i++ {
			go func(i int) {
				defer wg.Done()
				reply, err := ep.session.SendDataMessage(6, 1, true, secs2.NewBinaryItem(payload))
				if err != nil {
					t.Logf("%s round %d error: %v", tag, i, err)
					errCount.Add(1)

					return
				}
				if reply == nil {
					t.Logf("%s round %d: nil reply", tag, i)
					errCount.Add(1)

					return
				}
				got, bErr := reply.Item().ToBinary()
				if bErr != nil || len(got) != bodySize {
					t.Logf("%s round %d: body mismatch (len=%d, err=%v)", tag, i, len(got), bErr)
					errCount.Add(1)
				}
				reply.Free()
			}(i)
		}
	}

	send(host, "host")
	send(eqp, "eqp")
	wg.Wait()

	require.Equal(int32(0), errCount.Load(), "expected zero errors in multi-block stability test")
}

// ---------------------------------------------------------------------------
// TestSECS1_WBitZero_FireAndForget
//
// Sends a message with W-bit=0. The sender should complete without
// waiting for a reply. The receiver should still get the message.
// ---------------------------------------------------------------------------
func TestSECS1_WBitZero_FireAndForget(t *testing.T) {
	require := require.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	port := getFreePort(t)

	receivedCh := make(chan string, 1)
	handler := func(msg *hsms.DataMessage, _ hsms.Session) {
		if v, err := msg.Item().ToASCII(); err == nil {
			receivedCh <- v
		}
	}

	eqp := newEndpoint(t, ctx, port, true, false, handler)
	require.NoError(eqp.conn.Open(false))
	defer closeEndpoint(t, eqp)

	host := newEndpoint(t, ctx, port, false, true)
	require.NoError(host.conn.Open(true))
	defer closeEndpoint(t, host)

	waitSelected(t, host, 5*time.Second)
	waitSelected(t, eqp, 5*time.Second)

	// W-bit=false
	err := host.session.SendDataMessageAsync(1, 1, false, secs2.NewASCIIItem("fire-and-forget"))
	require.NoError(err)

	select {
	case v := <-receivedCh:
		require.Equal("fire-and-forget", v)
	case <-time.After(5 * time.Second):
		t.Fatal("equipment did not receive fire-and-forget message")
	}
}

package hsmsssintegration

import (
	"context"
	"encoding/binary"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/hsmsss"
	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// TestDataMessage_Roundtrip
//
// Verifies a real active+passive pair can exchange S1F1/S1F2 in both
// directions with the correct item payload surviving the round-trip.
// ---------------------------------------------------------------------------
func TestDataMessage_Roundtrip(t *testing.T) {
	require := require.New(t)
	port := getFreePort(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	passive := newEndpoint(t, ctx, port, true, false, nil, echoHandler)
	active := newEndpoint(t, ctx, port, false, true, nil, echoHandler)
	defer closeEndpoint(t, passive)
	defer closeEndpoint(t, active)

	require.NoError(passive.conn.Open(false))
	require.NoError(active.conn.Open(false))
	waitState(t, active, hsms.SelectedState)
	waitState(t, passive, hsms.SelectedState)

	// active -> passive: S1F1 w-bit=true
	msg, err := hsms.NewDataMessage(1, 1, true, testSessionID, []byte{0, 0, 0, 1}, secs2.NewASCIIItem("hello"))
	require.NoError(err)
	reply, err := active.session.SendMessage(msg)
	require.NoError(err)
	require.NotNil(reply)
	dataReply, ok := reply.(*hsms.DataMessage)
	require.True(ok)
	asciiVal, err := dataReply.Item().ToASCII()
	require.NoError(err)
	require.Equal("hello", asciiVal)

	// passive -> active: S1F1 w-bit=true
	msg2, err := hsms.NewDataMessage(1, 1, true, testSessionID, []byte{0, 0, 0, 2}, secs2.NewASCIIItem("world"))
	require.NoError(err)
	reply2, err := passive.session.SendMessage(msg2)
	require.NoError(err)
	require.NotNil(reply2)
	dataReply2, ok := reply2.(*hsms.DataMessage)
	require.True(ok)
	asciiVal2, err := dataReply2.Item().ToASCII()
	require.NoError(err)
	require.Equal("world", asciiVal2)
}

// ---------------------------------------------------------------------------
// TestDataMessage_StabilityBidirectional
//
// 50 concurrent request/reply rounds from both sides simultaneously.
// ---------------------------------------------------------------------------
func TestDataMessage_StabilityBidirectional(t *testing.T) {
	require := require.New(t)
	port := getFreePort(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	passive := newEndpoint(t, ctx, port, true, false, nil, echoHandler)
	active := newEndpoint(t, ctx, port, false, true, nil, echoHandler)
	defer closeEndpoint(t, passive)
	defer closeEndpoint(t, active)

	require.NoError(passive.conn.Open(false))
	require.NoError(active.conn.Open(false))
	waitState(t, active, hsms.SelectedState)
	waitState(t, passive, hsms.SelectedState)

	const rounds = 50
	var wg sync.WaitGroup
	wg.Add(2 * rounds)

	var errCount atomic.Int32

	send := func(ep *endpoint, base byte) {
		for i := 0; i < rounds; i++ {
			go func(i int) {
				defer wg.Done()
				sys := [4]byte{base, byte(i >> 8), byte(i), 0}
				msg, err := hsms.NewDataMessage(1, 1, true, testSessionID, sys[:], secs2.NewASCIIItem("msg"))
				if err != nil {
					errCount.Add(1)

					return
				}
				reply, err := ep.session.SendMessage(msg)
				if err != nil || reply == nil {
					errCount.Add(1)
				}
			}(i)
		}
	}

	send(active, 0xA0)
	send(passive, 0xB0)
	wg.Wait()

	require.Equal(int32(0), errCount.Load(), "expected zero errors in bidirectional stability test")
}

// ---------------------------------------------------------------------------
// TestDataMessage_T3ReplyTimeout
//
// Raw peer never replies to data message; verify T3 fires within tolerance.
// ---------------------------------------------------------------------------
func TestDataMessage_T3ReplyTimeout(t *testing.T) {
	require := require.New(t)
	port := getFreePort(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// Short T3 for faster test
	opts := []hsmsss.ConnOption{hsmsss.WithT3Timeout(1 * time.Second)}
	passive := newEndpoint(t, ctx, port, true, false, opts) // no handler -> never replies
	active := newEndpoint(t, ctx, port, false, true, opts)
	defer closeEndpoint(t, passive)
	defer closeEndpoint(t, active)

	require.NoError(passive.conn.Open(false))
	require.NoError(active.conn.Open(false))
	waitState(t, active, hsms.SelectedState)

	msg, err := hsms.NewDataMessage(1, 1, true, testSessionID, []byte{0, 0, 0, 1}, secs2.NewASCIIItem("ping"))
	require.NoError(err)

	start := time.Now()
	_, err = active.session.SendMessage(msg)
	elapsed := time.Since(start)

	require.Error(err, "expected T3 timeout error")
	require.GreaterOrEqual(elapsed, 900*time.Millisecond, "T3 fired too early")
	require.LessOrEqual(elapsed, 2*time.Second, "T3 fired too late")
}

// ---------------------------------------------------------------------------
// TestDataMessage_WBitZero_FireAndForget
//
// Sends a message with W-bit=0. Sender should get nil reply immediately.
// Receiver should still get the message.
// ---------------------------------------------------------------------------
func TestDataMessage_WBitZero_FireAndForget(t *testing.T) {
	require := require.New(t)
	port := getFreePort(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	receivedCh := make(chan *hsms.DataMessage, 1)
	handler := func(msg *hsms.DataMessage, _ hsms.Session) {
		receivedCh <- msg
	}

	passive := newEndpoint(t, ctx, port, true, false, nil, handler)
	active := newEndpoint(t, ctx, port, false, true, nil)
	defer closeEndpoint(t, passive)
	defer closeEndpoint(t, active)

	require.NoError(passive.conn.Open(false))
	require.NoError(active.conn.Open(false))
	waitState(t, active, hsms.SelectedState)
	waitState(t, passive, hsms.SelectedState)

	// W-bit=false (fire-and-forget)
	msg, err := hsms.NewDataMessage(1, 1, false, testSessionID, []byte{0, 0, 0, 1}, secs2.NewASCIIItem("fire"))
	require.NoError(err)

	err = active.session.SendMessageAsync(msg)
	require.NoError(err)

	// Receiver should get the message
	select {
	case got := <-receivedCh:
		asciiVal, aErr := got.Item().ToASCII()
		require.NoError(aErr)
		require.Equal("fire", asciiVal)
	case <-time.After(3 * time.Second):
		t.Fatal("receiver did not get the fire-and-forget message")
	}
}

// ---------------------------------------------------------------------------
// TestDataMessage_ZeroLengthBody
//
// S1F1 with nil body -> echoed back as nil item.
// ---------------------------------------------------------------------------
func TestDataMessage_ZeroLengthBody(t *testing.T) {
	require := require.New(t)
	port := getFreePort(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	passive := newEndpoint(t, ctx, port, true, false, nil, echoHandler)
	active := newEndpoint(t, ctx, port, false, true, nil)
	defer closeEndpoint(t, passive)
	defer closeEndpoint(t, active)

	require.NoError(passive.conn.Open(false))
	require.NoError(active.conn.Open(false))
	waitState(t, active, hsms.SelectedState)
	waitState(t, passive, hsms.SelectedState)

	msg, err := hsms.NewDataMessage(1, 1, true, testSessionID, []byte{0, 0, 0, 1}, nil)
	require.NoError(err)

	reply, err := active.session.SendMessage(msg)
	require.NoError(err)
	require.NotNil(reply)

	// echoHandler replies with msg.Item(), which is nil/empty for nil body
	dataReply, ok := reply.(*hsms.DataMessage)
	require.True(ok)
	// Nil body should echo back as empty item
	if dataReply.Item() != nil {
		require.True(dataReply.Item().IsEmpty(), "expected empty item for nil body echo")
	}
}

// ---------------------------------------------------------------------------
// TestDataMessage_LargeItem
//
// Sends a message with a large (~8KB) binary item to verify that the HSMS
// framing handles payloads larger than typical control messages correctly.
// ---------------------------------------------------------------------------
func TestDataMessage_LargeItem(t *testing.T) {
	require := require.New(t)
	port := getFreePort(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	passive := newEndpoint(t, ctx, port, true, false, nil, echoHandler)
	active := newEndpoint(t, ctx, port, false, true, nil)
	defer closeEndpoint(t, passive)
	defer closeEndpoint(t, active)

	require.NoError(passive.conn.Open(false))
	require.NoError(active.conn.Open(false))
	waitState(t, active, hsms.SelectedState)
	waitState(t, passive, hsms.SelectedState)

	// Build a ~8KB binary payload
	const payloadSize = 8192
	payload := make([]byte, payloadSize)
	for i := range payload {
		payload[i] = byte(i % 256)
	}

	msg, err := hsms.NewDataMessage(6, 1, true, testSessionID, []byte{0, 0, 0, 1}, secs2.NewBinaryItem(payload))
	require.NoError(err)

	reply, err := active.session.SendMessage(msg)
	require.NoError(err)
	require.NotNil(reply)

	dataReply, ok := reply.(*hsms.DataMessage)
	require.True(ok)

	gotBytes, err := dataReply.Item().ToBinary()
	require.NoError(err)
	require.Len(gotBytes, payloadSize)

	// Verify first/last bytes
	require.Equal(byte(0), gotBytes[0])
	require.Equal(byte(255), gotBytes[255])
	require.Equal(byte(0), gotBytes[256])
}

// ---------------------------------------------------------------------------
// TestDataMessage_RawPeerRoundtrip
//
// Verifies that a raw TCP peer can exchange data messages with an hsmsss
// connection. The raw peer manually constructs HSMS data frames.
// ---------------------------------------------------------------------------
func TestDataMessage_RawPeerRoundtrip(t *testing.T) {
	require := require.New(t)
	port := getFreePort(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	receivedCh := make(chan *hsms.DataMessage, 1)
	handler := func(msg *hsms.DataMessage, s hsms.Session) {
		receivedCh <- msg
		if msg.FunctionCode()%2 == 1 {
			_ = s.ReplyDataMessage(msg, secs2.NewASCIIItem("pong"))
		}
	}

	passive := newEndpoint(t, ctx, port, true, false, nil, handler)
	defer closeEndpoint(t, passive)

	require.NoError(passive.conn.Open(false))
	time.Sleep(200 * time.Millisecond) // let listener start

	rawConn := rawClientSelectHandshake(t, port)
	defer rawConn.Close()

	// Build S1F1 data frame: 10-byte header + SECS-II ASCII "ping"
	// Header: sessionID(2) + headerByte2(stream|wbit) + headerByte3(function) + PType(0) + SType(0) + systemBytes(4)
	header := make([]byte, 10)
	binary.BigEndian.PutUint16(header[0:2], testSessionID)
	header[2] = 0x81                       // stream=1, W-bit=1
	header[3] = 1                          // function=1
	header[4] = 0                          // PType
	header[5] = 0                          // SType=0 (data message)
	copy(header[6:10], []byte{0, 0, 0, 1}) // system bytes

	// ASCII item "ping": format=0x41(ASCII, 1-byte length), length=4, data="ping"
	secs2Body := []byte{0x41, 0x04, 0x70, 0x69, 0x6E, 0x67} // ASCII "ping"

	frame := make([]byte, 0, len(header)+len(secs2Body))
	frame = append(frame, header...)
	frame = append(frame, secs2Body...)
	require.NoError(writeFrame(rawConn, frame))

	// Read reply
	replyFrame, err := readFrame(rawConn)
	require.NoError(err)
	require.GreaterOrEqual(len(replyFrame), 10, "reply frame too short")
	require.Equal(byte(0), replyFrame[5], "SType should be 0 for data message")
	require.Equal(byte(0x01), replyFrame[2]&0x7F, "stream should be 1")
	require.Equal(byte(2), replyFrame[3], "function should be 2 (reply)")

	// Verify handler received it
	select {
	case got := <-receivedCh:
		asciiVal, aErr := got.Item().ToASCII()
		require.NoError(aErr)
		require.Equal("ping", asciiVal)
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not receive message")
	}
}

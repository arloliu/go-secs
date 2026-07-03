package hsmsss

// data_msg_test.go — v2 port of the v1 tests/hsmsss_integration/data_msg_test.go message-plane
// suite, re-pointed onto the v2 public surface (New / Open(ctx, mode) / hsms.Connection /
// SendDataMessage(ctx, s, f, w, item) / SECS2Endpoint data handlers). It exercises the SECS-II
// data plane end-to-end over a real active+passive loopback pair and — as the headline regression —
// PROVES the T25.1 reply-parity guard (isSecondaryReply in hsms/connection.go's DeliverOwnedFrame):
// in symmetric concurrent W-bit traffic both peers' System-Bytes generators advance identically, so
// their primaries collide on the wire; the guard keeps a peer's PRIMARY out of the local sender's
// reply registry so each side's SendDataMessage correlates to ITS OWN reply, never the peer's.
//
// The file is package hsmsss (white-box, like the rest of the suite) so it reuses the shared harness
// (newEndpoint / newEndpointPair / waitSelected / echoHandler / closeEndpoint from
// integration_helpers_test.go and freeLoopbackPort / dialPassive / expectSelectRsp / peerReadFrame /
// selectReqFrame / frameBytes from passive_test.go / active_test.go / reader_test.go). All readiness
// waits poll conn.State() through require.Eventually (never time.Sleep-to-sync) and run under -race.

import (
	"encoding/binary"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 1. TestDataMessage_Roundtrip
//
// A real active+passive pair completes the Select handshake and exchanges one
// W-bit S1F1/S1F2 transaction: the passive echoHandler replies with the received
// item, and the active SendDataMessage receives the correlated F2 secondary with
// the item payload surviving the round-trip.
// ---------------------------------------------------------------------------
func TestDataMessage_Roundtrip(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)
	ctx := t.Context()

	passive := newEndpoint(t, port, false, nil, echoHandler)
	active := newEndpoint(t, port, true, nil)
	defer closeEndpoint(t, active)
	defer closeEndpoint(t, passive)

	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	waitSelected(t, active)
	waitSelected(t, passive)

	reply, err := active.conn.SendDataMessage(ctx, 1, 1, true /* W-bit */, secs2.A("ping"))
	require.NoError(t, err)
	require.NotNil(t, reply)
	require.Equal(t, uint8(1), reply.Stream(), "reply stream is S1")
	require.Equal(t, uint8(2), reply.Function(), "reply function is F2 (primary F+1)")

	replyItem, err := reply.Item()
	require.NoError(t, err)
	got, err := replyItem.ToASCII()
	require.NoError(t, err)
	require.Equal(t, "ping", got, "the echoed reply payload must equal the sent payload")
}

// ---------------------------------------------------------------------------
// 2. TestDataMessage_StabilityBidirectional
//
// Both endpoints register echoHandler and reach Selected. Over 50 rounds BOTH
// sides concurrently drive a W-bit S1F1 round-trip with a distinct, side- and
// round-tagged payload. Every send must succeed with the reply item equal to
// THAT side's sent payload — zero errors, zero cross-delivered payloads. This is
// the concurrency-stress form of the round-trip.
// ---------------------------------------------------------------------------
func TestDataMessage_StabilityBidirectional(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)
	ctx := t.Context()

	passive := newEndpoint(t, port, false, nil, echoHandler)
	active := newEndpoint(t, port, true, nil, echoHandler)
	defer closeEndpoint(t, active)
	defer closeEndpoint(t, passive)

	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	waitSelected(t, active)
	waitSelected(t, passive)

	const rounds = 50

	var (
		wg       sync.WaitGroup
		errCount atomic.Int32
		mismatch atomic.Int32
	)

	send := func(ep *endpoint, tag string) {
		defer wg.Done()
		for i := range rounds {
			payload := fmt.Sprintf("%s-%d", tag, i)
			reply, err := ep.conn.SendDataMessage(ctx, 1, 1, true, secs2.A(payload))
			if err != nil || reply == nil {
				errCount.Add(1)
				continue
			}
			item, iErr := reply.Item()
			if iErr != nil {
				errCount.Add(1)
				continue
			}
			got, aErr := item.ToASCII()
			if aErr != nil {
				errCount.Add(1)
				continue
			}
			if got != payload {
				mismatch.Add(1)
			}
		}
	}

	wg.Add(2)
	go send(active, "A")
	go send(passive, "B")
	wg.Wait()

	require.Zero(t, errCount.Load(), "expected zero send errors in bidirectional stability test")
	require.Zero(t, mismatch.Load(), "expected zero cross-delivered payloads (each reply must echo its own send)")
}

// ---------------------------------------------------------------------------
// 3. TestDataMessage_BidirectionalWBitCollision  ← HEADLINE, proves T25.1
//
// The deterministic collision proof. Both endpoints register echoHandler and reach
// Selected. Then, in LOCK-STEP rounds (both sides at the same send count, so their
// System-Bytes generators advance identically → colliding System Bytes on the wire),
// both sides CONCURRENTLY send a W-bit S1F1 with a side- and round-tagged distinct
// payload. Each side's SendDataMessage MUST receive back ITS OWN payload as the F2
// secondary — never the peer's colliding-System-Bytes PRIMARY.
//
// This exercises isSecondaryReply (hsms/connection.go): only a SECONDARY
// (!WaitBit && Function%2==0) is offered to the reply registry; a peer's PRIMARY
// (odd function) is routed to the local handler, so it is never mis-consumed as the
// local sender's reply even though both registries hold the same System-Bytes key.
//
// TEETH: reverting isSecondaryReply to offer ALL inbound data to RouteReply first
// (pre-T25.1) makes this test FAIL (payload mismatch and/or T3 timeout).
// ---------------------------------------------------------------------------
func TestDataMessage_BidirectionalWBitCollision(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)
	ctx := t.Context()

	passive := newEndpoint(t, port, false, nil, echoHandler)
	active := newEndpoint(t, port, true, nil, echoHandler)
	defer closeEndpoint(t, active)
	defer closeEndpoint(t, passive)

	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	waitSelected(t, active)
	waitSelected(t, passive)

	// v2 System-Bytes asymmetry: the ACTIVE side consumes one System Bytes value for its
	// Select.req (the passive only responds, echoing the req's System Bytes), so after the
	// handshake the two per-connection generators are offset by one. Prime the passive with
	// a single round-trip to realign them, so the concurrent W-bit sends below draw IDENTICAL
	// System Bytes each round — the deterministic collision the T25.1 guard must survive.
	// (echo replies reuse the primary's System Bytes and do NOT consume the generator, so once
	// aligned the two generators stay in lock-step across the rounds.)
	warm, warmErr := passive.conn.SendDataMessage(ctx, 1, 1, true, secs2.A("warmup"))
	require.NoError(t, warmErr)
	require.NotNil(t, warm)

	// Lock-step rounds: each side sends exactly one W-bit primary per round with a now-aligned
	// generator, so their System Bytes collide on the wire every round.
	for k := range 20 {
		var (
			wg             sync.WaitGroup
			aReply, bReply *hsms.DataMessage
			aErr, bErr     error
			aPayload       = fmt.Sprintf("A-%d", k)
			bPayload       = fmt.Sprintf("B-%d", k)
		)

		wg.Go(func() {
			aReply, aErr = active.conn.SendDataMessage(ctx, 1, 1, true, secs2.A(aPayload))
		})
		wg.Go(func() {
			bReply, bErr = passive.conn.SendDataMessage(ctx, 1, 1, true, secs2.A(bPayload))
		})
		wg.Wait()

		// Each side must correlate to its OWN reply, never the peer's colliding primary.
		require.NoErrorf(t, aErr, "active send failed at round %d (a colliding primary was mis-consumed as the reply, or T3 fired)", k)
		require.NoErrorf(t, bErr, "passive send failed at round %d (a colliding primary was mis-consumed as the reply, or T3 fired)", k)
		require.NotNil(t, aReply, "active must receive a reply at round %d", k)
		require.NotNil(t, bReply, "passive must receive a reply at round %d", k)

		// Collision precondition (guards the alignment invariant Finding 1 depends on): a reply
		// reuses its primary's System Bytes, so equal reply System Bytes prove both sides drew the
		// SAME System Bytes this round. If the generators ever drift (e.g. setup changes so the
		// active issues a second control request, or auto-linktest gets enabled), the sends stop
		// colliding and this test would silently pass even against a broken guard — the exact trap
		// Finding 1 originally fell into. Fail loudly here instead.
		require.Equalf(t, aReply.SystemBytes(), bReply.SystemBytes(),
			"round %d: collision precondition NOT met — System Bytes did not collide, so this test is toothless", k)

		aItem, err := aReply.Item()
		require.NoError(t, err)
		aGot, err := aItem.ToASCII()
		require.NoError(t, err)
		require.Equalf(t, aPayload, aGot, "round %d: active must receive its OWN payload, not the peer's colliding primary", k)

		bItem, err := bReply.Item()
		require.NoError(t, err)
		bGot, err := bItem.ToASCII()
		require.NoError(t, err)
		require.Equalf(t, bPayload, bGot, "round %d: passive must receive its OWN payload, not the peer's colliding primary", k)
	}
}

// ---------------------------------------------------------------------------
// 4. TestDataMessage_T3ReplyTimeout
//
// A passive endpoint with NO data handler never replies. The active side sends a
// W-bit S1F1 with a short T3; SendDataMessage must return ErrT3Timeout, and a T3
// timeout must NOT tear down the link (the connection stays Selected).
// ---------------------------------------------------------------------------
func TestDataMessage_T3ReplyTimeout(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)
	ctx := t.Context()

	// Short T3 on the sender so the drop surfaces fast; passive has no handler → never replies.
	passive := newEndpoint(t, port, false, nil)
	active := newEndpoint(t, port, true, []Option{WithConnectionOption(hsms.WithT3(1 * time.Second))})
	defer closeEndpoint(t, active)
	defer closeEndpoint(t, passive)

	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	waitSelected(t, active)
	waitSelected(t, passive)

	reply, err := active.conn.SendDataMessage(ctx, 1, 1, true, secs2.A("x"))
	require.ErrorIs(t, err, hsms.ErrT3Timeout, "a dropped W-bit reply must surface as ErrT3Timeout")
	require.Nil(t, reply)

	// T3 timeout is a transaction-level failure, NOT a link failure: the link stays Selected.
	require.Equal(t, hsms.SelectedState, active.conn.State(), "a T3 timeout must not tear down the link")
}

// ---------------------------------------------------------------------------
// 5. TestDataMessage_WBitZero_FireAndForget  ← proves Fix B end-to-end
//
// A fire-and-forget send (no W-bit) via the BLOCKING SendDataMessage: Fix B short-circuits it in
// sendWaitReply the instant the frame is on the wire, so the send returns (nil, nil) PROMPTLY
// (bounded well under T3) instead of waiting out the full T3 reply timer. The receiver still
// delivers the message to its handler.
//
// TEETH: reverting Fix B (deleting the fireAndForget short-circuit so a !W send registers-and-waits)
// makes the prompt-return assertion below fail — the send blocks the full T3 and returns
// ErrT3Timeout.
// ---------------------------------------------------------------------------
func TestDataMessage_WBitZero_FireAndForget(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)
	ctx := t.Context()

	received := make(chan string, 1)
	capture := func(msg *hsms.DataMessage, _ hsms.SECS2Endpoint) {
		item, err := msg.Item()
		if err != nil {
			return
		}
		s, err := item.ToASCII()
		if err != nil {
			return
		}
		select {
		case received <- s:
		default:
		}
	}

	passive := newEndpoint(t, port, false, nil, capture)
	// Pin a generous T3 on the active side: with Fix B the send returns promptly regardless, but a
	// regression (no short-circuit) would block the full T3, so a large T3 makes the teeth bite.
	active := newEndpoint(t, port, true, []Option{WithConnectionOption(hsms.WithT3(5 * time.Second))})
	defer closeEndpoint(t, active)
	defer closeEndpoint(t, passive)

	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	waitSelected(t, active)
	waitSelected(t, passive)

	// Fire-and-forget via the BLOCKING SendDataMessage(replyExpected=false): Fix B returns
	// (nil, nil) the instant the frame is on the wire — no reply-channel register, no T3 arm.
	type sendResult struct {
		reply *hsms.DataMessage
		err   error
	}
	done := make(chan sendResult, 1)
	start := time.Now()
	go func() {
		reply, err := active.conn.SendDataMessage(ctx, 1, 1, false, secs2.A("hi"))
		done <- sendResult{reply: reply, err: err}
	}()

	select {
	case res := <-done:
		require.NoError(t, res.err, "a !W SendDataMessage must return promptly with no error (Fix B), not ErrT3Timeout")
		require.Nil(t, res.reply, "a fire-and-forget send has no reply to return")
		require.Less(t, time.Since(start), 1*time.Second,
			"a !W SendDataMessage must return well under T3 (=5s); a T3-bounded wait would be the Fix B regression")
	case <-time.After(2 * time.Second):
		t.Fatal("fire-and-forget SendDataMessage(!W) blocked > 2s (< T3=5s): Fix B short-circuit missing")
	}

	// The receiver still delivered the message to its handler.
	require.Eventually(t, func() bool {
		select {
		case got := <-received:
			require.Equal(t, "hi", got)
			return true
		default:
			return false
		}
	}, 3*time.Second, 10*time.Millisecond, "receiver must observe the fire-and-forget message")
}

// ---------------------------------------------------------------------------
// 6. TestDataMessage_ZeroLengthBody
//
// A W-bit round-trip carrying a zero-length ASCII item. The empty body must survive
// the round-trip: the echoed reply carries the matching empty ASCII payload.
// ---------------------------------------------------------------------------
func TestDataMessage_ZeroLengthBody(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)
	ctx := t.Context()

	passive := newEndpoint(t, port, false, nil, echoHandler)
	active := newEndpoint(t, port, true, nil)
	defer closeEndpoint(t, active)
	defer closeEndpoint(t, passive)

	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	waitSelected(t, active)
	waitSelected(t, passive)

	reply, err := active.conn.SendDataMessage(ctx, 1, 1, true, secs2.A(""))
	require.NoError(t, err)
	require.NotNil(t, reply)

	replyItem, err := reply.Item()
	require.NoError(t, err)
	got, err := replyItem.ToASCII()
	require.NoError(t, err)
	require.Equal(t, "", got, "a zero-length ASCII body must round-trip as an empty payload")
}

// ---------------------------------------------------------------------------
// 7. TestDataMessage_LargeItem
//
// A W-bit round-trip carrying an ~8 KB binary item, larger than any control frame.
// The returned bytes must equal the sent bytes exactly — framing and decode preserve
// large payloads.
// ---------------------------------------------------------------------------
func TestDataMessage_LargeItem(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)
	ctx := t.Context()

	passive := newEndpoint(t, port, false, nil, echoHandler)
	active := newEndpoint(t, port, true, nil)
	defer closeEndpoint(t, active)
	defer closeEndpoint(t, passive)

	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	waitSelected(t, active)
	waitSelected(t, passive)

	const payloadSize = 8192
	payload := make([]byte, payloadSize)
	for i := range payload {
		payload[i] = byte(i % 256)
	}

	reply, err := active.conn.SendDataMessage(ctx, 6, 1, true, secs2.B(payload))
	require.NoError(t, err)
	require.NotNil(t, reply)

	replyItem, err := reply.Item()
	require.NoError(t, err)
	got, err := replyItem.ToBinary()
	require.NoError(t, err)
	require.Equal(t, payload, got, "an ~8 KB binary payload must round-trip byte-for-byte")
}

// ---------------------------------------------------------------------------
// 8. TestDataMessage_RawPeerRoundtrip
//
// A hand-rolled raw TCP peer selects a passive v2 endpoint (echoHandler), then sends
// a raw W-bit S1F1 data frame and reads the endpoint's S1F2 reply. Confirms the v2
// endpoint interoperates with a scripted peer: the reply is a well-formed F2 secondary
// carrying the echoed body.
// ---------------------------------------------------------------------------
func TestDataMessage_RawPeerRoundtrip(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)
	ctx := t.Context()

	passive := newEndpoint(t, port, false, nil, echoHandler)
	defer closeEndpoint(t, passive)

	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))

	client := dialPassive(t, port)
	defer func() { _ = client.Close() }()

	// Raw Select handshake: send Select.req, read+verify the passive's success Select.rsp.
	sb := [4]byte{0x00, 0x00, 0x00, 0x2A}
	_, err := client.Write(selectReqFrame(sb))
	require.NoError(t, err)
	expectSelectRsp(t, client)

	waitSelected(t, passive)

	// Build a raw W-bit S1F1 data frame. dataFrame strips the W-bit (it builds no-reply
	// frames), so construct the 10-byte header inline with the W-bit set in the stream byte.
	body := secs2.A("ping").ToBytes()
	header := make([]byte, 10)
	binary.BigEndian.PutUint16(header[0:2], 0xFFFF) // HSMS-SS control/data session ID
	header[2] = 0x01 | 0x80                         // stream 1, W-bit set
	header[3] = 0x01                                // function 1
	header[4] = 0                                   // PType
	header[5] = 0                                   // SType 0 = data
	copy(header[6:10], sb[:])                       // system bytes
	frame := frameBytes(uint32(10+len(body)), header, body)
	_, err = client.Write(frame)
	require.NoError(t, err)

	// Read the endpoint's echoed S1F2 reply.
	reply, err := peerReadFrame(client, 3*time.Second)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(reply), 10, "reply frame must carry a 10-byte header")
	require.Equal(t, byte(0), reply[5], "reply SType must be 0 (data message)")
	require.Equal(t, byte(1), reply[2]&0x7F, "reply stream must be 1")
	require.Zero(t, reply[2]&0x80, "reply must not set the W-bit (a secondary expects no reply)")
	require.Equal(t, byte(2), reply[3], "reply function must be 2 (S1F2 secondary)")
	require.Equal(t, sb[:], reply[6:10], "reply must reuse the primary's system bytes (E37 §8.2.6.9)")
	require.Equal(t, body, reply[10:], "reply body must echo the sent item bytes")
}

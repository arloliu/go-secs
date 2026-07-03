package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// parityFactories is the set of real transports every cross-transport parity scenario runs over.
// Each scenario is a single table-driven test that sub-tests once per factory, so HSMS-SS and
// SECS-I are held to the identical behaviour through the shared hsms.Connection surface.
func parityFactories() []transportFactory {
	return []transportFactory{hsmsssFactory(), secs1Factory()}
}

// asciiBody encodes s as the raw SECS-II body bytes of an ASCII item — the transport-agnostic
// representation the scripted peers exchange (HSMS message body / reassembled SECS-I block body).
func asciiBody(s string) []byte { return secs2.NewASCIIItem(s).ToBytes() }

// latestScriptable returns the current-generation peer as a scriptablePeer, failing the test if the
// live peer does not implement the extended scripting surface.
func latestScriptable(t *testing.T, df *dialFactory) scriptablePeer {
	t.Helper()

	peer, ok := df.latest().(scriptablePeer)
	require.True(t, ok)

	return peer
}

// requireASCIIReply asserts that a reply DataMessage carries an ASCII item equal to want.
func requireASCIIReply(t *testing.T, reply *hsms.DataMessage, want string) {
	t.Helper()

	item, err := reply.Item()
	require.NoError(t, err)

	got, err := item.ToASCII()
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// requireASCIIBody asserts that a raw SECS-II body decodes to an ASCII item equal to want.
func requireASCIIBody(t *testing.T, body []byte, want string) {
	t.Helper()

	item, err := secs2.Decode(body)
	require.NoError(t, err)

	got, err := item.ToASCII()
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// TestParity_OpenRoundtripClose is the baseline cross-transport scenario: the real transport opens
// over net.Pipe, reaches Selected against the scripted peer (SECS-I auto-commits to Selected the
// instant the line is up — there is no Select handshake), completes a W-bit round-trip whose reply
// the peer echoes, and closes cleanly. It proves the shared core runs deterministically over the
// in-memory pipe with no real network, for both the full- and half-duplex transports.
func TestParity_OpenRoundtripClose(t *testing.T) {
	for _, f := range parityFactories() {
		t.Run(f.name, func(t *testing.T) {
			conn, _ := f.open(t) // dials over net.Pipe; Open(ctx, OpenWaitSelected) -> Selected
			defer f.close(t, conn)

			reply, err := conn.SendDataMessage(context.Background(), 1, 1, true, secs2.NewASCIIItem("ping"))
			require.NoError(t, err)
			requireASCIIReply(t, reply, "ping") // the scripted peer echoes W-bit data by default
		})
	}
}

// TestParity_HandlerReply drives the reply direction through a registered handler: the connection's
// AddDataMessageHandler answers each inbound primary via ep.ReplyDataMessage, and the PEER originates
// the W-bit primary. The handler is registered before the peer send so the reply is deterministic.
func TestParity_HandlerReply(t *testing.T) {
	for _, f := range parityFactories() {
		t.Run(f.name, func(t *testing.T) {
			conn, df := f.open(t)
			defer f.close(t, conn)

			// The handler echoes the primary's own item straight back as its secondary reply.
			conn.AddDataMessageHandler(func(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
				item, err := msg.Item()
				if err != nil {
					return
				}
				_ = ep.ReplyDataMessage(context.Background(), msg, item)
			})

			peer := latestScriptable(t, df)

			reply, err := peer.sendPrimary(1, 1, asciiBody("handler-echo"))
			require.NoError(t, err)
			requireASCIIBody(t, reply, "handler-echo")
		})
	}
}

// TestParity_FireAndForget drives a non-W-bit SendDataMessageAsync (reply NOT expected) and asserts
// the peer observes it on its inbound() stream within a bounded timeout. No reply is produced.
func TestParity_FireAndForget(t *testing.T) {
	for _, f := range parityFactories() {
		t.Run(f.name, func(t *testing.T) {
			conn, df := f.open(t)
			defer f.close(t, conn)

			peer := latestScriptable(t, df)

			require.NoError(t, conn.SendDataMessageAsync(context.Background(), 1, 1, false, secs2.NewASCIIItem("fire-and-forget")))

			select {
			case body := <-peer.inbound():
				requireASCIIBody(t, body, "fire-and-forget")
			case <-time.After(2 * time.Second):
				t.Fatal("peer did not observe the fire-and-forget message")
			}
		})
	}
}

// TestParity_ConcurrentBidirectional guards reply correlation when both ends have a W-bit exchange
// outstanding: the connection's registered handler echoes an inbound primary while the connection
// also issues its own W-bit send (which the peer auto-echoes). Each direction must get its OWN
// correct reply, with no System-Bytes cross-talk.
//
// For HSMS this is genuine full-duplex concurrency — the connection send runs in a goroutine while
// the peer originates on the test goroutine, so both transactions are in flight at once; this is the
// real guard. For SECS-I the line is half-duplex, so genuine simultaneity would require ENQ-contention
// arbitration, which is deliberately scoped to the per-package secs1 suite rather than this parity
// table. The SECS-I sub-run therefore drives the strongest deterministic form the half-duplex line
// allows: the two directions serialize into distinct line turns, and each is still asserted to receive
// its own correlated reply.
func TestParity_ConcurrentBidirectional(t *testing.T) {
	for _, f := range parityFactories() {
		t.Run(f.name, func(t *testing.T) {
			conn, df := f.open(t)
			defer f.close(t, conn)

			conn.AddDataMessageHandler(func(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
				item, err := msg.Item()
				if err != nil {
					return
				}
				_ = ep.ReplyDataMessage(context.Background(), msg, item)
			})

			peer := latestScriptable(t, df)

			if f.name == "hsmsss" {
				// Genuine full-duplex concurrency: both W-bit transactions in flight simultaneously.
				var (
					wg        sync.WaitGroup
					connReply *hsms.DataMessage
					connErr   error
				)

				wg.Go(func() {
					connReply, connErr = conn.SendDataMessage(context.Background(), 1, 1, true, secs2.NewASCIIItem("from-conn"))
				})

				peerReply, peerErr := peer.sendPrimary(3, 3, asciiBody("from-peer"))
				wg.Wait()

				require.NoError(t, connErr)
				requireASCIIReply(t, connReply, "from-conn")

				require.NoError(t, peerErr)
				requireASCIIBody(t, peerReply, "from-peer")

				return
			}

			// SECS-I: deterministic serialized bidirectional exchange (distinct line turns).
			connReply, connErr := conn.SendDataMessage(context.Background(), 1, 1, true, secs2.NewASCIIItem("from-conn"))
			require.NoError(t, connErr)
			requireASCIIReply(t, connReply, "from-conn") // connection→peer: peer auto-echoes

			peerReply, peerErr := peer.sendPrimary(3, 3, asciiBody("from-peer"))
			require.NoError(t, peerErr)
			requireASCIIBody(t, peerReply, "from-peer") // peer→connection: handler replies
		})
	}
}

// TestParity_ReplyTimeoutRecovers shortens the reply timer (T3), withholds the peer's reply so a
// W-bit send fails on that protocol timer, then restores replies and proves a fresh send still
// round-trips — the connection stays usable after the reply timeout. The send context is kept long so
// T3, not caller cancellation, is what surfaces the error, uniformly across both transports.
func TestParity_ReplyTimeoutRecovers(t *testing.T) {
	for _, f := range parityFactories() {
		t.Run(f.name, func(t *testing.T) {
			conn, df := f.open(t)
			defer f.close(t, conn)

			peer := latestScriptable(t, df)

			// Shorten the reply timer (T3) so a withheld reply fails on the PROTOCOL reply timer, not
			// on caller cancellation — the send context below stays generously long, so T3 is what
			// fires. A caller-cancelled send is a different path and would not prove the reply-timeout
			// behaviour this scenario is named for.
			require.NoError(t, conn.UpdateConfigOptions(hsms.WithT3(200*time.Millisecond)))

			// Withhold the reply: the W-bit send must fail when its T3 reply timer expires.
			peer.setWithholdReplies(true)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			_, err := conn.SendDataMessage(ctx, 1, 1, true, secs2.NewASCIIItem("withheld"))
			require.Error(t, err)

			// Restore replies: a fresh W-bit send must succeed and round-trip.
			peer.setWithholdReplies(false)

			reply, err := conn.SendDataMessage(context.Background(), 1, 1, true, secs2.NewASCIIItem("after-timeout"))
			require.NoError(t, err)
			requireASCIIReply(t, reply, "after-timeout")
		})
	}
}

// TestParity_InvoluntaryDropReconnects proves the lifecycle survives an involuntary peer disconnect:
// after Open reaches Selected, the current-generation peer is dropped abruptly (its pipe end closes,
// so the connection's recv sees EOF and tears down). The lifecycle keeps running after Open returned,
// so the core's reconnect loop redials, mints a fresh-generation peer, and drives the FSM back to
// Selected — after which a fresh W-bit round-trip succeeds against that new peer. Both transports
// reconnect through the shared core loop.
func TestParity_InvoluntaryDropReconnects(t *testing.T) {
	for _, f := range parityFactories() {
		t.Run(f.name, func(t *testing.T) {
			// Register the recorder before Open so it observes both the initial Selected and the
			// re-Selected that follows the drop.
			rec, handler := newStateRecorder()
			conn, df := f.openWith(t, func(c hsms.Connection) {
				c.AddConnStateChangeHandler(handler)
			})
			defer f.close(t, conn)

			require.Equal(t, hsms.SelectedState, conn.State())

			// Abruptly drop the live peer: the connection's recv sees EOF and the FSM tears down.
			dropped := latestScriptable(t, df)
			dropped.dropLine()

			// First observe the fresh teardown, then the fresh re-Select. Waiting on NotConnected first
			// also absorbs the initial Selected notification (delivered asynchronously by the notifier, it
			// may still be in flight when Open returns), so the subsequent Selected wait cannot mistake the
			// stale initial Selected for the reconnect's — it detects the genuine re-Selected edge, which
			// only a fresh dial (a new generation) can produce.
			require.True(t, rec.awaitState(hsms.NotConnectedState, 3*time.Second),
				"connection did not tear down after the involuntary drop")
			require.True(t, rec.awaitState(hsms.SelectedState, 3*time.Second),
				"connection did not re-Select after the involuntary drop")

			// The reconnect must have dialed a fresh-generation peer, distinct from the dropped one.
			reconnected := latestScriptable(t, df)
			require.True(t, reconnected != dropped, "reconnect must dial a fresh-generation peer")

			// A fresh W-bit round-trip must now succeed against the new-generation peer.
			reply, err := conn.SendDataMessage(context.Background(), 1, 1, true, secs2.NewASCIIItem("after-reconnect"))
			require.NoError(t, err)
			requireASCIIReply(t, reply, "after-reconnect")
		})
	}
}

// dataMetricsSnapshot captures the three data-message counters the parity claim tracks, so a scenario
// can assert the exact delta a fixed set of operations produces.
type dataMetricsSnapshot struct {
	send uint64
	recv uint64
	err  uint64
}

// snapshotDataMetrics reads the current data-message counters. The counters are atomic and every
// operation that moves them completes synchronously before the driving call returns, so a snapshot
// taken right after the operations reflects them exactly.
func snapshotDataMetrics(m *hsms.ConnectionMetrics) dataMetricsSnapshot {
	return dataMetricsSnapshot{
		send: m.DataMsgSendCount(),
		recv: m.DataMsgRecvCount(),
		err:  m.DataMsgErrCount(),
	}
}

// TestParity_MetricsMoveIdentically pins the exact data-message counter deltas a fixed operation set
// produces, and asserts they are the SAME for both transports — that is the parity claim. The set is
// metricsRoundtrips successful W-bit round-trips (each commits one primary to the wire and receives
// one reply) followed by one withheld W-bit send whose reply never arrives, so the reply timer expires
// and the send is counted as a data-send error. The asserted deltas are concrete numbers, held
// identically across both transports.
//
// The reply timer (not caller cancellation) is what the send-error counter tracks, so the withheld
// send runs against a shortened reply timeout with a generously long context — the timer fires first
// and the failure is recorded as a send error.
func TestParity_MetricsMoveIdentically(t *testing.T) {
	const (
		metricsRoundtrips = 3

		// Deltas the fixed operation set must produce, IDENTICALLY on both transports:
		wantSendDelta = metricsRoundtrips + 1 // every round-trip primary + the withheld primary reach the wire
		wantRecvDelta = metricsRoundtrips     // only the round-trips get a reply; the withheld send gets none
		wantErrDelta  = 1                     // the one withheld W-bit send fails when its reply timer expires
	)

	for _, f := range parityFactories() {
		t.Run(f.name, func(t *testing.T) {
			conn, df := f.open(t)
			defer f.close(t, conn)

			peer := latestScriptable(t, df)

			before := snapshotDataMetrics(conn.Metrics())

			for i := 0; i < metricsRoundtrips; i++ {
				reply, err := conn.SendDataMessage(context.Background(), 1, 1, true, secs2.NewASCIIItem("metric-roundtrip"))
				require.NoError(t, err)
				requireASCIIReply(t, reply, "metric-roundtrip")
			}

			// Shorten the reply timeout so the withheld send fails on the reply timer (a counted data-send
			// error) rather than on caller cancellation (a caller action, deliberately not counted). The
			// round-trips above already completed well within the default timeout.
			require.NoError(t, conn.UpdateConfigOptions(hsms.WithT3(200*time.Millisecond)))

			// One withheld W-bit send: the primary reaches the wire but the reply never comes, so the
			// (now short) reply timer expires and the send is counted as a data-send error. The context is
			// generously long so the reply timer — not the context — is what fires.
			peer.setWithholdReplies(true)

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()

			_, err := conn.SendDataMessage(ctx, 1, 1, true, secs2.NewASCIIItem("metric-withheld"))
			require.Error(t, err)

			after := snapshotDataMetrics(conn.Metrics())

			require.Equal(t, uint64(wantSendDelta), after.send-before.send, "DataMsgSendCount delta")
			require.Equal(t, uint64(wantRecvDelta), after.recv-before.recv, "DataMsgRecvCount delta")
			require.Equal(t, uint64(wantErrDelta), after.err-before.err, "DataMsgErrCount delta")
		})
	}
}

// TestParity_StateChangeSequence asserts each transport emits ITS OWN connection-state sequence across
// a clean open-then-close — the sequences are transport-specific, NOT identical. HSMS-SS runs the full
// E37 handshake, so it steps NotConnected->NotSelected->Selected on the way up and ...->NotConnected on
// Close. SECS-I has no Select handshake — a live line IS the selected session — so it auto-commits
// straight to Selected with NO NotSelected edge, then ...->NotConnected on Close. The recorder is
// registered before Open so the connect transitions are observed.
func TestParity_StateChangeSequence(t *testing.T) {
	for _, f := range parityFactories() {
		t.Run(f.name, func(t *testing.T) {
			rec, handler := newStateRecorder()
			conn, _ := f.openWith(t, func(c hsms.Connection) {
				c.AddConnStateChangeHandler(handler)
			})

			require.Equal(t, hsms.SelectedState, conn.State())

			// Close joins the state notifier before returning, so the full sequence — including the
			// terminal ...->NotConnected edge — is settled and recorded once f.close returns.
			f.close(t, conn)

			var want []stateEdge
			switch f.name {
			case "hsmsss":
				want = []stateEdge{
					{prev: hsms.NotConnectedState, next: hsms.NotSelectedState},
					{prev: hsms.NotSelectedState, next: hsms.SelectedState},
					{prev: hsms.SelectedState, next: hsms.NotConnectedState},
				}
			case "secs1":
				want = []stateEdge{
					{prev: hsms.NotConnectedState, next: hsms.SelectedState},
					{prev: hsms.SelectedState, next: hsms.NotConnectedState},
				}
			default:
				t.Fatalf("unhandled transport %q", f.name)
			}

			got := rec.pairs()
			require.Equal(t, want, got, "%s state-change sequence", f.name)

			// SECS-I must never pass through NotSelected — it has no Select handshake.
			if f.name == "secs1" {
				for _, e := range got {
					require.NotEqual(t, hsms.NotSelectedState, e.prev, "SECS-I must not emit a NotSelected edge")
					require.NotEqual(t, hsms.NotSelectedState, e.next, "SECS-I must not emit a NotSelected edge")
				}
			}
		})
	}
}

// TestParity_GenerationIsolationAcrossReopen races a burst of async sends against Close, then reopens
// the SAME connection and proves the reopened generation is clean and uncontaminated by the prior one.
// The burst sends carry a distinguishable payload and race teardown: each may individually succeed or
// fail cleanly, and the first guard is that Close-under-load neither panics nor deadlocks and returns.
// The connection is then reopened (same connection, same dial factory — a fresh generation) and must
// complete a clean W-bit round-trip, with the reopened generation's peer never observing the prior
// generation's payload.
//
// Reusing the same connection is the point: it exercises the connection's own Close->Open reset path,
// so the guard is that reopening after a concurrent Close-under-load neither panics/deadlocks nor lets
// a prior generation's in-flight state corrupt the fresh one. (Each generation still gets its own
// net.Pipe, so raw byte isolation is additionally structural.)
func TestParity_GenerationIsolationAcrossReopen(t *testing.T) {
	const burst = 16

	for _, f := range parityFactories() {
		t.Run(f.name, func(t *testing.T) {
			conn, df := f.open(t)

			// Fire the burst and Close concurrently so the async sends genuinely race teardown.
			var wg sync.WaitGroup
			release := make(chan struct{})

			for i := 0; i < burst; i++ {
				wg.Go(func() {
					<-release
					// A fresh item per send: never share a pooled item across concurrent sends.
					_ = conn.SendDataMessageAsync(context.Background(), 1, 1, false, secs2.NewASCIIItem("stale-gen-one"))
				})
			}

			close(release)

			closeErr := make(chan error, 1)
			go func() { closeErr <- conn.Close() }()

			select {
			case err := <-closeErr:
				require.NoError(t, err, "Close must complete cleanly under concurrent async-send load")
			case <-time.After(5 * time.Second):
				t.Fatal("Close did not return under concurrent async-send load (possible deadlock)")
			}

			wg.Wait()

			// Generation two on the SAME connection lineage: reopen the very connection that was just
			// closed under load. Reopen dials a fresh generation through the same dial factory, so this
			// exercises the connection's own Close->Open reset path — a prior generation's in-flight
			// state must not leak into the fresh one, and the reopened connection must round-trip cleanly.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			require.NoError(t, conn.Open(ctx, hsms.OpenWaitSelected))
			require.Equal(t, hsms.SelectedState, conn.State())
			defer func() { _ = conn.Close() }()

			peer2 := latestScriptable(t, df) // the reopened generation's peer (same factory, newest)

			reply, err := conn.SendDataMessage(context.Background(), 1, 1, true, secs2.NewASCIIItem("gen-two-clean"))
			require.NoError(t, err)
			requireASCIIReply(t, reply, "gen-two-clean")

			// Drain everything the reopened generation's peer observed: it must be only the clean
			// payload, never the prior generation's.
			for {
				select {
				case body := <-peer2.inbound():
					item, derr := secs2.Decode(body)
					require.NoError(t, derr)
					got, derr := item.ToASCII()
					require.NoError(t, derr)
					require.NotEqual(t, "stale-gen-one", got, "a prior generation's payload leaked into the reopened generation")
				default:
					return
				}
			}
		})
	}
}

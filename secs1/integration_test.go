package secs1

// integration_test.go — T6: first true end-to-end validation of the secs1 transport over its
// public API (secs1.New / secs1.NewConfig / hsms.Connection). Stands up a real active↔passive
// secs1 pair over a loopback TCP socket and exercises the full send→wire→receive→decode→reply
// path that T4 (send adapter) and T5 (inbound assembler) built in isolation. Prior tasks tested
// halves with scripted-peers and mock runtimes; T6 wires both through the real engine.
//
// Role wiring (SEMI E4 §8.2):
//   - passive endpoint: WithPassive(), WithEquipment() — equipment, listens. Accepts R=0.
//   - active endpoint:  WithActive(),  WithHost()      — host, dials. Accepts R=1.
//
// Active (host) sends R=0 → passive equipment's assembler accepts (rBit != isEquip → 0 != true).
// Passive replies R=1 → active host's assembler accepts (rBit != isEquip → 1 != false).
// Both endpoints share DeviceID=0; the active dials after the passive's Open returns (which
// completes ListenTCP synchronously, so no race between bind and dial).

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// secs1E is a thin wrapper for integration test readability.
type secs1E struct{ conn hsms.Connection }

// newSECS1Passive builds a passive (equipment) endpoint on the given port. Integration timers are
// short-but-safe: T1=100 ms, T2=2 s, T4=2 s, T3=3 s (core default). handlers are registered
// before Open. The passive role and IsEquip=true are hardcoded (only the active can override T3).
func newSECS1Passive(t *testing.T, port int, handlers ...hsms.DataMessageHandler) *secs1E {
	t.Helper()

	cfg, err := NewConfig("127.0.0.1", port,
		WithPassive(),
		WithEquipment(), // equipment role: accepts R=0 from host, replies R=1
		WithDeviceID(0),
		WithT1(100*time.Millisecond),
		WithT2(2*time.Second),
		WithT4(2*time.Second),
		WithConnectionOption(hsms.WithT3(3*time.Second)),
	)
	require.NoError(t, err)

	conn, err := New(cfg)
	require.NoError(t, err)

	if len(handlers) > 0 {
		conn.AddDataMessageHandler(handlers...)
	}

	return &secs1E{conn: conn}
}

// newSECS1Active builds an active (host) endpoint targeting the given port. Integration timers
// default to T1=100 ms, T2=2 s, T4=2 s, T3=3 s; opts append after the base set so a test can
// override T3 or other knobs (e.g. the T3-timeout test shortens T3 to 500 ms).
func newSECS1Active(t *testing.T, port int, opts ...Option) *secs1E {
	t.Helper()

	base := make([]Option, 0, 7+len(opts))
	base = append(base,
		WithActive(),
		WithHost(), // host role: sends R=0, accepts R=1 replies
		WithDeviceID(0),
		WithT1(100*time.Millisecond),
		WithT2(2*time.Second),
		WithT4(2*time.Second),
		WithConnectionOption(hsms.WithT3(3*time.Second)), // overridable via opts
	)
	base = append(base, opts...)

	cfg, err := NewConfig("127.0.0.1", port, base...)
	require.NoError(t, err)

	conn, err := New(cfg)
	require.NoError(t, err)

	return &secs1E{conn: conn}
}

// waitSECS1Selected polls conn.State() until SelectedState or a generous timeout.
// secs1 "Selected" means the TCP link is established and usable — no HSMS Select handshake;
// the transport auto-commits Selected at TCPUp (D5b-2 / G-B).
func waitSECS1Selected(t *testing.T, ep *secs1E) {
	t.Helper()
	require.Eventually(t, func() bool {
		return ep.conn.State() == hsms.SelectedState
	}, 10*time.Second, 5*time.Millisecond, "timeout waiting for secs1 SelectedState")
}

// closeSECS1Endpoint closes ep and asserts no error.
func closeSECS1Endpoint(t *testing.T, ep *secs1E) {
	t.Helper()
	if ep == nil || ep.conn == nil {
		return
	}
	require.NoError(t, ep.conn.Close())
}

// secs1EchoHandler replies to odd-function primaries by echoing the received item via
// ReplyDataMessage. Even-function messages (secondaries / replies) are ignored because
// handlers run on the receive path and secondaries are already consumed by the reply registry.
func secs1EchoHandler(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
	if msg.Function()%2 != 1 {
		return
	}

	item, err := msg.Item()
	if err != nil {
		return
	}

	_ = ep.ReplyDataMessage(context.Background(), msg, item)
}

// ---------------------------------------------------------------------------
// (a) TestSECS1_Roundtrip_SingleBlock
//
// Active host sends W-bit S1F1 with a small ASCII payload; passive equipment's echo
// handler replies S1F2. Asserts reply non-nil, S1F2, payload survives the round-trip.
// Payload is small enough to fit in one SECS-I block (encoded body ≤ 244 bytes).
// ---------------------------------------------------------------------------

func TestSECS1_Roundtrip_SingleBlock(t *testing.T) {
	t.Parallel()

	port := freePort(t)
	ctx := t.Context()

	passive := newSECS1Passive(t, port, secs1EchoHandler)
	active := newSECS1Active(t, port)
	defer closeSECS1Endpoint(t, active)
	defer closeSECS1Endpoint(t, passive)

	// Open passive first: startPassive completes ListenTCP synchronously so the active's dial
	// finds a bound listener without a race.
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	waitSECS1Selected(t, passive)
	waitSECS1Selected(t, active)

	reply, err := active.conn.SendDataMessage(ctx, 1, 1, true /* W-bit */, secs2.A("ping"))
	require.NoError(t, err)
	require.NotNil(t, reply)
	require.Equal(t, uint8(1), reply.Stream(), "reply stream must be S1")
	require.Equal(t, uint8(2), reply.Function(), "reply function must be F2 (primary F+1 secondary)")

	replyItem, err := reply.Item()
	require.NoError(t, err)
	got, err := replyItem.ToASCII()
	require.NoError(t, err)
	require.Equal(t, "ping", got, "single-block ASCII payload must survive the round-trip")
}

// ---------------------------------------------------------------------------
// (a) TestSECS1_Roundtrip_MultiBlock
//
// Active host sends W-bit S1F1 carrying a 600-character ASCII payload.
// Encoded SECS-II body: 1 (format byte 0x42) + 2 (length bytes for 600>255) + 600 = 603 bytes.
// At 244 bytes/block: ceil(603/244) = 3 SECS-I blocks (244, 244, 115).
// This proves T4 splitBody + T5 assembleFrame integrate end-to-end over the live wire.
// ---------------------------------------------------------------------------

func TestSECS1_Roundtrip_MultiBlock(t *testing.T) {
	t.Parallel()

	port := freePort(t)
	ctx := t.Context()

	// 600-char ASCII → 603-byte encoded body → 3 blocks (244 + 244 + 115).
	const payloadLen = 600
	payload := strings.Repeat("x", payloadLen)

	passive := newSECS1Passive(t, port, secs1EchoHandler)
	active := newSECS1Active(t, port)
	defer closeSECS1Endpoint(t, active)
	defer closeSECS1Endpoint(t, passive)

	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	waitSECS1Selected(t, passive)
	waitSECS1Selected(t, active)

	reply, err := active.conn.SendDataMessage(ctx, 1, 1, true /* W-bit */, secs2.A(payload))
	require.NoError(t, err)
	require.NotNil(t, reply)
	require.Equal(t, uint8(1), reply.Stream(), "reply stream must be S1")
	require.Equal(t, uint8(2), reply.Function(), "reply function must be F2")

	replyItem, err := reply.Item()
	require.NoError(t, err)
	got, err := replyItem.ToASCII()
	require.NoError(t, err)
	require.Equal(t, payload, got,
		"600-char multi-block payload (603 encoded bytes, 3 SECS-I blocks) must round-trip byte-for-byte")
}

// ---------------------------------------------------------------------------
// (b) TestSECS1_T3ReplyTimeout
//
// Passive has NO data handler — it never replies. Active sends a W-bit S1F1 with a
// short T3 (500 ms). SendDataMessage must return hsms.ErrT3Timeout and return bounded
// near T3, not hang. A T3 timeout is a transaction-level event: the link stays Selected.
// ---------------------------------------------------------------------------

func TestSECS1_T3ReplyTimeout(t *testing.T) {
	t.Parallel()

	port := freePort(t)
	ctx := t.Context()

	const shortT3 = 500 * time.Millisecond

	// Passive has no handler: receives the primary but never replies.
	passive := newSECS1Passive(t, port)
	active := newSECS1Active(t, port, WithConnectionOption(hsms.WithT3(shortT3)))
	defer closeSECS1Endpoint(t, active)
	defer closeSECS1Endpoint(t, passive)

	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	waitSECS1Selected(t, passive)
	waitSECS1Selected(t, active)

	reply, err := active.conn.SendDataMessage(ctx, 1, 1, true /* W-bit */, secs2.A("x"))
	require.ErrorIs(t, err, hsms.ErrT3Timeout,
		"a W-bit send with no replying peer must surface as ErrT3Timeout")
	require.Nil(t, reply)

	// A T3 timeout is NOT a line fault: the secs1 link must remain Selected.
	require.Equal(t, hsms.SelectedState, active.conn.State(),
		"ErrT3Timeout is transaction-level; the secs1 link must stay Selected")
}

// ---------------------------------------------------------------------------
// (c) TestSECS1_LinktestInert
//
// secs1 structurally prevents a linktest goroutine from arming:
//   - NewConfig never calls WithLinktestInterval, so the embedded linktestInterval is 0 (the
//     core default, disabled). ConnectionConfig has no public LinktestInterval() accessor to
//     assert directly; the invariant is structural (only NewConfig constructs the config, and
//     it never sets the interval).
//   - The SECS-I write path drops HSMS control frames (SType≠0) before the wire, so even
//     if the core somehow queued a linktest it would never reach the half-duplex line.
//
// This test documents the invariant operationally: an idle Selected link stays Selected for a
// bounded window, and a follow-up round-trip still succeeds (no stray bytes have desynchronized
// the line).
// ---------------------------------------------------------------------------

func TestSECS1_LinktestInert(t *testing.T) {
	t.Parallel()

	port := freePort(t)
	ctx := t.Context()

	passive := newSECS1Passive(t, port, secs1EchoHandler)
	active := newSECS1Active(t, port)
	defer closeSECS1Endpoint(t, active)
	defer closeSECS1Endpoint(t, passive)

	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	waitSECS1Selected(t, passive)
	waitSECS1Selected(t, active)

	// Observe: the link must hold Selected over a bounded idle window. A rogue linktest would
	// either send a control frame (SType≠0) — which the SECS-I write path drops before the wire
	// — or corrupt the half-duplex timing, causing a T2/T4 tear-down visible as a State change.
	const idleWindow = 300 * time.Millisecond
	require.Never(t, func() bool {
		return active.conn.State() != hsms.SelectedState
	}, idleWindow, 10*time.Millisecond,
		"an idle secs1 connection must hold Selected; a rogue linktest would desync the half-duplex line")

	// A follow-up round-trip after the idle window proves the line is clean and not desynchronized.
	reply, err := active.conn.SendDataMessage(ctx, 1, 1, true /* W-bit */, secs2.A("after-idle"))
	require.NoError(t, err)
	require.NotNil(t, reply)
	require.Equal(t, uint8(1), reply.Stream())
	require.Equal(t, uint8(2), reply.Function())

	replyItem, err := reply.Item()
	require.NoError(t, err)
	got, err := replyItem.ToASCII()
	require.NoError(t, err)
	require.Equal(t, "after-idle", got,
		"after an idle window the line must not be desynchronized: round-trip must still succeed")
}

// ---------------------------------------------------------------------------
// Inbound-handler reentrancy: a handler-initiated synchronous send must use a
// separate goroutine (the documented workaround).
//
// A data-message handler runs INLINE on the single line-engine goroutine — the same goroutine that
// transmits — so a SYNCHRONOUS send issued directly from a handler would wait on that very goroutine
// and deadlock the line (see the package doc). The supported pattern is to dispatch the synchronous
// send to a SEPARATE goroutine, which the freed line engine then services. This proves that pattern
// works end-to-end: the passive's handler spawns a goroutine that issues a W-bit SendDataMessage back
// to the active, the active's handler echoes it, and the reply arrives (no deadlock).
// ---------------------------------------------------------------------------

func TestSECS1_HandlerSyncSendViaGoroutine(t *testing.T) {
	t.Parallel()

	port := freePort(t)
	ctx := t.Context()

	replyGot := make(chan string, 1)

	// The active echoes any primary it receives, via the ASYNC ReplyDataMessage (safe from a handler).
	activeEcho := func(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
		if msg.Function()%2 != 1 {
			return
		}
		item, err := msg.Item()
		if err != nil {
			return
		}
		_ = ep.ReplyDataMessage(context.Background(), msg, item)
	}

	// On the trigger primary, the passive dispatches a SYNCHRONOUS W-bit send to the active on a
	// SEPARATE goroutine (the documented workaround) and records the reply payload.
	passiveHandler := func(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
		if msg.Function()%2 != 1 {
			return
		}
		go func() {
			sendCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			reply, err := ep.SendDataMessage(sendCtx, 2, 1, true /* W-bit */, secs2.A("from-handler"))
			if err != nil || reply == nil {
				replyGot <- "ERR"
				return
			}
			item, iErr := reply.Item()
			if iErr != nil {
				replyGot <- "ERR-ITEM"
				return
			}
			s, aErr := item.ToASCII()
			if aErr != nil {
				replyGot <- "ERR-ASCII"
				return
			}
			replyGot <- s
		}()
	}

	passive := newSECS1Passive(t, port, passiveHandler)
	active := newSECS1Active(t, port)
	active.conn.AddDataMessageHandler(activeEcho)
	defer closeSECS1Endpoint(t, active)
	defer closeSECS1Endpoint(t, passive)

	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	waitSECS1Selected(t, passive)
	waitSECS1Selected(t, active)

	// Trigger: the active sends a primary to the passive fire-and-forget (so the active does not block
	// waiting), which fires the passive's handler.
	require.NoError(t, active.conn.SendDataMessageAsync(ctx, 1, 1, false, secs2.A("trigger")))

	select {
	case s := <-replyGot:
		require.Equal(t, "from-handler", s,
			"a handler-spawned goroutine's synchronous send must complete — the freed line engine services it")
	case <-time.After(15 * time.Second):
		t.Fatal("handler-initiated synchronous send (in a separate goroutine) never completed")
	}
}

// ---------------------------------------------------------------------------
// TestSECS1_LiveT2Update
//
// Proves T2 is live-updatable end to end: a raw peer that never grants the line (drains everything,
// never sends EOT/ACK — the same misbehaving-peer pattern TestSECS1_D5b10_RetryExhaustionReconnects
// uses) forces every SendDataMessage attempt to RTY-exhaust on the T2 EOT-wait. The connection starts
// with the LONG default T2 (10s) and default RetryLimit (3) — if T2 were cached at construction rather
// than read live, the send below would take upwards of (RetryLimit+1)*10s = 40s+. Instead, T2 is
// shrunk live via UpdateConfigOptions AFTER reaching Selected, so the send should fail in roughly
// (RetryLimit+1)*50ms = ~200ms, comfortably inside the 2s bound.
// ---------------------------------------------------------------------------

func TestSECS1_LiveT2Update(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	require.True(t, ok)
	port := tcpAddr.Port

	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				_, _ = io.Copy(io.Discard, c) // drain everything; never grant EOT
				_ = c.Close()
			}(c)
		}
	}()

	// Long default T2 (10s) and default RetryLimit (3) — if T2 were NOT live-updatable, the send
	// below would take far longer than this test's bound.
	cfg, err := NewConfig("127.0.0.1", port, WithActive(), WithHost())
	require.NoError(t, err)

	active, err := New(cfg)
	require.NoError(t, err)
	defer func() { require.NoError(t, active.Close()) }()

	require.NoError(t, active.Open(t.Context(), hsms.OpenBackground))
	require.Eventually(t, func() bool {
		return active.State() == hsms.SelectedState
	}, 10*time.Second, 5*time.Millisecond, "active must reach Selected on TCP connect")

	// Shrink T2 live, well below the default, via the shared hsms.ConnOption path.
	require.NoError(t, active.UpdateConfigOptions(hsms.WithT2(50*time.Millisecond)))

	start := time.Now()
	reply, err := active.SendDataMessage(t.Context(), 1, 1, true, secs2.A("x"))
	elapsed := time.Since(start)

	require.Error(t, err, "an RTY-exhausted send with no line grant must fail")
	require.Nil(t, reply)
	require.Less(t, elapsed, 2*time.Second, "must fail near the NEW 50ms T2 (times RetryLimit+1), not the original 10s default")
}

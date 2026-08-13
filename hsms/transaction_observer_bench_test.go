package hsms

import (
	"testing"

	"github.com/arloliu/go-secs/v2/secs2"
)

// benchSendConn is the benchmark-only counterpart of newTestSendConn (connection_send_test.go),
// which takes a *testing.T and so cannot be reused from a *testing.B.
// It builds a Selected connection engine wired to a fresh mockTransport with a live epoch,
// the same minimum newTestSendConn assembles.
func benchSendConn(b *testing.B, opts ...ConnOption) *connection {
	b.Helper()

	cfg := DefaultConnectionConfig()
	if err := cfg.apply(opts...); err != nil {
		b.Fatalf("apply: %v", err)
	}

	conn, err := NewConnection(cfg, &mockTransport{})
	if err != nil {
		b.Fatalf("NewConnection: %v", err)
	}

	c, ok := conn.(*connection)
	if !ok {
		b.Fatalf("NewConnection did not return *connection")
	}

	sup := newSupervisor(func(_, _ ConnState) {}, &c.handlers, &c.lifecycleSubs)
	sup.state.Store(uint32(SelectedState))
	c.sup.Store(sup)

	e := newEpoch(b.Context(), cfg.logger, cfg.senderQueueSize)
	e.setConn(fakeConn{})
	c.cur.Store(e)

	return c
}

// BenchmarkWriteMessage_ObserverUnset is the WithTransactionObserver zero-cost guard (design note 3, task-4-report.md):
// it pins WriteMessage's per-call cost on the fire-and-forget (W-bit clear) path with NO observer configured,
// so the delta against BenchmarkWriteMessage_ObserverSet below isolates exactly what the observer machinery adds —
// one atomic cfg.Load() plus a nil check, per WithTransactionObserver's godoc.
func BenchmarkWriteMessage_ObserverUnset(b *testing.B) {
	c := benchSendConn(b)
	msg, err := NewDataMessage(1, 1, false, 0x0001, [4]byte{0, 0, 0, 1}, secs2.A("x"))
	if err != nil {
		b.Fatalf("NewDataMessage: %v", err)
	}

	ctx := b.Context()

	b.ReportAllocs()
	for b.Loop() {
		if _, err := c.WriteMessage(ctx, msg); err != nil {
			b.Fatalf("WriteMessage: %v", err)
		}
	}
}

// BenchmarkWriteMessage_ObserverSet is BenchmarkWriteMessage_ObserverUnset's counterpart with a
// no-op observer installed, isolating the added per-call cost: the time.Now/time.Since pair,
// the classifyTxOutcome switch, and the observer call itself.
func BenchmarkWriteMessage_ObserverSet(b *testing.B) {
	c := benchSendConn(b, WithTransactionObserver(func(TxEvent) {}))
	msg, err := NewDataMessage(1, 1, false, 0x0001, [4]byte{0, 0, 0, 1}, secs2.A("x"))
	if err != nil {
		b.Fatalf("NewDataMessage: %v", err)
	}

	ctx := b.Context()

	b.ReportAllocs()
	for b.Loop() {
		if _, err := c.WriteMessage(ctx, msg); err != nil {
			b.Fatalf("WriteMessage: %v", err)
		}
	}
}

package hsmsss

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
)

// benchWireSink keeps the benchmark observer's work from being optimized away.
var benchWireSink atomic.Int64

// BenchmarkWireObserver_RoundTrip measures one W-bit S1F1 / S1F2 round trip over a real loopback pair,
// with no wire observer and with an observer installed on both sides.
// Each round trip moves four observed frames: the primary and the reply, each written by one side and read by the other.
// The observer copies each frame into a reused buffer, the minimum a recorder does.
func BenchmarkWireObserver_RoundTrip(b *testing.B) {
	b.Run("observer=off", func(b *testing.B) { benchWireRoundTrip(b, nil) })

	b.Run("observer=on", func(b *testing.B) { benchWireRoundTrip(b, newBenchWireObserver) })
}

// newBenchWireObserver returns an observer with one copy buffer per direction.
// Inbound calls come from the receive goroutine and outbound calls from the writers, under the write lock,
// so the two directions can run concurrently but never share a buffer.
func newBenchWireObserver() func(hsms.WireEvent) {
	var bufs [3][]byte

	return func(ev hsms.WireEvent) {
		bufs[ev.Direction] = append(bufs[ev.Direction][:0], ev.Frame...)
		benchWireSink.Add(int64(len(bufs[ev.Direction])))
	}
}

func benchWireRoundTrip(b *testing.B, newObserver func() func(hsms.WireEvent)) {
	b.Helper()

	var passiveObs, activeObs func(hsms.WireEvent)
	if newObserver != nil {
		passiveObs, activeObs = newObserver(), newObserver()
	}

	port := benchLoopbackPort(b)
	passive := benchEndpoint(b, port, false, passiveObs)
	active := benchEndpoint(b, port, true, activeObs)

	selected := make(chan struct{})
	var once sync.Once
	cancel := active.SubscribeLifecycle(func(ev hsms.LifecycleEvent) {
		if ev.Current == hsms.SelectedState {
			once.Do(func() { close(selected) })
		}
	})
	b.Cleanup(cancel)

	passive.AddDataMessageHandler(echoHandler)

	ctx := context.Background()
	if err := passive.Open(ctx, hsms.OpenBackground); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = passive.Close() })

	if err := active.Open(ctx, hsms.OpenBackground); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = active.Close() })

	// The passive commits Selected before it answers the Select.req, so the active's Selected implies both.
	select {
	case <-selected:
	case <-time.After(10 * time.Second):
		b.Fatal("pair did not reach Selected")
	}

	body := secs2.A("ping")

	b.ReportAllocs()
	for b.Loop() {
		if _, err := active.SendDataMessage(ctx, 1, 1, true, body); err != nil {
			b.Fatal(err)
		}
	}
}

// benchEndpoint builds one side of the benchmark pair with auto-linktest off.
func benchEndpoint(b *testing.B, port int, active bool, observer func(hsms.WireEvent)) hsms.Connection {
	b.Helper()

	opts := []Option{
		WithConnectionOption(hsms.WithT3(3 * time.Second)),
		WithConnectionOption(hsms.WithT5(200 * time.Millisecond)),
		WithConnectionOption(hsms.WithLinktestInterval(0)),
		WithConnectionOption(hsms.WithWireObserver(observer)),
	}
	if active {
		opts = append(opts, WithActive())
	} else {
		opts = append(opts, WithPassive())
	}

	cfg, err := NewConfig("127.0.0.1", port, opts...)
	if err != nil {
		b.Fatal(err)
	}

	conn, err := New(cfg)
	if err != nil {
		b.Fatal(err)
	}

	return conn
}

// benchLoopbackPort returns a loopback port that was free a moment ago.
func benchLoopbackPort(b *testing.B) int {
	b.Helper()

	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		b.Fatal(err)
	}

	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		b.Fatal("listener addr is not *net.TCPAddr")
	}

	_ = ln.Close()

	return addr.Port
}

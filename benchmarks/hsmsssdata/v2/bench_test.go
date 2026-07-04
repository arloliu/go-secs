// Package v2 benchmarks a full active/passive HSMS-SS pair over real
// loopback TCP against the current go-secs v2 working tree. See
// hsmsssdata/v1 for the v1 counterpart — same benchmark names, same payload
// shapes, so benchstat can diff the two result files directly.
//
// v2 collapses the host/equipment-role + separate Session model into a
// single hsms.Connection that IS its own endpoint (see hsmsss/harness_endpoint_test.go
// in the main module for the pattern this mirrors). Messages and items are
// immutable and unpooled — no Free() calls anywhere in this file.
package v2

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/hsmsss"
	"github.com/arloliu/go-secs/v2/logger"
	"github.com/arloliu/go-secs/v2/secs2"
)

// noopLogger discards everything. The library logs expected teardown noise
// ("use of closed network connection" on Close()) at ErrorLevel, and
// FatalLevel maps to the same underlying slog threshold as ErrorLevel —
// there is no level that silences it, so every benchmark connection is wired
// with this sink instead of the default os.Stdout logger to keep captured
// benchmark text output clean.
type noopLogger struct{}

func (noopLogger) Debug(string, ...any)        {}
func (noopLogger) Info(string, ...any)         {}
func (noopLogger) Warn(string, ...any)         {}
func (noopLogger) Error(string, ...any)        {}
func (noopLogger) Fatal(string, ...any)        {}
func (n noopLogger) With(...any) logger.Logger { return n }
func (noopLogger) Level() logger.LogLevel      { return logger.FatalLevel }
func (noopLogger) SetLevel(logger.LogLevel)    {}

// freeLoopbackPort binds 127.0.0.1:0, records the OS-chosen port, and closes
// the listener so the passive connection can bind that concrete port right
// after. Using a fresh ephemeral port per benchmark (rather than a fixed
// port) avoids collisions with any benchmark run concurrently in another
// process (e.g. the v1 package's own bench run).
func freeLoopbackPort(b *testing.B) int {
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

func newBenchConn(port int, active bool) (hsms.Connection, error) {
	opts := []hsmsss.Option{
		hsmsss.WithConnectionOption(hsms.WithT3(3 * time.Second)),
		hsmsss.WithConnectionOption(hsms.WithT5(500 * time.Millisecond)),
		hsmsss.WithConnectionOption(hsms.WithT6(3 * time.Second)),
		hsmsss.WithConnectionOption(hsms.WithT7(15 * time.Second)),
		hsmsss.WithConnectionOption(hsms.WithT8(1 * time.Second)),
		hsmsss.WithConnectionOption(hsms.WithLinktestInterval(0)),
		hsmsss.WithConnectionOption(hsms.WithLogger(noopLogger{})),
	}

	if active {
		opts = append(opts, hsmsss.WithActive())
	} else {
		opts = append(opts, hsmsss.WithPassive())
	}

	cfg, err := hsmsss.NewConfig("127.0.0.1", port, opts...)
	if err != nil {
		return nil, err
	}

	return hsmsss.New(cfg)
}

func echoHandler(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
	item, err := msg.Item()
	if err != nil {
		return
	}
	_ = ep.ReplyDataMessage(context.Background(), msg, item)
}

// waitSelected polls conn.State() until it reaches hsms.SelectedState or the
// deadline elapses.
func waitSelected(b *testing.B, conn hsms.Connection) {
	b.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if conn.State() == hsms.SelectedState {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	b.Fatal("timeout waiting for SelectedState")
}

// benchConnection brings up an active/passive pair on a fresh loopback port,
// then drives b.N synchronous send+reply round trips of a fresh item built
// by mkItem.
func benchConnection(b *testing.B, mkItem func() secs2.Item) {
	b.Helper()

	port := freeLoopbackPort(b)

	passiveConn, err := newBenchConn(port, false)
	if err != nil {
		b.Fatal(err)
	}

	activeConn, err := newBenchConn(port, true)
	if err != nil {
		b.Fatal(err)
	}

	passiveConn.AddDataMessageHandler(echoHandler)

	ctx := context.Background()
	if err := passiveConn.Open(ctx, hsms.OpenBackground); err != nil {
		b.Fatal(err)
	}
	if err := activeConn.Open(ctx, hsms.OpenBackground); err != nil {
		b.Fatal(err)
	}

	waitSelected(b, passiveConn)
	waitSelected(b, activeConn)

	defer func() {
		_ = activeConn.Close()
		_ = passiveConn.Close()
	}()

	b.ReportAllocs()
	for b.Loop() {
		item := mkItem()
		if _, err := activeConn.SendDataMessage(ctx, 1, 1, true, item); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkConnection_SmallItem_RoundTrip(b *testing.B) {
	benchConnection(b, smallItem)
}

func BenchmarkConnection_StructuredList_RoundTrip(b *testing.B) {
	benchConnection(b, structuredListItem)
}

func BenchmarkConnection_WaferMap_RoundTrip(b *testing.B) {
	benchConnection(b, waferMapItem)
}

func BenchmarkConnection_Recipe_RoundTrip(b *testing.B) {
	benchConnection(b, recipeItem)
}

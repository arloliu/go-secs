// Package v1 benchmarks a full active-host / passive-equipment HSMS-SS pair
// over real loopback TCP against the latest published go-secs v1 release.
// See hsmsssdata/v2 for the v2 counterpart — same benchmark names, same
// payload shapes, so benchstat can diff the two result files directly.
//
// The methodology mirrors the repo's own hsmsss/conn_bench_test.go: a fresh
// item is built per iteration (send takes ownership and frees it on the pool
// side), and the reply is freed per iteration so allocs/op reflects
// steady-state traffic, not pool growth from a leaked reply.
package v1

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/hsmsss"
	"github.com/arloliu/go-secs/logger"
	"github.com/arloliu/go-secs/secs2"
)

const testSessionID = 9527

// noopLogger discards everything. The library logs expected teardown noise
// (receiverTask "use of closed network connection" on Close()) at ErrorLevel,
// and FatalLevel maps to the same underlying slog threshold as ErrorLevel —
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
// process (e.g. the v2 package's own bench run).
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

func newBenchConn(ctx context.Context, port int, isHost bool, isActive bool) (*hsmsss.Connection, error) {
	opts := []hsmsss.ConnOption{
		hsmsss.WithT3Timeout(3 * time.Second),
		hsmsss.WithT5Timeout(500 * time.Millisecond),
		hsmsss.WithT6Timeout(3 * time.Second),
		hsmsss.WithT7Timeout(15 * time.Second),
		hsmsss.WithT8Timeout(1 * time.Second),
		hsmsss.WithConnectRemoteTimeout(2 * time.Second),
		hsmsss.WithAutoLinktest(false),
		hsmsss.WithLogger(noopLogger{}),
	}

	if isHost {
		opts = append(opts, hsmsss.WithHostRole())
	} else {
		opts = append(opts, hsmsss.WithEquipRole())
	}

	if isActive {
		opts = append(opts, hsmsss.WithActive())
	} else {
		opts = append(opts, hsmsss.WithPassive())
	}

	connCfg, err := hsmsss.NewConnectionConfig("127.0.0.1", port, opts...)
	if err != nil {
		return nil, err
	}

	return hsmsss.NewConnection(ctx, connCfg)
}

func echoHandler(msg *hsms.DataMessage, session hsms.Session) {
	_ = session.ReplyDataMessage(msg, msg.Item())
}

// benchConnection brings up an active-host/passive-equipment pair on a fresh
// loopback port, then drives b.N synchronous send+reply round trips of a
// fresh item built by mkItem.
func benchConnection(b *testing.B, mkItem func() secs2.Item) {
	b.Helper()

	ctx := context.Background()
	port := freeLoopbackPort(b)

	hostComm, err := newBenchConn(ctx, port, true, true)
	if err != nil {
		b.Fatal(err)
	}

	eqpComm, err := newBenchConn(ctx, port, false, false)
	if err != nil {
		b.Fatal(err)
	}

	hostSession := hostComm.AddSession(testSessionID)
	eqpSession := eqpComm.AddSession(testSessionID)
	eqpSession.AddDataMessageHandler(echoHandler)

	if err := eqpComm.Open(true); err != nil {
		b.Fatal(err)
	}

	if err := hostComm.Open(true); err != nil {
		b.Fatal(err)
	}

	defer func() {
		_ = eqpComm.Close()
		_ = hostComm.Close()
	}()

	b.ReportAllocs()
	for b.Loop() {
		item := mkItem()
		reply, err := hostSession.SendDataMessage(1, 1, true, item)
		if err != nil {
			b.Fatal(err)
		}
		if reply != nil {
			reply.Free()
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

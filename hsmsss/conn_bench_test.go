package hsmsss

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/logger"
	"github.com/arloliu/go-secs/secs2"
)

func Benchmark_ActiveHost_PassiveEQP_SmallItem(b *testing.B) {
	ctx := context.Background()

	logger.SetLevel(logger.ErrorLevel)

	// Construct a fresh item per iteration. SendDataMessage takes ownership of
	// the passed-in item (processSendRequest's defer msg.Free() eventually
	// returns the item to its sync.Pool with cleared values). Sharing one item
	// across iterations meant the second and subsequent iterations were
	// sending a pool-resident, value-cleared (empty list) item — silently
	// measuring a different workload than the test name advertises.
	mkItem := func() secs2.Item {
		return secs2.L(
			secs2.A("test"),
			secs2.F8(1, 2, 3, 4),
			secs2.BOOLEAN(true, false, false),
		)
	}

	benchConnection(b, ctx, mkItem, true)
}

func Benchmark_ActiveHost_PassiveEQP_LargeItem(b *testing.B) {
	ctx := context.Background()

	logger.SetLevel(logger.ErrorLevel)

	mkItem := func() secs2.Item {
		data := make([]secs2.Item, 100000)
		for i := range 100000 {
			data[i] = secs2.I8(int64(i))
		}

		return secs2.L(data...)
	}

	benchConnection(b, ctx, mkItem, true)
}

func benchConnection(b *testing.B, ctx context.Context, mkItem func() secs2.Item, hostIsActive bool) {
	hostComm, err := newBenchConn(ctx, true, hostIsActive)
	if err != nil {
		b.FailNow()
	}
	eqpComm, err := newBenchConn(ctx, false, !hostIsActive)
	if err != nil {
		fmt.Printf("err: %v\n", err)
		b.FailNow()
	}
	hostSession := hostComm.AddSession(testSessionID)
	eqpSession := eqpComm.AddSession(testSessionID)
	eqpSession.AddDataMessageHandler(msgBenchHandler)

	if err := eqpComm.Open(true); err != nil {
		fmt.Printf("err: %v\n", err)
		b.FailNow()
	}

	if err := hostComm.Open(true); err != nil {
		fmt.Printf("err: %v\n", err)
		b.FailNow()
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		item := mkItem()
		replyMsg, err := hostSession.SendDataMessage(1, 1, true, item)
		if err != nil {
			fmt.Printf("err: %v\n", err)
			b.FailNow()
		}
		// SendDataMessage returns ownership of the reply to us; free it so the
		// per-iteration count reflects real steady-state work, not message-pool
		// growth driven by leaked replies.
		if replyMsg != nil {
			replyMsg.Free()
		}
	}
	b.StopTimer()

	_ = eqpComm.Close()
	_ = hostComm.Close()
}

func newBenchConn(ctx context.Context, isHost bool, isActive bool) (*Connection, error) {
	opts := []ConnOption{
		WithHostRole(),
		WithT3Timeout(3 * time.Second),
		WithT5Timeout(500 * time.Millisecond),
		WithT6Timeout(3 * time.Second),
		WithT7Timeout(15 * time.Second),
		WithT8Timeout(1 * time.Second),
		WithConnectRemoteTimeout(2 * time.Second),
		WithAutoLinktest(true),
		WithLinktestInterval(500 * time.Millisecond),
	}

	if isHost {
		opts = append(opts, WithHostRole())
	} else {
		opts = append(opts, WithEquipRole())
	}

	if isActive {
		l := logger.GetLogger().With("role", "ACTIVE")
		opts = append(opts, WithActive(), WithLogger(l))
	} else {
		l := logger.GetLogger().With("role", "PASSIVE")
		opts = append(opts, WithPassive(), WithLogger(l))
	}

	connCfg, err := NewConnectionConfig(testIP, testPort, opts...)
	if err != nil {
		return nil, err
	}

	conn, err := NewConnection(ctx, connCfg)
	if err != nil {
		return nil, err
	}

	return conn, nil
}

func msgBenchHandler(msg *hsms.DataMessage, session hsms.Session) {
	_ = session.ReplyDataMessage(msg, msg.Item())
}

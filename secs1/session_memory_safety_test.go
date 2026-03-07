package secs1

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/logger"
	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/require"
)

func newNoHandlerTestComm(
	ctx context.Context,
	t testing.TB,
	port int,
	isHost bool,
	isActive bool,
	extraOpts ...ConnOption,
) *testComm {
	t.Helper()

	r := require.New(t)

	opts := []ConnOption{
		WithT1Timeout(MinT1Timeout),
		WithT2Timeout(MinT2Timeout),
		WithT3Timeout(3 * time.Second),
		WithT4Timeout(3 * time.Second),
		WithConnectTimeout(2 * time.Second),
		WithSendTimeout(2 * time.Second),
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

	opts = append(opts, extraOpts...)

	connCfg, err := NewConnectionConfig(testIP, port, opts...)
	r.NoError(err)
	r.NotNil(connCfg)

	conn, err := NewConnection(ctx, connCfg)
	r.NoError(err)
	r.NotNil(conn)

	session, ok := conn.AddSession(testSessionID).(*Session)
	r.True(ok)
	r.NotNil(session)

	return &testComm{
		t:             t,
		require:       r,
		conn:          conn,
		session:       session,
		recvReplyChan: make(chan *hsms.DataMessage, 16),
	}
}

func TestSession_RecvDataMsg_MultiHandlersReceiveDistinctMessages(t *testing.T) {
	req := require.New(t)
	ctx := t.Context()
	port := getPort()

	hostComm := newNoHandlerTestComm(ctx, t, port, true, true)
	eqpComm := newNoHandlerTestComm(ctx, t, port, false, false)

	defer func() {
		req.NoError(hostComm.close())
		req.NoError(eqpComm.close())
	}()

	ptrChan := make(chan string, 2)
	errChan := make(chan error, 2)

	eqpComm.session.AddDataMessageHandler(func(msg *hsms.DataMessage, session hsms.Session) {
		ptrChan <- fmt.Sprintf("%p", msg)
		errChan <- session.ReplyDataMessage(msg, msg.Item().Clone())
		msg.Free()
	})
	eqpComm.session.AddDataMessageHandler(func(msg *hsms.DataMessage, _ hsms.Session) {
		ptrChan <- fmt.Sprintf("%p", msg)
		errChan <- nil
		msg.Free()
	})

	req.NoError(eqpComm.open(false))
	req.NoError(hostComm.open(false))

	req.NoError(eqpComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	req.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	reply, err := hostComm.session.SendDataMessage(1, 1, true, secs2.A("MEMORY-SAFE"))
	req.NoError(err)
	req.NotNil(reply)
	req.EqualValues(1, reply.StreamCode())
	req.EqualValues(2, reply.FunctionCode())

	var ptrs []string
	for range 2 {
		select {
		case p := <-ptrChan:
			ptrs = append(ptrs, p)
		case <-time.After(3 * time.Second):
			t.Fatal("timeout waiting for handler pointer records")
		}
	}

	for range 2 {
		select {
		case handlerErr := <-errChan:
			req.NoError(handlerErr)
		case <-time.After(3 * time.Second):
			t.Fatal("timeout waiting for handler completion")
		}
	}

	req.Len(ptrs, 2)
	req.NotEqual(ptrs[0], ptrs[1], "handlers must not share the same *DataMessage instance")

	reply.Free()
}

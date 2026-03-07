package hsmsss

import (
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/require"
)

func TestSession_RecvDataMsg_MultiHandlersReceiveDistinctMessages(t *testing.T) {
	req := require.New(t)
	ctx := t.Context()
	port := getPort()

	hostComm := doNewTestComm(ctx, t, port, true, true)
	eqpComm := doNewTestComm(ctx, t, port, false, false)

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

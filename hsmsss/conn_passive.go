package hsmsss

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strconv"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/internal/pool"
)

func (c *Connection) passiveConnStateHandler(_ hsms.Connection, prevState hsms.ConnState, curState hsms.ConnState) {
	c.logger.Debug("passive: connection state changes", "prevState", prevState, "curState", curState)
	switch curState {
	case hsms.NotSelectedState:
		// start data message tasks and sender task before the message receiver task,
		// because the receiver task may receive select request message before the sender task started.
		if err := c.session.startDataMsgTasks(); err != nil {
			c.logger.Error("failed to start data message tasks", "error", err)
			c.stateMgr.ToNotConnectedAsync()
			return
		}

		if err := c.taskMgr.StartSender("senderTask", c.senderTask, c.cancelSenderTask, c.senderMsgChan); err != nil {
			c.logger.Error("failed to start sender task", "error", err)
			c.stateMgr.ToNotConnectedAsync()
			return
		}

		if err := c.taskMgr.StartReceiver("receiverTask", c.receiverTask, c.cancelReceiverTask); err != nil {
			c.logger.Error("failed to start receiver task", "error", err)
			c.stateMgr.ToNotConnectedAsync()
			return
		}

		c.logger.Debug("passive: not selected state, start to open passive connection")

		// Start T7 timeout goroutine with context cancellation support to prevent goroutine leaks.
		// The goroutine will exit early if the connection context is cancelled (e.g., during Close()).
		go func(ctx context.Context) {
			waitSelectTimer := pool.GetTimer(c.cfg.t7Timeout)
			defer pool.PutTimer(waitSelectTimer)

			select {
			case <-ctx.Done():
				// Connection closed, exit early without triggering state transition
				c.logger.Debug("T7 timeout cancelled by context", "method", "passiveConnStateHandler")
				return
			case <-waitSelectTimer.C:
				if c.stateMgr.IsNotSelected() {
					c.logger.Debug("wait selected state timeout", "method", "passiveConnStateHandler", "timeout", c.cfg.t7Timeout)
					c.stateMgr.ToNotConnectedAsync()
				}
			}
		}(c.ctx)

	case hsms.SelectedState:
		// do nothing

	case hsms.NotConnectedState:
		if c.opState.IsOpened() && !c.deselected.Load() {
			c.session.separateSession()
		}
		c.deselected.Store(false)

		isShutdown := c.shutdown.Load()
		c.logger.Debug("passive: start to close connection", "shutdown", isShutdown)
		// call closeListener() if Close() be called before connection closed
		if isShutdown {
			_ = c.closeListener()
		}

		_ = c.closeConn(c.cfg.closeConnTimeout)

		if !isShutdown {
			c.stateMgr.ToConnectingAsync()
		}

	case hsms.ConnectingState:
		c.logger.Debug("passive: start to try to open and listen")
		_ = c.doOpen(false)
	}
}

func (c *Connection) recvMsgPassive(msg hsms.HSMSMessage) {
	switch msg.Type() {
	case hsms.DataMsgType:
		if !c.isSelectedState() {
			c.logger.Warn("passive: reject msg by not selected state reason",
				hsms.MsgInfo(msg, "method", "recvMsgPassive", "state", c.stateMgr.State())...,
			)

			replyMsg := hsms.NewRejectReq(msg, hsms.RejectNotSelected)
			_, _ = c.sendMsg(replyMsg)

			break
		}

		// validate message using shared validation logic
		if err := c.validateMsg(msg); err != nil {
			c.logger.Debug("passive: invalid message received",
				hsms.MsgInfo(msg, "method", "recvMsgPassive", "error", err)...,
			)

			break
		}

		dataMsg, _ := msg.ToDataMessage()
		if dataMsg.FunctionCode()%2 != 0 { // primary message, put message to data message channel and invoke data message handlers
			c.session.recvDataMsg(dataMsg)
		} else { // secondary message, reply message to sender
			c.replyToSender(msg)
		}

	case hsms.SelectReqType:
		c.logger.Debug("passive: select.req received", hsms.MsgInfo(msg, "method", "recvMsgPassive")...)

		// reply: communication is already active
		if c.stateMgr.IsSelected() {
			replyMsg, _ := hsms.NewSelectRsp(msg, hsms.SelectStatusActived)
			_, _ = c.sendMsg(replyMsg)
			break
		}

		// transite to selected state
		c.logger.Debug("passive: to selected state after select.req received", "state", c.stateMgr.State())
		c.stateMgr.ToSelectedAsync()

		// reply select request
		replyMsg, _ := hsms.NewSelectRsp(msg, hsms.SelectStatusSuccess)
		_, _ = c.sendMsg(replyMsg)

	// handle deselect request from remote per SEMI E37 §7.7
	case hsms.DeselectReqType:
		c.logger.Debug("passive: deselect.req received", hsms.MsgInfo(msg, "method", "recvMsgPassive")...)
		if c.stateMgr.IsSelected() {
			replyMsg, _ := hsms.NewDeselectRsp(msg, hsms.DeselectStatusSuccess)
			// use sendMsgSync to ensure the response is written to TCP before disconnecting
			_ = c.sendMsgSync(replyMsg)
			// set deselected flag before transitioning to prevent sending separate.req
			c.deselected.Store(true)
			c.stateMgr.ToNotConnectedAsync()
		} else {
			replyMsg, _ := hsms.NewDeselectRsp(msg, hsms.DeselectStatusNotEstablished)
			_, _ = c.sendMsg(replyMsg)
		}

	// route deselect response to the sender waiting for reply
	case hsms.DeselectRspType:
		c.logger.Debug("passive: deselect.rsp received", hsms.MsgInfo(msg, "method", "recvMsgPassive")...)
		c.replyToSender(msg)

	case hsms.LinkTestReqType:
		c.logger.Debug("linktest request received", hsms.MsgInfo(msg, "method", "recvMsgPassive")...)
		replyMsg, _ := hsms.NewLinktestRsp(msg)
		_, err := c.sendMsg(replyMsg)
		if err != nil {
			c.logger.Error("failed to send linktest response", "error", err)
		}

	// reply to sender when linktest response received.
	case hsms.LinkTestRspType:
		c.logger.Debug("linktest response received", hsms.MsgInfo(msg, "method", "recvMsgPassive")...)
		c.replyToSender(msg)

	case hsms.SeparateReqType:
		c.logger.Debug("separate request received", hsms.MsgInfo(msg, "method", "recvMsgPassive")...)
		// Per §7.9.2: if not in SELECTED state, the Separate.req is ignored.
		if c.stateMgr.IsSelected() {
			c.stateMgr.ToNotConnectedAsync()
		} else {
			c.logger.Debug("passive: ignoring separate.req in non-selected state", "state", c.stateMgr.State())
		}

	// Per §8.3.20, an unsolicited response with no open transaction requires Reject(TransactionNotOpen).
	case hsms.SelectRspType:
		c.logger.Warn("passive: unexpected select.rsp received", hsms.MsgInfo(msg, "method", "recvMsgPassive")...)
		replyMsg := hsms.NewRejectReq(msg, hsms.RejectTransactionNotOpen)
		_, _ = c.sendMsg(replyMsg)

	// Per §7.10.1, the initiator takes appropriate local action.
	case hsms.RejectReqType:
		rejectReason, err := hsms.GetRejectReasonCode(msg)
		if err != nil {
			c.replyErrToSender(msg, err)
			return
		}

		c.logger.Warn("passive: reject received from remote",
			hsms.MsgInfo(msg, "method", "recvMsgPassive", "rejectReason", rejectReason, "state", c.stateMgr.State())...,
		)

		c.replyErrToSender(msg, rejectReasonErr(rejectReason))
	}
}

func (c *Connection) openPassive() error {
	c.logger.Debug("start openPassive")

	c.connCount.Store(0)

	c.listenerMutex.Lock()
	if c.listener == nil {
		listener, err := c.tryListen()
		if err != nil {
			c.listenerMutex.Unlock()
			return err
		}
		c.listener = listener
	}
	c.listenerMutex.Unlock()

	c.logger.Debug("listen success", "address", c.listener.Addr())

	return c.taskMgr.Start("tryAcceptConn", c.tryAcceptConn)
}

func (c *Connection) tryListen() (net.Listener, error) {
	address := net.JoinHostPort(c.cfg.host, strconv.Itoa(c.cfg.port))

	c.logger.Debug("try to listen", "address", address)
	var lc net.ListenConfig
	listener, err := lc.Listen(c.ctx, "tcp", address)
	if err != nil {
		c.logger.Error("failed to listen", "address", address, "error", err)
		return nil, err
	}

	return listener, nil
}

func (c *Connection) tryAcceptConn() bool {
	tcpListener := c.getTCPListener()
	// listener already closed, skip
	if tcpListener == nil {
		return false
	}

	if c.shutdown.Load() {
		c.logger.Debug("tryAcceptConn: shutdown, skip accept")
		c.stateMgr.ToNotConnectedAsync()
		return false
	}

	if !c.opState.IsOpening() {
		c.logger.Warn("tryAcceptConn skipped, opState is not opening", "opState", c.opState.String(), "sleep", c.cfg.t5Timeout)
		// respect the t5 timeout
		time.Sleep(c.cfg.t5Timeout)

		return true // retry to accept again
	}

	c.logger.Debug("try to accept connection", "method", "tryAcceptConn", "opState", c.opState.String())
	conn, err := tcpListener.Accept()
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			select {
			case <-c.ctx.Done():
				c.logger.Debug("accept canceled by context", "method", "tryAcceptConn", "error", err, "ctxError", c.ctx.Err())
				return false
			default:
				return true // re-accept if context is not done
			}
		}

		if !c.shutdown.Load() {
			if !isNetOpError(err) {
				c.logger.Error("failed to accept connection", "method", "tryAcceptConn", "error", err.Error())
			}

			return true // re-accept again
		}

		return false // terminate this task
	}

	connCount := c.connCount.Load()
	if connCount > 0 {
		c.logger.Warn("connection already existed", "method", "tryListenAccept", "remote_address", conn.RemoteAddr(), "connCount", connCount, "opState", c.opState.String())
		_ = conn.Close()
		return true // re-accept again
	}

	c.setupResources(conn)

	if !c.opState.ToOpened() {
		c.logger.Warn("failed to set connection state to opened state", "method", "tryListenAccept", "state", c.opState.String())
	}

	c.connCount.Add(1)

	c.logger.Debug("connection accepted", "method", "tryListenAccept", "remote_address", conn.RemoteAddr())

	c.stateMgr.ToNotSelectedAsync()

	return false // terminate this task, only accept new connection once
}

func (c *Connection) getTCPListener() *net.TCPListener {
	c.listenerMutex.Lock()
	defer c.listenerMutex.Unlock()
	if c.listener == nil {
		return nil
	}

	tcpListener, ok := c.listener.(*net.TCPListener)
	if !ok {
		c.logger.Error("failed to convert listener to TCPListener", "type", reflect.TypeOf(c.listener))
		return nil
	}

	err := tcpListener.SetDeadline(time.Now().Add(c.cfg.acceptConnTimeout))
	if err != nil {
		c.logger.Error("failed to set deadline for tcp listener", "error", err)
		return nil
	}

	return tcpListener
}

func (c *Connection) closeListener() error {
	c.listenerMutex.Lock()
	defer c.listenerMutex.Unlock()
	if c.listener != nil {
		err := c.listener.Close()
		c.listener = nil
		return err
	}

	return nil
}

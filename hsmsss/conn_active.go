package hsmsss

import (
	"context"
	"net"
	"strconv"
	"time"

	"github.com/arloliu/go-secs/hsms"
)

const (
	initialRetryDelay = 100 * time.Millisecond
	retryDelayFactor  = 2
)

func (c *Connection) scheduleActiveReconnect(delay time.Duration) bool {
	if delay <= 0 {
		delay = initialRetryDelay
	}
	if c.shutdown.Load() {
		return false
	}
	if !c.reconnectScheduled.CompareAndSwap(false, true) {
		return false
	}

	gen := c.reconnectGen.Load()

	// Never block the connection state manager handler.
	// NOTE: Do NOT use c.ctx here. c.ctx is canceled by closeConn() on disconnect,
	// but we still want reconnect scheduling to work after disconnects.
	go func(ctx context.Context, d time.Duration, g uint64) {
		defer c.reconnectScheduled.Store(false)

		timer := time.NewTimer(d)
		defer timer.Stop()

		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if c.reconnectGen.Load() != g {
				return
			}
			if c.shutdown.Load() {
				return
			}
			c.stateMgr.ToConnectingAsync()
		}
	}(c.pctx, delay, gen)

	return true
}

func (c *Connection) activeConnStateHandler(_ hsms.Connection, prevState hsms.ConnState, curState hsms.ConnState) {
	c.logger.Debug("active: connection state changes", "prevState", prevState, "curState", curState)
	switch curState {
	case hsms.NotSelectedState:
		// start data message tasks and sender task before the message receiver task
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

		err := c.session.selectSession()
		if err != nil {
			c.logger.Debug("failed to select session, switch to not-connected", "error", err)
			c.stateMgr.ToNotConnectedAsync()
		} else {
			c.logger.Debug("session selected, switch to selected state")
			c.stateMgr.ToSelectedAsync()
		}

	case hsms.NotConnectedState:
		if c.opState.IsOpened() {
			c.session.separateSession()
		}

		_ = c.closeConn(c.cfg.closeConnTimeout)

		c.logger.Debug("closeConn in connection state handler", "shutdown", c.shutdown.Load())
		if !c.shutdown.Load() {
			delay := c.retryDelay
			c.logger.Debug("not-connected state, schedule retry connection", "delay", delay)

			if c.scheduleActiveReconnect(delay) {
				// exponential backoff with a maximum delay of T5 timeout
				nextDelay := delay * retryDelayFactor
				if nextDelay > c.cfg.t5Timeout {
					nextDelay = c.cfg.t5Timeout
				}
				c.retryDelay = nextDelay
			}
		}

	case hsms.ConnectingState:
		c.logger.Debug("connecting state, try to connect to remote")
		_ = c.doOpen(false)

	case hsms.SelectedState:
		// reset retry delay upon successful connection
		c.retryDelay = initialRetryDelay
		// do nothing
	}
}

func (c *Connection) recvMsgActive(msg hsms.HSMSMessage) {
	switch msg.Type() {
	case hsms.DataMsgType:
		if !c.stateMgr.IsSelected() {
			c.logger.Warn("active: reject msg by not selected state reason",
				hsms.MsgInfo(msg, "method", "recvMsgActive", "state", c.stateMgr.State())...,
			)
			replyMsg := hsms.NewRejectReq(msg, hsms.RejectNotSelected)
			_, _ = c.sendMsg(replyMsg)

			break
		}

		if err := c.validateMsg(msg); err != nil {
			c.logger.Debug("active: invalid message received, reply error to sender",
				hsms.MsgInfo(msg, "method", "recvMsgActive", "error", err)...,
			)

			break
		}

		dataMsg, _ := msg.ToDataMessage()
		if dataMsg.FunctionCode()%2 != 0 {
			// primary message, put message to data message channel and invoke data message handlers
			c.session.recvDataMsg(dataMsg)
		} else {
			// secondary message, reply message to sender
			c.replyToSender(msg)
		}

	// receive the reject from the remote, it means the request is rejected by the remote.
	case hsms.RejectReqType:
		rejectReason, err := hsms.GetRejectReasonCode(msg)
		if err != nil {
			c.replyErrToSender(msg, err)
			return
		}

		if rejectReason == hsms.RejectNotSelected {
			c.logger.Warn("active: reject message by not selected state reason",
				hsms.MsgInfo(msg, "method", "recvMsgActive", "state", c.stateMgr.State())...,
			)
			c.replyErrToSender(msg, hsms.ErrNotSelectedState)
		}

	// the HSMS-SS doesn't support to accept select/deselect request in active mode.
	case hsms.SelectReqType, hsms.DeselectReqType:
		replyMsg := hsms.NewRejectReq(msg, hsms.RejectSTypeNotSupported)
		_, _ = c.sendMsg(replyMsg)

	// reply to sender when linktest or selected response received.
	case hsms.SelectRspType:
		c.logger.Debug("select.rsp received", hsms.MsgInfo(msg, "method", "recvMsgActive")...)
		c.replyToSender(msg)

	case hsms.LinkTestRspType:
		c.logger.Debug("linktest.rsp received", hsms.MsgInfo(msg, "method", "recvMsgActive")...)
		c.replyToSender(msg)

	case hsms.LinkTestReqType:
		c.logger.Debug("linktest request received", hsms.MsgInfo(msg, "method", "recvMsgActive")...)
		replyMsg, _ := hsms.NewLinktestRsp(msg)
		_, err := c.sendMsg(replyMsg)
		if err != nil {
			c.logger.Error("failed to send linktest response", hsms.MsgInfo(msg, "method", "recvMsgActive", "error", err)...)
		}

	case hsms.SeparateReqType:
		c.logger.Debug("separate request received", hsms.MsgInfo(msg, "method", "recvMsgActive")...)
		c.stateMgr.ToNotConnectedAsync()

	// ignore
	case hsms.DeselectRspType:
	}
}

func (c *Connection) openActive() bool {
	c.logger.Debug("start openActive")

	// terminate interval tasks when connect success
	if err := c.tryConnect(c.ctx); err == nil {
		c.metrics.resetConnRetryGauge()
		return false
	}

	if c.shutdown.Load() {
		c.logger.Debug("openActive: shutdown, skip connect")
		c.stateMgr.ToNotConnectedAsync()
		return false
	}

	c.metrics.incConnRetryGauge()

	return true
}

func (c *Connection) tryConnect(ctx context.Context) error {
	address := net.JoinHostPort(c.cfg.host, strconv.Itoa(c.cfg.port))
	dialer := &net.Dialer{KeepAlive: 30 * time.Second}

	dialCtx, cancel := context.WithTimeout(ctx, c.cfg.connectRemoteTimeout)
	defer cancel()

	conn, err := dialer.DialContext(dialCtx, "tcp", address)
	if err != nil {
		c.logger.Debug("failed to dial to equipment", "error", err)
		return err
	}

	c.setupResources(conn)

	if !c.opState.ToOpened() {
		c.logger.Warn("failed to set connection state to opened", "opState", c.opState.String())
	}

	c.logger.Debug("connected to the remote",
		"host", c.cfg.host,
		"port", c.cfg.port,
		"local_addr", conn.LocalAddr().String(),
		"remote_addr", conn.RemoteAddr().String(),
		"method", "connect",
	)

	c.stateMgr.ToNotSelectedAsync()

	return nil
}

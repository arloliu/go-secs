package hsmsss

import (
	"context"
	"net"
	"strconv"
	"time"

	"github.com/arloliu/go-secs/gem"
	"github.com/arloliu/go-secs/hsms"
)

func (c *Connection) activeConnStateHandler(_ hsms.Connection, prevState hsms.ConnState, curState hsms.ConnState) {
	select {
	case <-c.ctx.Done():
		return

	default:
		switch curState {
		case hsms.NotSelectedState:
			c.taskMgr.StartReceiver("receiverTask", c.conn, c.receiverTask, c.cancelReceiverTask)
			c.taskMgr.StartSender("senderTask", c.senderTask, c.senderMsgChan)
			c.session.startDataMsgTasks()

			err := c.session.selectSession()
			if err != nil {
				c.stateMgr.ToNotConnectedAsync()
			} else {
				c.stateMgr.ToSelectedAsync()
			}

		case hsms.NotConnectedState:
			if !c.recvSeparate.Load() && prevState == hsms.SelectedState {
				c.session.separateSession()
			}

			c.closeConn(c.cfg.closeConnTimeout)

			if !c.shutdown.Load() {
				_ = c.Open(false)
			}

		case hsms.SelectedState:
			// do nothing
		}
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

		// if session id mismatch and not a S9F1 message, reply S9F1.
		if msg.SessionID() != c.session.ID() && msg.StreamCode() != 9 && msg.FunctionCode() != 1 {
			_, _ = c.session.SendSECS2Message(gem.S9F1())
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
		c.recvSeparate.Store(true)
		c.stateMgr.ToNotConnectedAsync()

	// ignore
	case hsms.DeselectRspType, hsms.RejectReqType:
	}
}

func (c *Connection) openActive() bool {
	c.logger.Debug("start openActive")

	// terminate interval tasks when connect success
	if err := c.tryConnect(c.ctx); err == nil {
		c.metrics.resetConnRetryGauge()
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

	c.connMutex.Lock()
	c.conn = conn
	c.connMutex.Unlock()

	c.logger.Debug("connected to the remote",
		"host", c.cfg.host,
		"port", c.cfg.port,
		"method", "connect",
	)

	c.stateMgr.ToNotSelectedAsync()

	return nil
}

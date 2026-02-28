package hsmsss

import (
	"context"
	"net"
	"strconv"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/internal/pool"
)

const (
	retryDelayFactor = 2
)

// startConnectLoop launches the background connect-retry goroutine.
// Only one loop runs at a time (guarded by connectLoopRunning CAS).
func (c *Connection) startConnectLoop() {
	if !c.connectLoopRunning.CompareAndSwap(false, true) {
		return
	}

	gen := c.reconnectGen.Load()
	loopCtx := c.loopCtx

	go c.connectLoop(loopCtx, gen)
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

		if err := c.taskMgr.Start("senderTask", c.senderLoop); err != nil {
			c.logger.Error("failed to start sender task", "error", err)
			c.stateMgr.ToNotConnectedAsync()
			return
		}

		if err := c.taskMgr.StartReceiver("receiverTask", c.receiverTask, c.cancelReceiverTask); err != nil {
			c.logger.Error("failed to start receiver task", "error", err)
			c.stateMgr.ToNotConnectedAsync()
			return
		}

		// Start T7 timeout goroutine per §9.2.2 — disconnect if NOT SELECTED persists.
		// This provides defense-in-depth alongside the T6 timeout on selectSession().
		go func(ctx context.Context) {
			waitSelectTimer := pool.GetTimer(c.cfg.T7Timeout())
			defer pool.PutTimer(waitSelectTimer)

			select {
			case <-ctx.Done():
				c.logger.Debug("T7 timeout cancelled by context", "method", "activeConnStateHandler")
				return
			case <-waitSelectTimer.C:
				if c.stateMgr.IsNotSelected() {
					c.logger.Debug("T7 timeout in active mode, still not selected", "method", "activeConnStateHandler", "timeout", c.cfg.T7Timeout())
					c.stateMgr.ToNotConnectedAsync()
				}
			}
		}(c.getContext())

		// Execute selectSession asynchronously because it is a blocking handshake bound
		// by the T6 timeout. If the socket abruptly drops during this handshake, the
		// StateManager must remain unblocked to instantly process the NotConnected event,
		// cancel the connection context, and interrupt this handshake immediately.
		go func() {
			err := c.session.selectSession()
			if err != nil {
				c.logger.Debug("failed to select session, switch to not-connected", "error", err)
				c.stateMgr.ToNotConnectedAsync()
			} else {
				c.logger.Debug("session selected, switch to selected state")
				c.stateMgr.ToSelectedAsync()
			}
		}()

	case hsms.NotConnectedState:
		if c.opState.IsOpened() && !c.deselected.Load() {
			c.session.separateSession()
		}
		c.deselected.Store(false)

		_ = c.closeConn(c.cfg.CloseConnTimeout())

		c.logger.Debug("closeConn in connection state handler", "shutdown", c.shutdown.Load())
		if !c.shutdown.Load() {
			c.logger.Debug("not-connected state, schedule retry connection")
			c.startConnectLoop()
		}

	case hsms.ConnectingState:
		// The connect loop handles reconnection; nothing to do here.

	case hsms.SelectedState:
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
	// Per §7.10.1, the initiator takes appropriate local action.
	case hsms.RejectReqType:
		rejectReason, err := hsms.GetRejectReasonCode(msg)
		if err != nil {
			c.replyErrToSender(msg, err)
			return
		}

		c.logger.Warn("active: reject received from remote",
			hsms.MsgInfo(msg, "method", "recvMsgActive", "rejectReason", rejectReason, "state", c.stateMgr.State())...,
		)

		c.replyErrToSender(msg, rejectReasonErr(rejectReason))

	// In HSMS-SS active mode, the active side initiates its own select procedure,
	// so a Select.req from the remote is responded with Select.rsp per §7.4.2.
	case hsms.SelectReqType:
		c.logger.Debug("active: select.req received", hsms.MsgInfo(msg, "method", "recvMsgActive")...)
		if c.stateMgr.IsSelected() {
			// Already in SELECTED state — reply with "communication already active"
			replyMsg, _ := hsms.NewSelectRsp(msg, hsms.SelectStatusActived)
			_, _ = c.sendMsg(replyMsg)
		} else {
			// Not yet selected — this is a simultaneous select scenario (§7.4.3).
			// Accept the remote's select request and transition to SELECTED.
			replyMsg, _ := hsms.NewSelectRsp(msg, hsms.SelectStatusSuccess)
			_, _ = c.sendMsg(replyMsg)
			c.stateMgr.ToSelectedAsync()
		}

	// handle deselect request from remote per SEMI E37 §7.7
	case hsms.DeselectReqType:
		c.logger.Debug("active: deselect.req received", hsms.MsgInfo(msg, "method", "recvMsgActive")...)
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
		// Per §7.9.2: if not in SELECTED state, the Separate.req is ignored.
		if c.stateMgr.IsSelected() {
			c.stateMgr.ToNotConnectedAsync()
		} else {
			c.logger.Debug("active: ignoring separate.req in non-selected state", "state", c.stateMgr.State())
		}

	// route deselect response to the sender waiting for reply
	case hsms.DeselectRspType:
		c.logger.Debug("deselect.rsp received", hsms.MsgInfo(msg, "method", "recvMsgActive")...)
		c.replyToSender(msg)
	}
}

// openActive initiates the TCP connection for active mode.
//
// It tries a synchronous dial first so that callers using waitOpened=true
// can immediately block on WaitState. On failure, it starts the background
// connect loop for retries.
func (c *Connection) openActive() error {
	if err := c.tryConnect(c.getContext()); err == nil {
		return nil // connected on first attempt
	}

	if c.shutdown.Load() {
		c.stateMgr.ToNotConnectedAsync()

		return nil
	}

	// Start the background connect loop for retries.
	c.startConnectLoop()

	return nil
}

// connectLoop is the core retry loop for active mode. It exits when:
//   - loopCtx is cancelled (Close() was called),
//   - the parent context is cancelled,
//   - shutdown is set, or
//   - reconnectGen changes (Close() was called).
//
// Per SEMI E37 §9.2.1.1, the Entity should not initiate another active
// connect procedure until the T5 Connect Separation Time has elapsed
// after ANY termination (success or failure).
//
// But typically T5 is 10s, which is too long for quick recovery.
// Thus, any entry into this loop waits for configured initialRetryDelay
// with exponential backoff up to T5Timeout before attempting a dial.
func (c *Connection) connectLoop(loopCtx context.Context, gen uint64) {
	defer c.connectLoopRunning.Store(false)

	delay := min(c.cfg.InitialRetryDelay(), c.cfg.T5Timeout())

	for {
		c.metrics.incConnRetryGauge()

		timer := time.NewTimer(delay)

		select {
		case <-loopCtx.Done():
			timer.Stop()

			return

		case <-timer.C:
		}

		// Check guards after waking up.
		if c.reconnectGen.Load() != gen || c.shutdown.Load() {
			return
		}

		// Prepare opState for the dial attempt. After closeConn the state
		// is Closed with the per-connection context cancelled.
		if c.opState.IsClosed() {
			if !c.opState.ToOpening() {
				continue
			}

			c.createContext()
		}

		if err := c.tryConnect(c.getContext()); err != nil {
			// Exponential backoff.
			delay *= retryDelayFactor
			if delay > c.cfg.T5Timeout() {
				delay = c.cfg.T5Timeout()
			}

			continue
		}

		// Connected — reset metrics and exit. Future disconnects trigger
		// NotConnected → closeConn → startConnectLoop for a fresh loop.
		c.metrics.resetConnRetryGauge()

		return
	}
}

func (c *Connection) tryConnect(ctx context.Context) error {
	address := net.JoinHostPort(c.cfg.host, strconv.Itoa(c.cfg.port))
	// KeepAlive is intentionally omitted; setupResources applies the
	// configured keepAlivePeriod uniformly for both active and passive modes.
	dialer := &net.Dialer{}

	dialCtx, cancel := context.WithTimeout(ctx, c.cfg.ConnectRemoteTimeout())
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

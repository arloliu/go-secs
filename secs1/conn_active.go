package secs1

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
	maxRetryDelay     = 30 * time.Second
)

// activeConnStateHandler handles connection state transitions for active
// (TCP client) mode.
//
// State flow:
//
//	NotSelectedState  → start protocol loop, auto-promote to Selected
//	SelectedState     → ready (no-op)
//	NotConnectedState → close TCP, restart connect loop for auto-reconnect
//	ConnectingState   → no-op; the connect loop handles reconnection
func (c *Connection) activeConnStateHandler(_ hsms.Connection, _ hsms.ConnState, curState hsms.ConnState) {
	c.logger.Debug("secs1 active: state change", "state", curState)

	switch curState {
	case hsms.NotSelectedState:
		// TCP connected — start data message tasks and protocol loop,
		// then auto-promote to Selected (SECS-I has no select handshake).
		if err := c.session.startDataMsgTasks(); err != nil {
			c.logger.Error("secs1: failed to start data message tasks", "error", err)
			c.stateMgr.ToNotConnectedAsync()

			return
		}

		if err := c.startProtocolLoop(); err != nil {
			c.logger.Error("secs1: failed to start protocol loop", "error", err)
			c.stateMgr.ToNotConnectedAsync()

			return
		}

		c.stateMgr.ToSelectedAsync()

	case hsms.SelectedState:
		// Ready for communication.

	case hsms.NotConnectedState:
		_ = c.closeConn(c.cfg.closeTimeout)

		// Restart the connect loop for auto-reconnect.
		if !c.shutdown.Load() {
			c.startConnectLoop()
		}

	case hsms.ConnectingState:
		// The connect loop handles reconnection; nothing to do here.
	}
}

// openActive initiates the TCP connection for active mode.
//
// It tries a synchronous dial first so that callers using waitOpened=true
// can immediately block on WaitState. On failure, it starts the background
// connect loop for retries.
func (c *Connection) openActive() error {
	if err := c.tryConnect(c.ctx); err == nil {
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

// connectLoop is the core retry loop for active mode. It uses a local
// retryDelay variable (no shared atomic needed) and exits when:
//   - loopCtx is cancelled (Close() was called),
//   - the parent context is cancelled,
//   - shutdown is set, or
//   - reconnectGen changes (Close() was called).
func (c *Connection) connectLoop(loopCtx context.Context, gen uint64) {
	defer c.connectLoopRunning.Store(false)

	delay := initialRetryDelay

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

		if err := c.tryConnect(c.ctx); err != nil {
			// Revert so the next iteration can transition Closed → Opening.
			c.opState.Set(hsms.ClosedState)

			// Exponential backoff.
			delay *= retryDelayFactor
			if delay > maxRetryDelay {
				delay = maxRetryDelay
			}

			continue
		}

		// Connected — reset metrics and exit. Future disconnects trigger
		// NotConnected → closeConn → startConnectLoop for a fresh loop.
		c.metrics.resetConnRetryGauge()

		return
	}
}

// tryConnect performs the TCP dial and transitions to NotSelected on success.
func (c *Connection) tryConnect(ctx context.Context) error {
	address := net.JoinHostPort(c.cfg.host, strconv.Itoa(c.cfg.port))
	dialer := &net.Dialer{KeepAlive: 30 * time.Second}

	dialCtx, cancel := context.WithTimeout(ctx, c.cfg.connectTimeout)
	defer cancel()

	conn, err := dialer.DialContext(dialCtx, "tcp", address)
	if err != nil {
		c.logger.Debug("secs1 active: dial failed", "address", address, "error", err)

		return err
	}

	c.setupTCPConn(conn)

	if !c.opState.ToOpened() {
		c.logger.Warn("secs1 active: failed to set state to opened",
			"opState", c.opState.String())
	}

	c.logger.Debug("secs1 active: connected",
		"localAddr", conn.LocalAddr(),
		"remoteAddr", conn.RemoteAddr())

	c.stateMgr.ToNotSelectedAsync()

	return nil
}

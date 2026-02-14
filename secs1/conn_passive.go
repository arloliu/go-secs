package secs1

import (
	"errors"
	"net"
	"strconv"
	"time"

	"github.com/arloliu/go-secs/hsms"
)

// passiveConnStateHandler handles connection state transitions for passive
// (TCP server) mode.
//
// State flow:
//
//	ConnectingState   → open listener + start accept task
//	NotSelectedState  → start protocol loop, auto-promote to Selected
//	SelectedState     → ready (no-op)
//	NotConnectedState → close TCP; if not shutdown, re-enter Connecting
func (c *Connection) passiveConnStateHandler(_ hsms.Connection, _ hsms.ConnState, curState hsms.ConnState) {
	c.logger.Debug("secs1 passive: state change", "state", curState)

	switch curState {
	case hsms.ConnectingState:
		_ = c.doOpen(false)

	case hsms.NotSelectedState:
		// TCP accepted — start data message tasks and protocol loop,
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
		isShutdown := c.shutdown.Load()

		if isShutdown {
			_ = c.closeListener()
		}

		_ = c.closeConn(c.cfg.closeTimeout)

		if !isShutdown {
			c.stateMgr.ToConnectingAsync()
		}
	}
}

// openPassive starts the TCP listener (if needed) and begins accepting connections.
func (c *Connection) openPassive() error {
	c.connCount.Store(0)

	if err := c.ensureListener(); err != nil {
		return err
	}

	c.logger.Debug("secs1 passive: listening", "address", c.listener.Addr())

	return c.taskMgr.Start("acceptConn", c.acceptConnTask)
}

// ensureListener creates the TCP listener if one doesn't already exist.
func (c *Connection) ensureListener() error {
	c.listenerMutex.Lock()
	defer c.listenerMutex.Unlock()

	if c.listener != nil {
		return nil
	}

	address := net.JoinHostPort(c.cfg.host, strconv.Itoa(c.cfg.port))

	var lc net.ListenConfig

	listener, err := lc.Listen(c.ctx, "tcp", address)
	if err != nil {
		c.logger.Error("secs1 passive: failed to listen", "address", address, "error", err)

		return err
	}

	c.listener = listener

	return nil
}

// acceptConnTask blocks on Accept() in a loop. It returns false (stop) once
// a connection is accepted, or when shutdown/context cancellation occurs.
func (c *Connection) acceptConnTask() bool {
	tcpListener := c.getTCPListener()
	if tcpListener == nil {
		return false
	}

	if c.shutdown.Load() {
		return false
	}

	conn, err := tcpListener.Accept()
	if err != nil {
		return c.handleAcceptError(err)
	}

	// Only allow one connection at a time (SECS-I is single-session).
	if c.connCount.Load() > 0 {
		c.logger.Warn("secs1 passive: rejecting duplicate connection",
			"remoteAddr", conn.RemoteAddr())
		_ = conn.Close()

		return true
	}

	c.setupTCPConn(conn)

	if !c.opState.ToOpened() {
		c.logger.Warn("secs1 passive: failed to set state to opened",
			"opState", c.opState.String())
	}

	c.connCount.Add(1)

	c.logger.Debug("secs1 passive: connection accepted",
		"remoteAddr", conn.RemoteAddr())

	c.stateMgr.ToNotSelectedAsync()

	return false // stop accept loop — one connection at a time
}

// handleAcceptError handles errors from Accept(). Returns true to retry,
// false to stop the accept loop.
func (c *Connection) handleAcceptError(err error) bool {
	// Accept timeout — check context and retry.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		select {
		case <-c.ctx.Done():
			return false
		default:
			return true
		}
	}

	// Shutdown — stop.
	if c.shutdown.Load() {
		return false
	}

	// Closed listener (e.g. during shutdown) — don't log as error.
	if !isNetOpError(err) {
		c.logger.Error("secs1 passive: accept failed", "error", err)
	}

	return true
}

// getTCPListener retrieves the listener and sets the accept deadline.
func (c *Connection) getTCPListener() *net.TCPListener {
	c.listenerMutex.Lock()
	defer c.listenerMutex.Unlock()

	if c.listener == nil {
		return nil
	}

	tcpListener, ok := c.listener.(*net.TCPListener)
	if !ok {
		return nil
	}

	if err := tcpListener.SetDeadline(time.Now().Add(c.cfg.acceptTimeout)); err != nil {
		c.logger.Error("secs1 passive: failed to set accept deadline", "error", err)

		return nil
	}

	return tcpListener
}

// closeListener closes the TCP listener.
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

package hsmsss

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/go-secs/gem"
	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/internal/pool"
	"github.com/arloliu/go-secs/logger"
	"github.com/arloliu/go-secs/secs2"
	"github.com/puzpuzpuz/xsync/v3"
)

const (
	// defaultWriteBufferSize is the size of the buffered writer for TCP connections.
	defaultWriteBufferSize = 64 * 1024

	// replyChannelTimeout is the timeout for sending to reply channels.
	replyChannelTimeout = time.Second

	// closeCheckInterval is the interval for checking close status in Close() method.
	closeCheckInterval = 5 * time.Millisecond
)

// Connection represents an HSMS-SS (Single Session) connection, implementing the hsms.Connection interface.
// It manages the communication with a remote HSMS device, handling message exchange, connection state
// transitions, and various HSMS-specific functionalities.
type Connection struct {
	pctx      context.Context
	ctx       context.Context
	ctxCancel context.CancelFunc
	cfg       *ConnectionConfig
	logger    logger.Logger

	listener      net.Listener       // listener is for passive mode only
	listenerMutex sync.Mutex         // mutex for listener
	opState       hsms.AtomicOpState // operation state
	connCount     atomic.Int32       // connection counter for passive mode only
	session       *Session           // HSMS-SS has only one session

	resMutex   sync.RWMutex        // mutex for connection resources
	resources  connectionResources // resources for the connection, including reader and writer
	writeMutex sync.Mutex          // mutex for writing to the connection

	stateMgr *hsms.ConnStateMgr
	taskMgr  *hsms.TaskManager
	shutdown atomic.Bool // indicates if has entered shutdown mode

	// reconnectScheduled prevents overlapping reconnect timers in active mode.
	// It is intentionally independent from c.ctx (which is canceled on disconnect).
	reconnectScheduled atomic.Bool

	// reconnectGen invalidates reconnect timers across Close/Open cycles.
	// Incremented on Close() so timers scheduled before Close() cannot fire after a later Open().
	reconnectGen atomic.Uint64
	tickers      tickerCtl // linktest control

	senderMsgChan chan hsms.HSMSMessage // channel for sending messages to the senderTask, the lifetime is the same as this connection object
	replyMsgChans *xsync.MapOf[uint32, chan hsms.HSMSMessage]
	replyErrs     *xsync.MapOf[uint32, error]

	retryDelay time.Duration // delay between connection retries for active mode

	metrics ConnectionMetrics // connection metrics
}

type connectionResources struct {
	conn   net.Conn
	writer *bufio.Writer
}

// ensure Connection implements hsms.Connection interface.
var _ hsms.Connection = &Connection{}

// NewConnection creates a new HSMS-SS Connection with the given context and configuration.
// It initializes the connection state, task manager, and other necessary components.
// Returns an error if the configuration is invalid or if initialization fails.
func NewConnection(ctx context.Context, cfg *ConnectionConfig) (*Connection, error) {
	if cfg == nil {
		return nil, errors.New("connection config is nil")
	}
	conn := &Connection{
		cfg:           cfg,
		pctx:          ctx,
		logger:        cfg.logger,
		replyErrs:     xsync.NewMapOf[uint32, error](),
		replyMsgChans: xsync.NewMapOf[uint32, chan hsms.HSMSMessage](),
		taskMgr:       hsms.NewTaskManager(ctx, cfg.logger),
		retryDelay:    initialRetryDelay,
	}
	// create sender message channel at beginning and not close it.
	conn.senderMsgChan = make(chan hsms.HSMSMessage, cfg.senderQueueSize)

	conn.opState.Set(hsms.ClosedState)

	conn.createContext()

	if cfg.isActive {
		conn.stateMgr = hsms.NewConnStateMgr(ctx, conn, conn.activeConnStateHandler)
	} else {
		conn.stateMgr = hsms.NewConnStateMgr(ctx, conn, conn.passiveConnStateHandler)
	}

	conn.stateMgr.AddHandler(conn.linktestConnStateHandler)

	return conn, nil
}

// UpdateConfigOptions updates the connection configuration options.
func (c *Connection) UpdateConfigOptions(opts ...ConnOption) error {
	// Store original values that might trigger actions
	origAutoLinktest := c.cfg.AutoLinktest()
	origLinktestInterval := c.cfg.LinktestInterval()
	origT5Timeout := c.cfg.T5Timeout()

	// apply all options
	for _, opt := range opts {
		if _, ok := opt.(*connOptFunc); !ok {
			return errors.New("invalid ConnOption type")
		}

		if err := opt.apply(c.cfg); err != nil {
			return err
		}
	}

	// store linktest configuration changes
	curAutoLinktest := c.cfg.AutoLinktest()
	curLinktestInterval := c.cfg.LinktestInterval()

	// check if autoLinktest setting changed
	if curAutoLinktest != origAutoLinktest {
		if curAutoLinktest {
			// autoLinktest was enabled
			c.tickers.resetLinktestTicker(curLinktestInterval)
		} else {
			// autoLinktest was disabled
			c.tickers.stopLinktestTicker()
		}
	} else if curAutoLinktest && curLinktestInterval != origLinktestInterval {
		// only interval changed while autoLinktest is enabled
		c.tickers.resetLinktestTicker(curLinktestInterval)
	}

	// handle T5 timeout changes (for active connections)
	if c.cfg.T5Timeout() != origT5Timeout {
		c.tickers.resetActiveConnectTicker(c.cfg.T5Timeout())
	}

	return nil
}

// AddSession creates and adds a new Session to the connection with the specified session ID.
// For HSMS-SS, this method should only be called once, as it supports only a single session.
// It returns the newly created Session.
//
// IMPORTANT: This method is NOT thread-safe and MUST be called before Open().
// Calling AddSession after Open() or from multiple goroutines may result in data races.
func (c *Connection) AddSession(sessionID uint16) hsms.Session {
	c.session = NewSession(sessionID, c)

	return c.session
}

// GetLogger returns the logger associated with the HSMS-SS connection.
func (c *Connection) GetLogger() logger.Logger {
	return c.logger
}

// GetMetrics returns the metrics associated with the HSMS-SS connection
func (c *Connection) GetMetrics() *ConnectionMetrics {
	return &c.metrics
}

// IsSingleSession returns true, indicating that this is an HSMS-SS connection.
func (c *Connection) IsSingleSession() bool { return true }

// IsGeneralSession returns false, indicating that this is not an HSMS-GS connection.
func (c *Connection) IsGeneralSession() bool { return false }

// IsSECS1 returns false, indicating that this is not a SECS-I connection.
func (c *Connection) IsSECS1() bool { return false }

// open establishes the HSMS-SS connection.
// If waitOpened is true, it blocks until the connection is fully established (Selected state)
// or an error occurs.
// If waitOpened is false, it initiates the connection process and returns immediately.
func (c *Connection) Open(waitOpened bool) error {
	// reset shutdown flag to false when user call Open() again
	c.shutdown.Store(false)

	c.stateMgr.Start()

	return c.doOpen(waitOpened)
}

func (c *Connection) doOpen(waitOpened bool) error {
	if c.session == nil {
		return hsms.ErrSessionNil
	}

	if !c.opState.ToOpening() {
		c.logger.Warn("fail to set connection to opening state", "opState", c.opState.String())
		return nil
	}

	c.logger.Debug("start to open connection", "method", "Open", "opState", c.opState.String())

	c.createContext()

	if c.cfg.isActive {
		ticker, err := c.taskMgr.StartInterval("openActive", c.openActive, c.cfg.t5Timeout, true)
		if err != nil {
			c.logger.Error("failed to start active connection ticker", "error", err, "method", "Open")
			return fmt.Errorf("failed to start active connection ticker: %w", err)
		}

		c.tickers.setActiveConnectTicker(ticker)

		if waitOpened {
			return c.stateMgr.WaitState(c.ctx, hsms.SelectedState)
		}
	} else { // passive mode
		err := c.openPassive()
		if err != nil {
			return err
		}
	}

	return nil
}

// Close closes the HSMS-SS connection gracefully.
// It terminates all running tasks, closes the TCP connection, and resets the connection state.
func (c *Connection) Close() error {
	// Invalidate any pending reconnect timers from a previous lifecycle.
	c.reconnectGen.Add(1)
	c.reconnectScheduled.Store(false)

	c.shutdown.Store(true)
	c.logger.Debug("start to close connection", "method", "Close", "opState", c.opState.String())

	// stop state manager first
	c.stateMgr.Stop()

	// force to set state to not connected state if not already
	// this is to ensure that the connection is closed even if the state manager is not in a connected state
	if !c.isClosed() {
		c.logger.Debug("force to set connection desired state to not-connected", "method", "Close",
			"curDesiredState", c.stateMgr.DesiredState().String(),
			"curState", c.stateMgr.State().String(),
		)
		c.stateMgr.ToNotConnected()
	}

	closeTimer := pool.GetTimer(c.cfg.closeConnTimeout)
	defer pool.PutTimer(closeTimer)

	// Use a ticker instead of busy loop with sleep for better CPU efficiency
	checkTicker := time.NewTicker(closeCheckInterval)
	defer checkTicker.Stop()

	// wait for connection to be closed
	for {
		select {
		case <-closeTimer.C:
			if c.isClosed() {
				c.logger.Debug("wait for connection closed success at timeout", "timeout", c.cfg.closeConnTimeout, "opState", c.opState.String())
				return nil
			}

			c.logger.Error("close connection timeout",
				"timeout", c.cfg.closeConnTimeout,
				"opState", c.opState.String(),
				"curState", c.stateMgr.State().String(),
				"desiredState", c.stateMgr.DesiredState().String(),
			)

			return errors.New("close connection timeout")
		case <-checkTicker.C:
			if c.isClosed() {
				c.logger.Debug("wait for connection closed success")
				return nil
			}
		}
	}
}

func (c *Connection) isClosed() bool {
	return c.opState.IsClosed() && c.stateMgr.State() == hsms.NotConnectedState && c.stateMgr.DesiredState() == hsms.NotConnectedState
}

// drainSenderMsgChan drains the sender message channel until it is closed or empty.
func (c *Connection) drainSenderMsgChan(ctx context.Context) {
	lastSendMsgOk := true

drainLoop:
	for {
		select {
		case <-ctx.Done():
			c.logger.Debug("context done, stop draining sender message channel", "method", "drainSenderMsgChan")
			break drainLoop
		case msg, ok := <-c.senderMsgChan:
			if !ok {
				break drainLoop
			}

			// only continue to send message if msg is not nil and the last of senderTask success
			if msg != nil && lastSendMsgOk {
				lastSendMsgOk = c.senderTask(msg)
			}
		default:
			break drainLoop
		}
	}
}

func (c *Connection) setupResources(conn net.Conn) {
	c.resMutex.Lock()
	defer c.resMutex.Unlock()

	// create new resources atomically
	c.resources = connectionResources{
		conn:   conn,
		writer: bufio.NewWriterSize(conn, defaultWriteBufferSize),
	}
}

func (c *Connection) getConn() net.Conn {
	c.resMutex.RLock()
	defer c.resMutex.RUnlock()

	if c.resources.conn == nil {
		return nil
	}

	return c.resources.conn
}

// withResources executes a function with the connection's resources (conn, writer).
// It locks the resources mutex to ensure thread safety and returns an error if the connection is closed.
func (c *Connection) withResources(fn func(conn net.Conn, writer *bufio.Writer) error) error {
	c.resMutex.RLock()
	defer c.resMutex.RUnlock()

	if c.resources.conn == nil {
		return hsms.ErrConnClosed
	}

	return fn(c.resources.conn, c.resources.writer)
}

// closeTCP closes the TCP connection with a specified timeout and returns the remote address.
func (c *Connection) closeTCP(timeout time.Duration) string {
	conn := c.getConn()

	if conn == nil {
		return ""
	}

	remote := conn.RemoteAddr().String()
	c.logger.Debug("close TCP connection", "method", "closeConn")

	if tcp, ok := conn.(*net.TCPConn); ok {
		linger := int(timeout.Seconds())
		if linger > 0 {
			_ = tcp.SetLinger(linger)
		} else {
			_ = tcp.SetLinger(0)
		}
	}

	if !c.cfg.isActive {
		c.connCount.Add(-1)
	}

	if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		c.logger.Error("failed to close TCP connection", "method", "closeConn", "error", err)
	}

	return remote
}

// closeConn performs the actual connection closing process with a timeout.
// It cancels the context, stops the task manager, closes the TCP connection, and waits for
// all goroutines to terminate.
func (c *Connection) closeConn(timeout time.Duration) error {
	// 1. try to transition the connection state to closing
	if !c.opState.ToClosing() {
		if c.opState.IsClosed() {
			c.logger.Warn("connection already closed, no need to close again", "method", "closeConn", "opState", c.opState.String())
			return nil
		}
		c.logger.Warn("failed to set connection to closing state", "method", "closeConn", "opState", c.opState.String())

		return fmt.Errorf("failed to set connection to closing state, current state: %s", c.opState.String())
	}

	c.logger.Debug("start to close connection", "method", "closeConn", "opState", c.opState.String())

	// 2. create a new context for waiting all tasks to be terminated
	closeCtx, closeCtxCancel := context.WithTimeout(context.Background(), timeout)
	defer closeCtxCancel()

	// 3. drain sender message channel
	c.drainSenderMsgChan(closeCtx)

	// 4. cancel per-connection context created by createContext method
	if c.ctxCancel != nil {
		c.logger.Debug("trigger context cancel function", "method", "closeConn")
		c.ctxCancel()
	}

	// 5. try to close TCP connection with timeout FIRST to unblock receiver/sender tasks
	remoteAddr := c.closeTCP(timeout)

	// 6. stop all tasks
	c.taskMgr.Stop()

	// 7. wait for all tasks to be terminated in a goroutine to avoid blocking the close process.
	// the close context will be canceled when all tasks are terminated, or timeout occurs.
	go func() {
		c.logger.Debug("wait all goroutines terminated, taskMgr", "method", "closeConn")
		c.taskMgr.Wait()
		c.logger.Debug("all goroutines terminated", "method", "closeConn")

		// notify the close context that all tasks are terminated
		closeCtxCancel()
	}()

	// 8. wait all goroutines terminated or timeout
	<-closeCtx.Done()

	var closeErr error
	if !errors.Is(closeCtx.Err(), context.Canceled) {
		c.logger.Error("context close timeout", "method", "closeConn", "error", closeCtx.Err(), "timeout", timeout)
		closeErr = fmt.Errorf("close timeout: %w", closeCtx.Err())
	} else {
		c.logger.Debug("context closed", "method", "closeConn")
	}

	// 9. cleanup reply channels and queue
	c.dropAllReplyMsgs()

	// 10. final transition to closed state
	if !c.opState.ToClosed() {
		c.logger.Warn("failed to set connection to closed state", "method", "closeConn", "opState", c.opState.String())
		return fmt.Errorf("failed to set connection to closed state, current state: %s", c.opState.String())
	}

	c.logger.Debug("connection closed", "method", "closeConn", "remoteAddr", remoteAddr)

	return closeErr
}

// createContext creates a new context for the connection, derived from the parent context.
func (c *Connection) createContext() {
	c.ctx, c.ctxCancel = context.WithCancel(c.pctx)
}

// sendControlMsg sends an HSMS control message and optionally waits for a reply.
// It returns the received control message (if replyExpected is true) and an error if any occurred.
func (c *Connection) sendControlMsg(msg *hsms.ControlMessage, replyExpected bool) (*hsms.ControlMessage, error) {
	replyMsg, err := c.sendMsg(msg)
	if err != nil || replyMsg == nil {
		return nil, err
	}

	if !replyExpected {
		return nil, nil //nolint:nilnil
	}

	ctrlMsg, ok := replyMsg.ToControlMessage()
	if !ok {
		return nil, hsms.ErrNotControlMsg
	}

	return ctrlMsg, nil
}

// sendMsg sends an HSMS message (data or control) and waits for a reply if the message's W-bit is set.
// It returns the received reply message and an error if any occurred.
// It handles T3 and T6 timeouts and manages reply channels for concurrent message sending.
func (c *Connection) sendMsg(msg hsms.HSMSMessage) (hsms.HSMSMessage, error) {
	if c.logger.Level() == logger.DebugLevel {
		c.logger.Debug("start to send message",
			hsms.MsgInfo(msg, "method", "sendMsg", "state", c.stateMgr.State())...,
		)
	}

	if msg.Type() == hsms.DataMsgType && !c.stateMgr.IsSelected() {
		c.logger.Warn("failed to send message, not selected state",
			hsms.MsgInfo(msg, "method", "sendMsg", "state", c.stateMgr.State())...,
		)

		return nil, hsms.ErrNotSelectedState
	}

	if !msg.WaitBit() {
		if err := c.sendMsgAsync(msg); err != nil {
			return nil, err
		}

		return nil, nil //nolint:nilnil
	}

	// set T3 or T6 timeout
	timeout := c.cfg.t3Timeout
	if msg.IsControlMessage() {
		timeout = c.cfg.t6Timeout
	}

	sendMsgTimer := pool.GetTimer(timeout)
	defer pool.PutTimer(sendMsgTimer)

	id := msg.ID()
	replyMsgChan := c.addReplyExpectedMsg(id)

	err := c.sendMsgAsync(msg)
	if err != nil {
		c.removeReplyExpectedMsg(id)
		return nil, err
	}

	select {
	case <-c.ctx.Done():
		c.removeReplyExpectedMsg(id)
		return nil, hsms.ErrConnClosed

	case <-sendMsgTimer.C:
		c.removeReplyExpectedMsg(id)

		c.logger.Warn("send message timeout", hsms.MsgInfo(msg, "method", "sendMsg", "timeout", timeout)...)
		if msg.IsDataMessage() {
			// If entity is equipment, send SECS-II S9F9 when t3/t6 timeout.
			if c.cfg.isEquip {
				_, _ = c.session.SendSECS2Message(gem.S9F9())
			}

			return nil, hsms.ErrT3Timeout
		}

		return nil, hsms.ErrT6Timeout

	// wait reply message from receiverTask
	case replyMsg := <-replyMsgChan:
		if replyMsg == nil {
			// check if error existed
			if err, ok := c.replyErrs.LoadAndDelete(id); ok {
				return nil, err
			}

			return nil, hsms.ErrConnClosed
		}

		c.removeReplyExpectedMsg(id)

		if ctrlMsg, ok := replyMsg.ToControlMessage(); ok {
			if ctrlMsg.Type() == hsms.RejectReqType {
				return nil, fmt.Errorf("reject by reason %d", ctrlMsg.Header()[3])
			}
		}

		if c.logger.Level() == logger.DebugLevel {
			c.logger.Debug("reply message received", hsms.MsgInfo(replyMsg, "method", "sendMsg")...)
		}

		return replyMsg, nil
	}
}

// sendMsgSync sends an HSMS message synchronously over the TCP connection.
// It sets a write deadline based on the T8 timeout and handles potential errors during writing.
func (c *Connection) sendMsgSync(msg hsms.HSMSMessage) error {
	// free message after it sent
	defer msg.Free()

	if msg.Type() == hsms.DataMsgType && !c.stateMgr.IsSelected() {
		c.logger.Error("failed to send hsms data message, not selected state",
			hsms.MsgInfo(msg, "method", "sendMsgSync", "state", c.stateMgr.State().String())...,
		)

		return hsms.ErrNotSelectedState
	}

	buf := msg.ToBytes()

	if c.cfg.traceTraffic {
		c.logger.Info("trace: send message", hsms.MsgInfo(msg, "raw", hsms.MsgHexString(buf))...)
	}

	// lock the write mutex to ensure thread-safe writing to the connection
	c.writeMutex.Lock()
	defer c.writeMutex.Unlock()

	return c.withResources(func(conn net.Conn, writer *bufio.Writer) error {
		if conn == nil || writer == nil {
			return hsms.ErrConnClosed
		}

		err := conn.SetWriteDeadline(time.Now().Add(c.cfg.sendTimeout))
		if err != nil {
			return err
		}

		isDebugLevel := c.logger.Level() == logger.DebugLevel
		if isDebugLevel {
			c.logger.Debug("try to send message to remote", hsms.MsgInfo(msg, "method", "sendMsgSync", "timeout", c.cfg.sendTimeout)...)
		}

		// write to buffered writer
		_, err = writer.Write(buf)
		if err != nil {
			return err
		}

		// flush to ensure data is sent immediately
		err = writer.Flush()
		if err != nil {
			return err
		}

		if isDebugLevel {
			c.logger.Debug("message sent to remote", hsms.MsgInfo(msg, "method", "sendMsgSync")...)
		}

		return nil
	})
}

// sendMsgAsync sends an HSMS message asynchronously by sending it to the senderMsgChan.
// It uses a non-blocking select with a timeout to avoid blocking the caller.
func (c *Connection) sendMsgAsync(msg hsms.HSMSMessage) error {
	timer := pool.GetTimer(c.cfg.sendTimeout)
	defer pool.PutTimer(timer)

	select {
	case <-timer.C:
		return hsms.ErrSendMsgTimeout
	case c.senderMsgChan <- msg: // send message to sender message channel, senderTask will handle it.
		return nil
	}
}

// addReplyExpectedMsg adds a reply channel to the replyMsgChans map for a message that expects a reply.
func (c *Connection) addReplyExpectedMsg(id uint32) <-chan hsms.HSMSMessage {
	ch := make(chan hsms.HSMSMessage)
	c.replyMsgChans.Store(id, ch)

	return ch
}

// removeReplyExpectedMsg removes the reply channel from the replyMsgChans map for the given message ID.
func (c *Connection) removeReplyExpectedMsg(id uint32) {
	// find and delete the reply channel
	c.replyMsgChans.Delete(id)
}

// dropAllReplyMsgs closes all reply channels in the replyMsgChans map, effectively dropping any pending replies.
func (c *Connection) dropAllReplyMsgs() {
	// close all reply channels
	c.replyMsgChans.Range(func(id uint32, ch chan hsms.HSMSMessage) bool {
		if ch != nil {
			close(ch)
		}

		return true
	})

	c.replyMsgChans.Clear()
}

func (c *Connection) cancelSenderTask() {
	c.stateMgr.ToNotConnectedAsync()
}

// senderTask is the task function for the sender goroutine.
// It receives messages from the senderMsgChan and sends them synchronously over the connection.
func (c *Connection) senderTask(msg hsms.HSMSMessage) bool {
	if msg == nil {
		c.logger.Warn("received nil message in senderTask, ignore")
		return true // continue the sender task
	}

	c.metrics.incDataMsgSendCount()
	if msg.WaitBit() {
		c.metrics.incDataMsgInflightCount()
	}

	err := c.sendMsgSync(msg)
	if err != nil {
		c.metrics.incDataMsgErrCount()
		c.replyErrToSender(msg, err)

		if !isNetOpError(err) {
			c.logger.Error("failed to send message", "method", "senderTask", "error", err)
		}

		return false
	}

	// reset the linktest ticker when a message is sent successfully
	c.resetLinktest()

	return true
}

// cancelReceiverTask cancels the receiver task by transitioning the connection state to NotConnected.
func (c *Connection) cancelReceiverTask() {
	// transition the connection state to NotConnected
	c.stateMgr.ToNotConnectedAsync()
}

// receiverTask is the task function for the receiver goroutine.
// It reads and decodes HSMS messages from the connection and processes them accordingly.
//
// It respects the T8 timeout for reading messages and handles message length validation.
func (c *Connection) receiverTask(msgLenBuf []byte) bool {
	conn := c.getConn()

	if conn == nil {
		return false // stop the receiver task if connection is closed
	}

	// no timeout for reading message length - allow connection to idle
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		c.logger.Error("failed to clear read deadline", "error", err)
		return false // stop the receiver task
	}

	// read 4 bytes message length (blocks until data arrives)
	// the blocking read will returns when the connection is closed or an error occurs.
	if _, err := io.ReadFull(conn, msgLenBuf); err != nil {
		if !isNetError(err) {
			c.logger.Error("failed to read the length of HSMS message", "method", "receiverTask", "error", err)
		} else {
			c.logger.Debug("network error, failed to read the length of HSMS message",
				"method", "receiverTask",
				"error", err,
			)
		}

		// if connection closed or any error occurred other than timeout, stop the receiver task
		c.metrics.incDataMsgErrCount()

		return false
	}

	msgLen := binary.BigEndian.Uint32(msgLenBuf)
	if msgLen == 0 {
		c.metrics.incDataMsgErrCount()
		c.logger.Error("HSMS message length is zero", "method", "receiverTask")

		// zero-length message is invalid, stop the receiver task
		return false
	}

	if msgLen > secs2.MaxByteSize {
		c.metrics.incDataMsgErrCount()
		c.logger.Error("HSMS message length exceeds maximum allowed length",
			"method", "receiverTask",
			"msgLen", msgLen,
			"maxLen", secs2.MaxByteSize,
		)

		// the message length exceeds the maximum allowed length, stop the receiver task
		return false
	}

	// now set T8 timeout for reading the HSMS message
	if err := conn.SetReadDeadline(time.Now().Add(c.cfg.t8Timeout)); err != nil {
		c.logger.Error("failed to set read deadline", "error", err)
		return false
	}

	// no need to use message pool, the performance impact is negligible
	msgBuf := make([]byte, msgLen)

	if _, err := io.ReadFull(conn, msgBuf); err != nil {
		if !isNetError(err) {
			c.logger.Error("failed to read HSMS message", "method", "receiverTask", "error", err)
		} else {
			c.logger.Debug("network error, failed to read HSMS message", "method", "receiverTask", "error", err)
		}

		c.metrics.incDataMsgErrCount()

		// if connection closed or any error, including timeout, stop the receiver task
		// because the message is not complete or malformed.
		return false
	}

	// decode the HSMS message
	msg, err := hsms.DecodeMessage(msgLen, msgBuf)
	if err != nil {
		if c.cfg.traceTraffic {
			c.logger.Error("failed to decode HSMS message",
				hsms.MsgInfo(
					msg,
					"error", err,
					"raw", hsms.MsgHexString(msgLenBuf, msgBuf),
				)...,
			)
		} else {
			c.logger.Error("failed to decode HSMS message", "error", err)
		}
		c.metrics.incDataMsgErrCount()
		// if message decode failed, it means the message is malformed or not a valid HSMS message.
		// stop the receiver task
		return false
	}

	if c.cfg.traceTraffic {
		c.logger.Info("trace: received message", hsms.MsgInfo(msg, "raw", hsms.MsgHexString(msgLenBuf, msgBuf))...)
	}

	c.metrics.incDataMsgRecvCount()

	if c.cfg.isActive {
		c.recvMsgActive(msg)
	} else {
		c.recvMsgPassive(msg)
	}

	// reset the linktest ticker after receiving a message
	c.resetLinktest()

	return true
}

func (c *Connection) resetLinktest() {
	if !c.cfg.AutoLinktest() {
		return
	}

	c.tickers.resetLinktestTicker(c.cfg.LinktestInterval())
}

// replyToSender sends a received reply message to the corresponding reply channel in the replyMsgChans map.
func (c *Connection) replyToSender(msg hsms.HSMSMessage) {
	// if reply channel not found, send to data message handler.
	// it means the original request message is sent by sendMsgSync directly.
	replyChan, loaded := c.replyMsgChans.Load(msg.ID())
	if !loaded || replyChan == nil {
		// handle data message
		if dataMsg, ok := msg.ToDataMessage(); ok {
			if c.session != nil {
				c.session.recvDataMsg(dataMsg)
			} else {
				c.logger.Warn("session is nil, cannot handle data message", "msgID", msg.ID())
			}
		}

		return
	}

	// decrement inflight count when a reply message is matched to a waiting sender
	c.metrics.decDataMsgInflightCount()

	// set timeout for reply channel to avoid blocking forever
	// if the reply channel is full, it means the senderTask is not ready to receive the reply message.
	timer := pool.GetTimer(replyChannelTimeout)
	defer pool.PutTimer(timer)

	select {
	case <-c.ctx.Done(): // the connection context done, drop the message and exit
		c.replyMsgChans.Delete(msg.ID())
		return

	case <-timer.C: // reply channel send timeout, drop the message and exit
		c.logger.Warn("reply channel send timeout, drop the message", "msgID", msg.ID())
		c.replyMsgChans.Delete(msg.ID())
		return

	case replyChan <- msg:
		// successfully sent the reply message to the reply channel
		// let consumer to remove the reply channel from replyMsgChans map
	}
}

// replyErrToSender sends an error to the corresponding reply channel in the replyMsgChans map, indicating
// that an error occurred while processing a message that expected a reply.
func (c *Connection) replyErrToSender(msg hsms.HSMSMessage, err error) {
	if msg == nil {
		c.logger.Warn("received nil message in replyErrToSender, ignore")
		return
	}

	// find the reply channel by message ID and store the error in replyErrs map.
	// the reply message channel is used by sendMsg method to wait for the reply message.
	id := msg.ID()
	replyChan, ok := c.replyMsgChans.Load(id)
	if ok {
		c.replyErrs.Store(id, err)

		select {
		case <-c.ctx.Done(): // the connection context done, exit without block the process.
			return
		case replyChan <- nil:
			return
		}
	}

	// if reply channel not found, send to data message handler.
	// it means the original request message is sent by sendMsgAsync directly.
	errMsg := hsms.NewErrorDataMessage(
		msg.StreamCode(),
		msg.FunctionCode(),
		msg.SessionID(),
		msg.SystemBytes(),
		err,
	)

	if c.session != nil {
		c.session.recvDataMsg(errMsg)
	} else {
		c.logger.Warn("session is nil, cannot handle error data message", "msgID", msg.ID(), "error", err)
	}
}

func (c *Connection) linktestConnStateHandler(_ hsms.Connection, _ hsms.ConnState, curState hsms.ConnState) {
	// HSMS-SS limits the use of linktest is only selected mode
	if curState.IsSelected() {
		ticker, err := c.taskMgr.StartInterval("autoLinktestTask", c.autoLinktestTask, c.cfg.linktestInterval, false)
		if err != nil {
			c.logger.Error("failed to start linktest ticker", "error", err)
			return
		}

		// if auto linktest is disabled after connection is established, stop the ticker immediately.
		// the linktest ticker can be restarted when auto linktest is enabled again.
		if !c.cfg.AutoLinktest() {
			ticker.Stop()
		}

		c.tickers.setLinktestTicker(ticker)
	} else {
		// stop linktest ticker when not in selected state regardless of auto linktest setting.
		c.tickers.stopLinktestTicker()
	}
}

func (c *Connection) autoLinktestTask() bool {
	msg := hsms.NewLinktestReq(hsms.GenerateMsgSystemBytes())

	c.metrics.incLinktestSendCount()

	resMsg, err := c.sendMsg(msg)

	if errors.Is(err, hsms.ErrT6Timeout) {
		c.metrics.incLinktestErrCount()
		c.logger.Error("linktest T6 timeout")
		c.stateMgr.ToNotConnectedAsync()

		return false
	}

	// if connection closed, stop linktest task and doesn't need to increase error count
	if resMsg == nil || errors.Is(err, hsms.ErrConnClosed) {
		return false
	}

	if resMsg.Type() != hsms.LinkTestRspType {
		c.metrics.incLinktestErrCount()
		c.logger.Warn("linktest response is not LinktestRsp")

		return true
	}

	c.metrics.incLinktestRecvCount()

	return true
}

func (c *Connection) validateMsg(msg hsms.HSMSMessage) error {
	if !c.cfg.validateDataMessage {
		return nil // skip validation if not enabled
	}

	// if session id mismatch and not a S9F1 message, reply S9F1.
	isS9F1 := msg.StreamCode() == 9 && msg.FunctionCode() == 1
	if msg.SessionID() != c.session.ID() && !isS9F1 {
		_, _ = c.session.SendSECS2Message(gem.S9F1())
		return hsms.ErrUnrecognizedDeviceID
	}

	// TODO: validate other message fields if needed,e.g. S9F3, S9F5, ... etc.
	return nil
}

// isSelectedState checks if the connection is in the selected state.
// It tries to check the state several times to handle edge cases where the passive connection
// may change to selected state after the active connection, and the active connection may send
// data message before the passive connection transits to selected state.
// This is particularly useful in scenarios where the connection state may not be immediately
// reflected in the state manager due to timing issues.
func (c *Connection) isSelectedState() bool {
	isSelectedState := false
	for range 3 {
		isSelectedState = c.stateMgr.IsSelected()
		if isSelectedState {
			break
		}
		sleep := min(max(c.cfg.t7Timeout/100, 50*time.Millisecond), 100*time.Millisecond)
		time.Sleep(sleep)
	}

	return isSelectedState
}

// tickerCtl is a helper struct for managing interval tasks interval.
type tickerCtl struct {
	mu                  sync.RWMutex
	linktestTicker      *time.Ticker
	activeConnectTicker *time.Ticker
}

func (l *tickerCtl) setLinktestTicker(ticker *time.Ticker) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.linktestTicker = ticker
}

func (l *tickerCtl) stopLinktestTicker() {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if l.linktestTicker != nil {
		l.linktestTicker.Stop()
	}
}

func (l *tickerCtl) resetLinktestTicker(d time.Duration) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if l.linktestTicker != nil && d > 0 {
		l.linktestTicker.Reset(d)
	}
}

func (l *tickerCtl) setActiveConnectTicker(ticker *time.Ticker) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.activeConnectTicker = ticker
}

func (l *tickerCtl) resetActiveConnectTicker(d time.Duration) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if l.activeConnectTicker != nil && d > 0 {
		l.activeConnectTicker.Reset(d)
	}
}

func isNetOpError(err error) bool {
	opErr := &net.OpError{}
	return errors.As(err, &opErr)
}

func isConnTimedOutError(err error) bool {
	if err == nil {
		return false
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	if strings.Contains(err.Error(), "i/o timeout") {
		return true
	}

	return false
}

func isConnClosedError(err error) bool {
	return errors.Is(err, net.ErrClosed)
}

func isConnResetError(err error) bool {
	return strings.Contains(err.Error(), "connection reset by peer")
}

func isNetError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, io.EOF) ||
		isNetOpError(err) ||
		isConnClosedError(err) ||
		isConnTimedOutError(err) ||
		isConnResetError(err) {
		return true
	}

	return false
}

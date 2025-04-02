package hsmsss

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/go-secs/gem"
	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/internal/pool"
	"github.com/arloliu/go-secs/logger"
	"github.com/puzpuzpuz/xsync/v3"
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
	listenerMutex sync.Mutex         // TCP connection mutex
	conn          net.Conn           // TCP connection
	opState       hsms.AtomicOpState // operation state
	connMutex     sync.RWMutex       // TCP connection mutex
	connCount     atomic.Int32       // connection counter for passive mode only
	session       *Session           // HSMS-SS has only one session

	stateMgr     *hsms.ConnStateMgr
	taskMgr      *hsms.TaskManager
	shutdown     atomic.Bool // indicates if has entered shutdown mode
	recvSeparate atomic.Bool // indicates if spearate request has been received
	ticketMgr    tickerCtl   // linktest control

	senderMsgChan chan hsms.HSMSMessage
	replyMsgChans *xsync.MapOf[uint32, chan hsms.HSMSMessage]
	replyErrs     *xsync.MapOf[uint32, error]

	metrics ConnectionMetrics // connection metrics
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
	}

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

func (c *Connection) UpdateConfigOptions(opts ...ConnOption) error {
	var autoLinktest bool
	var linktestInterval time.Duration
	var t5Timeout time.Duration

	for _, opt := range opts {
		connOpt, ok := opt.(*connOptFunc)
		if !ok {
			return errors.New("invalid ConnOption type")
		}

		switch connOpt.name {
		case "WithAutoLinktest":
			autoLinktest = c.cfg.autoLinktest

		case "WithLinktestInterval":
			linktestInterval = c.cfg.linktestInterval

		case "WithT5Timeout":
			t5Timeout = c.cfg.t5Timeout
		}

		if err := opt.apply(c.cfg); err != nil {
			return err
		}
	}

	if c.cfg.autoLinktest != autoLinktest { // autoLinktest changed
		if c.cfg.autoLinktest { // enable autoLinktest
			c.ticketMgr.resetLinktestTicker(c.cfg.linktestInterval)
		} else { // disable autoLinktest
			c.ticketMgr.stopLinktestTicker()
		}
	} else if c.cfg.linktestInterval != linktestInterval { // autoLinktest doesn't changed ,linktestInterval changed
		if c.cfg.autoLinktest {
			c.ticketMgr.resetLinktestTicker(c.cfg.linktestInterval)
		} else {
			c.ticketMgr.resetLinktestTicker(time.Duration(1<<63 - 1))
		}
	}

	if c.cfg.t5Timeout != t5Timeout {
		c.ticketMgr.resetActiveConnectTicker(c.cfg.t5Timeout)
	}

	return nil
}

// AddSession creates and adds a new Session to the connection with the specified session ID.
// For HSMS-SS, this method should only be called once, as it supports only a single session.
// It returns the newly created Session.
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

	c.recvSeparate.Store(false)

	c.createContext()

	// create sender message channel before open connection
	// the sender message channel is used to send messages to the senderTask
	c.senderMsgChan = make(chan hsms.HSMSMessage, c.cfg.senderQueueSize)

	if c.cfg.isActive {
		ticker := c.taskMgr.StartInterval("openActive", c.openActive, c.cfg.t5Timeout, true)
		c.ticketMgr.setActiveConnectTicker(ticker)

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
		default:
			if c.isClosed() {
				c.logger.Debug("wait for connection closed success")
				return nil
			}

			runtime.Gosched() // yield the CPU to allow other goroutines to run
			time.Sleep(1 * time.Millisecond)
		}
	}
}

func (c *Connection) isClosed() bool {
	return c.opState.IsClosed() && c.stateMgr.State() == hsms.NotConnectedState && c.stateMgr.DesiredState() == hsms.NotConnectedState
}

// closeConn performs the actual connection closing process with a timeout.
// It cancels the context, stops the task manager, closes the TCP connection, and waits for
// all goroutines to terminate.
func (c *Connection) closeConn(timeout time.Duration) {
	if !c.opState.ToClosing() {
		if c.opState.IsClosed() {
			c.logger.Warn("connection already closed, no need to close again", "method", "closeConn", "opState", c.opState.String())
		} else {
			c.logger.Warn("failed to set connection to closing state", "method", "closeConn", "opState", c.opState.String())
		}

		return
	}

	c.logger.Debug("start to close connection", "method", "closeConn", "opState", c.opState.String())

	// drain sender message channel before close connection
drainLoop:
	for {
		select {
		case msg, ok := <-c.senderMsgChan:
			if !ok {
				break drainLoop
			}
			if msg != nil {
				_ = c.senderTask(msg)
			}
		default:
			break drainLoop
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if c.ctxCancel != nil {
		c.logger.Debug("trigger context cancel function", "method", "closeConn")
		c.ctxCancel()
	}

	c.taskMgr.Stop()

	// close TCP connection
	c.connMutex.Lock()
	remoteAddr := ""
	if c.conn != nil {
		remoteAddr = c.conn.RemoteAddr().String()
		c.logger.Debug("close TCP connection", "method", "closeConn")
		if tcpConn, ok := c.conn.(*net.TCPConn); ok {
			_ = tcpConn.SetLinger(0) // Set linger timeout to 0 to force close
		}
		if !c.cfg.isActive {
			c.connCount.Add(-1)
		}

		err := c.conn.Close()
		if err != nil && !errors.Is(err, net.ErrClosed) {
			c.logger.Error("failed to close TCP connection", "method", "closeConn", "error", err)
		}
	}
	c.connMutex.Unlock()

	go func() {
		c.logger.Debug("wait all goroutines terminated, taskMgr", "method", "closeConn")
		c.taskMgr.Wait()
		c.logger.Debug("all goroutines terminated", "method", "closeConn")
		cancel()
	}()

	// wait all goroutines terminated
	<-ctx.Done()

	if errors.Is(ctx.Err(), context.Canceled) {
		c.logger.Debug("close success", "method", "closeConn")
	} else {
		c.logger.Error("close timeout", "method", "closeConn", "error", ctx.Err(), "timeout", timeout)
	}

	// drop all message that wait reply
	c.dropAllReplyMsgs()

	// close sender message channel
	close(c.senderMsgChan)

	if !c.opState.ToClosed() {
		c.logger.Warn("failed to set connection to closed state", "method", "closeConn", "opState", c.opState.String())
	} else {
		c.logger.Debug("connection closed", "method", "closeConn", "remoteAddr", remoteAddr)
	}
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

	buf := msg.ToBytes()

	c.connMutex.RLock()
	defer c.connMutex.RUnlock()

	err := c.conn.SetWriteDeadline(time.Now().Add(c.cfg.t8Timeout))
	if err != nil {
		return err
	}

	if c.logger.Level() == logger.DebugLevel {
		c.logger.Debug("try to send message to remote", hsms.MsgInfo(msg, "method", "sendMsgSync", "timeout", c.cfg.t8Timeout)...)
	}

	_, err = c.conn.Write(buf)
	if err != nil {
		return err
	}

	if c.logger.Level() == logger.DebugLevel {
		c.logger.Debug("message sent to remote", hsms.MsgInfo(msg, "method", "sendMsgSync")...)
	}

	return nil
}

// sendMsgAsync sends an HSMS message asynchronously by sending it to the senderMsgChan.
// It uses a non-blocking select with a timeout to avoid blocking the caller.
func (c *Connection) sendMsgAsync(msg hsms.HSMSMessage) error {
	timer := pool.GetTimer(c.cfg.t8Timeout)
	defer pool.PutTimer(timer)

	select {
	case <-timer.C:
		return hsms.ErrT8Timeout
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
	_, _ = c.replyMsgChans.Compute(id, func(ch chan hsms.HSMSMessage, loaded bool) (chan hsms.HSMSMessage, bool) {
		if loaded {
			close(ch)
		}

		return nil, true
	})
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
	c.metrics.incDataMsgSendCount()
	if msg.WaitBit() {
		c.metrics.incDataMsgInflightCount()
	}

	err := c.sendMsgSync(msg)
	if err != nil {
		c.metrics.incDataMsgErrCount()
		c.replyErrToSender(msg.ID(), err)

		if !isNetOpError(err) {
			c.logger.Error("failed to send message", "method", "senderTask", "error", err)
		}

		return false
	}

	return true
}

// cancelReceiverTask cancels the receiver task by transitioning the connection state to NotConnected.
func (c *Connection) cancelReceiverTask() {
	c.stateMgr.ToNotConnectedAsync()
}

// receiverTask is the task function for the receiver goroutine.
// It reads and decodes HSMS messages from the connection and processes them accordingly.
func (c *Connection) receiverTask(reader *bufio.Reader, msgLenBuf []byte) bool {
	c.metrics.decDataMsgInflightCount()

	if _, err := io.ReadFull(reader, msgLenBuf); err != nil {
		if !isNetError(err) {
			c.logger.Error("failed to read the length of HSMS message", "method", "receiverTask", "error", err)
		}

		return false
	}

	msgLen := binary.BigEndian.Uint32(msgLenBuf)

	msgBuf := make([]byte, msgLen)
	if _, err := io.ReadFull(reader, msgBuf); err != nil {
		if err != io.EOF {
			c.logger.Error("failed to read HSMS message", "method", "receiverTask", "error", err)
		}

		return false
	}

	msg, err := hsms.DecodeMessage(msgLen, msgBuf)
	if err != nil {
		c.metrics.incDataMsgErrCount()
		c.logger.Error("failed to decode HSMS message")
	}

	c.metrics.incDataMsgRecvCount()

	if c.cfg.isActive {
		c.recvMsgActive(msg)
	} else {
		c.recvMsgPassive(msg)
	}

	return true
}

// replyToSender sends a received reply message to the corresponding reply channel in the replyMsgChans map.
func (c *Connection) replyToSender(msg hsms.HSMSMessage) {
	// use Compute to avoid race condition, the valueFn in Compute is thread-safe.
	_, _ = c.replyMsgChans.Compute(msg.ID(), func(replyChan chan hsms.HSMSMessage, loaded bool) (chan hsms.HSMSMessage, bool) {
		if loaded {
			if replyChan == nil {
				return nil, true
			}

			select {
			case <-c.ctx.Done(): // the connection context done, exit without block the process.
				return nil, true
			case replyChan <- msg:
				return replyChan, false
			}
		}

		// if reply channel not found, send to data message handler
		if dataMsg, ok := msg.ToDataMessage(); ok {
			c.session.recvDataMsg(dataMsg)
		}

		// set delete to true to not
		return nil, true
	})
}

// replyErrToSender sends an error to the corresponding reply channel in the replyMsgChans map, indicating
// that an error occurred while processing a message that expected a reply.
func (c *Connection) replyErrToSender(id uint32, err error) {
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
}

func (c *Connection) linktestConnStateHandler(_ hsms.Connection, _ hsms.ConnState, curState hsms.ConnState) {
	// HSMS-SS limits the use of linktest is only selected mode
	if curState.IsSelected() {
		ticker := c.taskMgr.StartInterval("autoLinktestTask", c.autoLinktestTask, c.cfg.linktestInterval, false)
		c.ticketMgr.setLinktestTicker(ticker)
	} else {
		c.ticketMgr.stopLinktestTicker()
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

// tickerCtl is a helper struct for managing interval tasks interval.
type tickerCtl struct {
	mu                  sync.Mutex
	linktestTicker      *time.Ticker
	activeConnectTicker *time.Ticker
}

func (l *tickerCtl) setLinktestTicker(ticker *time.Ticker) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.linktestTicker = ticker
}

func (l *tickerCtl) stopLinktestTicker() {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.linktestTicker != nil {
		l.linktestTicker.Stop()
	}
}

func (l *tickerCtl) resetLinktestTicker(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

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
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.activeConnectTicker != nil && d > 0 {
		l.activeConnectTicker.Reset(d)
	}
}

func isNetOpError(err error) bool {
	opErr := &net.OpError{}
	return errors.As(err, &opErr)
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

	if errors.Is(err, io.EOF) || isNetOpError(err) || isConnClosedError(err) || isConnResetError(err) {
		return true
	}

	return false
}

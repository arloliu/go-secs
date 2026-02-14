package secs1

import (
	"context"
	"errors"
	"fmt"
	"net"
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

const (
	// pollTimeout is the timeout for polling incoming bytes when the
	// protocol loop is idle. It trades off between CPU usage and latency
	// for outgoing messages.
	pollTimeout = 50 * time.Millisecond

	// replyChannelTimeout is the timeout for sending to reply channels.
	replyChannelTimeout = time.Second

	// closeCheckInterval is the interval for checking close status in Close().
	closeCheckInterval = 5 * time.Millisecond
)

// Sentinel errors for the SECS-I protocol.
var (
	// Block-transfer errors.
	ErrSendFailure      = errors.New("secs1: block send failure, retries exhausted")
	ErrT1Timeout        = errors.New("secs1: T1 inter-character timeout")
	ErrT2Timeout        = errors.New("secs1: T2 protocol timeout")
	ErrT3Timeout        = errors.New("secs1: T3 reply timeout")
	ErrT4Timeout        = errors.New("secs1: T4 inter-block timeout")
	ErrInvalidLength    = errors.New("secs1: invalid block length")
	ErrChecksumMismatch = errors.New("secs1: checksum mismatch")
	ErrContention       = errors.New("secs1: contention detected")
	ErrDuplicateBlock   = errors.New("secs1: duplicate block detected")
	ErrConnClosed       = errors.New("secs1: connection closed")
	ErrDeviceIDMismatch = errors.New("secs1: device ID mismatch")

	// Message-level errors.
	ErrBlockNumberMismatch = errors.New("secs1: block number mismatch in multi-block message")
	ErrHeaderMismatch      = errors.New("secs1: continuation block header mismatch")
	ErrMessageAborted      = errors.New("secs1: message aborted")

	// Connection-level errors.
	ErrSessionNil       = errors.New("secs1: session is nil")
	ErrNotSelectedState = errors.New("secs1: connection is not in selected state")
	ErrSendMsgTimeout   = errors.New("secs1: send message timeout")
)

// Connection represents a SECS-I over TCP/IP connection, implementing the
// [hsms.Connection] interface.
//
// It manages the communication with a remote SECS-I device, handling
// block-transfer protocol, message assembly/disassembly, connection state
// transitions, and the half-duplex ENQ/EOT/ACK/NAK protocol.
type Connection struct {
	pctx      context.Context
	ctx       context.Context
	ctxCancel context.CancelFunc
	cfg       *ConnectionConfig
	logger    logger.Logger

	// TCP state (passive mode).
	listener      net.Listener
	listenerMutex sync.Mutex

	opState   hsms.AtomicOpState
	connCount atomic.Int32 // passive mode only
	session   *Session

	// TCP resources.
	connMutex sync.RWMutex
	tcpConn   net.Conn

	// State management.
	stateMgr *hsms.ConnStateMgr
	taskMgr  *hsms.TaskManager
	shutdown atomic.Bool

	// Reconnect (active mode).
	connectLoopRunning atomic.Bool
	reconnectGen       atomic.Uint64
	loopCtx            context.Context    // cancelled on Close to wake the connect loop
	loopCancel         context.CancelFunc // cancels loopCtx

	// Message assembler for multi-block message assembly.
	// Created when the protocol loop starts, closed when it stops.
	assembler *messageAssembler

	// Application-level messaging.
	//
	// senderMsgChan is read by the protocol loop; producers write
	// sendRequests into it via queueSendRequest. It is created once in
	// NewConnection and never closed.
	senderMsgChan chan *sendRequest
	replyMsgChans *xsync.MapOf[uint32, chan *hsms.DataMessage]
	replyErrs     *xsync.MapOf[uint32, error]

	metrics ConnectionMetrics
}

// Compile-time check: Connection implements hsms.Connection.
var _ hsms.Connection = (*Connection)(nil)

// NewConnection creates a new SECS-I Connection with the given context and configuration.
//
// It initializes the connection state, task manager, and other necessary components.
// The caller must call [Connection.AddSession] before [Connection.Open].
func NewConnection(ctx context.Context, cfg *ConnectionConfig) (*Connection, error) {
	if cfg == nil {
		return nil, errors.New("secs1: connection config is nil")
	}

	c := &Connection{
		pctx:          ctx,
		cfg:           cfg,
		logger:        cfg.logger,
		replyMsgChans: xsync.NewMapOf[uint32, chan *hsms.DataMessage](),
		replyErrs:     xsync.NewMapOf[uint32, error](),
		taskMgr:       hsms.NewTaskManager(ctx, cfg.logger),
	}

	c.senderMsgChan = make(chan *sendRequest, cfg.senderQueueSize)
	c.opState.Set(hsms.ClosedState)
	c.createContext()

	if cfg.isActive {
		c.stateMgr = hsms.NewConnStateMgr(ctx, c, c.activeConnStateHandler)
	} else {
		c.stateMgr = hsms.NewConnStateMgr(ctx, c, c.passiveConnStateHandler)
	}

	return c, nil
}

// --- hsms.Connection interface ---

// Open establishes the SECS-I connection.
//
// If waitOpened is true, it blocks until the connection reaches the Selected
// state or an error occurs. If false, it initiates the connection process
// and returns immediately.
func (c *Connection) Open(waitOpened bool) error {
	c.shutdown.Store(false)
	c.loopCtx, c.loopCancel = context.WithCancel(c.pctx)
	c.stateMgr.Start()

	return c.doOpen(waitOpened)
}

// Close closes the SECS-I connection gracefully.
//
// It terminates all running tasks, closes the TCP connection, and resets
// the connection state.
func (c *Connection) Close() error {
	c.reconnectGen.Add(1)
	c.shutdown.Store(true)

	// Cancel loopCtx to wake the connect loop immediately.
	if c.loopCancel != nil {
		c.loopCancel()
	}

	c.logger.Debug("secs1: start to close connection", "opState", c.opState.String())

	c.stateMgr.Stop()

	if !c.isClosed() {
		c.stateMgr.ToNotConnected()
	}

	closeTimer := pool.GetTimer(c.cfg.closeTimeout)
	defer pool.PutTimer(closeTimer)

	checkTicker := time.NewTicker(closeCheckInterval)
	defer checkTicker.Stop()

	for {
		select {
		case <-closeTimer.C:
			if c.isClosed() {
				return nil
			}

			c.logger.Error("secs1: close connection timeout",
				"timeout", c.cfg.closeTimeout,
				"opState", c.opState.String())

			return errors.New("secs1: close connection timeout")

		case <-checkTicker.C:
			if c.isClosed() {
				return nil
			}
		}
	}
}

// AddSession creates and adds a new Session to the connection with the
// specified session ID.
//
// For SECS-I, this method should be called exactly once, as SECS-I is a
// single-session protocol. The session ID corresponds to the device ID.
//
// IMPORTANT: This method is NOT thread-safe and MUST be called before Open().
func (c *Connection) AddSession(sessionID uint16) hsms.Session {
	c.session = newSession(sessionID, c)

	return c.session
}

// GetLogger returns the logger associated with the connection.
func (c *Connection) GetLogger() logger.Logger {
	return c.logger
}

// GetMetrics returns the metrics associated with the connection.
func (c *Connection) GetMetrics() *ConnectionMetrics {
	return &c.metrics
}

// IsSingleSession returns true; SECS-I is a point-to-point single-session protocol.
func (c *Connection) IsSingleSession() bool { return true }

// IsGeneralSession returns false; SECS-I is not an HSMS-GS connection.
func (c *Connection) IsGeneralSession() bool { return false }

// IsSECS1 returns true; this is a SECS-I connection.
func (c *Connection) IsSECS1() bool { return true }

// --- Connection lifecycle ---

func (c *Connection) doOpen(waitOpened bool) error {
	if c.session == nil {
		return ErrSessionNil
	}

	if !c.opState.ToOpening() {
		c.logger.Warn("secs1: failed to set connection to opening state",
			"opState", c.opState.String())

		return nil
	}

	c.createContext()

	if c.cfg.isActive {
		if err := c.openActive(); err != nil {
			return err
		}

		if waitOpened {
			return c.stateMgr.WaitState(c.ctx, hsms.SelectedState)
		}
	} else {
		if err := c.openPassive(); err != nil {
			return err
		}
	}

	return nil
}

func (c *Connection) isClosed() bool {
	return c.opState.IsClosed() &&
		c.stateMgr.State() == hsms.NotConnectedState &&
		c.stateMgr.DesiredState() == hsms.NotConnectedState
}

func (c *Connection) createContext() {
	c.ctx, c.ctxCancel = context.WithCancel(c.pctx)
}

// --- TCP resource management ---

func (c *Connection) setupTCPConn(conn net.Conn) {
	c.connMutex.Lock()
	defer c.connMutex.Unlock()

	c.tcpConn = conn
}

func (c *Connection) getTCPConn() net.Conn {
	c.connMutex.RLock()
	defer c.connMutex.RUnlock()

	return c.tcpConn
}

// closeTCP closes the TCP connection and returns the remote address.
func (c *Connection) closeTCP(timeout time.Duration) string {
	c.connMutex.Lock()
	conn := c.tcpConn
	if conn == nil {
		c.connMutex.Unlock()

		return ""
	}

	// Nil the reference under the write lock so subsequent calls are no-ops.
	c.tcpConn = nil
	c.connMutex.Unlock()

	remote := conn.RemoteAddr().String()

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
		c.logger.Error("secs1: failed to close TCP connection", "error", err)
	}

	return remote
}

// closeConn performs the full connection closing sequence: drains the sender
// channel, cancels the context, closes TCP, and waits for all tasks.
func (c *Connection) closeConn(timeout time.Duration) error {
	if !c.opState.ToClosing() {
		if c.opState.IsClosed() {
			return nil
		}

		c.logger.Warn("secs1: failed to set connection to closing state",
			"opState", c.opState.String())

		return fmt.Errorf("secs1: failed to set connection to closing state: %s", c.opState.String())
	}

	closeCtx, closeCtxCancel := context.WithTimeout(context.Background(), timeout)
	defer closeCtxCancel()

	// Drain pending outgoing messages.
	c.drainSenderMsgChan(closeCtx)

	// Cancel per-connection context.
	if c.ctxCancel != nil {
		c.ctxCancel()
	}

	// Close TCP to unblock the protocol loop.
	remoteAddr := c.closeTCP(timeout)

	// Stop all tasks.
	c.taskMgr.Stop()

	// Wait for task termination with timeout.
	go func() {
		c.taskMgr.Wait()
		closeCtxCancel()
	}()

	<-closeCtx.Done()

	var closeErr error
	if !errors.Is(closeCtx.Err(), context.Canceled) {
		c.logger.Error("secs1: close timeout", "error", closeCtx.Err(), "timeout", timeout)
		closeErr = fmt.Errorf("secs1: close timeout: %w", closeCtx.Err())
	}

	// Cleanup reply channels.
	c.dropAllReplyMsgs()

	if !c.opState.ToClosed() {
		c.logger.Warn("secs1: failed to set connection to closed state",
			"opState", c.opState.String())

		return fmt.Errorf("secs1: failed to set connection to closed state: %s", c.opState.String())
	}

	c.logger.Debug("secs1: connection closed", "remoteAddr", remoteAddr)

	return closeErr
}

func (c *Connection) drainSenderMsgChan(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-c.senderMsgChan:
			if !ok {
				return
			}
		default:
			return
		}
	}
}

// --- Protocol loop ---

// startProtocolLoop starts the half-duplex SECS-I protocol loop as a managed task.
//
// The protocol loop alternates between sending outgoing messages and polling
// for incoming ENQ bytes, consistent with the SECS-I half-duplex nature.
func (c *Connection) startProtocolLoop() error {
	conn := c.getTCPConn()
	if conn == nil {
		return ErrConnClosed
	}

	// Create the block transport for this TCP connection.
	bt := newBlockTransport(conn, c.cfg, c.logger, c.handleReceivedBlock, c.metrics.incBlockRetryCount)

	// Create the message assembler for multi-block assembly.
	c.assembler = newMessageAssembler(c.cfg, c.handleCompleteMessage)

	return c.taskMgr.Start("protocolLoop", func() bool {
		return c.protocolLoopIteration(bt)
	})
}

// protocolLoopIteration performs a single iteration of the protocol loop.
//
// It checks for outgoing messages first (non-blocking), then polls for
// incoming ENQ if there is nothing to send.
func (c *Connection) protocolLoopIteration(bt *blockTransport) bool {
	// Priority: check for outgoing messages first.
	select {
	case <-c.ctx.Done():
		if c.assembler != nil {
			c.assembler.close()
		}

		return false

	case req := <-c.senderMsgChan:
		if req == nil {
			return true
		}

		c.handleOutgoingMessage(bt, req)

		return true

	default:
		// No outgoing message — poll for incoming traffic.
	}

	return c.pollForIncoming(bt)
}

// handleOutgoingMessage splits a DataMessage into blocks and sends them
// sequentially via the block-transfer protocol.
//
// If the request has a sentChan, it is signaled with nil on success or
// the error on failure. Otherwise, send failures are reported via
// replyErrToSender (fire-and-forget path).
func (c *Connection) handleOutgoingMessage(bt *blockTransport, req *sendRequest) {
	msg := req.msg
	blocks := SplitMessage(msg, c.cfg.deviceID, c.cfg.isEquip)

	for _, block := range blocks {
		if err := bt.sendBlock(c.ctx, block); err != nil {
			c.logger.Error("secs1: block send failed",
				"stream", msg.StreamCode(),
				"function", msg.FunctionCode(),
				"error", err)

			c.metrics.incDataMsgErrCount()

			if req.sentChan != nil {
				req.sentChan <- err
			} else {
				c.replyErrToSender(msg, err)
			}

			return
		}

		c.metrics.incBlockSendCount()
	}

	c.metrics.incDataMsgSendCount()

	if req.sentChan != nil {
		req.sentChan <- nil
	}
}

// pollForIncoming reads a single byte from the TCP connection with a short
// timeout. If ENQ is received, it initiates the receive flow (send EOT,
// receive block, feed to assembler).
func (c *Connection) pollForIncoming(bt *blockTransport) bool {
	b, err := bt.readByte(pollTimeout)
	if err != nil {
		// Timeout or context cancellation — normal in idle state.
		if isConnClosedError(err) || isConnResetError(err) {
			c.logger.Debug("secs1: connection closed during poll")
			c.stateMgr.ToNotConnectedAsync()

			return false
		}

		return true // timeout, continue polling
	}

	if b != ENQ {
		// Ignore unexpected bytes in idle state.
		c.logger.Debug("secs1: unexpected byte in idle state", "byte", fmt.Sprintf("0x%02X", b))

		return true
	}

	// Remote wants to send — respond with EOT and receive the block.
	if err := bt.writeByte(EOT); err != nil {
		c.logger.Error("secs1: failed to send EOT", "error", err)
		c.stateMgr.ToNotConnectedAsync()

		return false
	}

	block, err := bt.receiveBlock(c.ctx)
	if err != nil {
		c.logger.Debug("secs1: failed to receive block", "error", err)
		// Receive failure is not fatal to the connection; the sender will retry.
		return true
	}

	c.handleReceivedBlock(block)

	return true
}

// handleReceivedBlock is the callback invoked for each successfully received
// block, both during normal receive and during contention yield.
func (c *Connection) handleReceivedBlock(block *Block) {
	c.metrics.incBlockRecvCount()

	if c.assembler == nil {
		c.logger.Warn("secs1: received block but assembler not initialized")

		return
	}

	if err := c.assembler.processBlock(block); err != nil {
		c.logger.Debug("secs1: assembler rejected block", "error", err)

		// Send S9Fx error messages (equipment → host only).
		if c.cfg.isEquip && c.cfg.validateDataMessage && c.session != nil {
			switch {
			case errors.Is(err, ErrDeviceIDMismatch):
				_, _ = c.session.SendSECS2Message(gem.S9F1())
			case errors.Is(err, ErrBlockNumberMismatch):
				_, _ = c.session.SendSECS2Message(gem.S9F7())
			}
		}
	}
}

// handleCompleteMessage is called by the messageAssembler when a complete
// message (all blocks received) is ready for delivery to the application.
func (c *Connection) handleCompleteMessage(blocks []*Block) {
	msg, err := AssembleMessage(blocks)
	if err != nil {
		c.logger.Error("secs1: failed to assemble message", "error", err)
		c.metrics.incDataMsgErrCount()

		return
	}

	c.metrics.incDataMsgRecvCount()

	// Dispatch based on function code parity.
	if msg.FunctionCode()%2 != 0 {
		// Primary message — deliver to application handlers.
		if c.session != nil {
			c.session.recvDataMsg(msg)
		}
	} else {
		// Secondary (reply) message — match to waiting sender.
		c.replyToSender(msg)
	}
}

// --- Message sending ---

// sendMsg sends a DataMessage and optionally waits for its reply (if W-bit is set).
//
// This is the main entry point for the Session to send messages. It queues the
// message for the protocol loop and manages the T3 reply timeout.
//
// Per SEMI E4 §9.3.1, the reply timer (T3) is started only after the last
// block of the message has been successfully sent (ACK'd).
func (c *Connection) sendMsg(msg hsms.HSMSMessage) (hsms.HSMSMessage, error) {
	if !c.stateMgr.IsSelected() {
		return nil, ErrNotSelectedState
	}

	dataMsg, ok := msg.ToDataMessage()
	if !ok {
		return nil, hsms.ErrNotDataMsg
	}

	if !msg.WaitBit() {
		if err := c.sendMsgAsync(dataMsg); err != nil {
			return nil, err
		}

		return nil, nil //nolint:nilnil
	}

	// W-bit set: register reply channel and queue with send-completion notification.
	id := msg.ID()
	replyChan := c.addReplyExpectedMsg(id)

	sentChan := make(chan error, 1)
	req := &sendRequest{msg: dataMsg, sentChan: sentChan}

	if err := c.queueSendRequest(req); err != nil {
		c.removeReplyExpectedMsg(id)

		return nil, err
	}

	// Phase 1: Wait for all blocks to be sent (ACK'd) by the protocol loop.
	select {
	case <-c.ctx.Done():
		c.removeReplyExpectedMsg(id)

		return nil, ErrConnClosed

	case sendErr := <-sentChan:
		if sendErr != nil {
			c.removeReplyExpectedMsg(id)

			return nil, sendErr
		}
	}

	// Phase 2: All blocks sent — start T3 per §9.3.1.
	sendTimer := pool.GetTimer(c.cfg.t3Timeout)
	defer pool.PutTimer(sendTimer)

	c.metrics.incDataMsgInflightCount()

	select {
	case <-c.ctx.Done():
		c.removeReplyExpectedMsg(id)
		c.metrics.decDataMsgInflightCount()

		return nil, ErrConnClosed

	case <-sendTimer.C:
		c.removeReplyExpectedMsg(id)
		c.metrics.decDataMsgInflightCount()

		c.logger.Warn("secs1: T3 reply timeout",
			"stream", msg.StreamCode(),
			"function", msg.FunctionCode(),
			"timeout", c.cfg.t3Timeout)

		// If equipment, send S9F9 on T3 timeout.
		if c.cfg.isEquip && c.session != nil {
			_, _ = c.session.SendSECS2Message(gem.S9F9())
		}

		return nil, ErrT3Timeout

	case replyMsg := <-replyChan:
		c.metrics.decDataMsgInflightCount()
		c.removeReplyExpectedMsg(id)

		if replyMsg == nil {
			if err, ok := c.replyErrs.LoadAndDelete(id); ok {
				return nil, err
			}

			return nil, ErrConnClosed
		}

		return replyMsg, nil
	}
}

// sendRequest wraps a DataMessage with optional send-completion notification.
//
// When sentChan is non-nil, the protocol loop signals it after all blocks
// have been ACK'd (nil) or on the first send failure (error). This is used
// by sendMsg to start T3 only after the message is fully on the wire,
// per SEMI E4 §9.3.1.
type sendRequest struct {
	msg      *hsms.DataMessage
	sentChan chan error // nil for fire-and-forget (sendMsgAsync / sendMsgSync)
}

// sendMsgAsync queues a DataMessage for transmission by the protocol loop.
// This is a fire-and-forget path: the caller does not wait for send completion.
func (c *Connection) sendMsgAsync(msg *hsms.DataMessage) error {
	return c.queueSendRequest(&sendRequest{msg: msg})
}

// queueSendRequest puts a sendRequest onto the protocol loop's channel.
func (c *Connection) queueSendRequest(req *sendRequest) error {
	timer := pool.GetTimer(c.cfg.sendTimeout)
	defer pool.PutTimer(timer)

	select {
	case <-c.ctx.Done():
		return ErrConnClosed
	case <-timer.C:
		return ErrSendMsgTimeout
	case c.senderMsgChan <- req:
		return nil
	}
}

// sendMsgSync sends a DataMessage synchronously by queuing and waiting
// for the protocol loop to pick it up.
func (c *Connection) sendMsgSync(msg hsms.HSMSMessage) error {
	if !c.stateMgr.IsSelected() {
		return ErrNotSelectedState
	}

	dataMsg, ok := msg.ToDataMessage()
	if !ok {
		return hsms.ErrNotDataMsg
	}

	return c.sendMsgAsync(dataMsg)
}

// --- Reply channel management ---

func (c *Connection) addReplyExpectedMsg(id uint32) <-chan *hsms.DataMessage {
	ch := make(chan *hsms.DataMessage, 1)
	c.replyMsgChans.Store(id, ch)

	return ch
}

func (c *Connection) removeReplyExpectedMsg(id uint32) {
	c.replyMsgChans.Delete(id)
}

func (c *Connection) dropAllReplyMsgs() {
	c.replyMsgChans.Range(func(id uint32, ch chan *hsms.DataMessage) bool {
		if ch != nil {
			close(ch)
		}

		return true
	})

	c.replyMsgChans.Clear()
}

// replyToSender matches a received reply (secondary) message to the sender
// waiting on the corresponding reply channel.
//
// If no sender is waiting (e.g. the primary was sent without W-bit, or
// the reply arrived after T3 expiry), the message is delivered to the
// application's data message handler for application-level handling.
func (c *Connection) replyToSender(msg *hsms.DataMessage) {
	replyChan, loaded := c.replyMsgChans.Load(msg.ID())
	if !loaded || replyChan == nil {
		// No sender waiting — deliver to data message handler.
		c.logger.Debug("secs1: reply message has no waiting sender, forwarding to handler",
			"stream", msg.StreamCode(),
			"function", msg.FunctionCode(),
			"id", msg.ID())

		if c.session != nil {
			c.session.recvDataMsg(msg)
		}

		return
	}

	timer := pool.GetTimer(replyChannelTimeout)
	defer pool.PutTimer(timer)

	select {
	case <-c.ctx.Done():
		c.replyMsgChans.Delete(msg.ID())

	case <-timer.C:
		c.logger.Warn("secs1: reply channel send timeout, dropping message",
			"msgID", msg.ID())
		c.replyMsgChans.Delete(msg.ID())

	case replyChan <- msg:
		// Successfully delivered.
	}
}

// replyErrToSender sends an error to the sender waiting on the reply channel.
func (c *Connection) replyErrToSender(msg *hsms.DataMessage, err error) {
	if msg == nil {
		return
	}

	id := msg.ID()
	replyChan, ok := c.replyMsgChans.Load(id)

	if ok && replyChan != nil {
		c.replyErrs.Store(id, err)

		select {
		case <-c.ctx.Done():
		case replyChan <- nil:
		}

		return
	}

	// No waiting sender — deliver error as DataMessage to handler.
	errMsg := hsms.NewErrorDataMessage(
		msg.StreamCode(),
		msg.FunctionCode(),
		msg.SessionID(),
		msg.SystemBytes(),
		err,
	)

	if c.session != nil {
		c.session.recvDataMsg(errMsg)
	}
}

// --- Helpers ---

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

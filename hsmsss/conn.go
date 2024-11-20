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

	listener      net.Listener // listener is for passive mode only
	listenerMutex sync.Mutex   // TCP connection mutex
	conn          net.Conn     // TCP connection
	connMutex     sync.Mutex   // TCP connection mutex
	connCount     atomic.Int32 // connection counter for passive mode only
	session       *Session     // HSMS-SS has only one session

	stateMgr       *hsms.ConnStateMgr
	taskMgr        *hsms.TaskManager
	shutdown       atomic.Bool // indicates if has entered shutdown mode
	recvSeparate   atomic.Bool // indicates if spearate request has been received
	linktestTicker *time.Ticker

	senderMsgChan chan hsms.HSMSMessage
	replyMsgChans sync.Map
	replyErrs     sync.Map
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
		senderMsgChan: make(chan hsms.HSMSMessage, cfg.senderQueueSize),
		taskMgr:       hsms.NewTaskManager(ctx, cfg.logger),
	}

	conn.createContext()

	if cfg.isActive {
		conn.stateMgr = hsms.NewConnStateMgr(ctx, conn, conn.activeConnStateHandler)
	} else {
		conn.stateMgr = hsms.NewConnStateMgr(ctx, conn, conn.passiveConnStateHandler)
	}

	if cfg.autoLinktest {
		conn.stateMgr.AddHandler(conn.linktestConnStateHandler)
	}

	return conn, nil
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

// IsSingleSession returns true, indicating that this is an HSMS-SS connection.
func (c *Connection) IsSingleSession() bool { return true }

// IsGeneralSession returns false, indicating that this is not an HSMS-GS connection.
func (c *Connection) IsGeneralSession() bool { return false }

// Open establishes the HSMS-SS connection.
// If waitOpened is true, it blocks until the connection is fully established (Selected state)
// or an error occurs.
// If waitOpened is false, it initiates the connection process and returns immediately.
func (c *Connection) Open(waitOpened bool) error {
	if c.session == nil {
		return hsms.ErrSessionNil
	}

	c.recvSeparate.Store(false)
	c.shutdown.Store(false)

	c.createContext()

	if c.cfg.isActive {
		c.taskMgr.StartInterval("openActive", c.openActive, c.cfg.t5Timeout, true)
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
	c.stateMgr.ToNotConnected()

	// call closeListener() to ensure listener close
	if !c.cfg.isActive {
		_ = c.closeListener()
	}

	return nil
}

// closeConn performs the actual connection closing process with a timeout.
// It cancels the context, stops the task manager, closes the TCP connection, and waits for
// all goroutines to terminate.
func (c *Connection) closeConn(timeout time.Duration) {
	c.logger.Debug("start closeConn process")

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if c.ctxCancel != nil {
		c.logger.Debug("trigger context cancel function", "method", "closeConn")
		c.ctxCancel()
	}

	c.taskMgr.Stop()

	// close TCP connection
	c.connMutex.Lock()
	if c.conn != nil {
		c.logger.Debug("close TCP connection", "method", "closeConn")
		if tcpConn, ok := c.conn.(*net.TCPConn); ok {
			_ = tcpConn.SetLinger(0) // Set linger timeout to 0 to force close
		}
		if !c.cfg.isActive {
			c.connCount.Add(-1)
		}

		err := c.conn.Close()
		if err != nil {
			c.logger.Error("failed to close TCP connection", "method", "closeConn", "error", err)
		}
	}
	c.connMutex.Unlock()

	// drop all message that wait reply
	c.dropAllReplyMsgs()

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
	if msg.Type() != hsms.DataMsgType {
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
		if timeout == c.cfg.t3Timeout {
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
			if val, ok := c.replyErrs.LoadAndDelete(id); ok {
				if err, ok := val.(error); ok {
					return nil, err
				}
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
	if item, ok := c.replyMsgChans.LoadAndDelete(id); ok {
		if ch, ok := item.(chan hsms.HSMSMessage); ok {
			close(ch)
		}
	}
}

// dropAllReplyMsgs closes all reply channels in the replyMsgChans map, effectively dropping any pending replies.
func (c *Connection) dropAllReplyMsgs() {
	c.replyMsgChans.Range(func(key, value any) bool {
		c.replyMsgChans.Delete(key)
		return true
	})
}

// senderTask is the task function for the sender goroutine.
// It receives messages from the senderMsgChan and sends them synchronously over the connection.
func (c *Connection) senderTask(msg hsms.HSMSMessage) bool {
	err := c.sendMsgSync(msg)
	if err != nil {
		c.replyErrToSender(msg.ID(), err)

		opErr := &net.OpError{}
		if !errors.As(err, &opErr) {
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
	if _, err := io.ReadFull(reader, msgLenBuf); err != nil {
		if err != io.EOF && !errors.Is(err, net.ErrClosed) && !strings.Contains(err.Error(), "connection reset by peer") {
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
		c.logger.Error("failed to decode HSMS message")
	}

	if c.cfg.isActive {
		c.recvMsgActive(msg)
	} else {
		c.recvMsgPassive(msg)
	}

	return true
}

// replyToSender sends a received reply message to the corresponding reply channel in the replyMsgChans map.
func (c *Connection) replyToSender(msg hsms.HSMSMessage) {
	if val, ok := c.replyMsgChans.Load(msg.ID()); ok {
		if replyChan, ok := val.(chan hsms.HSMSMessage); ok {
			select {
			case <-c.ctx.Done(): // the connection context done, exit without block the process.
				return
			case replyChan <- msg:
				return
			}
		}

		return
	}

	// if reply channel not found, send to data message handler
	if dataMsg, ok := msg.ToDataMessage(); ok {
		c.session.recvDataMsg(dataMsg)
	}
}

// replyErrToSender sends an error to the corresponding reply channel in the replyMsgChans map, indicating
// that an error occurred while processing a message that expected a reply.
func (c *Connection) replyErrToSender(id uint32, err error) {
	if val, ok := c.replyMsgChans.Load(id); ok {
		if replyChan, ok := val.(chan hsms.HSMSMessage); ok {
			c.replyErrs.Store(id, err)
			select {
			case <-c.ctx.Done(): // the connection context done, exit without block the process.
				return
			case replyChan <- nil:
				return
			}
		}
	}
}

func (c *Connection) linktestConnStateHandler(_ hsms.Connection, _ hsms.ConnState, curState hsms.ConnState) {
	// HSMS-SS limits the use of linktest is only selected mode
	if curState.IsSelected() {
		c.linktestTicker = c.taskMgr.StartInterval("autoLinktestTask", c.autoLinktestTask, c.cfg.linktestInterval, false)
	} else if c.linktestTicker != nil {
		c.linktestTicker.Stop()
	}
}

func (c *Connection) autoLinktestTask() bool {
	msg := hsms.NewLinktestReq(hsms.GenerateMsgSystemBytes())

	_, err := c.sendMsg(msg)
	if errors.Is(err, hsms.ErrT6Timeout) {
		c.logger.Debug("linktest T6 timeout")
		c.stateMgr.ToNotConnectedAsync()
		return false
	}

	return true
}

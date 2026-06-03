package hsmsss

import (
	"bufio"
	"context"
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
//
// Connection State Architecture:
// This Connection relies on two parallel state tracking mechanisms by design:
//
//  1. opState (hsms.AtomicOpState): Tracks the physical lifecycle of the Go object
//     and underlying TCP socket (Closed -> Opening -> Opened -> Closing). It heavily
//     uses atomic Compare-And-Swap to ensure idempotent, lock-free teardowns
//     and to prevent race conditions during concurrent Open/Close calls.
//
//  2. stateMgr (*hsms.ConnStateMgr): Tracks the logical SEMI E37 protocol states
//     (NotConnected -> Connected -> Selected). It handles protocol-level transitions
//     and triggers user-facing or internal callbacks.
//
// Do not attempt to merge these. opState controls physical resources and goroutines,
// whereas stateMgr strictly controls standard protocol compliance.
//
// It manages the communication with a remote HSMS device, handling message exchange, connection state
// transitions, and various HSMS-specific functionalities.
type Connection struct {
	pctx context.Context

	// connCtxVal stores the current per-connection context.Context.
	// Written once per Open/reconnect cycle; read lock-free via connCtx().
	connCtxVal atomic.Pointer[ctxHolder]

	// connCancelFn stores the CancelFunc for the per-connection context.
	// Written once per Open/reconnect cycle; called in closeConn.
	connCancelFn atomic.Pointer[cancelHolder]

	// loopCtxVal stores the current background reconnect-loop context.Context.
	// Written once per Open cycle; read lock-free by startConnectLoop.
	loopCtxVal atomic.Pointer[ctxHolder]

	// loopCancelFn stores the CancelFunc for the background reconnect-loop context.
	// Written once per Open cycle; called in Close to stop the loop.
	loopCancelFn atomic.Pointer[cancelHolder]

	cfg    *ConnectionConfig
	logger logger.Logger

	listener      net.Listener       // listener is for passive mode only
	listenerMutex sync.Mutex         // mutex for listener
	opState       hsms.AtomicOpState // operation state
	connCount     atomic.Int32       // connection counter for passive mode only
	session       *Session           // HSMS-SS has only one session

	resMutex   sync.RWMutex        // mutex for connection resources
	resources  connectionResources // resources for the connection, including reader and writer
	writeMutex sync.Mutex          // mutex for writing to the connection

	stateMgr   *hsms.ConnStateMgr
	taskMgr    *hsms.TaskManager
	shutdown   atomic.Bool // indicates if has entered shutdown mode
	deselected atomic.Bool // indicates if disconnect was triggered by remote deselect

	// connectLoopRunning prevents multiple overlapping background reconnect loops.
	// It is checked and set atomically when starting the connect loop.
	connectLoopRunning atomic.Bool
	// connectLoopWg waits for the background loop to safely exit.
	connectLoopWg sync.WaitGroup

	// reconnectGen invalidates reconnect timers across Close/Open cycles.
	// Incremented on Close() so timers scheduled before Close() cannot fire after a later Open().
	reconnectGen atomic.Uint64
	tickers      tickerCtl // linktest control
	cfgUpdateMu  sync.Mutex

	// linktestFailCount tracks consecutive linktest T6 timeout failures.
	// It is compared against cfg.linktestFailThreshold to decide when to disconnect.
	// Reset to zero on successful linktest response or when entering Selected state.
	linktestFailCount atomic.Int32

	// senderMsgChan carries sendRequests to the senderTask. It is created once
	// in NewConnection and lives for the Connection's lifetime; queueSendRequest
	// is gated by sendClosed (see sendMu) so a closing connection cannot accept
	// new enqueues and a stranded frame cannot cross into the next connection.
	senderMsgChan chan *sendRequest

	// sendMu guards sendClosed. queueSendRequest holds it for reading across its
	// enqueue; closeConn takes it for writing to set sendClosed, which — because
	// the Lock waits for in-flight readers — makes the subsequent senderMsgChan
	// drain final. Open and renewConnCtx reset sendClosed for each new
	// connection generation.
	sendMu        sync.RWMutex
	sendClosed    bool
	replyMsgChans *xsync.MapOf[uint32, chan hsms.HSMSMessage]
	replyErrs     *xsync.MapOf[uint32, error]

	metrics ConnectionMetrics // connection metrics

	// closeErr holds the teardown result of the most recent closeConn cycle,
	// so the public Close method can surface a teardown timeout to the caller.
	// The closeConn call that owns the opState teardown resets this slot at the
	// start of the cycle and records the final result before the connection
	// reaches the closed state, so every Close() caller — including concurrent
	// ones — observes a consistent result. Holds *error; a stored nil error or
	// an absent pointer both mean "closed cleanly".
	closeErr atomic.Pointer[error]
}

// ctxHolder wraps a context.Context so that atomic.Pointer[ctxHolder]
// always stores a concrete pointer type, avoiding interface-type inconsistency panics.
type ctxHolder struct{ ctx context.Context }

// cancelHolder wraps a context.CancelFunc for the same reason.
type cancelHolder struct{ cancel context.CancelFunc }

type connectionResources struct {
	conn   net.Conn
	writer *bufio.Writer
}

// sendRequest wraps an HSMSMessage with an optional send-completion channel.
//
// When sentChan is non-nil, the sender task signals it with nil on success
// or with the error on failure. This is used by sendMsg to start the T3/T6
// timer only after the message is on the wire, per SEMI E37 §8.3.6.
type sendRequest struct {
	msg      hsms.HSMSMessage
	sentChan chan error // nil for fire-and-forget (sendMsgAsync / sendMsgSync)
}

// ensure Connection implements hsms.Connection interface.
var _ hsms.Connection = &Connection{}

// Test-only seams used by hsmsss/reply_close_race_test.go to deterministically
// reproduce the close-race interleavings between dropAllReplyMsgs and the
// receiverTask-driven reply routers. Production code leaves these nil; tests
// set and restore them within a single t.Run, never under concurrent tests.
var (
	replyToSenderPreSendHook     func()
	replyErrToSenderPreStoreHook func()
)

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
	// create sender message channel at beginning and not close it.
	conn.senderMsgChan = make(chan *sendRequest, cfg.senderQueueSize)

	conn.opState.Set(hsms.ClosedState)

	// connCtxVal starts as nil; connCtx() returns context.Background() for nil.
	// The real context is set in doOpen() before any goroutines are started.

	if cfg.IsActive() {
		conn.stateMgr = hsms.NewConnStateMgr(ctx, conn, conn.activeConnStateHandler)
	} else {
		conn.stateMgr = hsms.NewConnStateMgr(ctx, conn, conn.passiveConnStateHandler)
	}

	conn.stateMgr.AddHandler(conn.linktestConnStateHandler)

	return conn, nil
}

// UpdateConfigOptions updates the connection configuration options.
//
// Rollback guarantees no live config mutation; it does not guarantee zero
// external side effects produced by individual option apply funcs (e.g., DNS
// lookup in WithRemoteHost).
func (c *Connection) UpdateConfigOptions(opts ...ConnOption) error {
	c.cfgUpdateMu.Lock()
	defer c.cfgUpdateMu.Unlock()

	origAutoLinktest := c.cfg.AutoLinktest()
	origLinktestInterval := c.cfg.LinktestInterval()

	var errs error
	fnOpts := make([]*connOptFunc, 0, len(opts))
	for _, opt := range opts {
		fnOpt, ok := opt.(*connOptFunc)
		if !ok {
			errs = errors.Join(errs, errors.New("invalid ConnOption type"))
			continue
		}

		fnOpts = append(fnOpts, fnOpt)
	}

	scratch := c.cfg.snapshot()
	for _, fn := range fnOpts {
		before := scratch.nonRuntimeFields()
		beforeLogger := scratch.logger

		if err := fn.apply(scratch); err != nil {
			errs = errors.Join(errs, fmt.Errorf("option %q: %w", fn.name, err))
			continue
		}
		if !fn.runtime && (scratch.nonRuntimeFields() != before || !loggerSameInstance(scratch.logger, beforeLogger)) {
			errs = errors.Join(errs, fmt.Errorf("option %q cannot be changed at runtime", fn.name))
		}
	}

	if errs != nil {
		return errs
	}

	c.cfg.commitRuntime(scratch)

	curAutoLinktest := c.cfg.AutoLinktest()
	curLinktestInterval := c.cfg.LinktestInterval()

	if curAutoLinktest != origAutoLinktest {
		if curAutoLinktest {
			c.tickers.resetLinktestTicker(curLinktestInterval)
		} else {
			c.tickers.stopLinktestTicker()
		}
	} else if curAutoLinktest && curLinktestInterval != origLinktestInterval {
		c.tickers.resetLinktestTicker(curLinktestInterval)
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

// Open establishes the HSMS-SS connection.
// If waitOpened is true, it blocks until the connection is fully established (Selected state)
// or an error occurs.
// If waitOpened is false, it initiates the connection process and returns immediately.
//
// Note on active connections: While SEMI E37 §9.2.1.1 describes a T5 separation
// between active connect attempts, a strict T5 delay (often 10s) can be too long
// for fast recovery from transient network drops. To optimize recovery while still
// avoiding network flooding, the active connect loop uses an exponential backoff.
// Reconnects start with an initial delay (default 100ms) and double until reaching
// the configured T5 timeout.
func (c *Connection) Open(waitOpened bool) error {
	// reset shutdown flag to false when user call Open() again
	c.shutdown.Store(false)
	c.deselected.Store(false)

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

	// Cancel any previously-active per-connection context before creating new ones.
	// This also signals a dying background connect loop to exit, then we wait for it.
	if h := c.connCancelFn.Load(); h != nil {
		h.cancel()
	}
	if h := c.loopCancelFn.Load(); h != nil {
		h.cancel()
	}

	// Wait for any dying background reconnect loop to exit safely before creating
	// a fresh context, so we are never racing with a dying loop over connCtxVal.
	c.connectLoopWg.Wait()

	// Create fresh per-connection and loop contexts for this lifecycle.
	connCtx, connCancel := context.WithCancel(c.pctx)
	loopCtx, loopCancel := context.WithCancel(c.pctx)
	c.connCtxVal.Store(&ctxHolder{ctx: connCtx})
	c.connCancelFn.Store(&cancelHolder{cancel: connCancel})
	c.loopCtxVal.Store(&ctxHolder{ctx: loopCtx})
	c.loopCancelFn.Store(&cancelHolder{cancel: loopCancel})

	// Re-open the send gate for this new connection generation; the previous
	// closeConn closed it during teardown.
	c.sendMu.Lock()
	c.sendClosed = false
	c.sendMu.Unlock()

	if c.cfg.IsActive() {
		c.openActive(connCtx, loopCtx)

		if waitOpened {
			return c.stateMgr.WaitState(connCtx, hsms.SelectedState)
		}
	} else { // passive mode
		err := c.openPassive(connCtx)
		if err != nil {
			return err
		}
	}

	return nil
}

// Close closes the HSMS-SS connection gracefully.
// It terminates all running tasks, closes the TCP connection, and resets the connection state.
//
// Returns:
//   - nil: the connection closed cleanly within CloseConnTimeout.
//   - non-nil wrapping a deadline/timeout error: the connection reached the closed state, but
//     task teardown exceeded CloseConnTimeout; the caller may treat this as a warning.
//   - non-nil otherwise: the connection did not reach the closed state before CloseConnTimeout elapsed.
func (c *Connection) Close() error {
	// Invalidate any pending reconnect timers from a previous lifecycle.
	c.reconnectGen.Add(1)

	// ensure any background connect loop is cancelled
	if h := c.loopCancelFn.Load(); h != nil {
		h.cancel()
	}

	c.shutdown.Store(true)
	c.logger.Debug("start to close connection", "method", "Close", "opState", c.opState.String())

	// Force the transition to NotConnected BEFORE stopping the state manager.
	// Running ToNotConnected() here — while the async handler dispatcher is
	// still alive — ensures user-registered ConnStateChangeHandlers observe
	// the final transition. If we stopped first, the dispatcher would have
	// already exited and the event would be dropped.
	if !c.isClosed() {
		c.logger.Debug("force to set connection desired state to not-connected", "method", "Close",
			"curDesiredState", c.stateMgr.DesiredState().String(),
			"curState", c.stateMgr.State().String(),
		)
		c.stateMgr.ToNotConnected()
	}

	// Stop the state manager. wg.Wait inside Stop() blocks until the async
	// dispatcher has drained any event published by the ToNotConnected call
	// above, so user handlers run before Stop() returns.
	c.stateMgr.Stop()

	closeTimer := pool.GetTimer(c.cfg.CloseConnTimeout())
	defer pool.PutTimer(closeTimer)

	// Use a ticker instead of busy loop with sleep for better CPU efficiency
	checkTicker := time.NewTicker(closeCheckInterval)
	defer checkTicker.Stop()

	// wait for connection to be closed
	for {
		select {
		case <-closeTimer.C:
			if c.isClosed() {
				c.logger.Debug("wait for connection closed success at timeout", "timeout", c.cfg.CloseConnTimeout(), "opState", c.opState.String())
				return c.loadCloseErr()
			}

			c.logger.Error("close connection timeout",
				"timeout", c.cfg.CloseConnTimeout(),
				"opState", c.opState.String(),
				"curState", c.stateMgr.State().String(),
				"desiredState", c.stateMgr.DesiredState().String(),
			)

			return errors.New("close connection timeout")
		case <-checkTicker.C:
			if c.isClosed() {
				c.logger.Debug("wait for connection closed success")
				return c.loadCloseErr()
			}
		}
	}
}

func (c *Connection) isClosed() bool {
	return c.opState.IsClosed() && c.stateMgr.State() == hsms.NotConnectedState && c.stateMgr.DesiredState() == hsms.NotConnectedState
}

// loadCloseErr returns the error recorded by the most recent closeConn call,
// or nil if the connection closed cleanly.
func (c *Connection) loadCloseErr() error {
	if h := c.closeErr.Load(); h != nil {
		return *h
	}

	return nil
}

// drainSenderMsgChan drains pending sendRequests from the channel.
// Each drained request with a sentChan is signaled with ErrConnClosed so
// that any goroutine blocked in sendMsg wakes up immediately.
func (c *Connection) drainSenderMsgChan(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case req, ok := <-c.senderMsgChan:
			if !ok {
				return
			}

			if req != nil {
				if req.sentChan != nil {
					req.sentChan <- hsms.ErrConnClosed
				}
				if req.msg != nil {
					req.msg.Free()
				}
			}
		default:
			return
		}
	}
}

func (c *Connection) setupResources(conn net.Conn) {
	// Enable TCP Keep-Alive for half-open detection.
	// This is essential for passive mode (accepted connections don't go through
	// net.Dialer) and ensures both modes are covered uniformly.
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		period := c.cfg.KeepAlivePeriod()
		if period > 0 {
			_ = tcpConn.SetKeepAlive(true)
			_ = tcpConn.SetKeepAlivePeriod(period)
		} else {
			_ = tcpConn.SetKeepAlive(false)
		}
	}

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
// The conn reference is nil'd under the write lock so that subsequent calls are no-ops.
func (c *Connection) closeTCP(timeout time.Duration) string {
	c.resMutex.Lock()
	conn := c.resources.conn
	if conn == nil {
		c.resMutex.Unlock()

		return ""
	}

	// Nil the references under the write lock so subsequent calls are no-ops.
	c.resources.conn = nil
	c.resources.writer = nil
	c.resMutex.Unlock()

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

	if !c.cfg.IsActive() {
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

	// We own this teardown cycle (the ToClosing CAS above admitted exactly one
	// caller). Reset the recorded error so a previous cycle's or a reconnect's
	// result cannot leak into this Close. No Close() caller resets this slot,
	// which makes the teardown-error contract safe under concurrent Close().
	c.closeErr.Store(nil)

	c.logger.Debug("start to close connection", "method", "closeConn", "opState", c.opState.String())

	// 2. create a new context for waiting all tasks to be terminated
	closeCtx, closeCtxCancel := context.WithTimeout(context.Background(), timeout)
	defer closeCtxCancel()

	// 3. drain sender message channel
	c.drainSenderMsgChan(closeCtx)

	// 4. cancel per-connection context to unblock any goroutines waiting on it
	if h := c.connCancelFn.Load(); h != nil {
		c.logger.Debug("trigger context cancel function", "method", "closeConn")
		h.cancel()
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

	// 9. Close the send gate, then drain senderMsgChan a final time.
	//
	// Setting sendClosed under c.sendMu's write lock blocks every future
	// enqueue: a queueSendRequest already past its sendClosed check holds the
	// read lock, so this Lock waits for it to finish its send; any later
	// queueSendRequest observes sendClosed and returns ErrConnClosed without
	// enqueuing. Once the gate is closed, no goroutine — a taskMgr-joined task
	// or an external sendMsg caller alike — can put a frame into senderMsgChan,
	// so the drain that follows is definitive.
	//
	// This is unconditional: it runs on both the normal and the close-timeout
	// path. The gate is what makes the drain safe on the timeout path, where
	// taskMgr tasks may still be alive — they too are now blocked from
	// enqueuing, and draining only races consumers (harmless: a channel may
	// have many receivers). It runs before the opState transition to Closed
	// (step 11), so the connection cannot have been reopened, and senderMsgChan
	// persists for the Connection's lifetime, so without this drain a frame
	// stranded by a queueSendRequest racing teardown would be flushed onto the
	// next connection with stale system bytes and session ID.
	//
	// context.Background() is required, not closeCtx: drainSenderMsgChan selects
	// on ctx.Done(), and the already-cancelled closeCtx would let it return
	// before the channel is empty.
	c.sendMu.Lock()
	c.sendClosed = true
	c.sendMu.Unlock()
	c.drainSenderMsgChan(context.Background())

	// 10. cleanup reply channels and queue
	c.dropAllReplyMsgs()

	// Record the teardown result BEFORE the opState transition to Closed.
	// A concurrent Close() spins on isClosed() (which requires opState Closed);
	// storing closeErr first guarantees such a caller observes this result.
	recordedErr := closeErr
	c.closeErr.Store(&recordedErr)

	// 11. final transition to closed state
	if !c.opState.ToClosed() {
		c.logger.Warn("failed to set connection to closed state", "method", "closeConn", "opState", c.opState.String())
		return fmt.Errorf("failed to set connection to closed state, current state: %s", c.opState.String())
	}

	c.logger.Debug("connection closed", "method", "closeConn", "remoteAddr", remoteAddr)

	return closeErr
}

// connCtx returns the current per-connection context via a lock-free atomic load.
// Returns context.Background() before the first Open() call.
func (c *Connection) connCtx() context.Context {
	if h := c.connCtxVal.Load(); h != nil {
		return h.ctx
	}

	return context.Background()
}

// loadLoopCtx returns the current background reconnect-loop context via a lock-free atomic load.
func (c *Connection) loadLoopCtx() context.Context {
	if h := c.loopCtxVal.Load(); h != nil {
		return h.ctx
	}

	return context.Background()
}

// renewConnCtx cancels the current per-connection context and stores a fresh one.
// Called by the background reconnect loop after each TCP close, which must not
// cancel the loop's own loopCtx.
func (c *Connection) renewConnCtx() {
	if h := c.connCancelFn.Load(); h != nil {
		h.cancel()
	}

	connCtx, connCancel := context.WithCancel(c.pctx)
	c.connCtxVal.Store(&ctxHolder{ctx: connCtx})
	c.connCancelFn.Store(&cancelHolder{cancel: connCancel})

	// Re-open the send gate for the new connection generation; the preceding
	// closeConn closed it during teardown.
	c.sendMu.Lock()
	c.sendClosed = false
	c.sendMu.Unlock()
}

// sendControlMsg sends an HSMS control message and waits for a reply.
// It returns the received control message and an error if any occurred.
func (c *Connection) sendControlMsg(msg *hsms.ControlMessage) (*hsms.ControlMessage, error) {
	replyMsg, err := c.sendMsg(msg)
	if err != nil || replyMsg == nil {
		return nil, err
	}

	ctrlMsg, ok := replyMsg.ToControlMessage()
	if !ok {
		return nil, hsms.ErrNotControlMsg
	}

	return ctrlMsg, nil
}

// dropNotSelected records a not-Selected data-message drop and returns the
// ErrNotSelectedState sentinel. It is the single place that increments
// DataMsgDropNotSelectedCount: every send gate that rejects a data message
// because the link is not Selected returns through here, so the count can
// neither be forgotten nor double-applied. Logging and msg.Free() stay at the
// call site because they differ by gate — log level varies, and the entry gates
// own the Free while the write-boundary gate (driven by processSendRequest /
// Session.SendMessageSync) does not.
//
// Exactly-once holds structurally: the entry gates (sendMsg, sendMsgAsync) are
// terminal and return before queueing, so a single send call can reach at most
// one of them or the write-boundary gate in sendMsgSync — never two.
func (c *Connection) dropNotSelected() error {
	c.metrics.incDataMsgDropNotSelectedCount()

	return hsms.ErrNotSelectedState
}

// sendMsg sends an HSMS message (data or control) and waits for a reply if the message's W-bit is set.
// It returns the received reply message and an error if any occurred.
//
// For messages that expect a reply (W-bit set), the method uses a two-phase approach:
//   - Phase 1: Queue the message and wait for the senderTask to confirm it is on the wire.
//   - Phase 2: Start the T3/T6 timer and wait for the reply.
//
// This ensures the reply timer does not include queuing and TCP write latency,
// consistent with SEMI E37 §8.3.6.
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
		msg.Free()

		return nil, c.dropNotSelected()
	}

	if !msg.WaitBit() {
		if err := c.sendMsgAsync(msg); err != nil {
			return nil, err
		}

		return nil, nil //nolint:nilnil
	}

	// --- W-bit set: register reply channel and use 2-phase send ---

	id := msg.ID()
	isDataMsg := msg.IsDataMessage()
	// Capture primitives for slow-path logging before msg ownership transfers
	// to processSendRequest (which may Free it). Building the structured log
	// slice up front would allocate on every successful send; we defer the
	// alloc into the timeout / error branches below.
	msgType := msg.Type()
	stream := msg.StreamCode()
	function := msg.FunctionCode()
	replyMsgChan := c.addReplyExpectedMsg(id)

	// Terminal-branch orphan defense (post-impl review item 1 of 2): any of
	// the terminal branches below (Phase 1 ctx.Done / sendErr, Phase 2
	// ctx.Done / T3-T6 timer) returns without consuming replyMsgChan. If a
	// replyToSender or replyErrToSender goroutine successfully delivered into
	// the cap-1 buffer after its post-send Load saw the channel as still
	// registered but before this defer runs, the message (or nil err
	// sentinel) would be stranded in an orphan buffer. Drain non-blockingly
	// to Free the orphan and, for a nil sentinel, Delete the matching
	// replyErrs entry — see drainOrphanReply for the full rationale. No-op
	// on the reply success case because that branch consumes the buffer
	// first.
	defer c.drainOrphanReply(id, replyMsgChan)

	sentChan := make(chan error, 1)
	req := &sendRequest{msg: msg, sentChan: sentChan}

	// After queueing, msg ownership transfers to processSendRequest which
	// calls msg.Free(). Do not access msg beyond this point; use the
	// pre-captured primitives (isDataMsg / msgType / id / stream / function)
	// instead.
	if err := c.queueSendRequest(req); err != nil {
		c.removeReplyExpectedMsg(id)
		msg.Free()

		return nil, err
	}

	// Phase 1: Wait for the senderTask to confirm the message is on the wire.
	select {
	case <-c.connCtx().Done():
		c.removeReplyExpectedMsg(id)

		return nil, hsms.ErrConnClosed

	case sendErr := <-sentChan:
		if sendErr != nil {
			c.removeReplyExpectedMsg(id)

			return nil, sendErr
		}
	}

	// Phase 2: Message is on the wire — start T3 or T6 timer.
	timeout := c.cfg.T3Timeout()
	if !isDataMsg {
		timeout = c.cfg.T6Timeout()
	}

	sendMsgTimer := pool.GetTimer(timeout)
	defer pool.PutTimer(sendMsgTimer)

	c.metrics.incDataMsgInflightCount()

	select {
	case <-c.connCtx().Done():
		c.removeReplyExpectedMsg(id)
		c.metrics.decDataMsgInflightCount()

		return nil, hsms.ErrConnClosed

	case <-sendMsgTimer.C:
		c.removeReplyExpectedMsg(id)
		c.metrics.decDataMsgInflightCount()

		c.logger.Warn("send message timeout",
			hsms.MsgInfoFromFields(msgType, id, stream, function, "method", "sendMsg", "timeout", timeout)...,
		)
		if isDataMsg {
			// If entity is equipment, send SECS-II S9F9 when t3/t6 timeout.
			if c.cfg.IsEquip() {
				_, _ = c.session.SendSECS2Message(gem.S9F9())
			}

			return nil, hsms.ErrT3Timeout
		}

		return nil, hsms.ErrT6Timeout

	// wait reply message from receiverTask
	case replyMsg := <-replyMsgChan:
		c.metrics.decDataMsgInflightCount()

		if replyMsg == nil {
			// The transaction is terminating (peer Reject.req or send
			// failure); drop the reply channel so it does not leak while the
			// connection stays up — every other terminal path of sendMsg
			// already does this.
			c.removeReplyExpectedMsg(id)

			// check if error existed
			if err, ok := c.replyErrs.LoadAndDelete(id); ok {
				return nil, err
			}

			return nil, hsms.ErrConnClosed
		}

		c.removeReplyExpectedMsg(id)

		if ctrlMsg, ok := replyMsg.ToControlMessage(); ok {
			if ctrlMsg.Type() == hsms.RejectReqType {
				return nil, rejectReasonErr(int(ctrlMsg.Header()[3]))
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
	if msg.Type() == hsms.DataMsgType && !c.stateMgr.IsSelected() {
		// sendMsgSync is the write-boundary gate: it serves both the sender-task
		// dequeue path (processSendRequest, where a queued message can find the
		// link left Selected between enqueue and dequeue) and direct
		// Session.SendMessageSync callers. dropNotSelected counts the drop once
		// for both. msg.Free() is owned by the caller here (processSendRequest's
		// defer, or Session.SendMessageSync's defer), so this gate does not Free.
		c.logger.Error("failed to send hsms data message, not selected state",
			hsms.MsgInfo(msg, "method", "sendMsgSync", "state", c.stateMgr.State().String())...,
		)

		return c.dropNotSelected()
	}

	buf := msg.ToBytes()

	if c.cfg.TraceTraffic() {
		c.logger.Info("trace: send message", hsms.MsgInfo(msg, "raw", hsms.MsgHexString(buf))...)
	}

	// lock the write mutex to ensure thread-safe writing to the connection
	c.writeMutex.Lock()
	defer c.writeMutex.Unlock()

	return c.withResources(func(conn net.Conn, writer *bufio.Writer) error {
		if conn == nil || writer == nil {
			return hsms.ErrConnClosed
		}

		err := conn.SetWriteDeadline(time.Now().Add(c.cfg.SendTimeout()))
		if err != nil {
			return err
		}

		isDebugLevel := c.logger.Level() == logger.DebugLevel
		if isDebugLevel {
			c.logger.Debug("try to send message to remote", hsms.MsgInfo(msg, "method", "sendMsgSync", "timeout", c.cfg.SendTimeout())...)
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
// This is the fire-and-forget path: the caller does not wait for send completion.
//
// Ownership of msg transfers to the sender loop; processSendRequest will call msg.Free().
func (c *Connection) sendMsgAsync(msg hsms.HSMSMessage) error {
	// Mirror sendMsg (data-message guard above) and sendMsgSync: a data message
	// cannot be sent while not Selected. Gating here — rather than only at
	// dequeue in the sender loop — keeps senderMsgChan from filling with
	// undeliverable data messages while NotSelected, which would both choke the
	// sender task and starve the Select.req enqueue, producing a
	// NotSelected→NotConnected reconnect loop. Control messages (Select.req,
	// Linktest, …) are intentionally NOT gated: the Select handshake depends on
	// them being sendable while NotSelected.
	if msg.Type() == hsms.DataMsgType && !c.stateMgr.IsSelected() {
		msg.Free()

		return c.dropNotSelected()
	}

	if err := c.queueSendRequest(&sendRequest{msg: msg}); err != nil {
		msg.Free()

		return err
	}

	return nil
}

// queueSendRequest puts a sendRequest onto the senderTask's channel.
//
// It holds c.sendMu for reading across the enqueue. Once closeConn has taken
// c.sendMu for writing and set sendClosed, no queueSendRequest can enqueue: a
// caller already past the sendClosed check is holding the read lock, so
// closeConn's Lock waits for it to finish its send; a caller that arrives later
// observes sendClosed and returns ErrConnClosed without enqueuing. That ordering
// makes closeConn's final senderMsgChan drain definitive — no frame, from a
// taskMgr task or an external sendMsg caller alike, can be stranded past
// teardown.
//
// Holding the read lock across the select is safe: by the time closeConn wants
// the write lock, connCtx has been cancelled (closeConn step 4, before the
// gate-close), so any in-flight select exits promptly via its connCtx.Done()
// case. closeConn's Lock waits only that brief moment for in-flight senders to
// return — never the full SendTimeout.
func (c *Connection) queueSendRequest(req *sendRequest) error {
	c.sendMu.RLock()
	defer c.sendMu.RUnlock()

	if c.sendClosed {
		return hsms.ErrConnClosed
	}

	// Fast path: the sender channel is buffered to cfg.senderQueueSize; under
	// normal load it has room and we enqueue without touching the timer pool.
	// Safety: sendMu.RLock + the sendClosed check above hold the same teardown
	// invariants as the slow-path select; drainSenderMsgChan will pick up any
	// request stranded by a closeConn racing this enqueue.
	select {
	case c.senderMsgChan <- req:
		return nil
	default:
	}

	// Slow path: buffer full, fall back to a timer-bound select so we can
	// observe SendTimeout and connection cancellation while waiting for room.
	timer := pool.GetTimer(c.cfg.SendTimeout())
	defer pool.PutTimer(timer)

	select {
	case <-c.connCtx().Done():
		return hsms.ErrConnClosed
	case <-timer.C:
		return hsms.ErrSendMsgTimeout
	case c.senderMsgChan <- req:
		return nil
	}
}

// addReplyExpectedMsg adds a reply channel to the replyMsgChans map for a message that expects a reply.
// The channel is buffered with capacity 1 so that replyToSender never blocks
// waiting for the consumer, which prevents reply drops under high contention.
func (c *Connection) addReplyExpectedMsg(id uint32) <-chan hsms.HSMSMessage {
	ch := make(chan hsms.HSMSMessage, 1)
	c.replyMsgChans.Store(id, ch)

	return ch
}

// removeReplyExpectedMsg removes the reply channel from the replyMsgChans map for the given message ID.
func (c *Connection) removeReplyExpectedMsg(id uint32) {
	// find and delete the reply channel
	c.replyMsgChans.Delete(id)
}

// drainOrphanReply is sendMsg's defer-time cleanup. It non-blockingly
// receives any value left in replyMsgChan after a sendMsg terminal branch
// (Phase 1 ctx.Done / sendErr, Phase 2 ctx.Done / T3-T6 timer) fired
// without consuming the reply, and ensures the pool / replyErrs stay
// balanced:
//
//   - A non-nil DataMessage was delivered by replyToSender — Free it so
//     the pool sees the return.
//   - A nil sentinel was delivered by replyErrToSender (or by
//     dropAllReplyMsgs) — Delete the matching replyErrs entry, if any.
//     replyErrToSender's own post-send identity check only fires when the
//     map already lost the channel; the path where the identity check
//     observes the channel as still registered (because removal happens in
//     OUR terminal branch AFTER the check) lands here and would otherwise
//     strand replyErrs[id]. dropAllReplyMsgs separately Clears replyErrs,
//     so a nil from that origin makes the Delete a no-op — safe either way.
//
// The reply success case in sendMsg consumes the buffer before this defer
// runs, so the function is a no-op on that path.
func (c *Connection) drainOrphanReply(id uint32, replyMsgChan <-chan hsms.HSMSMessage) {
	select {
	case orphan := <-replyMsgChan:
		if orphan != nil {
			orphan.Free()
		} else {
			// Stranded replyErrs[id] from a Store-then-send by
			// replyErrToSender whose post-send identity check saw the
			// channel as still registered. Delete on a non-existent key
			// is a no-op, so this is safe when the nil came from any
			// other source.
			c.replyErrs.Delete(id)
		}
	default:
	}
}

// dropAllReplyMsgs drains every reply channel in replyMsgChans, delivers an
// explicit nil sentinel (which sendMsg treats identically to a closed-channel
// receive — see the replyMsg == nil branch at conn.go:840-852), then clears
// the map alongside replyErrs.
//
// We deliberately do NOT close(ch): receiverTask-driven replyToSender and
// replyErrToSender may hold a previously-loaded replyChan and attempt a send
// after this function runs; close(ch) would turn that send into a panic. The
// drain-then-nil-send pattern preserves the wake-up semantic for sendMsg
// without ever closing the channel, and the orphan-defense guards in
// replyToSender / replyErrToSender handle late sends that race past the Clear.
//
// Drain-then-nil ordering matters: drain first frees any buffered reply that
// would otherwise leak into the soon-orphaned channel buffer; the
// non-blocking nil-send then delivers the termination signal. Nil-first
// would land in an empty channel and be eaten by our own drain.
func (c *Connection) dropAllReplyMsgs() {
	c.replyMsgChans.Range(func(id uint32, ch chan hsms.HSMSMessage) bool {
		if ch != nil {
			// Drain any buffered reply so its DataMessage returns to the
			// pool (cap is 1; one read is sufficient).
			select {
			case msg := <-ch:
				if msg != nil {
					msg.Free()
				}
			default:
			}
			// Deliver the termination signal. Non-blocking: a sendMsg that
			// already exited via connCtx.Done() never receives, and the
			// leftover nil sits in the soon-orphaned buffer harmlessly.
			select {
			case ch <- nil:
			default:
			}
		}

		return true
	})

	c.replyMsgChans.Clear()

	// replyErrs is transaction-scoped; clear it alongside replyMsgChans so an
	// error stranded by a connCtx-done race in replyErrToSender cannot outlive
	// the connection. The orphan-defense in replyErrToSender's ctx.Done branch
	// covers the post-Clear interleaving where Store lands after Clear.
	c.replyErrs.Clear()
}

func (c *Connection) cancelSenderTask() {
	c.stateMgr.ToNotConnectedAsync()
}

// senderLoop reads sendRequests from the channel and sends them synchronously.
// It returns false to stop the task on fatal errors.
func (c *Connection) senderLoop() bool {
	select {
	case <-c.connCtx().Done():
		return false

	case req, ok := <-c.senderMsgChan:
		if !ok {
			return false
		}

		if req == nil || req.msg == nil {
			c.logger.Warn("received nil sendRequest in senderLoop, ignore")

			return true
		}

		if !c.processSendRequest(req) {
			c.cancelSenderTask()

			return false
		}

		return true
	}
}

// processSendRequest handles a single sendRequest: writes the message to TCP
// and signals the optional sentChan with the result.
func (c *Connection) processSendRequest(req *sendRequest) bool {
	msg := req.msg
	defer msg.Free()

	c.metrics.incDataMsgSendCount()

	err := c.sendMsgSync(msg)
	if err != nil {
		// Signal the caller that send failed.
		if req.sentChan != nil {
			req.sentChan <- err
		} else {
			c.replyErrToSender(msg, err)
		}

		// Defense-in-depth: a queued data message that becomes un-sendable
		// because the link left Selected between enqueue and dequeue (the
		// residual check-then-enqueue race the sendMsgAsync gate cannot fully
		// close) yields ErrNotSelectedState here. That is expected backpressure,
		// not a fault — count it as a drop, keep the sender task alive, and do
		// NOT escalate to teardown (return false → cancelSenderTask →
		// NotConnected), which is what produced the NotSelected→NotConnected
		// reconnect loop. All other errors (real network/connection failures)
		// remain fatal below and are counted as data-message errors.
		//
		// The drop is counted at the write boundary in sendMsgSync (the single
		// chokepoint), not here, to avoid double-counting and to cover direct
		// Session.SendMessageSync callers uniformly.
		if errors.Is(err, hsms.ErrNotSelectedState) {
			c.logger.Debug("dropping queued message: not selected",
				hsms.MsgInfo(msg, "method", "senderTask")...,
			)

			return true
		}

		c.metrics.incDataMsgErrCount()

		if !isNetOpError(err) {
			c.logger.Error("failed to send message", "method", "senderTask", "error", err)
		} else {
			c.logger.Error("network error sending message", "method", "senderTask", "error", err)
		}

		return false
	}

	// Signal the caller that send succeeded.
	if req.sentChan != nil {
		req.sentChan <- nil
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
// It uses messageReader to read and decode HSMS messages from the connection,
// then dispatches them to the appropriate handler.
//
// It respects the T8 timeout for reading messages and handles message length validation.
func (c *Connection) receiverTask(msgLenBuf []byte) bool {
	conn := c.getConn()
	if conn == nil {
		return false // stop the receiver task if connection is closed
	}

	reader := &messageReader{t8Timeout: c.cfg.T8Timeout(), idleReadTimeout: c.cfg.IdleReadTimeout()}

	msg, rawBody, err := reader.ReadMessage(conn, msgLenBuf)
	if err != nil {
		var hdrReject *headerRejectError
		if errors.As(err, &hdrReject) {
			// SEMI E37 §7.10.3 requires a Reject.req for an unsupported
			// PType/SType. Send it and keep the connection (the spec is
			// silent on lifecycle; see Task 4 notes).
			rejectMsg := hsms.NewRejectReqRaw(
				hdrReject.sessionID, hdrReject.pType, hdrReject.sType,
				hdrReject.systemBytes[:], hdrReject.reasonCode,
			)
			c.logger.Warn("received unsupported HSMS header, sending reject.req",
				"method", "receiverTask",
				"pType", hdrReject.pType, "sType", hdrReject.sType, "reason", hdrReject.reasonCode,
			)
			c.metrics.incDataMsgErrCount()
			if _, sendErr := c.sendMsg(rejectMsg); sendErr != nil {
				c.logger.Error("failed to send reject.req", "method", "receiverTask", "error", sendErr)
				return false
			}

			return true
		}

		// Exactly one log per error, at the appropriate level.
		// rawBody non-nil indicates a decode failure (payload was read successfully
		// but could not be decoded). Note: msg is nil on decode failure, so we must
		// NOT pass it to hsms.MsgInfo which would panic on nil.
		switch {
		case rawBody != nil && c.cfg.TraceTraffic():
			c.logger.Error("failed to decode HSMS message",
				"method", "receiverTask",
				"error", err,
				"raw", hsms.MsgHexString(msgLenBuf, rawBody),
			)
		case rawBody != nil:
			c.logger.Error("failed to decode HSMS message", "method", "receiverTask", "error", err)
		case !isNetError(err):
			c.logger.Error("failed to read HSMS message", "method", "receiverTask", "error", err)
		default:
			c.logger.Error("network error reading HSMS message", "method", "receiverTask", "error", err)
		}

		c.metrics.incDataMsgErrCount()

		return false
	}

	if c.cfg.TraceTraffic() {
		c.logger.Info("trace: received message", hsms.MsgInfo(msg, "raw", hsms.MsgHexString(msgLenBuf, rawBody))...)
	}

	c.metrics.incDataMsgRecvCount()

	if c.cfg.IsActive() {
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
				msg.Free()
			}
		}

		return
	}

	// Slow-path-only: the 3-case select preserves ctx-done responsiveness
	// during teardown. A non-blocking fast-path send before this select would
	// still need to handle the post-Clear orphan case below — replyToSender
	// already re-checks the map after a successful send to free orphan
	// messages, but a fast path doubles the surfaces that need that guard.
	// If a future perf pass justifies the fast path (Tier A4 in
	// tmp/hsmsss-hotpath-perf-analysis-revised.md), extend the same orphan
	// check there. For now, the slow-path-only shape is simpler and the
	// per-reply timer-pool cost is small.
	timer := pool.GetTimer(replyChannelTimeout)
	defer pool.PutTimer(timer)

	// Test seam: when set, fires after the Load above and before the send
	// select below. Used by reply_close_race_test.go to deterministically
	// reproduce the post-Clear orphan interleaving. nil in production.
	if hook := replyToSenderPreSendHook; hook != nil {
		hook()
	}

	select {
	case <-c.connCtx().Done(): // the connection context done, drop the message and exit
		c.replyMsgChans.Delete(msg.ID())
		msg.Free()

		return

	case <-timer.C: // reply channel send timeout, drop the message and exit
		c.logger.Warn("reply channel send timeout, drop the message", "msgID", msg.ID())
		c.replyMsgChans.Delete(msg.ID())
		msg.Free()

		return

	case replyChan <- msg:
		// Defend against a successful send into an orphaned channel: if
		// dropAllReplyMsgs cleared the map between our Load (above) and our
		// send, the waiting sendMsg has already exited (or never registered)
		// and our msg is stuck in the cap-1 buffer where nothing else will
		// Free it. Re-check the map; if the channel is no longer the
		// registered one for this id, drain our send and Free the orphan.
		if current, stillRegistered := c.replyMsgChans.Load(msg.ID()); !stillRegistered || current != replyChan {
			select {
			case stale := <-replyChan:
				if stale != nil {
					stale.Free()
				}
			default:
			}
		}
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
		// Test seam: when set, fires between the Load above and the Store
		// below. Used by reply_close_race_test.go to deterministically
		// reproduce the post-Clear orphan-replyErrs interleaving. nil in
		// production.
		if hook := replyErrToSenderPreStoreHook; hook != nil {
			hook()
		}

		c.replyErrs.Store(id, err)

		select {
		case <-c.connCtx().Done(): // the connection context done, exit without block the process.
			// If dropAllReplyMsgs.Clear() ran between our Load and our
			// Store, the err we just stored is stranded in replyErrs. Drop
			// it so the clean-shutdown invariant (replyErrs.Size() == 0
			// after close) holds. Delete on a non-existent key is a no-op.
			c.replyErrs.Delete(id)

			return
		case replyChan <- nil:
			// Post-impl review item 2 of 2: if dropAllReplyMsgs.Clear()
			// ran between our Load (above) and the Store just before this
			// select, the replyErrs entry we just stored is stranded. The
			// send-branch winning means sendMsg either already consumed
			// drop's nil and exited (and our nil now lands in an orphan
			// buffer that nothing will read), or the post-Clear/post-drain
			// window left the buffer empty so we landed in an orphan. In
			// both cases the err is stranded; re-check the map identity
			// to detect the race and Delete our entry so the clean-shutdown
			// invariant (replyErrs.Size() == 0 after close) holds.
			if current, stillRegistered := c.replyMsgChans.Load(id); !stillRegistered || current != replyChan {
				c.replyErrs.Delete(id)
			}

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
		errMsg.Free()
	}
}

func (c *Connection) linktestConnStateHandler(_ hsms.Connection, _ hsms.ConnState, curState hsms.ConnState) {
	// HSMS-SS limits the use of linktest is only selected mode
	if curState.IsSelected() {
		// reset consecutive failure counter on fresh connection
		c.linktestFailCount.Store(0)

		ticker, err := c.taskMgr.StartInterval("autoLinktestTask", c.autoLinktestTask, c.cfg.LinktestInterval(), false)
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

		failCount := int(c.linktestFailCount.Add(1))
		threshold := c.cfg.LinktestFailThreshold()

		if failCount >= threshold {
			c.logger.Error("linktest T6 timeout, threshold reached", "failCount", failCount, "threshold", threshold)
			c.linktestFailCount.Store(0)
			c.stateMgr.ToNotConnectedAsync()

			return false
		}

		c.logger.Warn("linktest T6 timeout, tolerating failure", "failCount", failCount, "threshold", threshold)

		return true
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

	// successful linktest response, reset consecutive failure counter
	c.linktestFailCount.Store(0)
	c.metrics.incLinktestRecvCount()

	return true
}

func (c *Connection) validateMsg(msg hsms.HSMSMessage) error {
	if !c.cfg.ValidateDataMessage() {
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

// tickerCtl is a helper struct for managing interval tasks interval.
type tickerCtl struct {
	mu             sync.RWMutex
	linktestTicker *time.Ticker
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

func isNetOpError(err error) bool {
	opErr := &net.OpError{}
	return errors.As(err, &opErr)
}

func isConnTimedOutError(err error) bool {
	if err == nil {
		return false
	}

	var netErr net.Error

	return errors.As(err, &netErr) && netErr.Timeout()
}

func isConnClosedError(err error) bool {
	return errors.Is(err, net.ErrClosed)
}

func isConnResetError(err error) bool {
	s := err.Error()
	return strings.Contains(s, "connection reset by peer") || strings.Contains(s, "broken pipe")
}

func isNetError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		isNetOpError(err) ||
		isConnClosedError(err) ||
		isConnTimedOutError(err) ||
		isConnResetError(err) {
		return true
	}

	return false
}

// rejectReasonErr returns a pre-defined error for the given reject reason code.
func rejectReasonErr(reason int) error {
	switch reason {
	case hsms.RejectSTypeNotSupported:
		return hsms.ErrRejectSTypeNotSupported
	case hsms.RejectPTypeNotSupported:
		return hsms.ErrRejectPTypeNotSupported
	case hsms.RejectTransactionNotOpen:
		return hsms.ErrRejectTransactionNotOpen
	case hsms.RejectNotSelected:
		return hsms.ErrRejectNotSelected
	default:
		return fmt.Errorf("reject: unknown reason code %d", reason)
	}
}

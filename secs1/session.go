package secs1

import (
	"errors"
	"fmt"
	"sync"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/logger"
)

// Session represents a SECS-I session within a SECS-I connection.
//
// SECS-I is inherently single-session — the device ID identifies the
// equipment. This session wraps [hsms.BaseSession] to provide the same
// [hsms.Session] interface used by HSMS-SS, so application code can be
// written once and work with either transport.
//
// It implements the [hsms.Session] interface.
type Session struct {
	hsms.BaseSession
	id     uint16
	conn   *Connection
	cfg    *ConnectionConfig
	logger logger.Logger

	mu              sync.RWMutex
	dataMsgChans    []chan *hsms.DataMessage
	dataMsgHandlers []hsms.DataMessageHandler
}

var _ hsms.Session = (*Session)(nil)

// newSession creates a new SECS-I Session with the specified session ID
// and associated Connection.
func newSession(id uint16, conn *Connection) *Session {
	session := &Session{
		id:              id,
		conn:            conn,
		cfg:             conn.cfg,
		logger:          conn.logger,
		dataMsgChans:    make([]chan *hsms.DataMessage, 0),
		dataMsgHandlers: make([]hsms.DataMessageHandler, 0),
	}

	// Register SECS-I specific implementations to base session.
	session.RegisterIDFunc(session.ID)
	session.RegisterSendMessageFunc(session.SendMessage)
	session.RegisterSendMessageAsyncFunc(session.SendMessageAsync)

	return session
}

// ID returns the session ID (device ID) for this SECS-I session.
func (s *Session) ID() uint16 {
	return s.id
}

// SendMessage sends an HSMS message through the associated SECS-I connection
// and waits for its reply.
func (s *Session) SendMessage(msg hsms.HSMSMessage) (hsms.HSMSMessage, error) {
	return s.conn.sendMsg(msg)
}

// SendMessageAsync sends an HSMS message through the associated SECS-I
// connection asynchronously.
func (s *Session) SendMessageAsync(msg hsms.HSMSMessage) error {
	dataMsg, ok := msg.ToDataMessage()
	if !ok {
		return hsms.ErrNotDataMsg
	}

	return s.conn.sendMsgAsync(dataMsg)
}

// SendMessageSync sends an HSMS message through the associated SECS-I
// connection synchronously.
//
// It sends the message and blocks until it is sent to the connection's
// underlying transport layer.
func (s *Session) SendMessageSync(msg hsms.HSMSMessage) error {
	return s.conn.sendMsgSync(msg)
}

// AddConnStateChangeHandler adds one or more ConnStateChangeHandler functions
// to be invoked when the connection state changes.
//
// Handlers should be registered before Open() is called. They are invoked in
// registration order and must not block.
func (s *Session) AddConnStateChangeHandler(handlers ...hsms.ConnStateChangeHandler) {
	s.conn.stateMgr.AddHandler(handlers...)
}

// AddDataMessageHandler adds one or more DataMessageHandler functions to be
// invoked when a data message is received.
//
// Handlers should be registered before Open() is called. They are invoked
// in registration order. Each handler receives messages on its own channel
// and must not block.
func (s *Session) AddDataMessageHandler(handlers ...hsms.DataMessageHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.dataMsgHandlers = append(s.dataMsgHandlers, handlers...)
}

// startDataMsgTasks creates data message channels and starts goroutines
// for each registered data message handler.
func (s *Session) startDataMsgTasks() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.dataMsgChans = make([]chan *hsms.DataMessage, 0)

	var errs error

	for i, handler := range s.dataMsgHandlers {
		dataMsgChan := make(chan *hsms.DataMessage, s.cfg.dataMsgQueueSize)

		name := fmt.Sprintf("dataMsgTask-%d", i+1)
		if err := s.conn.taskMgr.StartRecvDataMsg(name, handler, s, dataMsgChan); err != nil {
			errs = errors.Join(errs, err)

			continue
		}

		s.dataMsgChans = append(s.dataMsgChans, dataMsgChan)
	}

	return errs
}

// recvDataMsg broadcasts a received data message to all data message
// handlers' channels.
func (s *Session) recvDataMsg(msg *hsms.DataMessage) {
	s.mu.RLock()
	chans := make([]chan *hsms.DataMessage, len(s.dataMsgChans))
	copy(chans, s.dataMsgChans)
	s.mu.RUnlock()

	if len(chans) == 0 {
		msg.Free()
		return
	}

	// Pre-clone for all additional handlers before sending any, to avoid a data
	// race where handler[0] calls msg.Free() while we are still cloning the original.
	deliverMsgs := make([]*hsms.DataMessage, len(chans))
	deliverMsgs[0] = msg

	for i := 1; i < len(chans); i++ {
		cloned, ok := msg.Clone().(*hsms.DataMessage)
		if !ok {
			s.logger.Warn("failed to clone data message for handler", "id", s.id, "msg_id", msg.ID())

			continue
		}

		deliverMsgs[i] = cloned
	}

	// Deliver all messages to their respective handler channels.
	for i, ch := range chans {
		if deliverMsgs[i] == nil {
			continue // clone failed for this handler
		}

		select {
		case <-s.conn.ctx.Done():
			s.logger.Debug("context done, stop receiving data message",
				"id", s.id, "msg_id", msg.ID())
			// Free all undelivered messages (including the original if not yet sent).
			for j := i; j < len(deliverMsgs); j++ {
				if deliverMsgs[j] != nil {
					deliverMsgs[j].Free()
				}
			}

			return
		case ch <- deliverMsgs[i]:
		}
	}
}

package hsmsss

import (
	"fmt"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/logger"
)

type Session struct {
	hsms.BaseSession
	id       uint16
	hsmsConn *Connection
	cfg      *ConnectionConfig
	logger   logger.Logger

	dataMsgChans    []chan *hsms.DataMessage
	dataMsgHandlers []hsms.DataMessageHandler
}

func NewSession(id uint16, hsmsConn *Connection) *Session {
	session := &Session{
		id:              id,
		hsmsConn:        hsmsConn,
		cfg:             hsmsConn.cfg,
		logger:          hsmsConn.logger,
		dataMsgChans:    make([]chan *hsms.DataMessage, 0),
		dataMsgHandlers: make([]hsms.DataMessageHandler, 0),
	}

	// assign HSMS-SS specific implementations to base session
	session.BaseSession.ID = session.ID
	session.BaseSession.SendMessage = session.SendMessage
	return session
}

func (s *Session) ID() uint16 {
	return s.id
}

func (s *Session) SendMessage(msg hsms.HSMSMessage) (hsms.HSMSMessage, error) {
	return s.hsmsConn.sendMsg(msg)
}

func (s *Session) AddConnStateChangeHandler(handlers ...hsms.ConnStateChangeHandler) {
	s.hsmsConn.stateMgr.AddHandler(handlers...)
}

func (s *Session) AddDataMessageHandler(handlers ...hsms.DataMessageHandler) {
	for _, handler := range handlers {
		s.dataMsgChans = append(s.dataMsgChans, make(chan *hsms.DataMessage, s.cfg.dataMsgQueueSize))
		s.dataMsgHandlers = append(s.dataMsgHandlers, handler)
	}
}

func (s *Session) startDataMsgTasks() {
	for i, handler := range s.dataMsgHandlers {
		name := fmt.Sprintf("dataMsgTask-%d", i+1)
		s.hsmsConn.taskMgr.StartRecvDataMsg(name, handler, s, s.dataMsgChans[i])
	}
}

// recvDataMsg broadcast message to all data message handlers' channel
func (s *Session) recvDataMsg(msg *hsms.DataMessage) {
	for _, dataMsgChan := range s.dataMsgChans {
		dataMsgChan <- msg
	}
}

func (s *Session) separateSession() {
	msg := hsms.NewSeparateReq(s.id, hsms.GenerateMsgSystemBytes())
	s.logger.Debug("send separate.req message and wait it to be sent", "method", "separateSession", "id", msg.ID())
	err := s.hsmsConn.sendMsgSync(msg)
	if err != nil {
		s.logger.Debug("failed to send separate control message", "method", "separateSession", "id", msg.ID(), "error", err)
	}
}

func (s *Session) selectSession() error {
	s.logger.Debug("send select.req", "method", "selectSession")
	// select request
	msg := hsms.NewSelectReq(s.id, hsms.GenerateMsgSystemBytes())
	replyMsg, err := s.hsmsConn.sendControlMsg(msg, true)
	if err != nil {
		return err
	}

	if replyMsg == nil || replyMsg.Type() != hsms.SelectRspType {
		return hsms.ErrInvalidRspMsg
	}

	// read select status
	selectStatus := replyMsg.Header()[3]
	switch selectStatus {
	case 0:
		// success
		s.logger.Debug("connection selected", "session_id", replyMsg.SessionID(), "type", replyMsg.Type())
		return nil
	default:
		s.logger.Warn("failed to select session", "session_id", replyMsg.SessionID(), "select_status", selectStatus)
		return hsms.ErrSelectFailed
	}
}

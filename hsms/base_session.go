package hsms

import "github.com/arloliu/go-secs/secs2"

// BaseSession implements common methods for HSMS-SS and HSMS-GS session.
type BaseSession struct {
	idFunc               func() uint16
	sendMessageFunc      func(msg HSMSMessage) (HSMSMessage, error)
	sendMessageAsyncFunc func(msg HSMSMessage) error
}

// NewBaseSession creates a new BaseSession with the specified ID generator function and message sending functions.
func NewBaseSession(
	idFunc func() uint16,
	sendMessageFunc func(msg HSMSMessage) (HSMSMessage, error),
	sendMessageAsyncFunc func(msg HSMSMessage) error,
) *BaseSession {
	return &BaseSession{idFunc: idFunc, sendMessageFunc: sendMessageFunc, sendMessageAsyncFunc: sendMessageAsyncFunc}
}

func (s *BaseSession) RegisterIDFunc(f func() uint16) {
	s.idFunc = f
}

func (s *BaseSession) RegisterSendMessageFunc(f func(msg HSMSMessage) (HSMSMessage, error)) {
	s.sendMessageFunc = f
}

func (s *BaseSession) RegisterSendMessageAsyncFunc(f func(msg HSMSMessage) error) {
	s.sendMessageAsyncFunc = f
}

// SendDataMessage sends an HSMS data message with the specified stream, function, and data item.
// It waits for a reply if replyExpected is true.
// It returns the received reply DataMessage if replyExpected is true, nil otherwise,
// and and an error if any occurred during sending or receiving.
//
// Item ownership: the library takes ownership of dataItem and will Free it
// (either on the success path or internally on send failure). Callers must
// not retain the reference and must not Free dataItem themselves, regardless
// of whether the call succeeds or returns an error. If you need to retry
// with the same logical item, call dataItem.Clone() before each attempt:
// passing the original would reuse a pointer the library has already
// returned to the SECS-II item pool and could race with concurrent
// decoders. The returned reply DataMessage (on success with replyExpected)
// is owned by the caller; Free it after use.
func (s *BaseSession) SendDataMessage(stream byte, function byte, replyExpected bool, dataItem secs2.Item) (*DataMessage, error) {
	if function%2 == 0 {
		return nil, ErrInvalidReqMsg
	}

	msg, err := newDataMessageWithID(stream, function, replyExpected, s.idFunc(), GenerateMsgID(), dataItem)
	if err != nil {
		return nil, err
	}

	replyMsg, err := s.sendMessageFunc(msg)
	if err != nil {
		if replyMsg == nil {
			return nil, err
		}

		// returns reply message as DataMessage with error if it can be converted
		dataMsg, ok := replyMsg.ToDataMessage()
		if !ok {
			return nil, err
		}

		return dataMsg, err
	}

	if !replyExpected {
		return nil, nil //nolint:nilnil
	}

	dataMsg, ok := replyMsg.(*DataMessage)
	if !ok {
		return nil, ErrNotDataMsg
	}

	return dataMsg, nil
}

// SendDataMessageAsync sends an HSMS data message asynchronously.
//
// It sends the message and returns immediately after sending,
// and let user specified data message handler to receive reply if any.
//
// Item ownership: same contract as [BaseSession.SendDataMessage] — library
// takes ownership of dataItem; do not retain the reference or Free it
// yourself. Clone before each call if you need to reuse the same logical
// value.
func (s *BaseSession) SendDataMessageAsync(stream byte, function byte, replyExpected bool, dataItem secs2.Item) error {
	if function%2 == 0 {
		return ErrInvalidReqMsg
	}

	msg, err := newDataMessageWithID(stream, function, replyExpected, s.idFunc(), GenerateMsgID(), dataItem)
	if err != nil {
		return err
	}

	return s.sendMessageAsyncFunc(msg)
}

// SendSECS2Message sends a SECS-II message and waits for its reply.
// It returns the received reply message (as a DataMessage) and an error if any occurred during sending or receiving.
//
// Item ownership: same contract as [BaseSession.SendDataMessage] — the
// library takes ownership of msg.Item() and will Free it. Do not retain a
// reference to msg.Item() after this call or Free it yourself. Clone if you
// need to reuse the same item across calls.
func (s *BaseSession) SendSECS2Message(msg secs2.SECS2Message) (*DataMessage, error) {
	if msg.FunctionCode()%2 == 0 {
		return nil, ErrInvalidReqMsg
	}

	dataMsg, err := newDataMessageWithID(msg.StreamCode(), msg.FunctionCode(), msg.WaitBit(), s.idFunc(), GenerateMsgID(), msg.Item())
	if err != nil {
		return nil, err
	}

	replyMsg, err := s.sendMessageFunc(dataMsg)
	if err != nil {
		return nil, err
	}

	if !msg.WaitBit() {
		return nil, nil //nolint:nilnil
	}

	replyDataMsg, ok := replyMsg.(*DataMessage)
	if !ok {
		return nil, ErrNotDataMsg
	}

	return replyDataMsg, nil
}

// SendSECS2MessageAsync sends a SECS-II message asynchronously.
// It sends the message and returns immediately after sending,
// and let user specified data message handler to receive reply if any.
//
// Item ownership: same contract as [BaseSession.SendDataMessage].
func (s *BaseSession) SendSECS2MessageAsync(msg secs2.SECS2Message) error {
	if msg.FunctionCode()%2 == 0 {
		return ErrInvalidReqMsg
	}

	dataMsg, err := newDataMessageWithID(msg.StreamCode(), msg.FunctionCode(), msg.WaitBit(), s.idFunc(), GenerateMsgID(), msg.Item())
	if err != nil {
		return err
	}

	return s.sendMessageAsyncFunc(dataMsg)
}

// ReplyDataMessage sends a reply to a previously received data message.
// It takes the original primary DataMessage and the data item for the reply as arguments.
// It returns an error if any occurred during sending the reply.
//
// It is a wrapper method to reply data message with the corresponding function code of primary message.
//
// Item ownership: the library takes ownership of dataItem; do not retain
// the reference or Free it yourself. In particular, passing
// primaryMsg.Item() shares the underlying pointer between primaryMsg and
// the reply — the library will Free that shared pointer when the reply is
// sent, and any subsequent access to primaryMsg.Item() (including another
// ReplyDataMessage call, or Free'ing primaryMsg) becomes use-after-free.
// Pass primaryMsg.Item().Clone() if you intend to keep primaryMsg usable
// after this call.
func (s *BaseSession) ReplyDataMessage(primaryMsg *DataMessage, dataItem secs2.Item) error {
	if primaryMsg.StreamCode() == 9 || primaryMsg.FunctionCode()%2 == 0 {
		return ErrInvalidReqMsg
	}

	replyMsg, err := NewDataMessage(
		primaryMsg.StreamCode(),
		primaryMsg.FunctionCode()+1,
		false,
		primaryMsg.SessionID(),
		primaryMsg.SystemBytes(),
		dataItem,
	)
	if err != nil {
		return err
	}

	_, err = s.sendMessageFunc(replyMsg)

	return err
}

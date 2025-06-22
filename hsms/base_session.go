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
func (s *BaseSession) SendDataMessage(stream byte, function byte, replyExpected bool, dataItem secs2.Item) (*DataMessage, error) {
	if function%2 == 0 {
		return nil, ErrInvalidReqMsg
	}

	msg, err := NewDataMessage(stream, function, replyExpected, s.idFunc(), GenerateMsgSystemBytes(), dataItem)
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
func (s *BaseSession) SendDataMessageAsync(stream byte, function byte, replyExpected bool, dataItem secs2.Item) error {
	if function%2 == 0 {
		return ErrInvalidReqMsg
	}

	msg, err := NewDataMessage(stream, function, replyExpected, s.idFunc(), GenerateMsgSystemBytes(), dataItem)
	if err != nil {
		return err
	}

	return s.sendMessageAsyncFunc(msg)
}

// SendSECS2Message sends a SECS-II message and waits for its reply.
// It returns the received reply message (as a DataMessage) and an error if any occurred during sending or receiving.
func (s *BaseSession) SendSECS2Message(msg secs2.SECS2Message) (*DataMessage, error) {
	if msg.FunctionCode()%2 == 0 {
		return nil, ErrInvalidReqMsg
	}

	dataMsg, err := NewDataMessage(msg.StreamCode(), msg.FunctionCode(), msg.WaitBit(), s.idFunc(), GenerateMsgSystemBytes(), msg.Item())
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
func (s *BaseSession) SendSECS2MessageAsync(msg secs2.SECS2Message) error {
	if msg.FunctionCode()%2 == 0 {
		return ErrInvalidReqMsg
	}

	dataMsg, err := NewDataMessage(msg.StreamCode(), msg.FunctionCode(), msg.WaitBit(), s.idFunc(), GenerateMsgSystemBytes(), msg.Item())
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

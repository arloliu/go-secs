package hsms

import (
	"github.com/arloliu/go-secs/secs2"
)

// Session defines the interface of an HSMS session within an HSMS connection.
// It provides methods for sending and receiving messages, handling data messages, and managing
// connection state change handlers.
type Session interface {
	// ID returns the session ID for this session.
	ID() uint16

	// SendMessage sends an HSMSMessage and waits for its reply.
	// It returns the received reply message and an error if any occurred during sending or receiving.
	SendMessage(msg HSMSMessage) (HSMSMessage, error)

	// SendSECS2Message sends a SECS-II message and waits for its reply.
	// It returns the received reply message (as a DataMessage) and an error if any occurred during sending or receiving.
	SendSECS2Message(msg secs2.SECS2Message) (*DataMessage, error)

	// SendDataMessage sends an HSMS data message with the specified stream, function, and data item.
	// It waits for a reply if replyExpected is true.
	// It returns the received reply DataMessage if replyExpected is true, nil otherwise,
	// and and an error if any occurred during sending or receiving.
	SendDataMessage(stream byte, function byte, replyExpected bool, dataItem secs2.Item) (*DataMessage, error)

	// ReplyDataMessage sends a reply to a previously received data message.
	// It takes the original primary DataMessage and the data item for the reply as arguments.
	// It returns an error if any occurred during sending the reply.
	//
	// It is a wrapper method to reply data message with the corresponding function code of primary message.
	ReplyDataMessage(primaryMsg *DataMessage, dataItem secs2.Item) error

	// AddConnStateChangeHandler adds one or more ConnStateChangeHandler functions to be invoked when the connection state changes.
	AddConnStateChangeHandler(handlers ...ConnStateChangeHandler)

	// AddDataMessageHandler adds one or more DataMessageHandler functions to be invoked when a data message is received.
	AddDataMessageHandler(handlers ...DataMessageHandler)
}

type BaseSession struct {
	ID          func() uint16
	SendMessage func(msg HSMSMessage) (HSMSMessage, error)
}

func (s *BaseSession) SendDataMessage(stream byte, function byte, replyExpected bool, dataItem secs2.Item) (*DataMessage, error) {
	if function%2 == 0 {
		return nil, ErrInvalidReqMsg
	}

	msg, err := NewDataMessage(stream, function, replyExpected, s.ID(), GenerateMsgSystemBytes(), dataItem)
	if err != nil {
		return nil, err
	}

	replyMsg, err := s.SendMessage(msg)
	if err != nil {
		return nil, err
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

func (s *BaseSession) SendSECS2Message(msg secs2.SECS2Message) (*DataMessage, error) {
	if msg.FunctionCode()%2 == 0 {
		return nil, ErrInvalidReqMsg
	}

	dataMsg, err := NewDataMessage(msg.StreamCode(), msg.FunctionCode(), msg.WaitBit(), s.ID(), GenerateMsgSystemBytes(), msg.Item())
	if err != nil {
		return nil, err
	}

	replyMsg, err := s.SendMessage(dataMsg)
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

	_, err = s.SendMessage(replyMsg)
	return err
}

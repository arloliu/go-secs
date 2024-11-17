package hsms

import (
	"errors"
	"testing"

	"github.com/arloliu/go-secs/gem"
	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/assert"
)

func TestSendDataMessage(t *testing.T) {
	tests := []struct {
		name          string
		stream        byte
		function      byte
		replyExpected bool
		dataItem      secs2.Item
		sendMessage   func(msg HSMSMessage) (HSMSMessage, error)
		expectedErr   error
		expectedReply *DataMessage
	}{
		{
			name:          "Invalid function code",
			stream:        1,
			function:      2,
			replyExpected: true,
			dataItem:      secs2.L(),
			sendMessage:   nil,
			expectedErr:   ErrInvalidReqMsg,
			expectedReply: nil,
		},
		{
			name:          "SendMessage error",
			stream:        1,
			function:      1,
			replyExpected: true,
			dataItem:      secs2.L(),
			sendMessage: func(msg HSMSMessage) (HSMSMessage, error) {
				return nil, errors.New("send message error")
			},
			expectedErr:   errors.New("send message error"),
			expectedReply: nil,
		},
		{
			name:          "No reply expected",
			stream:        1,
			function:      1,
			replyExpected: false,
			dataItem:      secs2.L(),
			sendMessage: func(msg HSMSMessage) (HSMSMessage, error) {
				return &DataMessage{}, nil
			},
			expectedErr:   nil,
			expectedReply: nil,
		},
		{
			name:          "Reply is not DataMessage",
			stream:        1,
			function:      1,
			replyExpected: true,
			dataItem:      secs2.L(),
			sendMessage: func(msg HSMSMessage) (HSMSMessage, error) {
				return &ControlMessage{}, nil
			},
			expectedErr:   ErrNotDataMsg,
			expectedReply: nil,
		},
		{
			name:          "Successful reply",
			stream:        1,
			function:      1,
			replyExpected: true,
			dataItem:      secs2.L(),
			sendMessage: func(msg HSMSMessage) (HSMSMessage, error) {
				return &DataMessage{}, nil
			},
			expectedErr:   nil,
			expectedReply: &DataMessage{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := &BaseSession{
				ID: func() uint16 {
					return 1
				},
				SendMessage: tt.sendMessage,
			}

			reply, err := session.SendDataMessage(tt.stream, tt.function, tt.replyExpected, tt.dataItem)
			assert.Equal(t, tt.expectedErr, err)
			assert.Equal(t, tt.expectedReply, reply)
		})
	}
}

func TestSendSECS2Message(t *testing.T) {
	tests := []struct {
		name          string
		msg           secs2.SECS2Message
		sendMessage   func(msg HSMSMessage) (HSMSMessage, error)
		expectedErr   error
		expectedReply *DataMessage
	}{
		{
			name:          "Invalid function code",
			msg:           gem.NewMessage(1, 2, true, secs2.L()),
			sendMessage:   nil,
			expectedErr:   ErrInvalidReqMsg,
			expectedReply: nil,
		},
		{
			name: "SendMessage error",
			msg:  gem.NewMessage(1, 1, true, secs2.L()),
			sendMessage: func(msg HSMSMessage) (HSMSMessage, error) {
				return nil, errors.New("send message error")
			},
			expectedErr:   errors.New("send message error"),
			expectedReply: nil,
		},
		{
			name: "No reply expected",
			msg:  gem.NewMessage(1, 1, false, secs2.L()),
			sendMessage: func(msg HSMSMessage) (HSMSMessage, error) {
				return &DataMessage{}, nil
			},
			expectedErr:   nil,
			expectedReply: nil,
		},
		{
			name: "Reply is not DataMessage",
			msg:  gem.NewMessage(1, 1, true, secs2.L()),
			sendMessage: func(msg HSMSMessage) (HSMSMessage, error) {
				return &ControlMessage{}, nil
			},
			expectedErr:   ErrNotDataMsg,
			expectedReply: nil,
		},
		{
			name: "Successful reply",
			msg:  gem.NewMessage(1, 1, true, secs2.L()),
			sendMessage: func(msg HSMSMessage) (HSMSMessage, error) {
				return &DataMessage{}, nil
			},
			expectedErr:   nil,
			expectedReply: &DataMessage{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := &BaseSession{
				ID: func() uint16 {
					return 1
				},
				SendMessage: tt.sendMessage,
			}

			reply, err := session.SendSECS2Message(tt.msg)
			assert.Equal(t, tt.expectedErr, err)
			assert.Equal(t, tt.expectedReply, reply)
		})
	}
}

func TestReplyDataMessage(t *testing.T) {
	tests := []struct {
		name        string
		primaryMsg  *DataMessage
		dataItem    secs2.Item
		sendMessage func(msg HSMSMessage) (HSMSMessage, error)
		expectedErr error
	}{
		{
			name: "Invalid primary message",
			primaryMsg: &DataMessage{
				stream:      9,
				function:    1,
				systemBytes: []byte{0x01, 0x02, 0x03, 0x04},
			},
			dataItem:    secs2.L(),
			sendMessage: nil,
			expectedErr: ErrInvalidReqMsg,
		},
		{
			name: "SendMessage error",
			primaryMsg: &DataMessage{
				stream:      1,
				function:    1,
				systemBytes: []byte{0x01, 0x02, 0x03, 0x04},
			},
			dataItem: secs2.L(),
			sendMessage: func(msg HSMSMessage) (HSMSMessage, error) {
				return nil, errors.New("send message error")
			},
			expectedErr: errors.New("send message error"),
		},
		{
			name: "Successful reply",
			primaryMsg: &DataMessage{
				stream:      1,
				function:    1,
				systemBytes: []byte{0x01, 0x02, 0x03, 0x04},
			},
			dataItem: secs2.L(),
			sendMessage: func(msg HSMSMessage) (HSMSMessage, error) {
				return &DataMessage{}, nil
			},
			expectedErr: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := &BaseSession{
				ID: func() uint16 {
					return 1
				},
				SendMessage: tt.sendMessage,
			}

			err := session.ReplyDataMessage(tt.primaryMsg, tt.dataItem)
			assert.Equal(t, tt.expectedErr, err)
		})
	}
}

//nolint:errcheck
package hsms

import (
	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/mock"
)

// MockSession implements Session interface for testing
type MockSession struct {
	mock.Mock
}

var _ Session = (*MockSession)(nil)

func (m *MockSession) RegisterIDFunc(f func() uint16) {
	m.Called(f)
}

func (m *MockSession) RegisterSendMessageFunc(f func(msg HSMSMessage) (HSMSMessage, error)) {
	m.Called(f)
}

func (m *MockSession) RegisterSendMessageAsyncFunc(f func(msg HSMSMessage) error) {
	m.Called(f)
}

func (m *MockSession) ID() uint16 {
	args := m.Called()
	return args.Get(0).(uint16)
}

func (m *MockSession) SendMessage(msg HSMSMessage) (HSMSMessage, error) {
	args := m.Called(msg)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(HSMSMessage), args.Error(1)
}

func (m *MockSession) SendMessageAsync(msg HSMSMessage) error {
	args := m.Called(msg)
	return args.Error(0)
}

func (m *MockSession) SendMessageSync(msg HSMSMessage) error {
	args := m.Called(msg)
	return args.Error(0)
}

func (m *MockSession) SendSECS2Message(msg secs2.SECS2Message) (*DataMessage, error) {
	args := m.Called(msg)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*DataMessage), args.Error(1)
}

func (m *MockSession) SendSECS2MessageAsync(msg secs2.SECS2Message) error {
	args := m.Called(msg)
	return args.Error(0)
}

func (m *MockSession) SendDataMessage(stream byte, function byte, replyExpected bool, dataItem secs2.Item) (*DataMessage, error) {
	args := m.Called(stream, function, replyExpected, dataItem)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*DataMessage), args.Error(1)
}

func (m *MockSession) SendDataMessageAsync(stream byte, function byte, replyExpected bool, dataItem secs2.Item) error {
	args := m.Called(stream, function, replyExpected, dataItem)
	return args.Error(0)
}

func (m *MockSession) ReplyDataMessage(primaryMsg *DataMessage, dataItem secs2.Item) error {
	args := m.Called(primaryMsg, dataItem)
	return args.Error(0)
}

func (m *MockSession) AddConnStateChangeHandler(handlers ...ConnStateChangeHandler) {
	m.Called(handlers)
}

func (m *MockSession) AddDataMessageHandler(handlers ...DataMessageHandler) {
	m.Called(handlers)
}

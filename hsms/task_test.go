package hsms

import (
	"bufio"
	"context"
	"net"
	"testing"
	"time"

	"github.com/arloliu/go-secs/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// MockConn is a mock implementation of net.Conn for testing purposes.
type MockConn struct {
	mock.Mock
}

var _ net.Conn = (*MockConn)(nil)

func (m *MockConn) Read(b []byte) (n int, err error) {
	args := m.Called(b)
	return args.Int(0), args.Error(1)
}

func (m *MockConn) Write(b []byte) (n int, err error) {
	args := m.Called(b)
	return args.Int(0), args.Error(1)
}

func (m *MockConn) Close() error {
	args := m.Called()
	return args.Error(0)
}

func (m *MockConn) LocalAddr() net.Addr {
	args := m.Called()
	return args.Get(0).(net.Addr)
}

func (m *MockConn) RemoteAddr() net.Addr {
	args := m.Called()
	return args.Get(0).(net.Addr)
}

func (m *MockConn) SetDeadline(t time.Time) error {
	args := m.Called(t)
	return args.Error(0)
}

func (m *MockConn) SetReadDeadline(t time.Time) error {
	args := m.Called(t)
	return args.Error(0)
}

func (m *MockConn) SetWriteDeadline(t time.Time) error {
	args := m.Called(t)
	return args.Error(0)
}

func TestTaskManager_StartReceiver(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockLogger := logger.NewMockLogger()
	mockLogger.On("Debug", mock.Anything, mock.Anything).Return()
	mockLogger.On("Error", mock.Anything, mock.Anything).Return()
	mockLogger.On("Fatal", mock.Anything, mock.Anything).Return()
	mockLogger.On("Info", mock.Anything, mock.Anything).Return()
	mockLogger.On("Warn", mock.Anything, mock.Anything).Return()

	taskMgr := NewTaskManager(ctx, mockLogger)

	mockConn := &MockConn{}
	_ = mockConn.On("Read", mock.Anything).Return(0, nil)

	taskFunc := func(reader *bufio.Reader, msgLenBuf []byte) bool {
		_, err := reader.Read(msgLenBuf)
		return err == nil
	}

	taskCancelFunc := func() {
		mockLogger.Debug("Task canceled")
	}

	taskMgr.StartReceiver("testReceiver", mockConn, taskFunc, taskCancelFunc)

	// Allow some time for the goroutine to start
	time.Sleep(100 * time.Millisecond)

	// Verify that the task is running
	assert.Equal(t, 1, taskMgr.TaskCount())

	// Cancel the context to stop the task
	cancel()

	// Allow some time for the goroutine to stop
	time.Sleep(100 * time.Millisecond)

	// Verify that the task has stopped
	assert.Equal(t, 0, taskMgr.TaskCount())

	mockConn.Mock.AssertExpectations(t)
	mockLogger.AssertNumberOfCalls(t, "Debug", 3)
	mockLogger.AssertNumberOfCalls(t, "Error", 0)
	mockLogger.AssertNumberOfCalls(t, "Fatal", 0)
	mockLogger.AssertNumberOfCalls(t, "Info", 0)
	mockLogger.AssertNumberOfCalls(t, "Warn", 0)
}

func TestTaskManager_Start(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockLogger := logger.NewMockLogger()
	mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

	taskMgr := NewTaskManager(ctx, mockLogger)

	taskFunc := func() bool {
		return true
	}

	taskMgr.Start("testTask", taskFunc)

	// Allow some time for the goroutine to start
	time.Sleep(100 * time.Millisecond)

	// Verify that the task is running
	assert.Equal(t, 1, taskMgr.TaskCount())

	// Cancel the context to stop the task
	cancel()

	// Allow some time for the goroutine to stop
	time.Sleep(100 * time.Millisecond)

	// Verify that the task has stopped
	assert.Equal(t, 0, taskMgr.TaskCount())
}

func TestTaskManager_StartSender(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockLogger := logger.NewMockLogger()
	mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

	taskMgr := NewTaskManager(ctx, mockLogger)

	inputChan := make(chan HSMSMessage, 1)
	taskFunc := func(msg HSMSMessage) bool {
		return true
	}

	taskMgr.StartSender("testSender", taskFunc, inputChan)

	// Allow some time for the goroutine to start
	time.Sleep(100 * time.Millisecond)

	// Verify that the task is running
	assert.Equal(t, 1, taskMgr.TaskCount())

	// Send a message to the channel
	inputChan <- &DataMessage{}

	// Allow some time for the message to be processed
	time.Sleep(100 * time.Millisecond)

	// Cancel the context to stop the task
	cancel()

	// Allow some time for the goroutine to stop
	time.Sleep(100 * time.Millisecond)

	// Verify that the task has stopped
	assert.Equal(t, 0, taskMgr.TaskCount())
}

func TestTaskManager_StartInterval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockLogger := logger.NewMockLogger()
	mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

	taskMgr := NewTaskManager(ctx, mockLogger)

	taskFunc := func() bool {
		return true
	}

	ticker := taskMgr.StartInterval("testInterval", taskFunc, 10*time.Millisecond, true)

	// Allow some time for the interval task to run
	time.Sleep(100 * time.Millisecond)

	// Verify that the task is running
	assert.Equal(t, 1, taskMgr.TaskCount())

	// Stop the ticker to stop the task
	cancel()
	ticker.Stop()

	// Allow some time for the goroutine to stop
	time.Sleep(100 * time.Millisecond)

	// Verify that the task has stopped
	assert.Equal(t, 0, taskMgr.TaskCount())
}

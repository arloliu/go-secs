package hsms

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/logger"
	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Helper function to create test DataMessages
func createTestDataMessage(t *testing.T, stream byte, function byte, sessionID uint16) *DataMessage { //nolint:unparam
	msg, err := NewDataMessage(
		stream,
		function,
		function%2 == 1, // replyExpected for odd function codes
		sessionID,
		GenerateMsgSystemBytes(), // Generate unique system bytes
		secs2.NewEmptyItem(),
	)
	require.NoError(t, err)

	return msg
}

func TestTaskManager_Start(t *testing.T) {
	t.Run("successful start", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		counter := atomic.Int32{}
		taskFunc := func() bool {
			counter.Add(1)
			return counter.Load() < 5 // run 5 times then stop
		}

		err := taskMgr.Start("testTask", taskFunc)
		assert.NoError(t, err)

		// Wait for task to complete
		taskMgr.Wait()

		assert.Equal(t, int32(5), counter.Load())
		assert.Equal(t, 0, taskMgr.TaskCount())
	})

	t.Run("task stops on context cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		runCount := atomic.Int32{}
		taskFunc := func() bool {
			runCount.Add(1)
			time.Sleep(10 * time.Millisecond)
			return true
		}

		err := taskMgr.Start("testTask", taskFunc)
		assert.NoError(t, err)

		time.Sleep(50 * time.Millisecond)
		cancel()

		taskMgr.Wait()
		assert.Equal(t, 0, taskMgr.TaskCount())
		assert.Greater(t, runCount.Load(), int32(0))
	})

	t.Run("start after stop fails", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)
		taskMgr.Stop()

		err := taskMgr.Start("testTask", func() bool { return true })
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "task manager already stopped")
	})

	t.Run("task function returns false stops task", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		runOnce := atomic.Bool{}
		taskFunc := func() bool {
			runOnce.Store(true)
			return false // stop immediately
		}

		err := taskMgr.Start("testTask", taskFunc)
		assert.NoError(t, err)

		taskMgr.Wait()
		assert.True(t, runOnce.Load())
		assert.Equal(t, 0, taskMgr.TaskCount())
	})
}

func TestTaskManager_StartReceiver(t *testing.T) {
	t.Run("successful receiver", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		recvCount := atomic.Int32{}
		taskFunc := func(msgLenBuf []byte) bool {
			assert.Len(t, msgLenBuf, 4)
			recvCount.Add(1)
			return recvCount.Load() < 3
		}

		cancelCalled := false
		taskCancelFunc := func() {
			cancelCalled = true
		}

		err := taskMgr.StartReceiver("testReceiver", taskFunc, taskCancelFunc)
		assert.NoError(t, err)

		taskMgr.Wait()
		assert.Equal(t, int32(3), recvCount.Load())
		assert.True(t, cancelCalled)
	})

	t.Run("nil cancel func is ok", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		err := taskMgr.StartReceiver("testReceiver", func([]byte) bool { return false }, nil)
		assert.NoError(t, err)

		taskMgr.Wait()
		assert.Equal(t, 0, taskMgr.TaskCount())
	})

	t.Run("context cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		blockingTask := func(msgLenBuf []byte) bool {
			time.Sleep(100 * time.Millisecond)
			return true
		}

		err := taskMgr.StartReceiver("testReceiver", blockingTask, nil)
		assert.NoError(t, err)

		time.Sleep(50 * time.Millisecond)
		assert.Equal(t, 1, taskMgr.TaskCount())

		cancel()
		taskMgr.Wait()
		assert.Equal(t, 0, taskMgr.TaskCount())
	})
}

func TestTaskManager_StartSender(t *testing.T) {
	t.Run("successful sender", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		inputChan := make(chan HSMSMessage, 3)
		processedCount := atomic.Int32{}

		taskFunc := func(msg HSMSMessage) bool {
			assert.NotNil(t, msg)
			processedCount.Add(1)
			return true
		}

		cancelCalled := false
		taskCancelFunc := func() {
			cancelCalled = true
		}

		err := taskMgr.StartSender("testSender", taskFunc, taskCancelFunc, inputChan)
		assert.NoError(t, err)

		// Send messages
		inputChan <- createTestDataMessage(t, 1, 1, 1)
		inputChan <- createTestDataMessage(t, 1, 3, 1)
		inputChan <- createTestDataMessage(t, 2, 1, 1)
		close(inputChan)

		taskMgr.Wait()
		assert.Equal(t, int32(3), processedCount.Load())
		assert.True(t, cancelCalled)
	})

	t.Run("nil input channel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		err := taskMgr.StartSender("testSender", func(HSMSMessage) bool { return true }, nil, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "input channel is nil")
	})

	t.Run("task func returns false", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		inputChan := make(chan HSMSMessage, 1)
		taskFunc := func(msg HSMSMessage) bool {
			return false // stop immediately
		}

		err := taskMgr.StartSender("testSender", taskFunc, nil, inputChan)
		assert.NoError(t, err)

		inputChan <- createTestDataMessage(t, 1, 1, 1)

		taskMgr.Wait()
		assert.Equal(t, 0, taskMgr.TaskCount())
	})

	t.Run("context cancellation while waiting", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		inputChan := make(chan HSMSMessage) // unbuffered channel
		taskFunc := func(msg HSMSMessage) bool {
			return true
		}

		err := taskMgr.StartSender("testSender", taskFunc, nil, inputChan)
		assert.NoError(t, err)

		time.Sleep(50 * time.Millisecond)
		cancel()

		taskMgr.Wait()
		assert.Equal(t, 0, taskMgr.TaskCount())
	})
}

func TestTaskManager_StartRecvDataMsg(t *testing.T) {
	t.Run("successful data message receiver", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		inputChan := make(chan *DataMessage, 2)
		processedMsgs := make([]*DataMessage, 0)
		var mu sync.Mutex

		taskFunc := func(msg *DataMessage, session Session) {
			mu.Lock()
			processedMsgs = append(processedMsgs, msg)
			mu.Unlock()
		}

		mockSession := &MockSession{}
		mockSession.On("ID").Return(uint16(1))

		err := taskMgr.StartRecvDataMsg("testDataReceiver", taskFunc, mockSession, inputChan)
		assert.NoError(t, err)

		// Send messages
		msg1 := createTestDataMessage(t, 1, 1, 1)
		msg2 := createTestDataMessage(t, 1, 3, 1)
		inputChan <- msg1
		inputChan <- msg2
		inputChan <- nil // nil message should be skipped
		close(inputChan)

		taskMgr.Wait()

		mu.Lock()
		assert.Len(t, processedMsgs, 2)
		assert.Contains(t, processedMsgs, msg1)
		assert.Contains(t, processedMsgs, msg2)
		mu.Unlock()
	})

	t.Run("nil input channel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		err := taskMgr.StartRecvDataMsg("test", func(*DataMessage, Session) {}, &MockSession{}, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "input channel is nil")
	})

	t.Run("nil session", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		inputChan := make(chan *DataMessage)
		err := taskMgr.StartRecvDataMsg("test", func(*DataMessage, Session) {}, nil, inputChan)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "session is nil")
	})

	t.Run("panic in handler is recovered", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()
		mockLogger.On("Error", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		inputChan := make(chan *DataMessage, 2)
		processedCount := atomic.Int32{}

		taskFunc := func(msg *DataMessage, session Session) {
			processedCount.Add(1)
			if processedCount.Load() == 1 {
				panic("test panic")
			}
		}

		mockSession := &MockSession{}
		err := taskMgr.StartRecvDataMsg("testDataReceiver", taskFunc, mockSession, inputChan)
		assert.NoError(t, err)

		inputChan <- createTestDataMessage(t, 1, 1, 1)
		inputChan <- createTestDataMessage(t, 1, 3, 1)
		close(inputChan)

		taskMgr.Wait()
		assert.Equal(t, int32(2), processedCount.Load())
		mockLogger.AssertCalled(t, "Error", mock.Anything, mock.Anything)
	})
}

func TestTaskManager_StartInterval(t *testing.T) {
	t.Run("successful interval task", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		runCount := atomic.Int32{}
		taskFunc := func() bool {
			runCount.Add(1)
			return runCount.Load() < 3
		}

		ticker, err := taskMgr.StartInterval("testInterval", taskFunc, 10*time.Millisecond, false)
		require.NoError(t, err)
		assert.NotNil(t, ticker)

		time.Sleep(50 * time.Millisecond)
		taskMgr.Stop()
		taskMgr.Wait()

		assert.GreaterOrEqual(t, runCount.Load(), int32(3))
	})

	t.Run("run now option", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		runCount := atomic.Int32{}
		taskFunc := func() bool {
			runCount.Add(1)
			return true
		}

		ticker, err := taskMgr.StartInterval("testInterval", taskFunc, 1*time.Hour, true)
		require.NoError(t, err)

		time.Sleep(50 * time.Millisecond)
		assert.Equal(t, int32(1), runCount.Load()) // Should run once immediately

		ticker.Stop()
		taskMgr.Stop()
		taskMgr.Wait()
	})

	t.Run("invalid interval", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		ticker, err := taskMgr.StartInterval("test", func() bool { return true }, 0, false)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid interval")
		assert.Nil(t, ticker)
	})

	t.Run("duplicate interval task", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		ticker1, err := taskMgr.StartInterval("testInterval", func() bool { return true }, 1*time.Hour, false)
		require.NoError(t, err)

		ticker2, err := taskMgr.StartInterval("testInterval", func() bool { return true }, 1*time.Hour, false)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "already exists")
		assert.Nil(t, ticker2)

		ticker1.Stop()
		taskMgr.Stop()
	})

	t.Run("runNow returns false", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		taskFunc := func() bool {
			return false // stop immediately
		}

		ticker, err := taskMgr.StartInterval("testInterval", taskFunc, 1*time.Hour, true)
		assert.NoError(t, err)
		assert.NotNil(t, ticker)

		time.Sleep(50 * time.Millisecond)
		assert.Equal(t, 0, taskMgr.TaskCount()) // Task should have stopped

		ticker.Stop()
	})

	t.Run("panic in interval task", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()
		mockLogger.On("Error", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		runCount := atomic.Int32{}
		taskFunc := func() bool {
			if runCount.Add(1) == 2 {
				panic("test panic")
			}

			return true
		}

		ticker, err := taskMgr.StartInterval("testInterval", taskFunc, 10*time.Millisecond, false)
		require.NoError(t, err)

		time.Sleep(50 * time.Millisecond)
		ticker.Stop()
		taskMgr.Stop()
		taskMgr.Wait()

		assert.Greater(t, runCount.Load(), int32(1))
		mockLogger.AssertCalled(t, "Error", mock.Anything, mock.Anything)
	})
}

func TestTaskManager_StopInterval(t *testing.T) {
	t.Run("stop existing interval", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		ticker, err := taskMgr.StartInterval("testInterval", func() bool { return true }, 1*time.Hour, false)
		require.NoError(t, err)

		err = taskMgr.StopInterval("testInterval")
		assert.NoError(t, err)

		// Verify ticker was removed
		err = taskMgr.StopInterval("testInterval")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "not found")

		ticker.Stop()
	})

	t.Run("stop non-existent interval", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		err := taskMgr.StopInterval("nonExistent")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}

func TestTaskManager_Wait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockLogger := logger.NewMockLogger()
	mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

	taskMgr := NewTaskManager(ctx, mockLogger)

	// Start multiple tasks
	err1 := taskMgr.Start("task1", func() bool { time.Sleep(50 * time.Millisecond); return false })
	err2 := taskMgr.Start("task2", func() bool { time.Sleep(100 * time.Millisecond); return false })

	assert.NoError(t, err1)
	assert.NoError(t, err2)

	start := time.Now()
	taskMgr.Wait()
	duration := time.Since(start)

	assert.GreaterOrEqual(t, duration, 100*time.Millisecond)
	assert.Equal(t, 0, taskMgr.TaskCount())
}

func TestTaskManager_Stop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockLogger := logger.NewMockLogger()
	mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

	taskMgr := NewTaskManager(ctx, mockLogger)

	// Start various tasks
	err1 := taskMgr.Start("task1", func() bool { time.Sleep(10 * time.Millisecond); return true })
	err2 := taskMgr.StartReceiver("receiver1", func([]byte) bool { time.Sleep(10 * time.Millisecond); return true }, nil)
	ticker, err3 := taskMgr.StartInterval("interval1", func() bool { return true }, 10*time.Millisecond, false)

	assert.NoError(t, err1)
	assert.NoError(t, err2)
	assert.NoError(t, err3)

	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, 3, taskMgr.TaskCount())

	taskMgr.Stop()
	taskMgr.Wait()

	assert.Equal(t, 0, taskMgr.TaskCount())
	ticker.Stop()
}

func TestTaskManager_ConcurrentOperations(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockLogger := logger.NewMockLogger()
	mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

	taskMgr := NewTaskManager(ctx, mockLogger)

	var wg sync.WaitGroup
	errors := make([]error, 10)

	// Start multiple tasks concurrently
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			taskName := fmt.Sprintf("task%d", idx)
			errors[idx] = taskMgr.Start(taskName, func() bool {
				time.Sleep(10 * time.Millisecond)
				return false
			})
		}(i)
	}

	wg.Wait()

	// Check all tasks started successfully
	for _, err := range errors {
		assert.NoError(t, err)
	}

	// Wait for all tasks to complete
	taskMgr.Wait()
	assert.Equal(t, 0, taskMgr.TaskCount())
}

func TestTaskManager_PanicRecovery(t *testing.T) {
	t.Run("panic during startup", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()
		mockLogger.On("Error", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		// Test panic during task startup using the actual startTask method
		panicOnStartup := atomic.Bool{}
		panicOnStartup.Store(true)

		taskFunc := func() bool {
			// This simulates panic in initialization code
			if panicOnStartup.Load() {
				panic("panic during initialization")
			}

			return false
		}

		// The panic should be caught by the defer in startTask
		err := taskMgr.Start("panicTest", taskFunc)
		assert.NoError(t, err) // Task should start successfully despite panic

		// Wait for task to complete
		taskMgr.Wait()
		assert.Equal(t, 0, taskMgr.TaskCount())
	})

	t.Run("panic in task function", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()
		mockLogger.On("Error", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		panicTask := func() bool {
			panic("test panic in task")
		}

		// Use callWithRecoverBool directly
		result := taskMgr.callWithRecoverBool("testTask", panicTask)
		assert.False(t, result)
		mockLogger.AssertCalled(t, "Error", mock.Anything, mock.Anything)
	})

	t.Run("panic after successful startup", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()
		mockLogger.On("Error", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		runCount := atomic.Int32{}
		taskFunc := func() bool {
			count := runCount.Add(1)
			if count == 3 {
				panic("panic after running for a while")
			}

			return true
		}

		err := taskMgr.Start("panicTask", taskFunc)
		assert.NoError(t, err) // Should start successfully

		// Let it run and panic
		time.Sleep(100 * time.Millisecond)

		taskMgr.Wait()
		assert.Equal(t, 0, taskMgr.TaskCount())
		assert.Equal(t, int32(3), runCount.Load())
	})

	t.Run("panic in runNow for interval", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()
		mockLogger.On("Error", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		panicFunc := func() bool {
			panic("panic in runNow")
		}

		// Should handle panic and return ticker
		ticker, err := taskMgr.StartInterval("panicInterval", panicFunc, 1*time.Hour, true)
		assert.NoError(t, err)
		assert.NotNil(t, ticker)

		// Verify the interval task didn't start due to runNow panic
		time.Sleep(50 * time.Millisecond)
		assert.Equal(t, 0, taskMgr.TaskCount())

		ticker.Stop()
		mockLogger.AssertCalled(t, "Error", mock.Anything, mock.Anything)
	})
}

func TestTaskManager_EdgeCases(t *testing.T) {
	t.Run("context already cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		err := taskMgr.Start("test", func() bool { return true })
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "task manager already stopped")
	})

	t.Run("wait after stop", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		taskMgr.Stop()
		taskMgr.Wait() // Should not panic or hang

		// Context should be recreated after Wait
		assert.NotNil(t, taskMgr.ctx)
	})

	t.Run("multiple stops", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		// Multiple stops should not panic
		taskMgr.Stop()
		taskMgr.Stop()
		taskMgr.Stop()
	})
}

func TestTaskManager_ContextDataRace(t *testing.T) { //nolint:cyclop
	t.Run("concurrent stop and start", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		// Start some long-running tasks
		for i := range 5 {
			_ = taskMgr.Start(fmt.Sprintf("task%d", i), func() bool {
				time.Sleep(10 * time.Millisecond)
				return true
			})
		}

		// Race condition: concurrent Stop/Wait/Start operations
		var wg sync.WaitGroup

		// Goroutine 1: Stop and Wait repeatedly
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				taskMgr.Stop()
				taskMgr.Wait()
				time.Sleep(1 * time.Millisecond)
			}
		}()

		// Goroutine 2: Try to start new tasks
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 10 {
				_ = taskMgr.Start(fmt.Sprintf("new_task%d", i), func() bool {
					time.Sleep(5 * time.Millisecond)
					return false
				})
				time.Sleep(1 * time.Millisecond)
			}
		}()

		// Goroutine 3: Check context status
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				ctx := taskMgr.getContext()
				select {
				case <-ctx.Done():
					// Context is cancelled
				default:
					// Context is active
				}
				time.Sleep(500 * time.Microsecond)
			}
		}()

		wg.Wait()
		taskMgr.Stop()
		taskMgr.Wait()
	})

	t.Run("wait recreates context during active tasks", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		// Channel to control task execution
		taskStarted := make(chan bool)

		// Start a task that checks context in select
		err := taskMgr.Start("controlled_task", func() bool {
			select {
			case taskStarted <- true:
			default:
			}

			// Check context cancellation in select to avoid blocking
			ctxLocal := taskMgr.getContext()
			select {
			case <-ctxLocal.Done():
				return false
			case <-time.After(5 * time.Millisecond):
				return true
			}
		})
		require.NoError(t, err)

		// Wait for task to start
		<-taskStarted

		var wg sync.WaitGroup

		// Goroutine 1: Stop, Wait, and recreate context
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 5 {
				taskMgr.Stop()
				taskMgr.Wait()

				// Start a new task after Wait() to verify context is recreated
				_ = taskMgr.Start(fmt.Sprintf("task_after_wait_%d", i), func() bool {
					time.Sleep(1 * time.Millisecond)
					return false
				})

				time.Sleep(1 * time.Millisecond)
			}
		}()

		// Goroutine 2: Try to start new tasks during Stop/Wait cycles
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 10 {
				_ = taskMgr.Start(fmt.Sprintf("concurrent_task_%d", i), func() bool {
					time.Sleep(2 * time.Millisecond)
					return false
				})
				time.Sleep(2 * time.Millisecond)
			}
		}()

		// Goroutine 3: Try to start intervals during Stop/Wait cycles
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 10 {
				ticker, err := taskMgr.StartInterval(fmt.Sprintf("interval%d", i), func() bool {
					return false
				}, 100*time.Millisecond, false)

				if err == nil && ticker != nil {
					// Clean up ticker
					go func(t *time.Ticker) {
						time.Sleep(10 * time.Millisecond)
						t.Stop()
					}(ticker)
				}

				time.Sleep(2 * time.Millisecond)
			}
		}()

		wg.Wait()
		taskMgr.Stop()
		taskMgr.Wait()
	})

	t.Run("channel operations during context switch", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		msgChan := make(chan HSMSMessage, 10)
		dataChan := make(chan *DataMessage, 10)

		processedCount := atomic.Int32{}

		// Start sender task
		err := taskMgr.StartSender("sender", func(msg HSMSMessage) bool {
			processedCount.Add(1)
			return true
		}, nil, msgChan)
		require.NoError(t, err)

		// Start data receiver task
		mockSession := &MockSession{}
		err = taskMgr.StartRecvDataMsg("receiver", func(msg *DataMessage, session Session) {
			processedCount.Add(1)
		}, mockSession, dataChan)
		require.NoError(t, err)

		var wg sync.WaitGroup

		// Goroutine 1: Send messages continuously
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				select {
				case msgChan <- createTestDataMessage(t, 1, 1, 1):
				case dataChan <- createTestDataMessage(t, 2, 2, 1):
				case <-time.After(5 * time.Millisecond):
				}
			}
		}()

		// Goroutine 2: Stop/Wait/Restart cycle
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(10 * time.Millisecond)

			for i := 0; i < 3; i++ {
				taskMgr.Stop()
				taskMgr.Wait()

				// Restart tasks
				_ = taskMgr.StartSender("sender", func(msg HSMSMessage) bool {
					processedCount.Add(1)
					return true
				}, nil, msgChan)

				_ = taskMgr.StartRecvDataMsg("receiver", func(msg *DataMessage, session Session) {
					processedCount.Add(1)
				}, mockSession, dataChan)

				time.Sleep(5 * time.Millisecond)
			}
		}()

		wg.Wait()
		close(msgChan)
		close(dataChan)

		taskMgr.Stop()
		taskMgr.Wait()

		// Verify some messages were processed
		assert.Greater(t, processedCount.Load(), int32(0))
	})

	t.Run("interval task during context recreation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockLogger := logger.NewMockLogger()
		mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

		taskMgr := NewTaskManager(ctx, mockLogger)

		tickCount := atomic.Int32{}

		// Start interval task
		ticker, err := taskMgr.StartInterval("ticker", func() bool {
			tickCount.Add(1)
			return true
		}, 5*time.Millisecond, false)
		require.NoError(t, err)
		require.NotNil(t, ticker)

		var wg sync.WaitGroup

		// Goroutine 1: Repeatedly stop/wait cycle
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				time.Sleep(10 * time.Millisecond)
				taskMgr.Stop()
				taskMgr.Wait()
			}
		}()

		// Goroutine 2: Try to access context during transitions
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				ctx := taskMgr.getContext()
				select {
				case <-ctx.Done():
				default:
				}
				time.Sleep(1 * time.Millisecond)
			}
		}()

		// Goroutine 3: Try to start new intervals
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				time.Sleep(5 * time.Millisecond)
				_, _ = taskMgr.StartInterval(fmt.Sprintf("ticker%d", i), func() bool {
					return false
				}, 100*time.Millisecond, false)
			}
		}()

		wg.Wait()
		taskMgr.Stop()
		taskMgr.Wait()
	})
}

// Run with: go test -race -run TestTaskManager_ContextDataRace
func TestTaskManager_ContextDataRaceWithRaceDetector(t *testing.T) {
	// This test specifically targets the Wait() context recreation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockLogger := logger.NewMockLogger()
	mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

	taskMgr := NewTaskManager(ctx, mockLogger)

	// Start a task that checks context frequently
	_ = taskMgr.Start("context_checker", func() bool {
		// This will call getContext() repeatedly
		for range 100 {
			ctx := taskMgr.getContext()
			select {
			case <-ctx.Done():
				return false
			default:
			}
		}

		return true
	})

	// Concurrently stop/wait/recreate context
	go func() {
		for range 10 {
			taskMgr.Stop()
			taskMgr.Wait()
			time.Sleep(time.Microsecond)
		}
	}()

	// Also try to start new tasks
	go func() {
		for i := range 10 {
			_ = taskMgr.Start(fmt.Sprintf("new_task_%d", i), func() bool {
				return false
			})
			time.Sleep(time.Microsecond)
		}
	}()

	time.Sleep(50 * time.Millisecond)
	taskMgr.Stop()
	taskMgr.Wait()
}

// Benchmark tests
func BenchmarkTaskManager_Start(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockLogger := logger.NewMockLogger()
	mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

	taskMgr := NewTaskManager(ctx, mockLogger)

	for i := 0; b.Loop(); i++ {
		err := taskMgr.Start(fmt.Sprintf("task%d", i), func() bool { return false })
		if err != nil {
			b.Errorf("failed to start task %d: %v", i, err)
		}
	}

	taskMgr.Wait()
}

func BenchmarkTaskManager_ConcurrentStart(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockLogger := logger.NewMockLogger()
	mockLogger.On("Debug", mock.Anything, mock.Anything).Return()

	taskMgr := NewTaskManager(ctx, mockLogger)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			err := taskMgr.Start(fmt.Sprintf("task%d", i), func() bool { return false })
			if err != nil {
				b.Errorf("failed to start task %d: %v", i, err)
			}
			i++
		}
	})

	taskMgr.Wait()
}

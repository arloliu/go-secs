package hsms

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/go-secs/logger"
)

// TaskFunc represents a function that performs a task within a goroutine managed by the TaskManager.
// It should return true to continue running the task, or false to stop the goroutine.
type TaskFunc func() bool

// TaskRecvFunc represents a function that performs a receive task within a goroutine managed by the TaskManager.
// It should return true to continue running the task, or false to stop the goroutine.
type TaskRecvFunc func(reader *bufio.Reader, msgLenBuf []byte) bool

// TaskCancelFunc represents a function that will be called when a goroutine managed by the TaskManager exits or is canceled.
// It can be used to perform cleanup actions or release resources associated with the goroutine.
type TaskCancelFunc func()

// TaskMsgFunc represents a function that processes an HSMS message within a goroutine managed by the TaskManager.
// It should return true to continue processing messages, or false to stop the goroutine.
type TaskMsgFunc func(msg HSMSMessage) bool

// TaskDataMsgFunc represents a function that processes an HSMS data message and its associated handler within a goroutine
// managed by the TaskManager. It should return true to continue processing messages, or false to stop the goroutine.
type TaskDataMsgFunc func(msg *DataMessage, handler DataMessageHandler) bool

// TaskManager manages the lifecycle of goroutines (tasks) within an HSMS application.
// It provides a structured way to start, stop, and wait for goroutines, ensuring proper
// cancellation and resource cleanup.
//
// The TaskManager uses a context.Context to manage the lifecycle of the goroutines. When the
// context is canceled, all running goroutines are signaled to stop. The TaskManager also uses
// a sync.WaitGroup to wait for all goroutines to terminate before returning from the Wait() method.
//
// Example Usage:
//
//	// Create a new TaskManager and use ctx as the parent context
//	taskMgr := hsms.NewTaskManager(ctx, logger)
//
//	// Start a goroutine
//	taskMgr.Start("myTask", func() bool {
//	    // ... task logic ...
//	    return true // Return true to continue running, false to stop
//	})
//
//	// ... other operations ...
//
//	// Stop all goroutines
//	taskMgr.Stop()
//
//	// Wait for all goroutines to terminate
//	taskMgr.Wait()
type TaskManager struct {
	pctx   context.Context
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	logger logger.Logger
	count  atomic.Int32
}

// NewTaskManager creates a new TaskManager with the given context as the parent context and logger.
func NewTaskManager(ctx context.Context, l logger.Logger) *TaskManager {
	mgr := &TaskManager{pctx: ctx, logger: l}
	mgr.ctx, mgr.cancel = context.WithCancel(ctx)

	return mgr
}

// Start starts a new goroutine with the given name and task function.
//
// The taskFunc should return true to continue running, or false to stop the goroutine.
func (mgr *TaskManager) Start(name string, taskFunc TaskFunc) {
	mgr.count.Add(1)

	mgr.logger.Debug("Start task", "name", name)
	mgr.wg.Add(1)
	go func() {
		defer mgr.wg.Done()
		defer func() {
			mgr.count.Add(-1)
			mgr.logger.Debug(fmt.Sprintf("%s task terminated", name), "task_count", mgr.TaskCount())
		}()

		for {
			select {
			case <-mgr.ctx.Done():
				return
			default:
				if !taskFunc() {
					return
				}
			}
		}
	}()
}

// StartReceiver starts a new goroutine with the given name, task function, and a cancel function.
//
// The taskFunc should return true to continue running, or false to stop the goroutine.
//
// The taskCancelFunc will be called when the goroutine exits or is canceled.
func (mgr *TaskManager) StartReceiver(name string, conn net.Conn, taskFunc TaskRecvFunc, taskCancelFunc TaskCancelFunc) {
	mgr.count.Add(1)

	mgr.logger.Debug("StartReceiver task", "name", name)
	mgr.wg.Add(1)
	go func() {
		if taskCancelFunc != nil {
			defer taskCancelFunc()
		}
		defer mgr.wg.Done()
		defer func() {
			mgr.count.Add(-1)
			mgr.logger.Debug(fmt.Sprintf("%s task terminated", name), "task_count", mgr.TaskCount())
		}()

		reader := bufio.NewReaderSize(conn, 64*1024)
		msgLenBuf := make([]byte, 4)

		for {
			select {
			case <-mgr.ctx.Done():
				return
			default:
				if !taskFunc(reader, msgLenBuf) {
					return
				}
			}
		}
	}()
}

// StartSender starts a new goroutine that receives HSMS messages from the given channel.
//
// The taskFunc should return true to continue receiving messages, or false to stop the goroutine.
func (mgr *TaskManager) StartSender(name string, taskFunc TaskMsgFunc, inputChan chan HSMSMessage) {
	mgr.count.Add(1)

	mgr.logger.Debug("StartSender task", "name", name)
	mgr.wg.Add(1)
	go func() {
		defer mgr.wg.Done()
		defer func() {
			mgr.count.Add(-1)
			mgr.logger.Debug(fmt.Sprintf("%s message task terminated", name), "task_count", mgr.TaskCount())
		}()

		for {
			select {
			case <-mgr.ctx.Done():
				return
			case msg := <-inputChan:
				if !taskFunc(msg) {
					return
				}
			}
		}
	}()
}

// StartRecvDataMsg starts a new goroutine that receives HSMS data messages from the given channel.
//
// The task function (DataMessageHandler) will be called for each received message.
func (mgr *TaskManager) StartRecvDataMsg(name string, taskFunc DataMessageHandler, session Session, inputChan chan *DataMessage) {
	mgr.count.Add(1)

	mgr.logger.Debug("StartRecvDataMsg task", "name", name)
	mgr.wg.Add(1)
	go func() {
		defer mgr.wg.Done()
		defer func() {
			mgr.count.Add(-1)
			mgr.logger.Debug(fmt.Sprintf("%s message task terminated", name), "task_count", mgr.TaskCount())
		}()

		for {
			select {
			case <-mgr.ctx.Done():
				return
			case msg := <-inputChan:
				taskFunc(msg, session)
			}
		}
	}()
}

// StartInterval starts a new goroutine that executes the given task function at the specified interval.
// If runNow is true, the task function is executed immediately before starting the interval.
// The function returns a *time.Ticker that can be used to stop the interval.
func (mgr *TaskManager) StartInterval(name string, taskFunc TaskFunc, interval time.Duration, runNow bool) *time.Ticker {
	ticker := time.NewTicker(interval)

	if runNow && !taskFunc() {
		defer mgr.logger.Debug(fmt.Sprintf("%s interval task terminated", name))
		ticker.Stop()
		return ticker
	}

	mgr.count.Add(1)

	mgr.wg.Add(1)

	go func(ticker *time.Ticker) {
		defer mgr.wg.Done()
		defer func() {
			mgr.count.Add(-1)
			mgr.logger.Debug(fmt.Sprintf("%s interval task terminated", name), "task_count", mgr.TaskCount())
		}()

		for {
			select {
			case <-mgr.ctx.Done():
				ticker.Stop()
				return

			case <-ticker.C:
				mgr.logger.Debug("StartInterval func", "name", name)
				if !taskFunc() {
					ticker.Stop()
					return
				}
			}
		}
	}(ticker)

	return ticker
}

// Stop signals all running goroutines.
func (mgr *TaskManager) Stop() {
	// terminate all tasks
	mgr.cancel()
}

// Wait waits for all goroutines to terminate.
func (mgr *TaskManager) Wait() {
	// wait all tasks be terminated
	mgr.wg.Wait()
	mgr.ctx, mgr.cancel = context.WithCancel(mgr.pctx)
}

// TaskCount returns the number of currently running goroutines.
func (mgr *TaskManager) TaskCount() int {
	return int(mgr.count.Load())
}

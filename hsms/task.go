package hsms

import (
	"context"
	"fmt"
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
type TaskRecvFunc func(msgLenBuf []byte) bool

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
	pctx    context.Context
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	logger  logger.Logger
	count   atomic.Int32
	tickers sync.Map     // map[string]*time.Ticker
	mu      sync.RWMutex // protect ctx and cancel
	taskMu  sync.RWMutex // protect task creation during Wait()
}

// NewTaskManager creates a new TaskManager with the given context as the parent context and logger.
func NewTaskManager(ctx context.Context, l logger.Logger) *TaskManager {
	mgr := &TaskManager{pctx: ctx, logger: l}
	mgr.ctx, mgr.cancel = context.WithCancel(ctx)
	return mgr
}

// getContext safely returns the current context
func (mgr *TaskManager) getContext() context.Context {
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()

	return mgr.ctx
}

// Start starts a new goroutine with the given name and task function.
//
// The taskFunc should return true to continue running, or false to stop the goroutine.
func (mgr *TaskManager) Start(name string, taskFunc TaskFunc) error {
	mgr.logger.Debug("Start task", "name", name)

	starter, err := mgr.newTaskStarter(name)
	if err != nil {
		return err
	}

	starter.startTask(func() {
		mgr.runTaskLoop(taskFunc)
	})

	return starter.waitForStart()
}

// StartReceiver starts a new goroutine with the given name, task function, and a cancel function.
//
// The taskFunc should return true to continue running, or false to stop the goroutine.
//
// The taskCancelFunc will be called when the goroutine exits or is canceled.
// StartReceiver starts a new goroutine with the given name, task function, and a cancel function.
func (mgr *TaskManager) StartReceiver(name string, taskFunc TaskRecvFunc, taskCancelFunc TaskCancelFunc) error {
	mgr.logger.Debug("StartReceiver task", "name", name)

	starter, err := mgr.newTaskStarter(name)
	if err != nil {
		return err
	}

	starter.startTask(func() {
		if taskCancelFunc != nil {
			defer taskCancelFunc()
		}

		msgLenBuf := make([]byte, 4)
		mgr.runTaskLoop(func() bool {
			return taskFunc(msgLenBuf)
		})
	})

	return starter.waitForStart()
}

// StartSender starts a new goroutine that receives HSMS messages from the given channel.
//
// The taskFunc should return true to continue receiving messages, or false to stop the goroutine.
func (mgr *TaskManager) StartSender(name string, taskFunc TaskMsgFunc, taskCancelFunc TaskCancelFunc, inputChan chan HSMSMessage) error {
	mgr.logger.Debug("StartSender task", "name", name)

	if inputChan == nil {
		return fmt.Errorf("input channel is nil")
	}

	starter, err := mgr.newTaskStarter(name)
	if err != nil {
		return err
	}

	starter.startTask(func() {
		if taskCancelFunc != nil {
			defer taskCancelFunc()
		}

		for {
			ctx := mgr.getContext()
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-inputChan:
				if !ok {
					mgr.logger.Debug("input channel closed", "name", name)
					return
				}
				if !taskFunc(msg) {
					return
				}
			}
		}
	})

	return starter.waitForStart()
}

// StartRecvDataMsg starts a new goroutine that receives HSMS data messages from the given channel.
//
// The task function (DataMessageHandler) will be called for each received message.
func (mgr *TaskManager) StartRecvDataMsg(name string, taskFunc DataMessageHandler, session Session, inputChan chan *DataMessage) error {
	mgr.logger.Debug("StartRecvDataMsg task", "name", name)

	if inputChan == nil {
		return fmt.Errorf("input channel is nil")
	}
	if session == nil {
		return fmt.Errorf("session is nil")
	}

	starter, err := mgr.newTaskStarter(name)
	if err != nil {
		return err
	}

	starter.startTask(func() {
		for {
			ctx := mgr.getContext()
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-inputChan:
				if !ok {
					mgr.logger.Debug("data message channel closed", "name", name)
					return
				}
				if msg == nil {
					mgr.logger.Debug("nil message received", "name", name)
					continue
				}

				// call handler with panic protection
				mgr.callWithRecover(name, func() {
					taskFunc(msg, session)
				})
			}
		}
	})

	return starter.waitForStart()
}

// StartInterval starts a new goroutine that executes the given task function at the specified interval.
// If runNow is true, the task function is executed immediately before starting the interval.
// The function returns a *time.Ticker that can be used to stop the interval.
func (mgr *TaskManager) StartInterval(name string, taskFunc TaskFunc, interval time.Duration, runNow bool) (*time.Ticker, error) {
	mgr.logger.Debug("StartInterval task", "name", name, "interval", interval, "runNow", runNow)

	if interval <= 0 {
		return nil, fmt.Errorf("invalid interval: %v", interval)
	}

	ticker := time.NewTicker(interval)

	// store ticker before starting goroutine
	if _, loaded := mgr.tickers.LoadOrStore(name, ticker); loaded {
		ticker.Stop()
		return nil, fmt.Errorf("interval task %s already exists", name)
	}

	// cleanup on any error
	cleanup := func() {
		ticker.Stop()
		mgr.tickers.Delete(name)
	}

	// run immediately if requested
	if runNow {
		if !mgr.callWithRecoverBool(name, taskFunc) {
			cleanup()
			mgr.logger.Debug(fmt.Sprintf("%s interval task terminated by runNow", name))
			return ticker, nil
		}
	}

	starter, err := mgr.newTaskStarter(name)
	if err != nil {
		cleanup()
		return nil, err
	}

	starter.startTask(func() {
		defer cleanup()

		for {
			ctx := mgr.getContext()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				mgr.logger.Debug("execute interval func", "name", name)
				if !mgr.callWithRecoverBool(name, taskFunc) {
					return
				}
			}
		}
	})

	if err := starter.waitForStart(); err != nil {
		cleanup()
		return nil, err
	}

	return ticker, nil
}

// callWithRecover calls a function with panic protection
func (mgr *TaskManager) callWithRecover(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			mgr.logger.Error("panic in task", "name", name, "panic", r)
		}
	}()

	fn()
}

// callWithRecoverBool calls a function that returns bool with panic protection
func (mgr *TaskManager) callWithRecoverBool(name string, fn func() bool) bool {
	defer func() {
		if r := recover(); r != nil {
			mgr.logger.Error("panic in task", "name", name, "panic", r)
		}
	}()

	return fn()
}

// Stop signals all running goroutines.
func (mgr *TaskManager) Stop() {
	// stop all tickers
	mgr.tickers.Range(func(key, value any) bool {
		ticker, ok := value.(*time.Ticker)
		if ok {
			ticker.Stop()
		}

		return true
	})

	// terminate all tasks
	mgr.mu.Lock()
	if mgr.cancel != nil {
		mgr.cancel()
	}
	mgr.mu.Unlock()
}

// StopInterval stops the interval task with the given name.
//
// It returns an error if the task is not found or if the task is not a *time.Ticker.
func (mgr *TaskManager) StopInterval(name string) error {
	if val, ok := mgr.tickers.LoadAndDelete(name); ok {
		ticker, ok := val.(*time.Ticker)
		if ok {
			ticker.Stop()
			return nil
		}

		return fmt.Errorf("ticker %s is not a *time.Ticker", name)
	}

	return fmt.Errorf("ticker %s not found", name)
}

// Wait waits for all goroutines to terminate.
func (mgr *TaskManager) Wait() {
	mgr.taskMu.Lock()
	defer mgr.taskMu.Unlock()

	// wait all tasks be terminated
	mgr.wg.Wait()

	// recreate context with lock
	mgr.mu.Lock()
	mgr.ctx, mgr.cancel = context.WithCancel(mgr.pctx)
	mgr.mu.Unlock()
}

// TaskCount returns the number of currently running goroutines.
func (mgr *TaskManager) TaskCount() int {
	return int(mgr.count.Load())
}

// taskStarter encapsulates common startup logic
type taskStarter struct {
	mgr     *TaskManager
	name    string
	started chan error
}

func (mgr *TaskManager) newTaskStarter(name string) (*taskStarter, error) {
	ctx := mgr.getContext()

	// check if already cancelled
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("task manager already stopped")
	default:
	}

	return &taskStarter{
		mgr:     mgr,
		name:    name,
		started: make(chan error, 1),
	}, nil
}

// startTask runs the common startup sequence for all tasks
func (s *taskStarter) startTask(taskBody func()) {
	s.mgr.taskMu.RLock()
	defer s.mgr.taskMu.RUnlock()

	s.mgr.wg.Add(1)

	go func() {
		defer s.mgr.wg.Done()

		// signal startup status
		func() {
			defer func() {
				if r := recover(); r != nil {
					s.started <- fmt.Errorf("panic during startup: %v", r)
				}
			}()

			s.mgr.count.Add(1)
			s.started <- nil
		}()

		// setup cleanup
		defer func() {
			s.mgr.count.Add(-1)
			s.mgr.logger.Debug(fmt.Sprintf("%s task terminated", s.name), "task_count", s.mgr.TaskCount())
		}()

		// run the actual task body
		taskBody()
	}()
}

// waitForStart waits for the task to start with timeout
func (s *taskStarter) waitForStart() error {
	ctx := s.mgr.getContext()

	select {
	case err := <-s.started:
		if err != nil {
			s.mgr.wg.Done() // compensate for failed start
			return fmt.Errorf("failed to start %s: %w", s.name, err)
		}

		return nil

	case <-time.After(5 * time.Second):
		return fmt.Errorf("timeout waiting for %s to start", s.name)

	case <-ctx.Done():
		return fmt.Errorf("context cancelled while starting %s", s.name)
	}
}

// runTaskLoop runs a task function in a loop with context cancellation
func (mgr *TaskManager) runTaskLoop(taskFunc func() bool) {
	defer func() {
		if r := recover(); r != nil {
			mgr.logger.Error("panic in task loop", "panic", r)
		}
	}()

	for {
		ctx := mgr.getContext()
		select {
		case <-ctx.Done():
			return
		default:
			if !taskFunc() {
				return
			}
		}
	}
}

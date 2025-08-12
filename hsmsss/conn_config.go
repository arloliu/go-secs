package hsmsss

import (
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/logger"
)

// ConnectionConfig represents the configuration parameters for an HSMS-SS (Single Session) connection.
type ConnectionConfig struct {
	mu sync.RWMutex

	// host specifies the host of the remote HSMS-SS device.
	host string

	// port specifies the TCP port number for the HSMS-SS connection.
	port int

	// isEquip indicates whether the HSMS connection is in the equipment role (true) or host role (false).
	// Defaults to false (host role).
	isEquip bool

	// IsActive indicates whether the connection should be established in active (true) or passive (false) mode.
	// Defaults to true (active mode).
	isActive bool

	// autoLinktest indicates whether to send periodic linktest requests automatically to the remote HSMS device.
	// This helps to ensure that the connection remains active and to detect potential communication issues.
	// Defaults to true.
	autoLinktest bool
	// linktestInterval defines the interval between automatic linktest requests.
	// This field is only relevant when autoLinktest is true.
	// Defaults to 10 seconds.
	linktestInterval time.Duration

	// t3Timeout defines the reply timeout (T3) for HSMS messages. It should be between 1 and 600 seconds.
	// It defines the maximum time to wait for a reply after sending a primary message that requires a reply.
	//
	// Defaults to 45 seconds.
	t3Timeout time.Duration

	// t5Timeout defines the connect separation time (T5). It should be between 1 and 240 seconds.
	//
	// Defaults to 10 seconds.
	t5Timeout time.Duration

	// t6Timeout defines the control timeout (T6) for control messages. It should be between 1 and 240 seconds.
	// It defines the maximum time to wait for a control message reply after sending a control message.
	//
	// Defaults to 5 seconds.
	t6Timeout time.Duration

	// t7Timeout defines the not selected timeout (T7). It should be between 1 and 240 seconds.
	// It is used to detect an idle or "zombie" connection that has been established at the TCP/IP level
	// but has not completed the HSMS-level handshake to become active.
	//
	// It starts as soon as the TCP/IP connection is established. It measures how long the connection remains in the NOT SELECTED state.
	//
	// Defaults to 10 seconds.
	t7Timeout time.Duration

	// t8Timeout defines the inter-character timeout (T8). It should be between 1 and 120 seconds.
	// It detects a stall in the transmission of a single, complete HSMS message.
	//
	// When an entity begins receiving an HSMS message, the T8 timer is started (or reset) after each byte is successfully received.
	// If the next byte is not received before the T8 timer expires, the HSMS message is considered incomplete and is discarded.
	// This timeout is used to detect incomplete messages and ensure that the connection remains responsive.
	//
	// If the T8 timer expires, it means there has been an unacceptably long pause during the transmission of one message.
	// The receiving entity assumes the message is incomplete or corrupt and that the communication link has failed.
	// This will lead to the termination of the TCP/IP connection.
	//
	// Defaults to 5 seconds.
	t8Timeout time.Duration

	// connectRemoteTimeout defines the timeout for establishing a connection in active mode.
	// It should be between 1 and 30 seconds.
	// Defaults to 3 seconds.
	//
	// This field is only relevant for active mode.
	connectRemoteTimeout time.Duration

	// acceptConnTimeout defines the timeout for each iteration of accepting a connection in passive mode.
	// It should between 1 and 2 seconds and shorter than closeConnTimeout.
	// Defaults to 1 second.
	//
	// This field is only relevant for passive mode.
	acceptConnTimeout time.Duration

	// closeConnTimeout defines the timeout for closing whole HSMS-SS connection.
	// It should be between 1 and 30 seconds.
	// Defaults to 3 seconds.
	closeConnTimeout time.Duration

	// sendTimeout defines the timeout for sending messages to the remote HSMS device.
	// It should be between 1 and 30 seconds.
	//
	// Defaults to 3 seconds.
	//
	// Added in v1.8.0
	sendTimeout time.Duration

	// senderQueueSize defines the size of the sender queue, which buffers messages before sending them
	// to the remote HSMS device.
	//
	// This option allows you to control the backpressure level for unsent messages.
	// A larger queue size can accommodate bursts of messages but might consume more memory.
	//
	// Defaults to 10.
	senderQueueSize int

	// dataMsgQueueSize defines the size of the data message queue, which buffers received primary messages before
	// invoking registered data message handler by AddDataMessageHandler.
	//
	// Defaults to 10.
	dataMsgQueueSize int

	// validateDataMessage indicates whether to validate the data message before invoking the registered data message handler.
	// If true, the data message will be validated and returns S9Fx messages if validation fails.
	validateDataMessage bool

	// traceTraffic indicates whether to trace the network traffic for debugging purposes.
	// If true, the connection will log detailed information about sent and received messages.
	// This can be useful for debugging and monitoring the HSMS-SS communication.
	//
	// Defaults to false.
	traceTraffic bool

	// logger provides a logger instance for logging HSMS-related events and errors.
	logger logger.Logger
}

// NewConnectionConfig creates a new HSMS-SS connection configuration with the given host, port number, and optional functional options.
//
// It initializes a ConnectionConfig struct with default values and then applies the provided options to customize the configuration.
//
// The host parameter specifies the host of the remote HSMS device.
// The port parameter specifies the TCP port number for the HSMS connection.
//
// The opts parameter is a variadic argument that accepts a list of ConnOption functions to customize the configuration.
// See the documentation for ConnOption and the various WithXXX functions for available configuration options.
//
// Returns a pointer to the initialized ConnectionConfig and an error if any occurred during the configuration process.
func NewConnectionConfig(host string, port int, opts ...ConnOption) (*ConnectionConfig, error) {
	cfg := &ConnectionConfig{
		isEquip:              false,
		isActive:             true,
		autoLinktest:         true,
		linktestInterval:     10 * time.Second,
		t3Timeout:            45 * time.Second,
		t5Timeout:            10 * time.Second,
		t6Timeout:            5 * time.Second,
		t7Timeout:            10 * time.Second,
		t8Timeout:            5 * time.Second,
		connectRemoteTimeout: 3 * time.Second,
		acceptConnTimeout:    1 * time.Second,
		closeConnTimeout:     3 * time.Second,
		sendTimeout:          1 * time.Second,
		senderQueueSize:      10,
		dataMsgQueueSize:     10,
		validateDataMessage:  true,
		traceTraffic:         false,
		logger:               logger.GetLogger(),
	}

	if err := withRemoteHost(host).apply(cfg); err != nil {
		return cfg, err
	}

	if err := withPort(port).apply(cfg); err != nil {
		return cfg, err
	}

	for _, opt := range opts {
		if err := opt.apply(cfg); err != nil {
			return cfg, err
		}
	}

	return cfg, nil
}

// AutoLinktest returns whether the automatic periodic linktest mechanism is enabled.
func (cfg *ConnectionConfig) AutoLinktest() bool {
	cfg.mu.RLock()
	defer cfg.mu.RUnlock()

	return cfg.autoLinktest
}

// LinktestInterval returns the interval between automatic periodic linktest requests.
func (cfg *ConnectionConfig) LinktestInterval() time.Duration {
	cfg.mu.RLock()
	defer cfg.mu.RUnlock()

	return cfg.linktestInterval
}

// T3Timeout returns the reply timeout (T3) for HSMS messages.
//
// Added in v1.11.0.
func (cfg *ConnectionConfig) T3Timeout() time.Duration {
	cfg.mu.RLock()
	defer cfg.mu.RUnlock()

	return cfg.t3Timeout
}

// T5Timeout returns the connect separation time (T5).
//
// Added in v1.11.0.
func (cfg *ConnectionConfig) T5Timeout() time.Duration {
	cfg.mu.RLock()
	defer cfg.mu.RUnlock()

	return cfg.t5Timeout
}

// T6Timeout returns the control timeout (T6) for control messages.
//
// Added in v1.11.0.
func (cfg *ConnectionConfig) T6Timeout() time.Duration {
	cfg.mu.RLock()
	defer cfg.mu.RUnlock()

	return cfg.t6Timeout
}

// T7Timeout returns the not selected timeout (T7).
//
// Added in v1.11.0.
func (cfg *ConnectionConfig) T7Timeout() time.Duration {
	cfg.mu.RLock()
	defer cfg.mu.RUnlock()

	return cfg.t7Timeout
}

// T8Timeout returns the inter-character timeout (T8).
//
// Added in v1.11.0.
func (cfg *ConnectionConfig) T8Timeout() time.Duration {
	cfg.mu.RLock()
	defer cfg.mu.RUnlock()

	return cfg.t8Timeout
}

// ConnOption represents a functional option for configuring a ConnectionConfig.
type ConnOption interface {
	apply(*ConnectionConfig) error
}

type connOptFunc struct {
	name      string
	runtime   bool
	applyFunc func(*ConnectionConfig) error
}

func (c *connOptFunc) apply(cfg *ConnectionConfig) error { return c.applyFunc(cfg) }

func newConnOptFunc(name string, runtime bool, f func(*ConnectionConfig) error) *connOptFunc {
	return &connOptFunc{
		name:      name,
		runtime:   runtime,
		applyFunc: f,
	}
}

// withRemoteHost sets the host for the HSMS-SS connection.
// It returns a ConnOption that validates the host updates the configuration.
// An error is returned if the configuration is nil.
func withRemoteHost(host string) ConnOption {
	return newConnOptFunc("withRemoteHost", false, func(cfg *ConnectionConfig) error {
		if cfg == nil {
			return hsms.ErrConnConfigNil
		}

		// Check if it's a valid IP address
		if ip := net.ParseIP(host); ip != nil {
			cfg.host = host
			return nil
		}

		// If not an IP, check if it's a valid domain name
		host = strings.TrimPrefix(host, ".")
		host = strings.TrimSuffix(host, ".")
		if _, err := net.LookupHost(host); err == nil {
			cfg.host = host
			return nil
		}

		return errors.New("invalid host")
	})
}

// withPort sets the TCP port number for the HSMS-SS connection.
// It returns a ConnOption that validates the port number and updates the configuration.
// An error is returned if the port number is out of the valid range (1-65535) or if the configuration is nil.
func withPort(port int) ConnOption {
	return newConnOptFunc("withPort", false, func(cfg *ConnectionConfig) error {
		if cfg == nil {
			return hsms.ErrConnConfigNil
		}

		if port < 0 || port > 65535 {
			return errors.New("port is out of range [1, 65535]")
		}
		cfg.port = port

		return nil
	})
}

// WithEquipRole sets the HSMS-SS connection as equipment role.
// It returns a ConnOption that updates the configuration to indicate an equipment role.
// An error is returned if the configuration is nil.
//
// The default role is host.
//
// This option can't be changed at runtime.
func WithEquipRole() ConnOption {
	return newConnOptFunc("WithEquipRole", false, func(cfg *ConnectionConfig) error {
		if cfg == nil {
			return hsms.ErrConnConfigNil
		}

		cfg.mu.Lock()
		cfg.isEquip = true
		cfg.mu.Unlock()

		return nil
	})
}

// WithHostRole sets the HSMS-SS connection as host role.
// It returns a ConnOption that updates the configuration to indicate an host role.
// An error is returned if the configuration is nil.
//
// The default role is host.
//
// This option can't be changed at runtime.
func WithHostRole() ConnOption {
	return newConnOptFunc("WithHostRole", false, func(cfg *ConnectionConfig) error {
		if cfg == nil {
			return hsms.ErrConnConfigNil
		}

		cfg.mu.Lock()
		cfg.isEquip = false
		cfg.mu.Unlock()

		return nil
	})
}

// WithActive sets the connection mode to active.
// It returns a ConnOption that updates the configuration to indicate an active connection.
// An error is returned if the configuration is nil.
//
// The default mode is active.
//
// This option can't be changed at runtime.
func WithActive() ConnOption {
	return newConnOptFunc("WithActive", false, func(cfg *ConnectionConfig) error {
		if cfg == nil {
			return hsms.ErrConnConfigNil
		}

		cfg.mu.Lock()
		cfg.isActive = true
		cfg.mu.Unlock()

		return nil
	})
}

// WithPassive sets the connection mode to passive.
// It returns a ConnOption that updates the configuration to indicate a passive connection.
// An error is returned if the configuration is nil.
//
// The default mode is active.
//
// This option can't be changed at runtime.
func WithPassive() ConnOption {
	return newConnOptFunc("WithPassive", false, func(cfg *ConnectionConfig) error {
		if cfg == nil {
			return hsms.ErrConnConfigNil
		}

		cfg.mu.Lock()
		cfg.isActive = false
		cfg.mu.Unlock()

		return nil
	})
}

// WithAutoLinktest enables or disables the automatic periodic linktest mechanism.
//
// When enabled (val = true), the HSMS connection will automatically send linktest requests to the
// remote device at the interval specified by WithLinktestInterval. This helps to ensure that the
// connection remains active and to detect potential communication issues.
//
// When disabled (val = false), no automatic linktest requests will be sent.
//
// An error is returned if the provided ConnectionConfig is nil.
//
// The default value is true.
//
// This option can be changed at runtime.
func WithAutoLinktest(val bool) ConnOption {
	return newConnOptFunc("WithAutoLinktest", true, func(cfg *ConnectionConfig) error {
		if cfg == nil {
			return hsms.ErrConnConfigNil
		}

		cfg.mu.Lock()
		cfg.autoLinktest = val
		cfg.mu.Unlock()

		return nil
	})
}

// WithLinktestInterval sets the interval between automatic periodic linktest requests.
//
// This option defines how often linktest requests are sent to the remote HSMS device when
// AutoLinktest is enabled. The interval should be a positive time.Duration value.
//
// This setting has no effect if autoLinktest is disabled by WithAutoLinktest(false).
//
// An error is returned if the provided ConnectionConfig is nil.
//
// The default value is 10 seconds.
//
// This option can be changed at runtime.
func WithLinktestInterval(interval time.Duration) ConnOption {
	return newConnOptFunc("WithLinktestInterval", true, func(cfg *ConnectionConfig) error {
		if cfg == nil {
			return hsms.ErrConnConfigNil
		}

		if interval <= 0 {
			return errors.New("linktest interval must be positive")
		}

		cfg.mu.Lock()
		cfg.linktestInterval = interval
		cfg.mu.Unlock()

		return nil
	})
}

// WithT3Timeout sets the reply timeout (T3) for HSMS messages.
// It returns a ConnOption that validates the timeout value and updates the configuration.
// An error is returned if the timeout is outside the valid range (1-600 seconds) or if the configuration is nil.
//
// The default value is 45 seconds.
//
// This option can be changed at runtime.
func WithT3Timeout(val time.Duration) ConnOption {
	return newConnOptFunc("WithT3Timeout", true, func(cfg *ConnectionConfig) error {
		if cfg == nil {
			return hsms.ErrConnConfigNil
		}

		if val < 1*time.Second || val > 600*time.Second {
			return errors.New("t3 timeout out of range [1, 600]")
		}

		cfg.mu.Lock()
		cfg.t3Timeout = val
		cfg.mu.Unlock()

		return nil
	})
}

// WithT5Timeout sets the connect separation time (T5).
// It returns a ConnOption that validates the timeout value and updates the configuration.
// An error is returned if the timeout is outside the valid range (0.01-240 seconds) or if the configuration is nil.
//
// The default value is 10 seconds.
//
// This option can be changed at runtime.
func WithT5Timeout(val time.Duration) ConnOption {
	return newConnOptFunc("WithT5Timeout", true, func(cfg *ConnectionConfig) error {
		if cfg == nil {
			return hsms.ErrConnConfigNil
		}

		if val < 10*time.Millisecond || val > 240*time.Second {
			return errors.New("t5 timeout out of range [0.01, 240]")
		}

		cfg.mu.Lock()
		cfg.t5Timeout = val
		cfg.mu.Unlock()

		return nil
	})
}

// WithT6Timeout sets the control timeout (T6) for control messages.
// It returns a ConnOption that validates the timeout value and updates the configuration.
// An error is returned if the timeout is outside the valid range (1-240 seconds) or if the configuration is nil.
//
// The default value is 5 seconds.
//
// This option can be changed at runtime.
func WithT6Timeout(val time.Duration) ConnOption {
	return newConnOptFunc("WithT6Timeout", true, func(cfg *ConnectionConfig) error {
		if cfg == nil {
			return hsms.ErrConnConfigNil
		}

		if val < 1*time.Second || val > 240*time.Second {
			return errors.New("t6 timeout out of range [1, 240]")
		}

		cfg.mu.Lock()
		cfg.t6Timeout = val
		cfg.mu.Unlock()

		return nil
	})
}

// WithT7Timeout sets the not selected timeout (T7).
// It returns a ConnOption that validates the timeout value and updates the configuration.
// An error is returned if the timeout is outside the valid range (1-240 seconds) or if the configuration is nil.
//
// The default value is 10 seconds.
//
// This option can be changed at runtime.
func WithT7Timeout(val time.Duration) ConnOption {
	return newConnOptFunc("WithT7Timeout", true, func(cfg *ConnectionConfig) error {
		if cfg == nil {
			return hsms.ErrConnConfigNil
		}

		if val < 1*time.Second || val > 240*time.Second {
			return errors.New("t7 timeout out of range [1, 240]")
		}

		cfg.mu.Lock()
		cfg.t7Timeout = val
		cfg.mu.Unlock()

		return nil
	})
}

// WithT8Timeout sets the inter-character timeout (T8).
// It returns a ConnOption that validates the timeout value and updates the configuration.
// An error is returned if the timeout is outside the valid range (1-120 seconds) or if the configuration is nil.
//
// The default value is 5 seconds.
//
// This option can be changed at runtime.
func WithT8Timeout(val time.Duration) ConnOption {
	return newConnOptFunc("WithT8Timeout", true, func(cfg *ConnectionConfig) error {
		if cfg == nil {
			return hsms.ErrConnConfigNil
		}

		if val < 1*time.Second || val > 120*time.Second {
			return errors.New("t8 timeout out of range [1, 120]")
		}

		cfg.mu.Lock()
		cfg.t8Timeout = val
		cfg.mu.Unlock()

		return nil
	})
}

// WithConnectRemoteTimeout sets the timeout for establishing a connection in active mode.
// It returns a ConnOption that validates the timeout value and updates the configuration.
// An error is returned if the timeout is outside the valid range (0.1-30 seconds) or if the configuration is nil.
//
// The default value is 1 second.
//
// This option can be changed at runtime.
func WithConnectRemoteTimeout(val time.Duration) ConnOption {
	return newConnOptFunc("WithConnectRemoteTimeout", true, func(cfg *ConnectionConfig) error {
		if cfg == nil {
			return hsms.ErrConnConfigNil
		}

		if val < 100*time.Millisecond || val > 30*time.Second {
			return errors.New("connect remote timeout out of range [1, 30]")
		}

		cfg.mu.Lock()
		cfg.connectRemoteTimeout = val
		cfg.mu.Unlock()

		return nil
	})
}

// WithAcceptConnTimeout sets the timeout for each iteration of accepting a connection in passive mode.
// It returns a ConnOption that validates the timeout value and updates the configuration.
// An error is returned if the timeout is outside the valid range (1-2 seconds) or if the configuration is nil.
//
// The default value is 1 second.
//
// This option can be changed at runtime.
func WithAcceptConnTimeout(val time.Duration) ConnOption {
	return newConnOptFunc("WithAcceptConnTimeout", true, func(cfg *ConnectionConfig) error {
		if cfg == nil {
			return hsms.ErrConnConfigNil
		}

		if val < 1*time.Second || val > 2*time.Second {
			return errors.New("accept connection timeout out of range [1, 2]")
		}

		cfg.mu.Lock()
		cfg.acceptConnTimeout = val
		cfg.mu.Unlock()

		return nil
	})
}

// WithCloseConnTimeout sets the timeout for close a connection in active mode.
// It returns a ConnOption that validates the timeout value and updates the configuration.
// An error is returned if the timeout is outside the valid range (3-30 seconds) or if the configuration is nil.
//
// The default value is 3 seconds.
//
// This option can be changed at runtime.
func WithCloseConnTimeout(val time.Duration) ConnOption {
	return newConnOptFunc("WithCloseConnTimeout", true, func(cfg *ConnectionConfig) error {
		if cfg == nil {
			return hsms.ErrConnConfigNil
		}

		if val < 1*time.Second || val > 30*time.Second {
			return errors.New("accept connection timeout out of range [1, 30]")
		}

		cfg.mu.Lock()
		cfg.closeConnTimeout = val
		cfg.mu.Unlock()

		return nil
	})
}

// WithSendTimeout sets the timeout for sending messages to the remote HSMS device.
// It returns a ConnOption that validates the timeout value and updates the configuration.
// An error is returned if the timeout is outside the valid range (1-30 seconds) or if the configuration is nil.
//
// The default value is 1 second.
//
// This option can be changed at runtime.
//
// Added in v1.8.0
func WithSendTimeout(val time.Duration) ConnOption {
	return newConnOptFunc("WithSendTimeout", true, func(cfg *ConnectionConfig) error {
		if cfg == nil {
			return hsms.ErrConnConfigNil
		}

		if val < 1*time.Second || val > 30*time.Second {
			return errors.New("send timeout out of range [1, 30]")
		}

		cfg.mu.Lock()
		cfg.sendTimeout = val
		cfg.mu.Unlock()

		return nil
	})
}

// WithSenderQueueSize sets the size of the sender queue, which buffers messages before sending them
// to the remote HSMS device.
//
// This option allows you to control the backpressure level for unsent messages.
// A larger queue size can accommodate bursts of messages but might consume more memory.
//
// The queue size must be within the range of 1 to 1000.
// An error is returned if the queue size is invalid or if the provided ConnectionConfig is nil.
//
// The default value is 10.
//
// This option can't be changed at runtime.
func WithSenderQueueSize(size int) ConnOption {
	return newConnOptFunc("WithSenderQueueSize", false, func(cfg *ConnectionConfig) error {
		if cfg == nil {
			return hsms.ErrConnConfigNil
		}
		if size < 1 || size > 1000 {
			return errors.New("the sender queue size out of range [1, 1000]")
		}

		cfg.mu.Lock()
		cfg.senderQueueSize = size
		cfg.mu.Unlock()

		return nil
	})
}

// WithDataMsgQueueSize sets the size of the data message queue, which buffers received primary messages before
// invoking registered data message handler by AddDataMessageHandler.
//
// The queue size must be within the range of 1 to 1000.
// An error is returned if the queue size is invalid or if the provided ConnectionConfig is nil.
//
// The default value is 10.
//
// This option can't be changed at runtime.
func WithDataMsgQueueSize(size int) ConnOption {
	return newConnOptFunc("WithDataMsgQueueSize", false, func(cfg *ConnectionConfig) error {
		if cfg == nil {
			return hsms.ErrConnConfigNil
		}
		if size < 1 || size > 1000 {
			return errors.New("the data message queue size out of range [1, 1000]")
		}

		cfg.mu.Lock()
		cfg.dataMsgQueueSize = size
		cfg.mu.Unlock()

		return nil
	})
}

// WithValidateDataMessage enables or disables the validation of data messages before invoking the registered data message handler.
// When enabled (value = true), the data message will be validated, and if validation fails,
// it will return S9Fx messages to the sender.
func WithValidateDataMessage(value bool) ConnOption {
	return newConnOptFunc("WithValidateDataMessage", true, func(cfg *ConnectionConfig) error {
		if cfg == nil {
			return hsms.ErrConnConfigNil
		}

		cfg.mu.Lock()
		cfg.validateDataMessage = value
		cfg.mu.Unlock()

		return nil
	})
}

// WithTraceTraffic enables or disables the tracing of network traffic for debugging purposes.
// When enabled (value = true), the connection will log detailed information about sent and received messages.
// This can be useful for debugging and monitoring the HSMS-SS communication.
func WithTraceTraffic(value bool) ConnOption {
	return newConnOptFunc("WithTraceTraffic", true, func(cfg *ConnectionConfig) error {
		if cfg == nil {
			return hsms.ErrConnConfigNil
		}

		cfg.mu.Lock()
		cfg.traceTraffic = value
		cfg.mu.Unlock()

		return nil
	})
}

// WithLogger sets the logger for the HSMS-SS connection.
// It returns a ConnOption that updates the configuration with the provided logger.
// An error is returned if the configuration is nil.
//
// The default logger is the global logger instance.
//
// This option can't be changed at runtime.
func WithLogger(l logger.Logger) ConnOption {
	return newConnOptFunc("WithLogger", false, func(cfg *ConnectionConfig) error {
		if cfg == nil {
			return hsms.ErrConnConfigNil
		}

		cfg.mu.Lock()
		cfg.logger = l
		cfg.mu.Unlock()

		return nil
	})
}

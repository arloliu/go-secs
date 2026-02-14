package secs1

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/arloliu/go-secs/logger"
)

// Default timeout values per SEMI E4 Table 4.
const (
	DefaultT1Timeout = 500 * time.Millisecond // Inter-character timeout
	DefaultT2Timeout = 10 * time.Second       // Protocol timeout
	DefaultT3Timeout = 45 * time.Second       // Reply timeout
	DefaultT4Timeout = 45 * time.Second       // Inter-block timeout

	DefaultRetryLimit = 3 // RTY: max retries

	DefaultConnectTimeout = 3 * time.Second // TCP dial timeout (active mode)
	DefaultAcceptTimeout  = 1 * time.Second // Accept deadline per iteration (passive mode)
	DefaultCloseTimeout   = 3 * time.Second
	DefaultSendTimeout    = 3 * time.Second

	DefaultSenderQueueSize  = 10
	DefaultDataMsgQueueSize = 10
)

// Timeout range limits per SEMI E4 Table 4.
const (
	MinT1Timeout = 100 * time.Millisecond
	MaxT1Timeout = 10 * time.Second

	MinT2Timeout = 200 * time.Millisecond
	MaxT2Timeout = 25 * time.Second

	MinT3Timeout = 1 * time.Second
	MaxT3Timeout = 120 * time.Second

	MinT4Timeout = 1 * time.Second
	MaxT4Timeout = 120 * time.Second

	MaxRetryLimit = 31

	MaxDeviceID = 32767 // 15-bit
)

// ConnectionConfig holds all configuration for a SECS-I over TCP/IP connection.
type ConnectionConfig struct {
	host string
	port int

	// deviceID is the 15-bit SECS-I device ID.
	deviceID uint16

	// isActive: true = TCP client (typically Host), false = TCP server (typically Equipment).
	isActive bool

	// isEquip indicates whether this end is Equipment (true) or Host (false).
	//
	// Per SEMI E4 §7.5 and §10.1, the Master/Slave contention role is determined
	// by this setting: Equipment is always Master, Host is always Slave.
	isEquip bool

	// SECS-I protocol timeouts (SEMI E4 Table 4).
	t1Timeout time.Duration
	t2Timeout time.Duration
	t3Timeout time.Duration
	t4Timeout time.Duration

	// retryLimit is RTY: max number of send retries per block.
	retryLimit int

	// TCP-level timeouts.
	connectTimeout time.Duration
	acceptTimeout  time.Duration
	closeTimeout   time.Duration
	sendTimeout    time.Duration

	// Feature toggles.
	duplicateDetection  bool
	validateDataMessage bool // when true and isEquip, send S9Fx on protocol errors

	// Queue sizes.
	senderQueueSize  int
	dataMsgQueueSize int

	logger logger.Logger
}

// NewConnectionConfig creates a new SECS-I connection configuration.
//
// host is the remote (or bind) address. port is the TCP port.
// opts are functional options applied in order; see With* functions.
func NewConnectionConfig(host string, port int, opts ...ConnOption) (*ConnectionConfig, error) {
	cfg := &ConnectionConfig{
		isActive:           true,
		isEquip:            false, // default: Host role (= Slave per SEMI E4 §7.5)
		t1Timeout:          DefaultT1Timeout,
		t2Timeout:          DefaultT2Timeout,
		t3Timeout:          DefaultT3Timeout,
		t4Timeout:          DefaultT4Timeout,
		retryLimit:         DefaultRetryLimit,
		connectTimeout:     DefaultConnectTimeout,
		acceptTimeout:      DefaultAcceptTimeout,
		closeTimeout:       DefaultCloseTimeout,
		sendTimeout:        DefaultSendTimeout,
		duplicateDetection: true,
		senderQueueSize:    DefaultSenderQueueSize,
		dataMsgQueueSize:   DefaultDataMsgQueueSize,
		logger:             logger.GetLogger(),
	}

	if err := cfg.setHost(host); err != nil {
		return nil, err
	}
	if err := cfg.setPort(port); err != nil {
		return nil, err
	}

	for _, opt := range opts {
		if err := opt.apply(cfg); err != nil {
			return nil, err
		}
	}

	return cfg, nil
}

func (cfg *ConnectionConfig) setHost(host string) error {
	if ip := net.ParseIP(host); ip != nil {
		cfg.host = host
		return nil
	}

	host = strings.TrimPrefix(host, ".")
	host = strings.TrimSuffix(host, ".")
	if _, err := net.LookupHost(host); err == nil {
		cfg.host = host
		return nil
	}

	return fmt.Errorf("secs1: invalid host %q", host)
}

func (cfg *ConnectionConfig) setPort(port int) error {
	if port < 0 || port > 65535 {
		return fmt.Errorf("secs1: port %d out of range [0, 65535]", port)
	}
	cfg.port = port

	return nil
}

// --- Getters ---

// Host returns the configured host address.
func (cfg *ConnectionConfig) Host() string { return cfg.host }

// Port returns the configured TCP port.
func (cfg *ConnectionConfig) Port() int { return cfg.port }

// Addr returns "host:port".
func (cfg *ConnectionConfig) Addr() string { return fmt.Sprintf("%s:%d", cfg.host, cfg.port) }

// DeviceID returns the 15-bit device ID.
func (cfg *ConnectionConfig) DeviceID() uint16 { return cfg.deviceID }

// IsActive returns true if the connection is in active (TCP client) mode.
func (cfg *ConnectionConfig) IsActive() bool { return cfg.isActive }

// IsPassive returns true if the connection is in passive (TCP server) mode.
func (cfg *ConnectionConfig) IsPassive() bool { return !cfg.isActive }

// IsEquip returns true if this end is configured as Equipment.
func (cfg *ConnectionConfig) IsEquip() bool { return cfg.isEquip }

// IsHost returns true if this end is configured as Host.
func (cfg *ConnectionConfig) IsHost() bool { return !cfg.isEquip }

// IsMaster returns true if this end uses the Master contention role.
//
// Per SEMI E4 §7.5: "The equipment is designated as the master."
// This is always equivalent to IsEquip() and is not independently configurable.
func (cfg *ConnectionConfig) IsMaster() bool { return cfg.isEquip }

// IsSlave returns true if this end uses the Slave contention role.
//
// Per SEMI E4 §7.5: "The host is designated as the slave."
// This is always equivalent to IsHost() and is not independently configurable.
func (cfg *ConnectionConfig) IsSlave() bool { return !cfg.isEquip }

// T1Timeout returns the inter-character timeout.
func (cfg *ConnectionConfig) T1Timeout() time.Duration { return cfg.t1Timeout }

// T2Timeout returns the protocol timeout.
func (cfg *ConnectionConfig) T2Timeout() time.Duration { return cfg.t2Timeout }

// T3Timeout returns the reply timeout.
func (cfg *ConnectionConfig) T3Timeout() time.Duration { return cfg.t3Timeout }

// T4Timeout returns the inter-block timeout.
func (cfg *ConnectionConfig) T4Timeout() time.Duration { return cfg.t4Timeout }

// RetryLimit returns the maximum number of send retries per block (RTY).
func (cfg *ConnectionConfig) RetryLimit() int { return cfg.retryLimit }

// DuplicateDetection returns whether duplicate block detection is enabled.
func (cfg *ConnectionConfig) DuplicateDetection() bool { return cfg.duplicateDetection }

// ValidateDataMessage returns whether data message validation is enabled.
// When enabled and the connection role is Equipment, the connection will
// automatically send S9Fx error messages (e.g. S9F1, S9F7) on protocol
// violations detected during block assembly.
func (cfg *ConnectionConfig) ValidateDataMessage() bool { return cfg.validateDataMessage }

// GetLogger returns the configured logger.
func (cfg *ConnectionConfig) GetLogger() logger.Logger { return cfg.logger }

// --- ConnOption ---

// ConnOption is a functional option for configuring a ConnectionConfig.
type ConnOption interface {
	apply(*ConnectionConfig) error
}

type connOptFunc func(*ConnectionConfig) error

func (f connOptFunc) apply(cfg *ConnectionConfig) error { return f(cfg) }

// WithActive sets the connection to active mode (TCP client, typically Host).
// This is the default.
func WithActive() ConnOption {
	return connOptFunc(func(cfg *ConnectionConfig) error {
		cfg.isActive = true
		return nil
	})
}

// WithPassive sets the connection to passive mode (TCP server, typically Equipment).
func WithPassive() ConnOption {
	return connOptFunc(func(cfg *ConnectionConfig) error {
		cfg.isActive = false
		return nil
	})
}

// WithEquipRole configures this end as Equipment.
//
// Per SEMI E4 §7.5: Equipment is always the Master in contention resolution.
// This also sets the default TCP mode to passive (TCP server) if not explicitly overridden.
func WithEquipRole() ConnOption {
	return connOptFunc(func(cfg *ConnectionConfig) error {
		cfg.isEquip = true
		return nil
	})
}

// WithHostRole configures this end as Host.
//
// Per SEMI E4 §7.5: Host is always the Slave in contention resolution.
// This is the default role.
func WithHostRole() ConnOption {
	return connOptFunc(func(cfg *ConnectionConfig) error {
		cfg.isEquip = false
		return nil
	})
}

// WithDeviceID sets the 15-bit device ID. Must be in [0, 32767].
func WithDeviceID(id uint16) ConnOption {
	return connOptFunc(func(cfg *ConnectionConfig) error {
		if id > MaxDeviceID {
			return fmt.Errorf("secs1: device ID %d exceeds maximum %d", id, MaxDeviceID)
		}
		cfg.deviceID = id

		return nil
	})
}

// WithT1Timeout sets the inter-character timeout.
// Per SEMI E4 Table 4: 100ms–10s, resolution 100ms.
func WithT1Timeout(d time.Duration) ConnOption {
	return connOptFunc(func(cfg *ConnectionConfig) error {
		if d < MinT1Timeout || d > MaxT1Timeout {
			return fmt.Errorf("secs1: T1 timeout %v out of range [%v, %v]", d, MinT1Timeout, MaxT1Timeout)
		}
		cfg.t1Timeout = d

		return nil
	})
}

// WithT2Timeout sets the protocol timeout.
// Per SEMI E4 Table 4: 200ms–25s, resolution 200ms.
func WithT2Timeout(d time.Duration) ConnOption {
	return connOptFunc(func(cfg *ConnectionConfig) error {
		if d < MinT2Timeout || d > MaxT2Timeout {
			return fmt.Errorf("secs1: T2 timeout %v out of range [%v, %v]", d, MinT2Timeout, MaxT2Timeout)
		}
		cfg.t2Timeout = d

		return nil
	})
}

// WithT3Timeout sets the reply timeout.
// Per SEMI E4 Table 4: 1s–120s, resolution 1s.
func WithT3Timeout(d time.Duration) ConnOption {
	return connOptFunc(func(cfg *ConnectionConfig) error {
		if d < MinT3Timeout || d > MaxT3Timeout {
			return fmt.Errorf("secs1: T3 timeout %v out of range [%v, %v]", d, MinT3Timeout, MaxT3Timeout)
		}
		cfg.t3Timeout = d

		return nil
	})
}

// WithT4Timeout sets the inter-block timeout.
// Per SEMI E4 Table 4: 1s–120s, resolution 1s.
func WithT4Timeout(d time.Duration) ConnOption {
	return connOptFunc(func(cfg *ConnectionConfig) error {
		if d < MinT4Timeout || d > MaxT4Timeout {
			return fmt.Errorf("secs1: T4 timeout %v out of range [%v, %v]", d, MinT4Timeout, MaxT4Timeout)
		}
		cfg.t4Timeout = d

		return nil
	})
}

// WithRetryLimit sets the maximum number of retries per block (RTY).
// Per SEMI E4 Table 4: 0–31.
func WithRetryLimit(n int) ConnOption {
	return connOptFunc(func(cfg *ConnectionConfig) error {
		if n < 0 || n > MaxRetryLimit {
			return fmt.Errorf("secs1: retry limit %d out of range [0, %d]", n, MaxRetryLimit)
		}
		cfg.retryLimit = n

		return nil
	})
}

// WithDuplicateDetection enables or disables duplicate block detection (SEMI E4 §9.4.2).
// Enabled by default.
func WithDuplicateDetection(enabled bool) ConnOption {
	return connOptFunc(func(cfg *ConnectionConfig) error {
		cfg.duplicateDetection = enabled

		return nil
	})
}

// WithValidateDataMessage enables or disables automatic S9Fx error
// reporting during block assembly. When enabled and the connection role
// is Equipment, the connection sends S9F1 (Unrecognized Device ID) on
// device ID mismatch and S9F7 (Illegal Data) on block number mismatch.
// Disabled by default.
func WithValidateDataMessage(enabled bool) ConnOption {
	return connOptFunc(func(cfg *ConnectionConfig) error {
		cfg.validateDataMessage = enabled

		return nil
	})
}

// WithConnectTimeout sets the TCP dial timeout for active mode.
func WithConnectTimeout(d time.Duration) ConnOption {
	return connOptFunc(func(cfg *ConnectionConfig) error {
		if d <= 0 {
			return errors.New("secs1: connect timeout must be positive")
		}
		cfg.connectTimeout = d

		return nil
	})
}

// WithSendTimeout sets the TCP write timeout for sending data.
func WithSendTimeout(d time.Duration) ConnOption {
	return connOptFunc(func(cfg *ConnectionConfig) error {
		if d <= 0 {
			return errors.New("secs1: send timeout must be positive")
		}
		cfg.sendTimeout = d

		return nil
	})
}

// WithLogger sets the logger for the connection.
func WithLogger(l logger.Logger) ConnOption {
	return connOptFunc(func(cfg *ConnectionConfig) error {
		if l == nil {
			return errors.New("secs1: logger must not be nil")
		}
		cfg.logger = l

		return nil
	})
}

// WithSenderQueueSize sets the size of the outgoing message queue.
func WithSenderQueueSize(size int) ConnOption {
	return connOptFunc(func(cfg *ConnectionConfig) error {
		if size < 1 {
			return errors.New("secs1: sender queue size must be >= 1")
		}
		cfg.senderQueueSize = size

		return nil
	})
}

// WithDataMsgQueueSize sets the size of the incoming data message queue.
func WithDataMsgQueueSize(size int) ConnOption {
	return connOptFunc(func(cfg *ConnectionConfig) error {
		if size < 1 {
			return errors.New("secs1: data message queue size must be >= 1")
		}
		cfg.dataMsgQueueSize = size

		return nil
	})
}

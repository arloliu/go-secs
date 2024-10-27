package hsms

import (
	"github.com/arloliu/go-secs/logger"
)

// DataMessageHandler is a function type that represents a handler for processing received HSMS data messages.
//
// The handler function receives two arguments:
//   - msg: The received DataMessage.
//   - session: The Session associated with the received message.
type DataMessageHandler func(*DataMessage, Session)

// Connection defines the interface for a HSMS connection.
type Connection interface {
	// Open establishes the HSMS connection.
	// If waitOpened is true, it blocks until the connection is fully established or an error occurs.
	// If waitOpened is false, it returns immediately after initiating the connection process.
	Open(waitOpened bool) error

	// Close closes the HSMS connection.
	Close() error

	// AddSession creates and adds a new session to the connection with the specified session ID.
	// It returns the newly created Session.
	AddSession(sessionID uint16) Session

	// GetLogger returns the logger associated with the HSMS connection.
	GetLogger() logger.Logger

	// IsSingleSession returns true if the connection is an HSMS-SS (Single Session) connection, false otherwise.
	IsSingleSession() bool

	// IsGeneralSession returns true if the connection is an HSMS-GS (General Session) connection, false otherwise.
	IsGeneralSession() bool
}

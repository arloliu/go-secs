package hsms

import "errors"

var (
	ErrInvalidStreamCode  = errors.New("invalid stream code, should be in range of [0, 127]")
	ErrInvalidWaitBit     = errors.New("invalid wait bit, should be 0 or 1")
	ErrInvalidSystemBytes = errors.New("invalid system bytes, length is not 4")
)

var (
	ErrConnConfigNil  = errors.New("connection config is nil")
	ErrSessionNil     = errors.New("session is nil")
	ErrConnClosed     = errors.New("connection closed")
	ErrSelectFailed   = errors.New("select failed")
	ErrInvalidReqMsg  = errors.New("message is not a valid request/primary message")
	ErrInvalidRspMsg  = errors.New("message is not a valid response/secondary message")
	ErrNotDataMsg     = errors.New("message is not a data message")
	ErrNotControlMsg  = errors.New("message is not a control message")
	ErrNotImplemented = errors.New("unimplemented method")
)

var (
	// ErrInvalidTransition is returned when an attempt is made to transition the connection state to an invalid state.
	ErrInvalidTransition = errors.New("invalid state transition")
	ErrNotSelectedState  = errors.New("current state is not the selected state")
)

var (
	ErrT3Timeout = errors.New("T3 timeout")
	ErrT5Timeout = errors.New("T5 timeout")
	ErrT6Timeout = errors.New("T6 timeout")
	ErrT7Timeout = errors.New("T7 timeout")
	ErrT8Timeout = errors.New("T8 timeout")
)

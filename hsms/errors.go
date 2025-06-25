package hsms

import "errors"

var (
	// ErrInvalidHeaderLength indicates that an invalid header length was provided.
	// The header length should be 10 bytes.
	ErrInvalidHeaderLength = errors.New("invalid header length, should be 10 bytes")

	// ErrInvalidPType indicates that an invalid PType was provided.
	// The PType should be 0 for SECS-II message.
	ErrInvalidPType = errors.New("invalid PType, should be 0 for SECS-II message")

	// ErrInvalidDataMsgSType indicates that an invalid SType was provided.
	// The SType should be 0 for data message.
	ErrInvalidDataMsgSType = errors.New("invalid SType for data message, should be 0")
	// ErrInvalidControlMsgSType indicates that an invalid SType was provided.
	// The SType should be 0 for data message.
	ErrInvalidControlMsgSType = errors.New("invalid SType for control message, should be in range of [1, 9]")

	// ErrInvalidRejectMsg indicates that the message is not a valid reject control message.
	//
	// Added in v1.8.0
	ErrInvalidRejectMsg = errors.New("the message is not a reject control message")
	// ErrInvalidRejectReason indicates that an invalid reject reason was provided.
	//
	// Added in v1.8.0
	ErrInvalidRejectReason = errors.New("invalid reject reason, should be in range of [1, 4]")

	// ErrInvalidStreamCode indicates that an invalid stream code was provided.
	// Valid stream codes are in the range of 0 to 127.
	ErrInvalidStreamCode = errors.New("invalid stream code, should be in range of [0, 127]")

	// ErrInvalidWaitBit indicates that an invalid wait bit value was provided.
	// The wait bit should be either 0 or 1.
	ErrInvalidWaitBit = errors.New("invalid wait bit, should be 0 or 1")

	// ErrInvalidSystemBytes indicates that invalid system bytes were provided.
	// System bytes should be a 4-byte array.
	ErrInvalidSystemBytes = errors.New("invalid system bytes, length is not 4")
)

var (
	// ErrConnConfigNil indicates that a nil ConnectionConfig was provided.
	ErrConnConfigNil = errors.New("connection config is nil")

	// ErrSessionNil indicates that a nil Session was encountered.
	ErrSessionNil = errors.New("session is nil")

	// ErrConnClosed indicates that the connection is closed.
	ErrConnClosed = errors.New("connection closed")

	// ErrSendMsgTimeout indicates that sending a message timed out.
	//
	// Added in v1.8.0
	ErrSendMsgTimeout = errors.New("send message timeout")

	// ErrSelectFailed indicates that the session selection failed.
	ErrSelectFailed = errors.New("select failed")

	// ErrInvalidReqMsg indicates that the message is not a valid request/primary message.
	ErrInvalidReqMsg = errors.New("message is not a valid request/primary message")

	// ErrInvalidRspMsg indicates that the message is not a valid response/secondary message.
	ErrInvalidRspMsg = errors.New("message is not a valid response/secondary message")

	// ErrNotDataMsg indicates that the message is not a data message.
	ErrNotDataMsg = errors.New("message is not a data message")

	// ErrNotControlMsg indicates that the message is not a control message.
	ErrNotControlMsg = errors.New("message is not a control message")
)

var (
	// ErrInvalidTransition is returned when an attempt is made to transition the connection
	// state to an invalid state.
	ErrInvalidTransition = errors.New("invalid state transition")

	// ErrNotSelectedState indicates that the current connection state is not the selected state.
	ErrNotSelectedState = errors.New("current state is not the selected state")
)

var (
	// ErrT3Timeout indicates that a T3 timeout has occurred.
	// This occurs when a reply message is not received within the T3 timeout period after sending a primary message.
	ErrT3Timeout = errors.New("T3 timeout")

	// ErrT5Timeout indicates that a T5 timeout has occurred.
	// This occurs when the connect separation time (T5) has elapsed.
	ErrT5Timeout = errors.New("T5 timeout")

	// ErrT6Timeout indicates that a T6 timeout has occurred.
	// This occurs when a reply to a control message is not received within the T6 timeout period.
	ErrT6Timeout = errors.New("T6 timeout")

	// ErrT7Timeout indicates that a T7 timeout has occurred.
	// This occurs when the equipment fails to transition to the Selected state within the T7 timeout period.
	ErrT7Timeout = errors.New("T7 timeout")

	// ErrT8Timeout indicates that a T8 timeout has occurred.
	// This occurs when the inter-character timeout (T8) has elapsed during message transmission.
	ErrT8Timeout = errors.New("T8 timeout")
)

var (
	// ErrUnrecognizedDeviceID indicates that an unrecognized Device ID was received in a message.
	// This error is typically sent in response to an S9F1 message.
	//
	// Added in v1.9.0
	ErrUnrecognizedDeviceID = errors.New("unrecognized device id")

	// ErrUnrecognizedStreamType indicates that an unrecognized Stream Type was received in a message.
	// This error is typically sent in response to an S9F3 message.
	//
	// Added in v1.9.0
	ErrUnrecognizedStreamType = errors.New("unrecognized stream type")

	// ErrUnrecognizedFunctionType indicates that an unrecognized Function Type wasreceived in a message.
	// This error is typically sent in response to an S9F5 message.
	//
	// Added in v1.9.0
	ErrUnrecognizedFunctionType = errors.New("unrecognized function type")

	// ErrIllegalData indicates that the data is illegal for the Function Type.
	// This error is typically sent in response to an S9F7 message.
	//
	// Added in v1.9.0
	ErrIllegalData = errors.New("illegal data")

	// ErrTransactionTimeout indicates that the equipment failed to process a message
	// from the host within the timeout period.
	// This error is typically sent in response to an S9F9 message.
	//
	// Added in v1.9.0
	ErrTransactionTimeout = errors.New("transaction timeout")

	// ErrDataTooLong indicates that the length of the data in a message exceeds
	// the maximum message length that the equipment can process.
	// This error is typically sent in response to an S9F11 message.
	//
	// Added in v1.9.0
	ErrDataTooLong = errors.New("data too long")

	// ErrConversationTimeout indicates that the host failed to respond to a message
	// within the timeout period.
	// This error is typically sent in response to an S9F13 message.
	//
	// Added in v1.9.0
	ErrConversationTimeout = errors.New("conversation timeout")
)

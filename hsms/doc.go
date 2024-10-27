// Package hsms provides a High-Speed SECS Message Services (HSMS) implementation for communication
// with semiconductor manufacturing equipment according to the SEMI E37 standard.
//
// This package offers functionalities for establishing/decoding HSMS control and data message and defining generic interface
// for HSMS session. It also provides generic connection state manager for handling HSMS state transition.
//
// Message Types:
// The package defines constants representing different types of HSMS messages, categorized by their function:
//   - DataMsgType:  Data message containing SECS-II data.
//   - SelectReqType, SelectRspType:  Session establishment messages.
//   - DeselectReqType, DeselectRspType: Session termination messages.
//   - LinkTestReqType, LinkTestRspType: Link testing messages.
//   - RejectReqType:  Reject message for errors or rejections.
//   - SeparateReqType:  Separate request message for graceful disconnect.
//
// HSMSMessage Interface:
// The HSMSMessage interface extends the SECS2Message interface and provides methods for:
//   - Accessing message attributes (type, session ID, system bytes, header).
//   - Converting between HSMS message types (control and data messages).
//   - Serializing messages to bytes for transmission (`ToBytes()`).
//   - Managing message lifecycle and resource release (`Free()`).
//   - Creating deep copies of messages (`Clone()`).
//
// Stream/Function Quote Customization:
// The package provides functions to customize the quoting of stream and function codes in SML (SECS Message Language)
// representations of HSMS messages:
//   - UseStreamFunctionNoQuote: No quotes around stream and function codes.
//   - UseStreamFunctionSingleQuote: Single quotes (') around stream and function codes.
//   - UseStreamFunctionDoubleQuote: Double quotes (") around stream and function codes.
package hsms

package hsms

import (
	"github.com/arloliu/go-secs/secs2"
)

// Session defines the interface of an HSMS session within an HSMS connection.
// It provides methods for sending and receiving messages, handling data messages, and managing
// connection state change handlers.
type Session interface {
	// RegisterIDFunc registers a function to generate a unique ID for this session.
	RegisterIDFunc(f func() uint16)

	// RegisterSendMessageFunc registers a function to send an HSMS message and wait for its reply.
	RegisterSendMessageFunc(f func(msg HSMSMessage) (HSMSMessage, error))

	// RegisterSendMessageAsyncFunc registers a function to send an HSMS message asynchronously.
	RegisterSendMessageAsyncFunc(f func(msg HSMSMessage) error)

	// ID returns the session ID for this session.
	ID() uint16

	// SendMessage sends an HSMSMessage and waits for its reply.
	//
	// It returns the received reply message and an error if any occurred during sending or receiving.
	SendMessage(msg HSMSMessage) (HSMSMessage, error)

	// SendMessageAsync sends an HSMSMessage asynchronously.
	//
	// It sends the message  and returns immediately after sending,
	// and let user specified data message handler to receive reply if any.
	SendMessageAsync(msg HSMSMessage) error

	// SendMessageSync sends an HSMSMessage synchronously.
	//
	// It sends the message and blocks until it's sent to the connection's underlying transport layer.
	// It returns an error if any occurred during sending.
	//
	// Note: it does not wait for a reply.
	//
	// Added in v1.8.0
	SendMessageSync(msg HSMSMessage) error

	// SendSECS2Message sends a SECS-II message and waits for its reply.
	//
	// It returns the received reply message (as a DataMessage) and an error if any occurred during sending or receiving.
	//
	// Item ownership: implementations take ownership of msg.Item() and will
	// Free it. Callers must not retain a reference to msg.Item() after this
	// call or Free it themselves. Clone before each call if you need to
	// reuse the same item.
	SendSECS2Message(msg secs2.SECS2Message) (*DataMessage, error)

	// SendSECS2MessageAsync sends a SECS-II message asynchronously.
	//
	// It sends the message and returns immediately after sending,
	// and let user specified data message handler to receive reply if any.
	//
	// Item ownership: same contract as [Session.SendSECS2Message].
	SendSECS2MessageAsync(msg secs2.SECS2Message) error

	// SendDataMessage sends an HSMS data message with the specified stream, function, and data item.
	//
	// It waits for a reply if replyExpected is true.
	// It returns the received reply DataMessage if replyExpected is true, nil otherwise,
	// and and an error if any occurred during sending or receiving.
	//
	// Item ownership: implementations take ownership of dataItem and will
	// Free it (on success or on internal send failure). Callers must not
	// retain the reference and must not Free dataItem themselves, regardless
	// of whether the call succeeds or returns an error. If you need to
	// retry with the same logical item, call dataItem.Clone() before each
	// attempt: passing the original would reuse a pointer the library has
	// already returned to the SECS-II item pool and could race with
	// concurrent decoders. The returned reply DataMessage (on success with
	// replyExpected) is owned by the caller; Free it after use.
	SendDataMessage(stream byte, function byte, replyExpected bool, dataItem secs2.Item) (*DataMessage, error)

	// SendDataMessageAsync sends an HSMS data message asynchronously.
	//
	// It sends the message and returns immediately after sending,
	// and let user specified data message handler to receive reply if any.
	//
	// Item ownership: same contract as [Session.SendDataMessage].
	SendDataMessageAsync(stream byte, function byte, replyExpected bool, dataItem secs2.Item) error

	// ReplyDataMessage sends a reply to a previously received data message.
	//
	// It takes the original primary DataMessage and the data item for the reply as arguments.
	// It returns an error if any occurred during sending the reply.
	//
	// It is a wrapper method to reply data message with the corresponding function code of primary message.
	//
	// Item ownership: implementations take ownership of dataItem. Passing
	// primaryMsg.Item() shares the pointer between primaryMsg and the
	// reply — the library will Free that shared pointer when the reply is
	// sent, so any later access to primaryMsg.Item() (including another
	// ReplyDataMessage or Free'ing primaryMsg) is use-after-free. Pass
	// primaryMsg.Item().Clone() if you intend to keep primaryMsg usable
	// after this call.
	ReplyDataMessage(primaryMsg *DataMessage, dataItem secs2.Item) error

	// AddConnStateChangeHandler adds one or more ConnStateChangeHandler
	// functions to be invoked when the connection state changes.
	//
	// Implementations dispatch handlers asynchronously on a dedicated
	// goroutine, so handlers may perform blocking work (including sending
	// messages with the W-bit set). See [ConnStateChangeHandler] for the
	// full contract.
	AddConnStateChangeHandler(handlers ...ConnStateChangeHandler)

	// AddDataMessageHandler adds one or more DataMessageHandler functions to be invoked when a data message is received.
	//
	// It is used to handle data messages asynchronously.
	// The handlers are invoked in the order they are added.
	AddDataMessageHandler(handlers ...DataMessageHandler)
}

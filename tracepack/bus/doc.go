// Package bus is the codec of the bus message, layout 1,
// which carries one record of a durable-bus capture from its producer to the consumers
// (the tracepack storage specification §4, "Bus message" and "Large records",
// in docs/specs/tracepack/ of the go-secs repository).
// It imports no NATS client: it works on header values and bodies, and the caller's client moves them.
//
// # Producer
//
// A producer encodes the capture's descriptor with [tracepack.EncodeDescriptor], for the start message's HeaderDescriptor header,
// and each record with NewRecord and EncodeBody, inline or by claim-check as Inline says;
// Subject, MsgID and PayloadDigest give the subject and header values,
// and CheckStart is its size preflight, which it runs on every message it encodes.
//
// # Consumer
//
// The log service, the consumer that writes the capture's packs, checks each message with CheckMessage:
// ErrMalformed marks a message never written, its seq a seq gap,
// a layout-1 record of a kind layout 1 does not carry included;
// ErrUnsupportedLayout a message of a Tpk-Layout this module does not know, which it defers with an alert, never a seq gap;
// and ErrNotAccepted a start whose descriptor it cannot accept, on which it defers the capture.
// CheckPayload checks a claim-check object once it is fetched (ErrPayloadMismatch),
// and its payload as CheckMessage checks an inline one,
// [tracepack.DecodeDescriptor] gives the producer's values of a start from Record.Descriptor,
// and DeriveRecord sets the values the consumer derives from the payload as received,
// classified by a [tracepack.Classifier] before any masking.
// The consumer then writes the records with the tracepack Writer.
//
// # Errors
//
// A consumer acts on the first of these an error matches:
// ErrMalformed, the seq is a seq gap: terminate the message and alert;
// ErrUnsupportedLayout, defer the message and alert, until a consumer that knows the layout takes it;
// ErrNotAccepted, defer the capture and alert;
// ErrPayloadMismatch, defer the message, or fetch the object again;
// [tracepack.ErrInvalidArgument], a call this package cannot take, such as CheckPayload on an inline record or CheckStart with a max_payload below MinMaxPayload, is the caller's bug;
// [tracepack.ErrInvalidRecord] from DeriveRecord, a classifier's trailing_bytes the record cannot hold, is the classifier's bug;
// any other error may be transient.
// Each sentinel of this package is tested before any cause it wraps:
// an error wrapping ErrMalformed or ErrNotAccepted may also wrap a decoder's [tracepack.ErrCorrupt] or [*tracepack.FieldError],
// and the bus sentinel decides.
//
// Every error this package returns keeps the "tracepack: " prefix of the module's errors.
package bus

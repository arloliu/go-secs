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
// and ErrNotAccepted a start whose descriptor it cannot accept, on which it defers the capture.
// CheckPayload checks a claim-check object once it is fetched (ErrPayloadMismatch),
// and its payload as CheckMessage checks an inline one,
// [tracepack.DecodeDescriptor] gives the producer's values of a start from Record.Descriptor,
// and DeriveRecord sets the values the consumer derives from the payload as received,
// classified by a [tracepack.Classifier] before any masking.
// The consumer then writes the records with the tracepack Writer.
//
// Every error this package returns keeps the "tracepack: " prefix of the module's errors.
package bus

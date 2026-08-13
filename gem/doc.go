// Package gem provides pure value builders and body decoders for SEMI E30 (GEM) role messages.
//
// Every builder returns a [secs2.SECS2Message] that can be sent via Connection.SendSECS2Message on an established HSMS or SECS-I connection.
//
// The transport-agnostic base builder is [secs2.NewMessage]; callers that need stream/function pairs not covered here should use it directly.
//
// Equipment-defined identifiers are passed as [secs2.Item] values so callers control the SECS-II type (ASCII, integer, binary, …) without coupling to gem.
//
// # Decoding a received body
//
// Each builder has a matching decoder that reads a received body back into a result struct:
// [DecodeS1F14] fills an [S1F14Reply], [DecodeS6F11] fills an [S6F11Body].
// A secondary (even) function's result type ends in Reply, a primary (odd) function's in Body,
// and a message with no body carries no decoder.
//
// A decoder takes the message body as a [secs2.Item] — the value SECS2Message.Item returns — rather than a message,
// so gem stays independent of any transport package.
// The result struct's fields mirror the builder's parameters in order, under the E5 data item names,
// so a body built by [S1F14] decodes through [DecodeS1F14] into the same three values.
//
// # The strict-shape contract
//
// A decoder accepts exactly the body shape its result type's Body: line documents, and nothing else.
// A list of the wrong length, an item of the wrong SECS-II type, a value too wide for the E5 data item's declared width, or a missing position all return an error naming the E5 field or the body position that failed.
// A mismatch is never reported as a zero value with a nil error.
//
// Three shapes are deliberately permissive:
//
//   - A repeated group (an SVID list, a report list) accepts any number of elements, including none.
//   - A group SEMI E5 marks optional — an error code and its text, a set of limit attributes — decodes in either its full or its omitted form,
//     and the omitted fields keep their zero value.
//   - A numeric field accepts any width of its own family:
//     a U8 item decodes into a field the standard declares U4, an I1 into an I4, an F4 into an F8.
//     The value must still fit the declared width, so nothing is ever truncated —
//     a U8 item carrying more than a uint32 can hold is an error, not a wrapped value.
//
// Fields whose SECS-II type is equipment-defined stay [secs2.Item]:
// the decoder returns them unexamined rather than guessing a type the standard leaves open.
//
// Equipment that pads or extends a body beyond the standard shape will not decode here.
// That case is served by the manual path, [secs2.NewCursor] over the body item, which reads only the positions a caller asks for.
package gem

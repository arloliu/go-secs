// Package gem provides pure value builders and body decoders for SEMI E30 (GEM) role messages.
//
// It covers the messages SEMI E5 defines in streams 1 to 10 and 12 to 19 (E5 defines no stream 11).
// The builders, decoders, and item constants are generated from a transcription of SEMI E5 kept as YAML
// in tools/gemgen/data of the go-secs repository.
//
// Every builder returns a [secs2.SECS2Message] that can be sent via Connection.SendSECS2Message on an established HSMS or SECS-I connection.
//
// The transport-agnostic base builder is [secs2.NewMessage]; callers that need stream/function pairs not covered here should use it directly.
//
// Equipment-defined identifiers are passed as [secs2.Item] values so callers control the SECS-II type (ASCII, integer, binary, …) without coupling to gem.
//
// # Item values
//
// An E5 data item with defined values and a single SECS-II format has a named type and typed constants:
// [S1F14] takes a [COMMACK] such as [COMMACKAccepted],
// and [S3F17] takes a [CARRIERACTION] such as [CARRIERACTIONProceedWithCarrier].
// The matching decoder field has the same type.
// An item the equipment may send in several formats keeps [secs2.Item] as its parameter and field type,
// and its defined values are untyped constants passed to the secs2 constructor of the chosen format,
// as in secs2.A([OBJTYPECarrier]).
// The values come from SEMI E5 and, for the GEM300 vocabularies (carrier actions, process job commands, object types, and the like),
// from the standard that defines them; a constant from another standard than E5 names it in its doc.
//
// # Short forms
//
// Where SEMI E5 names a shorter form of a message, a second builder with a descriptive suffix sends it:
// [S16F28Success] sends the S16F28 acknowledge with an empty error list.
// A short form has no decoder of its own;
// the main decoder of the message, here [DecodeS16F28], decodes both forms.
//
// # Decoding a received body
//
// Each message's main builder has a matching decoder that reads a received body back into a result struct:
// [DecodeS1F14] fills an [S1F14Reply], [DecodeS6F11] fills an [S6F11Body].
// A secondary (even) function's result type ends in Reply, a primary (odd) function's in Body,
// and a message with no body carries no decoder.
//
// A decoder takes the message body as a [secs2.Item] — the value SECS2Message.Item returns — rather than a message,
// so gem stays independent of any transport package.
// The result struct's fields mirror the builder's parameters in order, under the E5 data item names,
// so a body built by [S1F14] decodes through [DecodeS1F14] into the same three values.
// An item SEMI E5 lets be sent zero-length, such as the LINKID of S6F25,
// is a pointer both ways, and the doc of its builder and result type says so:
// [S6F25] sends a nil linkid as a zero-length item,
// and [DecodeS6F25] decodes a zero-length LINKID as nil.
//
// A repeated group of one fixed-format item is a slice of values both ways:
// [S18F4] takes its statuses as strings, and [S18F4Reply.STATUSES] holds them so.
//
// # The strict-shape contract
//
// A decoder accepts exactly the body shape its result type's Body: line documents, and nothing else.
// A list of the wrong length, an item of the wrong SECS-II type, a value too wide for the E5 data item's declared width, or a missing position all return an error naming the E5 field or the body position that failed.
// A mismatch is never reported as a zero value with a nil error.
//
// Four shapes are deliberately permissive:
//
//   - A repeated group (an SVID list, a report list) accepts any number of elements, including none.
//     An empty group decodes as an empty slice; test it with len, not against nil.
//   - A group SEMI E5 marks optional — an error code and its text, a set of limit attributes — decodes in either its full or its omitted form,
//     and the omitted fields keep their zero value.
//     Where E5 lets a whole body be sent zero-length, as for S6F16, an empty list decodes with every field zero.
//   - An item SEMI E5 lets be sent zero-length decodes as a nil pointer, as described above.
//   - A numeric field accepts any width of its own family:
//     a U8 item decodes into a field the standard declares U4, an I1 into an I4, an F4 into an F8.
//     The value must still fit the declared width, so nothing is ever truncated —
//     a U8 item carrying more than a uint32 can hold is an error, not a wrapped value.
//
// Fields whose SECS-II type is equipment-defined stay [secs2.Item]:
// the decoder returns them as they arrived rather than guessing a type the standard leaves open.
// It rejects only a list where the item's E5 formats exclude one,
// and accepts a format E5 does not list, such as an ASCII ALID.
//
// Equipment that pads or extends a body beyond the standard shape will not decode here.
// That case is served by the manual path, [secs2.NewCursor] over the body item, which reads only the positions a caller asks for.
package gem

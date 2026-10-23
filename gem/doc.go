// Package gem provides pure value builders and body decoders for SEMI E30 (GEM) role messages.
//
// It covers the messages SEMI E5 defines in streams 1 to 10 and 12 to 19 (E5 defines no stream 11).
// The builders, decoders, groups, item constants, and catalog are generated from a transcription of SEMI E5 kept as YAML in tools/gemgen/data of the go-secs repository.
//
// Every builder returns a [secs2.SECS2Message] that can be sent via Connection.SendSECS2Message on an established HSMS or SECS-I connection.
//
// The transport-agnostic base builder is [secs2.NewMessage]; callers that need stream/function pairs not covered here should use it directly.
//
// Equipment-defined identifiers are passed as [secs2.Item] values so callers control the SECS-II type (ASCII, integer, binary, …) without coupling to gem.
//
// Set every [secs2.Item] argument and field a builder or a group takes.
// A nil item, a typed nil such as (*secs2.ListItem)(nil) included,
// makes the message carry an error wrapping [ErrNilItem] that names where,
// such as "gem: nil item at Report.VALUES[1]",
// so sending it fails instead of sending a body of the wrong shape.
// The error surfaces from Error() on the message's item,
// and hsms.NewDataMessage rejects the message with it.
// A pointer to a group whose fields are all nil is no exception:
// &Deadband{} carries that error, and nil, not &Deadband{}, sends L[0].
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
// The alarm code [ALCD] combines a category with a set bit:
// [S5F1] takes gem.ALCDAlarmSet|gem.ALCDPersonalSafety,
// and [ALCD.Category] and [ALCD.Set] read the two parts of a received code.
// [PFCD], the predefined form code of S6F9, is a named type without constants, since E5 defines no values for it.
//
// Every named type with numeric constants has a String method that returns the constant's name,
// such as "COMMACKAccepted", and "COMMACK(5)" for a value with no constant,
// so %v and %s print the name, %q quotes it, and %x and %X hex-encode it;
// format with %d, or convert with int(v), for the number.
// [ALCD.String] joins the set bit and the category, as in "ALCDAlarmSet|ALCDPersonalSafety".
//
// # Time values
//
// The TIME, TIMESTAMP, and STIME data items and the CLOCK variable are strings in builders and result structs alike.
// The TimeFormat equipment constant picks their form (SEMI E5 Table 4):
// [TimeFormat12] YYMMDDhhmmss, [TimeFormat16] YYYYMMDDhhmmsscc, or [TimeFormatExtended] YYYY-MM-DDThh:mm:ss.sTZD.
// [FormatTime] writes a [time.Time] in the form the constant selects, as for the TIME of [S2F31],
// and [ParseTime] reads any of the three forms back, reporting which one it found.
//
//	s, err := gem.FormatTime(time.Now(), gem.TimeFormat16)
//	// ...
//	t, format, err := gem.ParseTime(reply.TIME, time.Now())
//
// A 12-byte time names no century, so ParseTime takes a reference time and picks the year nearest to it;
// the 12- and 16-byte forms name no zone either, so their time is in the reference's Location.
// Parsing is strict E5 and checks the shape and the field ranges only, never whether the time is plausible.
//
// A CLOCK value reported in an S6F11 arrives as a [secs2.Item] in [Report.VALUES],
// so take its ASCII value at the position the report defines before calling ParseTime.
// An empty STIME, which E5 lets S6F1 send when the time is to be derived from SMPLN,
// and an empty TIME in S2F18, which means no time exists, are the caller's cases to handle:
// ParseTime rejects an empty string.
//
// # Short forms
//
// Where SEMI E5 names a shorter form of a message, a second builder with a descriptive suffix sends it:
// [S16F28Success] sends the S16F28 acknowledge with an empty error list.
// A short form has no decoder of its own;
// the main decoder of the message, here [DecodeS16F28], decodes both forms.
//
// # Groups
//
// Where a message repeats a list, each element is a named struct, a group,
// shared by every message that repeats a list of the same shape:
// [Report] holds the RPTID and the values of one report,
// [S6F11] takes the reports as Report values, and [DecodeS6F11] returns them so.
// A group's doc gives its shape, and its fields follow the result-struct rules below.
// Its Item method builds its list, for a body assembled by hand with [secs2.NewMessage].
//
//	msg := gem.S6F11(dataid, ceid,
//		gem.Report{RPTID: secs2.U4(7), VALUES: []secs2.Item{secs2.A("IDLE"), secs2.U4(42)}},
//		gem.Report{RPTID: secs2.U4(8)},
//	)
//
// A repeat whose elements are themselves repeats is a slice of slices:
// [S6F27] takes one []Report per trace sample.
// A repeat of one fixed-format item is a slice of values both ways:
// [S18F4] takes its statuses as strings, and [S18F4Reply.STATUSES] holds them so.
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
//
// An item SEMI E5 lets be sent zero-length, such as the LINKID of S6F25,
// is a pointer both ways, and the doc of its builder and result type says so:
// [S6F25] sends a nil linkid as a zero-length item,
// and [DecodeS6F25] decodes a zero-length LINKID as nil.
// A pointer to a zero value, such as new(uint32(0)), sends and decodes as that value.
// A group's field follows the same rule,
// and a list E5 lets be sent as L[0] inside a group, or as a repeat's element, is a pointer to its group:
// [Limit.DEADBAND] is a *[Deadband], nil for L[0],
// and [S15F24] takes each recipe's descriptors as []*RecipeDescriptor.
//
// # The strict-shape contract
//
// With no options, a decoder accepts exactly the body shape its result type's Body: line documents, and nothing else.
// A list of the wrong length, an item of the wrong SECS-II type, a value too wide for the E5 data item's declared width, or a missing position all return an error
// naming its path from the body, such as REPORTS[1].VIDS.
// A mismatch is never reported as a zero value with a nil error.
// Every element of a repeat is checked at full depth, groups inside groups included.
//
// Four shapes are deliberately permissive:
//
//   - A repeat (an SVID list, a report list) accepts any number of elements, including none.
//     An empty repeat decodes as an empty slice; test it with len, not against nil.
//   - A list SEMI E5 lets be sent short, such as the error code and text of S5F14, decodes in either its full or its short form.
//     In a body, the omitted fields keep their zero value;
//     where a pointer to a group stands for the list, L[0] decodes as nil, as described above.
//     Where E5 lets a whole body be sent as an empty list, as for S6F16, L[0] decodes with every field zero.
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
// [DecodeS2F14] alone accepts an empty list as a constant value,
// which E5 defines there to mean the constant does not exist.
//
// # Decode options
//
// Every decoder takes options after the body item.
// [AllowTrailing] accepts a list holding more items than E5 declares, in the body or in a group,
// for equipment that appends its own items to a standard body;
// the decoder reads the declared items and ignores the rest.
//
//	body, err := gem.DecodeS5F1(msg.Item(), gem.AllowTrailing())
//
// It relaxes only the list length: a list holding fewer items, an item of the wrong format,
// and a list where E5 excludes one are still errors.
// A body that departs from E5 in another way will not decode here.
// That case is served by the manual path, [secs2.NewCursor] over the body item, which reads only the positions a caller asks for.
//
// # Catalog
//
// [Lookup] describes a message by its stream, its function, and the [Role] of its sender:
// its E5 name and mnemonic, its [Direction], its reply bit and reply function,
// and a Decode function that decodes a received body into the message's result type,
// so a receiver needs no switch over stream and function.
// The role picks the form where the host and the equipment send different bodies, as for S1F13.
// Lookup does not check the direction:
// compare [MessageInfo.Direction] with the sender, for instance before answering with [S9F5].
//
//	info, ok := gem.Lookup(msg.StreamCode(), msg.FunctionCode(), gem.RoleEquipment)
//	if ok && info.Decode != nil {
//		body, err := info.Decode(msg.Item(), gem.AllowTrailing())
//		// body holds the form's result type, such as an S6F11Body.
//	}
//
// [AcceptReply] builds the reply that accepts a primary as a whole,
// such as S6F12 with [ACKC6Accepted] for an event report.
// The reply asserts what E5 says its acknowledge value means, which for some messages is more than receipt:
// HCACK 0 in S2F42 says the command has been performed.
// Where completion comes later, build the reply with the message's builder instead.
//
// # Refusing a message
//
// The equipment's control state comes first (SEMI E30, OFF-LINE state).
// While OFF-LINE, the equipment answers every primary from the host except S1F13 and S1F17 with SxF0,
// the [Abort] message of the primary's stream,
// and it sends Stream 9 errors only about those two messages.
// A primary without the W-bit gets no SxF0:
// function 0 is sent in lieu of an expected reply (E5 §10.4.1),
// and this package reads E30's "any primary message" within that limit.
//
// A message the equipment cannot process is a message fault, answered on Stream 9 (E5 §8.3, §10.13).
// The Stream 9 error primaries, the odd functions S9F1 through S9F13, go from the equipment to the host;
// only S9F0, like every function 0, goes both ways.
// The equipment answers an unrecognized stream with [S9F3],
// an unrecognized function with [S9F5], including a message E5 defines only in the other direction,
// illegal data with [S9F7], and more data than it can handle with [S9F11].
// While ON-LINE, E30 allows SxF0 only to close the open transaction of a message fault,
// and discourages even that (E30, ON-LINE state);
// the equipment may then close the faulty message's W-bit transaction with hsms.ReplyAbort.
//
// "Unrecognized" is relative to the messages the equipment supports, which its documentation lists;
// any other message it receives causes an error message (E5 §11.2.2).
// [Lookup] says whether E5 defines a message, not whether this equipment supports it.
//
// The MHEAD of S9F1, S9F3, S9F5, S9F7, and S9F11 is the header of the message in error.
// Under HSMS it is the received message's 10-byte header, hsms.DataMessage.HeaderBytes (E37 §9.4.2.1).
// Under SECS-I it is the header of the offending block, which a received hsms.DataMessage does not carry:
// its header holds zero in bytes 4 and 5, where a block header carries the E-bit and block number, and it has no R-bit.
// The secs1 package sends the block-level S9F1 and S9F7 itself, in the equipment role.
//
// The host has no Stream 9 error to send, so it refuses a message only with hsms.ReplyAbort.
// E30 names one such refusal: the host denies the equipment's request to go ON-LINE by answering S1F1 with S1F0.
// This package advises the host to keep to the equipment's ON-LINE limit on SxF0 as well.
//
// An unknown ID inside a well-formed message, such as a CEID that does not exist, is no message fault:
// the message's own acknowledge reports it, as [DRACK], [LRACK], [ERACK], and [HCACK] do, rather than Stream 9.
package gem

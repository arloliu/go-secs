# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

A connection can end a transaction early from either side,
and a `gem` builder fails the send instead of sending a body of the wrong shape.
`gem` also converts E5 time strings and prints its named item values by name.

- `hsms.ReplyAbort` answers a received primary with SxF0, the Abort Transaction message (SEMI E5 §10.4.1),
  so the sender need not wait out its reply timer.
- `hsms.WithAbortReplyError` makes a send answered with SxF0 return `hsms.ErrTransactionAborted`,
  and `hsms.WithS9Correlation` ends a send the peer answered with a Stream 9 error message at once, with an `*hsms.S9Error`,
  instead of waiting out T3.
  Both are off by default.
- `hsms.WithEquipmentRole` tells the connection core which side it plays,
  and only the equipment answers a session-ID mismatch with S9F1,
  since Stream 9 error messages travel from equipment to host (SEMI E5 §10.13).
- A nil `secs2.Item` passed to a `gem` builder or set in a group field fails the send and names where it was,
  instead of vanishing from the list that held it.
  `secs2.NewErrorItem` is the item that carries such an error to the send.
- `gem.FormatTime` and `gem.ParseTime` convert between `time.Time` and the three TIME forms of SEMI E5 Table 4.
- The named `gem` item types, such as `gem.COMMACK`, print their constant's name.

The public API only gains additions.
Several behavior changes need a look before upgrading; see Upgrade notes.

### Upgrade notes

- `gem`: a nil `secs2.Item` passed to a builder or set in a group field, a typed nil such as `(*secs2.ListItem)(nil)` included,
  now makes the message carry an error wrapping `gem.ErrNilItem` that names where,
  such as `gem: nil item at S6F11 CEID`,
  and `SendSECS2Message` returns that error without sending anything.
  Before, the result depended on the kind of nil and where it sat.
  `secs2.L` skipped a nil interface,
  so the message went out with a shorter list than E5 defines, and no error.
  A typed nil inside a list already failed the send,
  but with `secs2: list contains nil item`, which named no location;
  it now fails with the `gem.ErrNilItem` error above.
  A nil of either kind passed as the whole body became an empty body (see the `S1F6` bullet below).
- `gem`: `&Deadband{}`, a deadband whose fields are all nil, now carries that error too.
  Set `Limit.DEADBAND` to nil to send the empty list `L[0]`.
- `gem`: `S1F6`, `S1F8`, `S2F10`, and `S5F5` called with a nil item used to send the message header-only;
  they now carry the error.
  Pass a zero-length item, such as `secs2.L()` or `secs2.A("")`, for the zero-length form E5 defines.
- `gem`: `S7F6`, `S7F8`, and `S7F36` called with nil for every item used to send the zero-length list `L[0]`;
  that call now carries the error.
  Call `S7F6Denied`, `S7F8NoSuchMID`, or `S7F36NoSuchMID`, which send the same bytes.
- `hsms`, `hsmsss`: with `hsms.WithSessionIDValidation` on, a connection in the host role, the default,
  still drops a message whose session ID is not its own,
  but no longer answers it with S9F1.
  An equipment connection that relied on that S9F1 without a role preset adds `hsmsss.WithEquipRole` or `hsms.WithEquipmentRole(true)`.
  The presets `hsmsss.WithEquipRole` and `secs1.WithEquipment` set the flag, so a connection built with one is unchanged.
  SECS-I connections behave as before:
  the block assembler drops a block with a foreign device ID before this check runs.
- `hsmsss`: `Config.IsEquip` reports the role flag that `WithEquipRole`, `WithHostRole`, and `hsms.WithEquipmentRole` set,
  instead of whether the auto S9F9 is on.
  A config that sets only `WithConnectionOption(hsms.WithAutoS9F9(true))`, without a role preset,
  now reports `IsEquip() == false` and, with session-ID validation on, sends no S9F1;
  add `WithEquipRole` to keep both.
  A later `WithConnectionOption(hsms.WithAutoS9F9(false))` no longer turns an equipment config into a host one.
- `secs2`: `NewItemError` given an error that wraps an `*ItemError`, such as `fmt.Errorf("field: %w", itemErr)`,
  now keeps that error whole.
  Before, it dropped the outermost layer:
  the result read as `itemErr`'s text without `field: `, and `errors.Is` did not reach the wrapper.
  Code that compares the text of such an error sees the added context.

- `gem`: the named numeric item types, such as `COMMACK`, `HCACK`, and `ALCD`, now have a `String` method,
  so `%v`, `%s`, `%q`, `%x`, and `%X` print the constant's name, such as `COMMACKAccepted`, instead of the number,
  and `COMMACK(5)` for a value with no constant.
  `%x` and `%X` hex-encode that name.
  `%d`, `%#v`, integer conversions, JSON, and the wire encoding are unchanged.
  Code that parses or compares the text these verbs produce should use `%d` or `int(v)` for the number,
  and `%x` with `byte(v)` for hexadecimal.

### Added

- `hsms`: `ReplyAbort(ctx, ep, primary)` answers primary with SxF0:
  the same stream and System Bytes, function 0, no W-bit, and no body.
  It is sent like a reply from `ReplyDataMessage`, bound to the generation that delivered primary,
  so a primary whose generation has ended returns `ErrConnClosed` instead of reaching a reconnected peer.
  hsmsss and secs1 users call `hsms.ReplyAbort(ctx, conn, primary)`;
  their `Connection` interfaces are unchanged.
- `hsms`: `AbortReplier`, the interface `ReplyAbort` asserts the endpoint to.
  Every connection `hsmsss.New`, `secs1.New`, and `hsms.NewConnection` return implements it,
  and so does the `SECS2Endpoint` a handler receives.
- `hsms`: `ErrNoReplyExpected`, which `ReplyAbort` returns without sending anything for a primary without the W-bit,
  and `ErrAbortUnsupported`, which it returns for an endpoint that does not implement `AbortReplier`.
  `ErrAbortUnsupported` wraps `errors.ErrUnsupported`.
- `hsmstest`: `FakeEndpoint.ReplyAbort` records the SxF0 as the session builds it, under the method name `ReplyAbort`,
  so a handler that refuses primaries can be tested against the fake.
- `hsms`: `WithAbortReplyError(true)` makes `SendDataMessage` and `SendSECS2Message` return `(reply, ErrTransactionAborted)`
  when the peer answers with SxF0, the SxF0 message included.
  The abort takes precedence over a body that fails to decode, and a peer Reject.req still returns its `*RejectError`.
  A send reads the setting once, when it starts waiting for its reply.
- `hsms`: `WithS9Correlation(true)` ends a send still waiting for its reply
  when the peer answers it with S9F1, S9F3, S9F5, S9F7, or S9F11 whose MHEAD body identifies it:
  `SendDataMessage` and `SendSECS2Message` return `(nil, *S9Error)`,
  and `S9Error` carries the S9 function, the MHEAD, and the S9 message.
  Over HSMS all 10 MHEAD bytes must equal the header the message was sent with;
  over SECS-I the R-bit, E-bit, and block number are ignored.
  The S9 still reaches the data handlers,
  and a genuine reply that arrives after it reaches them as an orphan secondary.
  S9F9, S9F13, and an S9 whose body is not a 10-byte binary item end no send.
- `hsms`: the `TxOutcome` values `TxAborted` and `TxPeerError`, appended after the existing ones,
  which the transaction observer reports for a send ended by either option.
  Neither error is transient or a timeout, no auto S9F9 follows, and no metric counts either as an error.
- `hsms`: `WithEquipmentRole` and `ConnectionConfig.EquipmentRole`,
  the role flag that gates the notifications the connection core sends on its own.
  `hsmsss.WithEquipRole`, `hsmsss.WithHostRole`, `secs1.WithEquipment`, and `secs1.WithHost` set it alongside `WithAutoS9F9`;
  each flag keeps the last value applied to it.
- `gem`: `Abort(stream)` builds the header-only SxF0 of a stream,
  for a relay that frames its own messages and sends them with `ForwardDataMessage`.
  The package documentation gains a section on when to refuse a message with SxF0 or with Stream 9.
- `gem`: `ErrNilItem`, which the error a nil item carries wraps.
  A nil item in a repeat names its index, as in `gem: nil item at Report.VALUES[1]` or `gem: nil item at S1F20 OBJECTS[1][1]`.
  The field is named by its decode label,
  so `ProcessJob.material` names the same field in a send error and in a decode error.
- `gem`: `S7F6Denied`, `S7F8NoSuchMID`, and `S7F36NoSuchMID`, the zero-length replies E5 names,
  decoded by the main decoder of each message.
- `gem`: `FormatTime(t, format, opts...)` and `ParseTime(s, ref)` convert between `time.Time` and the TIME, TIMESTAMP, STIME, and CLOCK strings.
  They use the forms of the TimeFormat equipment constant (SEMI E5 Table 4):
  `TimeFormat12` (`YYMMDDhhmmss`), `TimeFormat16` (`YYYYMMDDhhmmsscc`), and `TimeFormatExtended` (`YYYY-MM-DDThh:mm:ss.sTZD`, SEMI E148).
  `ParseTime` detects the form and returns it.
  A 12-byte time takes its century from `ref`, the year nearest to it,
  and the 12- and 16-byte forms take `ref`'s Location.
  Parsing is strict E5 and checks the shape and field ranges only.
  Output is truncated, never rounded, and the extended form has 3 fraction digits unless `WithFractionDigits` picks 1 to 6.
  Every error wraps `ErrInvalidTime`.
- `gem`: a `String` method on every named item type with numeric constants, and on `TimeFormat` (see Upgrade notes).
- `secs2`: `NewErrorItem(err)` returns an item that carries err,
  so `hsms.NewDataMessage` rejects any body that contains it.
  Its `Error()`, and that of every list holding it, reports err's text unchanged, and `errors.Is` reaches err.

### Changed

- `gem`: every builder and group checks each `secs2.Item` it takes and replaces a nil one with an error item (see Upgrade notes).
  When nothing is nil, the caller's slice is passed on as is, with no allocation;
  otherwise it is copied, never modified.
- `hsms`: with `WithSessionIDValidation` on, only an equipment-role connection answers a session-ID mismatch with S9F1 (see Upgrade notes).
- `hsmsss`: `Config.IsEquip` reports `EquipmentRole` instead of `AutoS9F9` (see Upgrade notes).
- `gem`: the named numeric item types print their constant's name with `%v` and the other string verbs (see Upgrade notes).

### Fixed

- `secs2`: `NewItemError` no longer panics on an `errors.Join` that holds an `*ItemError`.
  It flattens only a direct `*ItemError` and keeps any other error whole,
  so a contextual wrapper such as `fmt.Errorf("field: %w", err)` keeps its text and its identity.

## [2.7.0] - 2026-10-22

`gem` covers every SEMI E5 message of streams 1 to 10 and 12 to 19.

- Builders, decoders, and their tests are generated for streams 3, 4, 7, 8, 10, and 12 to 19: 315 more messages.
- E5 data items with defined values get typed constants,
  from the E5 tables and from the GEM300 standards that define value vocabularies for E5 items.
- Builders for the short forms E5 names, pointers for items E5 lets be sent zero-length, nil for that form,
  and values in and out for repeated groups of one fixed-format item.
- A named struct for each list shape a message repeats, such as `Report` and `ErrorInfo`,
  which builders take and decoders return, checked element by element.
- Decoders reject a list where E5 excludes one, such as a list sent as an ALID.
- Decoder options, starting with `AllowTrailing`, which accepts a list holding more items than E5 declares.
- A catalog: `Lookup` describes and decodes any message by stream and function, and `AcceptReply` builds the reply that accepts one.
- A fuzz target runs every generated decoder over arbitrary bytes.

Several shipped `gem` signatures change; see Upgrade notes.

### Upgrade notes

- `gem`: acknowledge and status items that E5 defines values for now take and decode as named types instead of `byte` or `uint8`:
  `S1F16` and `S1F16Reply.OFLACK` use `OFLACK`, `S1F18` and `S1F18Reply.ONLACK` use `ONLACK`,
  `S6F6` and `S6F6Reply.GRANT6` use `GRANT6`, and `S6F23` and `S6F23Body.RSDC` use `RSDC`.
  An untyped constant such as `0` still compiles; a `byte` or `uint8` variable needs a conversion.
- `gem`: the alarm code ALCD and the predefined form code PFCD now take and decode as named types instead of `byte`:
  `S5F1` and `S5F1Body.ALCD` use `ALCD`,
  and `S6F9` and `S6F9Body.PFCD` use `PFCD`.
  An untyped constant such as `0x86` still compiles; a `byte` variable needs a conversion.
- `gem`: SPD in `S2F3` and `S2F6` is a binary item of any length, as E5 defines it,
  so the builders take and the decoders return `[]byte` instead of a single `byte`.
- `gem`: LINKID, which E5 has S6F25 send zero-length for a change notification, is a pointer both ways:
  `S6F25` takes `linkid *uint32`, which it sends zero-length when nil,
  and `S6F25Body.LINKID` is a `*uint32`, nil when LINKID arrived zero-length.
  Call `S6F25(dataid, opid, new(uint32(7)), …)` for a confirmation report, and pass nil for a change notification.
- `gem`: `S5F5` takes one item holding every ALID, as E5 defines it, instead of a list of ALID items,
  and `S5F5Body.ALIDS` is that item: call `S5F5(secs2.U4(1, 2, 3))` rather than `S5F5(secs2.U4(1), secs2.U4(2), secs2.U4(3))`,
  and `S5F5(secs2.U4())` to ask for every alarm.
- `gem`: a repeated group of one fixed-format item takes and decodes as values instead of `secs2.Item` elements:
  `S2F12` takes `spids ...string` and `S2F12Reply.SPIDS` is `[]string`,
  and `S5F9` takes `recoveryActions ...string` and `S5F9Body.RECOVERYACTIONS` is `[]string`.
  Call `S2F12("SP1", "SP2")` rather than `S2F12(secs2.A("SP1"), secs2.A("SP2"))`.
- `gem`: a repeated list element is a struct both ways, one named type per list shape (see Added),
  instead of a `secs2.Item` the caller built and the decoder returned unexamined.
  The builder takes the structs, and the result field holding them changes type the same way.
  `gem.Report` is now that struct for an event report's reports, not a helper function:
  call `S6F11(dataid, ceid, gem.Report{RPTID: rptid, VALUES: []secs2.Item{v1, v2}})`
  rather than `S6F11(dataid, ceid, gem.Report(rptid, v1, v2))`.
  The builders of the shipped streams that change, with their element types:
  - reports: `S2F33` takes `ReportDefinition`, `S2F35` `EventLink`, `S6F11` and `S6F16` `Report`,
    `S6F13` and `S6F18` `AnnotatedReport`, and `S6F30` takes `reports []Report`;
  - namelists, whose parameters and fields are renamed for what they now hold:
    `S1F12(variables ...StatusVariableName)` and `S1F12Reply.VARIABLES` replace `svids` and `SVIDS`,
    `S1F22(variables ...DataVariableName)` and `S1F22Reply.VARIABLES` replace `vids` and `VIDS`,
    and `S1F24(events ...CollectionEventName)` and `S1F24Reply.EVENTS` replace `ceids` and `CEIDS`;
  - constants and commands: `S2F15` takes `ConstantValue`, `S2F30` `ConstantName`,
    `S2F41` `CommandParam`, and `S2F42` `CommandParamAck`;
  - spooling and limits: `S2F43` takes `SpoolStream`, `S2F44` `SpoolStreamAck`,
    `S2F45` `VariableLimits`, `S2F46` `VariableLimitAck`, and `S2F48` `VariableLimitAttributes`;
  - alarms and data: `S5F6` and `S5F8` take `Alarm`, `S6F3` and `S6F8` `DataSet`, `S6F9` `DataSetValues`,
    and `S6F22` `VariableValue`;
    `Alarm.ALCD` is a `*ALCD`, nil for the zero-length code E5 lets S5F6 return for an alarm whose code does not exist;
  - errors: `S1F20` and `S6F25` take their errors as `ErrorInfo`, and `S6F25` its attributes as `[]RecipeAttribute`.
- `gem`: a repeat whose elements are themselves lists of one shape is a slice of slices:
  `S1F20` takes `objects [][]secs2.Item`, and `S6F27` takes `samples ...[]Report`,
  one `[]Report` per trace sample; their result fields change the same way.
- `gem`: every decoder takes options after the body, `DecodeSxFy(item secs2.Item, opts ...DecodeOption) (T, error)`.
  A call such as `DecodeS6F11(item)` compiles unchanged,
  but a decoder stored as a `func(secs2.Item) (T, error)` value no longer fits that type:
  declare it as `func(secs2.Item, ...gem.DecodeOption) (T, error)`, or wrap the call in a closure.

### Added

- `gem`: generated builders, decoders, and result types for every E5 message of streams 3, 4, 7, 8, 10, and 12 to 19,
  315 messages in all, with round-trip tests for each.
  A body E5 lets be sent as an empty list decodes from `L,0` with every field zero.
- `gem`: typed constants for E5 data items with defined values, from the E5 Table 3 value lists,
  and from E40.1, E58, E58.1, E87.1, E90, E90.1, E94.1, and E116 for the GEM300 vocabularies
  (for example `CARRIERACTION`, `PRCMDNAME`, `OBJTYPE`, `PRSTATE`).
  An item with a single format gets a named type, a string type for an ASCII item,
  used by the builder parameter and the decoder field;
  an item the equipment may send in several formats gets untyped constants and keeps `secs2.Item`.
  A constant from another standard than E5, or with a spelling another standard differs on, says so in its doc.
- `gem`: short-form builders for the forms E5 names, decoded by the message's main decoder:
  `S5F14Accepted`, `S5F15Success`, `S5F18Success`, `S15F41Delete`, `S16F27NoParams`, `S16F28Success`, and `S17F5Untriggered`.
- `gem`: an item E5 lets be sent zero-length is a pointer as builder parameter, result field, and group field,
  and nil stands for the zero-length item, so a builder can send that form and a decoder reports it:
  the LINKID of S6F25, the FNLOC, FFROT, and ORLOC of S12F1 and S12F3, the FNLOC and ORLOC of S12F4,
  the TRAUTOD and RPTOC of S17F5, the ALCD of `Alarm` in S5F6 and S5F8,
  and the VERIFYRSPSTAT of `PDESendResult` in S19F13.
  A pointer to a zero value, such as `new(uint32(0))` or `new(false)`, sends and decodes as that value, not as nil.
  A decoder also reads a zero-length item of another width of the same kind as nil, such as a U4 for a U2 FNLOC.
- `gem`: a repeated group of one fixed-format item is a slice of values both ways:
  the builder takes the values and the decoder returns them,
  as a named type where the item has one (`S19F1` takes `attributes ...PDEATTRIBUTE`).
  It applies across streams 2, 5, 13 to 16, 18, and 19,
  for example to `S16F17(prjobids ...string)` and the STATUS lists of stream 18.
- `gem`: a named struct for each list shape a message repeats, 67 in all, in the new `groups.go`,
  such as `Report`, `ErrorInfo`, `Alarm`, `CommandParam`, and `Attribute`.
  Each struct serves both directions: builders take it as a repeated element, decoders return it,
  and its `Item` method builds its list for a body assembled with `secs2.NewMessage`.
  Its fields follow the result-struct rules: E5 item names, values for fixed-format items, `secs2.Item` otherwise.
  A list inside an element that E5 lets be sent as `L,0`, such as a limit's deadband, is a pointer, nil for `L,0`.
  Every message that repeats a structured list uses them.
- `gem`: `PRCMDNAMEStartProcess`, the SEMI E40 spelling of the process job start command, `STARTPROCESS`,
  which equipment following E40 expects instead of E5's `START`.
- `gem`: `DecodeOption`, which every decoder takes, and `AllowTrailing`, the first option:
  a list of fixed length may hold more items than E5 declares, in the body or in a group,
  and the decoder reads the declared ones and ignores the rest,
  for equipment that appends its own items to a standard body, such as a captured S5F1 with a fourth item.
  A list holding fewer items, or a list E5 lets be sent short with a length between its short and full forms, is still an error;
  item formats and the lists E5 excludes are still checked.
- `gem`: constants for the alarm categories E5 defines for ALCD, such as `ALCDPersonalSafety`,
  and `ALCDAlarmSet` for its set bit, with the methods `Set` and `Category` that read the two parts of a code:
  `S5F1(gem.ALCDAlarmSet|gem.ALCDPersonalSafety, alid, altx)` reports a personal-safety alarm set.
  A category E5 reserves or leaves to the equipment decodes as is.
- `gem`: the named type `PFCD` for the predefined form code of `S6F9`, which E5 defines no values for.
- `gem`: `FuzzDecoders` runs every generated decoder over arbitrary wire bytes, strictly and with `AllowTrailing`,
  and `make fuzz-test` covers `gem`.
- `gem`: `Lookup(stream, function, sender)` returns a `MessageInfo` for every message `gem` generates:
  its E5 name and mnemonic, its `Direction`, the reply bit, the reply function, whether it has a body,
  whether it was checked against E5, and `Decode`, which decodes a received body into the message's result type.
  `Role`, `RoleHost` or `RoleEquipment`, picks the form the sender sends,
  so the host's and the equipment's S1F2, S1F13, and S1F14 each decode with their own decoder.
  `Lookup` does not check the direction; the caller compares it, for instance before an S9F5.
  `Role` and `Direction` have `String` methods for logs, such as "equipment" and "host-to-equipment".
- `gem`: `AcceptReply(stream, function, replier)` builds the reply that accepts a primary as a whole,
  such as S6F12 with `ACKC6Accepted` for S6F11, S1F14 with `COMMACKAccepted` from the host for S1F13,
  or S2F42 with `HCACKAcknowledgeCommandPerformed` and no invalid parameters for S2F41.
  It covers the replies whose acknowledge code has a value meaning acceptance
  and whose lists, if any, E5 sends empty only when nothing went wrong.
  It reports false for a reply that carries data, such as the equipment's S1F14 or S14F2,
  for a direction that does not let the two roles exchange the message, and for an invalid `Role`.
  The reply asserts what E5 says its value means, which for several messages is more than receipt:
  HCACK and CAACK 0 say the command has been performed, RMACK 0 that the action completed,
  and TBLACK, TIACK, EAC, DRACK, LRACK, and ERACK 0 that the request was carried out;
  RSACK and RRACK 0 mean "Acknowledge, OK", which E5 distinguishes from ready.
  Where completion comes later, or the caller only forwards the request,
  build the reply with the message's builder instead, for example with HCACK 4, CAACK 4, or RMACK 3.

### Changed

- `gem`: `S1F16`, `S1F18`, `S6F6`, and `S6F23` and their result types
  use the named types `OFLACK`, `ONLACK`, `GRANT6`, and `RSDC` (see Upgrade notes).
- `gem`: `S5F1`, `S6F9`, and their result types use the named types `ALCD` and `PFCD`,
  and the `Alarm` of `S5F6` and `S5F8` holds its code as `*ALCD` (see Upgrade notes).
- `gem`: `S2F3` and `S2F6` take SPD as `[]byte`, and their decoders return it so (see Upgrade notes).
- `gem`: `S6F25` sends a nil LINKID zero-length, and `DecodeS6F25` accepts a zero-length LINKID and returns it as nil (see Upgrade notes).
- `gem`: `S2F12` and `S5F9` take their SPIDs and recovery actions as strings,
  and their decoders return them so (see Upgrade notes).
- `gem`: the builders and decoders of the messages that repeat a list take and return its struct,
  and those of S1F20 and S6F27 a slice of slices (see Upgrade notes).
  A decoder checks every element at full depth,
  and a malformed one is an error naming its path from the body, such as `REPORTS[1].VIDS`;
  the elements used to be returned unexamined.
- `gem`: `DecodeS6F8`, `DecodeS6F16`, and `DecodeS6F18` accept an empty body list, `L,0`,
  which E5 allows when there is no data or no report for the CEID.
  They previously rejected it as a list of the wrong length.
- `gem`: godoc shows an empty list as `L[0]{}` instead of `L[0]{  }`.
- `gem`: `S5F5` sends its ALIDs as one item, `<ALID1,...,ALIDn>`, and `DecodeS5F5` accepts that item (see Upgrade notes).
  It previously sent a list of items, which E5 does not define for this message.
- `gem`: `S2F49` and `S6F29` set the reply bit, since S2F50 and S6F30 are their replies.
  They previously sent W=0, so the equipment's acknowledge or trace data never came back.
- `gem`: decoders reject a list where E5 excludes one:
  an equipment-defined item whose E5 formats do not include L, such as PPID, ALID, or VID,
  is an error when it arrives as a list, whether in the body, in a group, or as an element of a repeat,
  for example `gem: REPORTS[0].VIDS[0]: got list, want a non-list item`.
  The decoders of streams 1, 2, 5, 6, and 9, shipped in v2.6.0, accepted such a list and returned it.
  Only lists are rejected: an item in a format E5 does not list, such as an ASCII ALID or CEID, still decodes.
  `DecodeS2F14` alone accepts an empty list `L,0` as a constant value,
  which E5 defines there to mean the constant does not exist.
- `gem`: every decoder takes `opts ...DecodeOption` after the body item (see Upgrade notes).

## [2.6.0] - 2026-09-28

Wire-level and socket-level observability for HSMS-SS,
and socket and generation identity on lifecycle and transaction events.

- `WithWireObserver` reports every HSMS frame that crosses the socket, in both directions,
  as its exact wire bytes, control frames and the frames of a refused peer included,
  timed so that a reply never precedes the frame it answers.
- `WithSocketObserver` reports each socket the connection dials or accepts, refused ones included,
  when it comes up and when it closes, with the cause and the failure that closed it.
- `LifecycleEvent` and `TxEvent` name the socket and connection generation they belong to,
  so the wire, socket, lifecycle, and transaction streams join on the same two values.
  `LifecycleEvent` also carries the time its transition fired, and `TxEvent` the primary's session ID.
- Only the HSMS-SS transport reports wire and socket events;
  a SECS-I connection built on the same core reports neither.

The public API only gains additions.
One behavior change is visible to the peer; see Upgrade notes.

### Upgrade notes

- `hsmsss`: on a peer Separate, a linktest failure, or a failed active Select,
  the socket now closes as soon as the transport reports the disconnect,
  rather than later, when the connection tears the link down.
  For a peer Separate this is the immediate close SEMI E37.1 §7.6 asks for.
  The peer sees its TCP connection close sooner;
  none of these paths sent a farewell Separate before, and none does now.

### Added

- `hsms`: `WithWireObserver` installs a synchronous hook
  that receives a `WireEvent` for each complete HSMS frame the connection reads or writes:
  its `WireDirection`, the socket and generation it crossed, its time `At`, and the frame bytes,
  length prefix and header included.
  Every frame is reported, data and control alike,
  including the frames the connection sends on its own and inbound frames it goes on to reject.
  An outbound frame is reported only after its write succeeded.
  `At` carries a monotonic reading.
  For an inbound frame it is the time the frame was fully read;
  for an outbound frame it is the time immediately before its write was issued, not when the write returned,
  so a reply's `At` never precedes that of the frame it answers.
  Every frame of a socket is reported before that socket's `SocketClosed` event:
  a `SocketClosed` is held back while a frame report is in progress on the socket
  and is then delivered by the goroutine that made that report,
  so closing a socket never waits on the hook, and neither do `Close` and `Stop`.
  The courtesy Separate of a graceful close and the frames of a refused peer's exchange
  are delivered from a goroutine of their own, since the goroutines that write them are ones `Close` waits for;
  a hook that never returns there leaks that goroutine and never delivers that socket's `SocketClosed`, nothing else.
  `Frame` is valid only during the call,
  and the hook must not send on the same connection.
  With an observer installed, outbound frames are copied into a buffer reused for the life of the link,
  so observation allocates nothing per frame once that buffer has grown.
- `hsms`: `WithSocketObserver` installs a synchronous hook
  that receives a `SocketEvent` for each socket the connection dials or accepts:
  `SocketConnected` or `SocketAccepted` when it comes up,
  `SocketRefused` for an extra passive peer rejected because a session is already live,
  reported as soon as the connection decides to refuse it, before the refusal exchange,
  and exactly one `SocketClosed`, whichever side or path closed it,
  whose `At` is the time the socket was closed even when the event is delivered later.
  `SocketEvent.Err` on the close names the failure that initiated it —
  a read or write error, a refused or unanswered Select, a failed linktest, or `ErrT7Timeout` —
  and is nil for a close the application or the peer asked for.
  `SocketEvent.Cause` names why the socket closed;
  a peer that closed its side of an adopted socket without a Separate.req gives `CauseIOError` with `errors.Is(Err, io.EOF)`.
  `Err` and `Cause` are one close reason, taken from whichever path closed the socket first,
  so a socket's `Cause` can differ from the cause of the lifecycle transition that ended its generation.
  A refused socket and its frames carry generation 0.
  The hook runs on the goroutine that dialed, accepted, refused or closed the socket,
  and `Close` and `Stop` wait for a call in progress there to return, so it must be cheap and must return;
  a `SocketClosed` held back behind a frame report is delivered by the reporting goroutine
  and is waited for only through that goroutine's bounded join, or not at all from a delivery goroutine.
- `hsms`: `LifecycleEvent.Socket`, `LifecycleEvent.Generation`, and `LifecycleEvent.At`:
  the generation a transition belongs to, that generation's socket, and the time the transition fired.
  All three are taken when the transition happens,
  so a notification delivered after a reconnect still names the link it reports,
  and one that a slow handler or subscriber delayed still carries the time its transition fired.
- `hsms`: `TxEvent.SessionID`, `TxEvent.Socket`, and `TxEvent.Generation`.
  A send reports the generation it was bound to when it started, even when it completes after a reconnect,
  and a send that found no open connection reports socket and generation 0.
- `hsms`: `ErrT7Timeout`, the failure a `SocketClosed` event carries for a link the T7 not-selected timer tore down.
  `IsTransient` and `IsTimeout` both report true for it.

### Changed

- `hsmsss`: every socket the transport dials or accepts, a refused extra peer's included, now closes through one close gate,
  which keeps the close reason (the failure and its cause) of whichever path closed it first
  and reports the close exactly once.
  An involuntary disconnect the transport detects itself —
  a read error, a peer Separate, a failed linktest, or a failed active Select —
  closes the socket through that gate before it reports the disconnect,
  so the socket no longer stays open until the connection's teardown (see Upgrade notes).
- `github.com/stretchr/testify` moved from v1.9.0 to v1.12.1,
  the version the nested `tracepack` module already pins.

### Tests

- `hsms`: three tests no longer depend on `require.Eventually` deferring its first check to the first tick,
  which testify 1.10 and later stopped doing.
  Each now waits on the edge it was implicitly assuming:
  the reopened connection's own bring-up reports, a successor generation reaching Selected,
  and the supervisor reacting to the terminal transition before the notify buffer is drained.
- `hsms`: a reconnect test waits for the Selected notification itself before counting it,
  instead of the state, which the notifier goroutine reports later.

## [2.5.0] - 2026-09-27

Callback isolation, connection-lifecycle hardening, and complete format-22 (`W`) support.

- Application callbacks can no longer crash the process or silently stall a link.
  Panics are recovered, logged with a stack, and counted;
  a callback that calls `runtime.Goexit` is detected.
- `Close` interrupts a blocked `Open`,
  and `Open`'s ctx now also bounds an active transport's first dial.
- Known paths by which a previous link's inbound frames, replies, timers, or delayed state reports
  could reach the link that replaced it after a reconnect are closed.
- Custom transports get a tighter, documented `TransportRuntime` contract.
- A zero-length `W` item decodes,
  and `W` items round-trip through SML byte for byte.

The public API only gains additions.
A few behavior changes can affect existing callers; see Upgrade notes.

### Upgrade notes

- `hsms`: under `OpenBackground`, a ctx that expires while the first dial is still blocked
  now makes `Open` roll back and return the ctx error.
  Previously the connection fell into the cold-peer background retry.
- `hsms`: a synchronous data send whose write fails after teardown has started returns `ErrConnClosed`,
  not the raw socket error.
  `errors.Is` still matches the wrapped socket error.
- `hsms`: recovered callback panics are now logged at Error.
  Panics from a `StateChangeHandler`, a `SubscribeLifecycle` callback, or the async send error handler
  used to be recovered silently.
- `hsms`: `ConnectionMetrics.Reconnecting` now rises when the reconnect loop starts its first backoff,
  not the instant the drop is detected.
- Custom transports (`hsms.NewConnection`):
  `Stop` must close any socket the transport handed to `TransportRuntime.TCPUp`.
  `TCPUp` and `SelectLost` refuse their state commit once the generation's teardown has begun,
  and a `Start` that reports TCP-up and then fails now always fails the initial `Open`.
  `hsmsss` and `secs1` already meet the new contract.
- `sml`: inside a quoted `W` value, `\\`, `\'`, and `\"` are now escapes,
  and adjacent quoted runs concatenate.
  Hand-written `W` literals that relied on those characters being kept verbatim now read differently.

### Added

- `hsms`: `ConnectionMetrics.HandlerPanicCount` counts the panics recovered from application callbacks
  the connection runs on its own goroutines:
  `DataMessageHandler`, `DecodeErrorHandler`, `StateChangeHandler`, `SubscribeLifecycle` callbacks, and the async send error handler.
- `hsms`: `CauseHandlerExit`, the `TransitionCause` reported when a callback calls `runtime.Goexit` instead of returning
  and the connection drops the generation that callback was running for.
- `secs2`: `NewEmptyLocalizedStrItem` builds a zero-length localized-string (format 22) item,
  encoded as `49 00` with no LSH.
  `LocalizedStrItem.HasLocalizedStrHeader` reports whether an item carries an LSH.
  `ToLocalizedStrHeader` reports 0 for a zero-length item, the same as for LSH 0;
  `Size` tells them apart (0 for the zero-length item, at least 2 otherwise).
- `secs2`: `IsReservedLSH` reports whether an LSH code is reserved by SEMI E5: 0 (`LSHNone`) and 15 through 32767.
  Reserved codes must not be sent,
  but `NewLocalizedStrItem` and decoding still accept them unchanged.
- `sml`: `<W[0]>` is the SML form of a zero-length localized-string item, and the encoder writes one that way.
  `<W>` still means a UTF-8 item with empty text (`49 02 00 02`),
  and `<W[0]>` with a value is a parse error.

### Changed

#### Callbacks

- `hsms`: a panicking `DataMessageHandler` or `DecodeErrorHandler` is recovered instead of crashing the process,
  and the connection stays up.
  The message still reaches the remaining handlers and, for a data message, the channels,
  subject to the usual teardown checks.
  A decode-error message never reaches the channels.
- `hsms`: every recovered callback panic is logged at Error with its stack and counted in `HandlerPanicCount`.
- `hsms`: a callback that calls `runtime.Goexit` instead of returning,
  for example through `testing.T.FailNow`, is detected and logged at Error.
  For a `DataMessageHandler`, a `DecodeErrorHandler`, or the async send error handler,
  the connection also drops the generation the callback was running for,
  so it reconnects instead of sitting Selected with no receiver.
  For a `StateChangeHandler` or `SubscribeLifecycle` callback it is logged only,
  and later state notifications of that `Open` are no longer delivered.

#### Connection lifecycle and metrics

- `hsms`: `Connection.Open`'s ctx now bounds an active transport's first dial
  in both `OpenWaitSelected` and `OpenBackground` modes.
  Previously it bounded only the wait for Selected,
  and the dial ran until the OS connect timeout or `WithConnectTimeout`.
  A custom `DialFunc` that ignores its ctx is not interruptible this way.
- `hsms`: `ConnectionMetrics.Reconnecting` excludes the dropped generation's own teardown
  and the wait for that generation's `Start` to return,
  a window a slow custom `Start` could otherwise stretch out indefinitely.
  It still never reads above 1.
- `hsmsss`: a passive connection logs its accept-retry warning off the accept goroutine,
  and suppresses repeats while one is still being logged,
  so a blocking `Logger` can no longer stall `Close`.

#### Custom transports

- `hsms`: the transport contract `NewConnection` accepts now states
  that `Stop` must close any socket it handed to `TransportRuntime.TCPUp`.
  A `TCPUp` report that lands after teardown has already closed the generation's socket still publishes that connection,
  and the core never closes it.
- `hsms`, `secs1`: `TransportRuntime.TCPUp` refuses its state commit, leaving the state unchanged,
  when the current generation's teardown has begun or that generation has already reported TCP-up.
  The socket is still handed to the generation; only the move to `NotSelected` is refused.
  Previously such a report could move a dying or dropped link back to `NotSelected`.
  Normal bring-up is unaffected.
- `hsms`: `TransportRuntime.SelectLost` is refused, leaving the state unchanged,
  when the current generation's teardown has begun, as `TCPUp` and `CommitSelected` already are.
  Previously an unnamed Select-lost commit skipped that check.
- `hsms`: when a custom transport's `Start` reports TCP-up and then returns an error,
  the initial `Start` of an `Open` cycle always fails `Open`, even under `OpenBackground`.
  Previously `Open` could instead return nil and start a background retry,
  depending on whether the drop had been processed first.
  In a later reconnect generation, the drop's reaction owns the retry (see Fixed).

#### SML

- `sml`, `secs2`: a `W` item round-trips through SML byte for byte, LSH included.
  The encoder and `LocalizedStrItem.ToSML` write printable UTF-8 in double-quoted runs that escape only `"` and `\`,
  write every other byte as a `0xHH` token instead of a Go escape such as `\n` or `\xff`,
  and write an LSH other than UTF-8 as a leading decimal token, for example `<W 8 0x82 0xA0>`.
  The parser reads that grammar in both modes:
  `\\`, `\'`, and `\"` inside the quotes are escapes,
  and adjacent runs concatenate (`<W 'a' 'b'>` is `ab`, not `a' 'b`).
  A backslash before any other character is kept, so older encoder output such as `<W "a\nb">` reads as before.
  In non-strict mode, a value the grammar rejects falls back to the old verbatim rule, so `<W 'it's'>` still parses;
  strict mode rejects it.
  `A` and `J` items are unchanged.

#### Documentation

- `hsms`, `hsmsss`, `secs1`: the `WithLinktestInterval`, `WithTCPKeepAlive`, `WithDialer`, and `WithListener` docs
  explain how an idle link detects a silent half-open peer.
  Linktest is off by default, so detection falls to TCP keep-alive, which can take minutes.
  A custom dialer or listener must configure keep-alive itself
  unless `WithTCPKeepAlive` is set and the socket is a `*net.TCPConn`.
  The README example enables a 30-second linktest.
- `hsms`, `hsmsss`, `secs1`: the `Connection.Close`, `WithCloseTimeout`, `WithLogger`, `WithDialer`, and `WithListener` docs
  state the real close bound:
  a best-effort farewell Separate of at most 500ms,
  then the configured close timeout for the teardown join and the notifier join.
  They also state the caller obligations it depends on:
  a blocking custom `net.Conn`, `DialFunc`, `ListenFunc`, or `Logger` can still keep `Close` from completing.
- `hsms`: the `ConnectionMetrics.Reconnects` docs state what is counted:
  every successful transport start after an involuntary drop, including before any generation of the cycle has selected,
  but not the initial background retry of a cold peer.
  The counting itself is unchanged.

### Fixed

#### Localized strings

- `secs2`: a zero-length localized-string (format 22) item, `49 00`, decodes, alone or inside a list,
  and re-encodes to the same two bytes.
  SEMI E5 allows a zero-length item of any format.
  Previously one such item made the whole message body undecodable.
  A 1-byte body (`49 01 xx`) is still rejected.

#### Leftovers from a previous link

- `hsms`, `hsmsss`: a receive goroutine left running past the close timeout,
  for example by a blocking `DataMessageHandler`,
  no longer delivers its frame into the next generation after a reconnect.
  Previously such a frame could reach later handlers and channels,
  complete a transaction on the new link with a reply meant for the old one,
  or answer on the new link.
  A frame read just as teardown starts is now dropped rather than delivered.
- `hsms`: the data-message fan-out checks for teardown before each handler and each channel,
  not only before the first handler.
  Once it sees the connection tearing down, later handlers and channels do not receive the message.
- `hsms`: `ReplyDataMessage` returns `ErrConnClosed` and sends nothing
  when the primary was received on a connection generation that has since ended.
  Previously the reply went out on the new link with the old transaction's System Bytes,
  which the peer did not expect or could have matched to a different transaction.
  A primary received on another connection, or a reply built by hand, is not checked.
- `hsmsss`: the T7 and auto-linktest timers of an ended generation
  can no longer cancel or replace the timers of the generation that replaced it,
  which could leave the new link without T7 or linktest protection.

#### Sends

- `hsms`: a synchronous data send whose write fails after teardown has started —
  a voluntary `Close` or an involuntary drop —
  reports `ErrConnClosed` and is no longer counted in `DataMsgErrCount`.
  The socket error stays wrapped, so `errors.Is` still matches it,
  and an `ErrConnClosed` from the transport itself comes back unwrapped.
  `WithTransactionObserver` now reports a cancellation rather than a send failure.
- `hsms`: while a synchronous send waits for its reply,
  a reply that has already arrived wins over a simultaneous teardown or protocol timeout,
  and a teardown wins over a simultaneous protocol timeout, which is then not counted.
  Previously the outcome was random.
  Caller cancellation behaves as before.

#### Open, Close, and reconnect

- `hsms`: `Close` no longer blocks behind an `Open` that is itself blocked,
  either in `OpenWaitSelected`'s wait for Selected or in an active transport's first dial.
  `Close` interrupts either, and `Open` returns `ErrConnClosed`.
  Previously `Close` waited until `Open` returned on its own:
  forever for an `OpenWaitSelected` wait with no peer,
  or about two minutes on Linux for a blocked first dial without `WithConnectTimeout`.
- `hsms`: a disconnect landing after the Select commit but before its own report reached the state machine
  is no longer silently absorbed.
  Previously it could leave the connection down with no reconnect and no `NotConnected` notification.
  A TCP-up report that a disconnect had already overtaken is now discarded,
  instead of reviving `NotSelected` on the dropped link and leaving it stuck there.
- `hsms`: a failed `Open` no longer leaves a reconnect loop running past its return.
  Previously a drop reaction racing the rollback could start a loop
  that slept out its full backoff before a later `Open` reaped it.

#### Custom transports

- `hsms`: a custom transport whose `Start` reports TCP-up and then returns an error
  no longer runs two reconnect loops against each other.
  Previously one loop could orphan the other's live generation;
  a passive transport on the same port was left wedged on "address already in use".
  The failing `Start` now hands the reconnect to the drop's reaction, so only one loop proceeds.
- `hsms`: a custom transport's `Start` failing after TCP-up, before any drop is reported,
  no longer leaves the state stuck at `NotSelected` with a dead link.
  The core reports the drop itself, and the usual reconnect follows.
- `hsms`: a disconnect or T7 expiry reported without a generation identity
  (`secs1`, or a transport using only `TransportRuntime`)
  is applied only to the generation that was current when it was reported.
  Previously, if it was processed only after a reconnect,
  it could knock the new link back to `NotConnected` and make it refuse a Select.
- `hsms`: a Select-accepted, Select-lost, or TCP-up commit reported without a generation identity
  is bound to the generation it was validated against,
  and its report is discarded once a successor generation has replaced that one.
  Previously a delayed report could land on the new link:
  Select-accepted could mark it Selected without its own handshake,
  Select-lost could report its bring-up under the wrong cause,
  and TCP-up could report its bring-up on the old generation's behalf.
  A late call made after the new generation is already current still targets the new generation.

## [2.4.2] - 2026-09-25

### Fixed

- `hsmsss`: a passive connection retries a transient `Accept` failure, such as descriptor exhaustion,
  instead of treating it as teardown.
  Previously a failure on the first `Accept` left the listener open but unserved, with no reconnect,
  and a failure while a session was live stopped refusing extra connections.
- `hsms`: `Close` no longer deadlocks when called from a `StateChangeHandler` or `SubscribeLifecycle` callback,
  and no longer waits indefinitely on a callback that blocks.
  Waiting for the state-change notifier is now bounded by the close timeout;
  on expiry `Close` returns `ErrCloseTimeout`.
  Previously the call never returned and held the lifecycle lock, so every later `Open` or `Close` hung too.
  A failed `Open` applies the same bound to its rollback.
  A callback left running this way finishes delivering its cycle's queued transitions after the shutdown returns,
  possibly concurrently with a reopened cycle's callbacks.
- `hsms`, `hsmsss`: a control response (Linktest.rsp, Select.rsp, Deselect.rsp) whose System Bytes match a pending data transaction no longer completes it.
  Previously `SendDataMessage` and `SendSECS2Message` returned no reply and no error,
  and the real reply was then treated as unsolicited.
  The stray response is now answered with a Reject.req (`RejectTransactionNotOpen`),
  and the transaction keeps waiting for its reply.
- `hsms`: when `Close` times out because the transport could not stop in time,
  the returned `ErrCloseTimeout` now carries the transport's own error
  instead of a misleading "0 tasks live".
- `hsmsss`: an active connection whose Select.req is answered with status 1 (Communication Already Active)
  on a connection that never selected now drops the link at once with `CauseSelectRejected`
  and reconnects with backoff, instead of waiting for T7 and reporting `CauseT7Timeout`.
  If the connection also fails while the answer is in flight,
  that failure's cause may be reported instead.
- `hsms`: the reconnect backoff now keeps growing across reconnects that never reach Selected,
  for example a peer that refuses every Select.req.
  Previously it restarted from the initial delay after every TCP connect,
  reconnecting roughly ten times a second forever against such a peer.
  It still restarts from the initial delay after a selected link drops, and on Open.
  On a passive connection the same growth damps how fast a re-listen cycle repeats,
  for example a peer that connects and is dropped by T7 without ever selecting.
- `hsms`: `TransportRuntime.CommitSelected` now returns false once the connection has torn down the current generation,
  also for a transport that carries no generation identity (such as `secs1` or a custom transport).
  Previously such a late commit could still move the state to Selected.

### Changed

- `hsms`: the `DataMessageHandler` and `DecodeErrorHandler` docs now state that calling `Close` from inside a handler always waits out the close timeout and returns `ErrCloseTimeout`,
  because `Close` waits for the goroutine running the handler;
  call `Close` on another goroutine instead.
- `hsms`: the `DataMsgErrCount` docs now state that a data write already on the socket when `Close` tears the connection down
  can fail with the socket's own error, and that such a failure is counted;
  only a send that reports `ErrConnClosed` is excluded.

## [2.4.1] - 2026-08-15

### Security

- `sml`: programmatic item trees now stop at the same nesting depth as parser and wire decoder input,
  and padded trailing input no longer amplifies parser preallocation.

### Fixed

- Public constructors, options, cursor traversal, list traversal, send and reply helpers, and SML logging helpers return errors or diagnostics for nil-like inputs instead of panicking.
- Invalid SECS-II lists no longer emit a child count that disagrees with their wire payload.
- Strict SML output escapes `>` and single-line item colons no longer alter header parsing.
- SECS-I reports EOF and socket failures as I/O errors rather than T1/T2 expiry,
  and its core deadline hooks no longer alter line-engine socket deadlines.
- HSMS-SS inbound control tracing follows live logger and trace updates.

### Known follow-ups

- A dribbling SECS-I peer can keep malformed-frame draining active until disconnect.
- Large legal SECS-II lists can expand to a much larger decoded object graph.
  SML input that declares counts it never supplies preallocates several times what honest input needs.
- Framing protocol failures still report the existing `CauseIOError` lifecycle cause.
- Typed-nil and zero-value guards at the `secs2` item boundary cost a few nanoseconds each.
  They apply at every cursor hop, every terminal accessor, and every `Equal` comparison.
  A two-hop cursor extraction takes about 9 ns longer than in v2.4.0, with allocations unchanged.

## [2.4.0] - 2026-08-14

Consumer ergonomics and observability:
typed value extraction, generated GEM decoders, channel delivery,
lifecycle and transaction observability, and error retryability classification —
plus a family of connection-generation isolation fixes present since v2.0.0.

### Added

- `secs2`: `Cursor` extracts a typed value from a nested item in one chained call —
  `secs2.NewCursor(item).At(1, 0).ASCII()`.
  Errors accumulate across hops (one check per extraction, not one per hop)
  and name the failing hop's index and depth.
  Scalar accessors allocate nothing on the happy path.
- `gem`: a body decoder for every generated builder — 122 pairs across streams 1, 2, 5, 6, and 9
  (`DecodeS1F14` fills an `S1F14Reply`, `DecodeS6F11` fills an `S6F11Body`, …).
  Decoders take a `secs2.Item`, keeping `gem` transport-independent.
  Shape checking is strict: wrong arity, type, or width returns an error naming the E5 field —
  never a zero value with a nil error.
  Repeated groups accept any count, optional groups decode in full or omitted form,
  and equipment-defined fields pass through as `secs2.Item`.
  Non-standard bodies are served by the manual `secs2.NewCursor` path.
- `hsms`: `SECS2Endpoint.AddDataMessageChan` delivers every inbound data message —
  primaries and orphan secondaries — to a registered channel,
  as an alternative to callback handlers.
  Split the stream with the new `DataMessage.IsPrimary()`.
  Delivery, backpressure, and lifetime follow `AddDataMessageHandler`'s contract (see godoc).
- `hsms`: `Connection.SubscribeLifecycle` observes state transitions with the reason each happened —
  a `TransitionCause` such as `CauseLocalClose`, `CausePeerSeparate`, `CauseT6Timeout`,
  `CauseLinktestFail`, or `CauseIOError` —
  so a local close, a peer separate, and a dropped socket are distinguishable without log correlation.
  Subscriptions are cancellable, persist across `Open`/`Close`,
  and share the legacy `AddConnStateChangeHandler` notifier's ordering and panic isolation.
- `hsms`: `WithTransactionObserver` reports one `TxEvent` per completed synchronous send
  (`SendDataMessage`, `SendSECS2Message`, `ForwardDataMessage`),
  carrying the outcome (`TxReplied`, `TxT3Timeout`, `TxRejected`, …) and duration.
  It runs on the caller's goroutine and bridges to a metrics backend such as Prometheus
  without the library importing one.
  Unset, it costs one nil check on an already-loaded config pointer.
- `hsms`: `IsTransient(err)` and `IsTimeout(err)` classify errors from the send and lifecycle
  surfaces of both transports, replacing hand-rolled `errors.Is` chains.
  Timer and connection-state sentinels (`ErrT3Timeout`, `ErrNotSelectedState`, …) are transient;
  caller bugs (`ErrMessageTooLarge`, …) and unknown errors are not —
  misclassifying a permanent failure risks a retry storm, the reverse only fails a job early.
  The `TransientError` / `TimeoutError` marker interfaces let other transports and callers opt in;
  `secs1.ErrSendFailed` is marked with its identity unchanged.

### Changed

- **Breaking:** `hsms.SECS2Endpoint` gained `AddDataMessageChan` and
  `hsms.Connection` gained `SubscribeLifecycle` — the first interface additions since v2.0.0 GA.
  Hand-rolled implementations of either interface fail to compile until they add the method.
  Embed `hsmstest.FakeEndpoint` (which implements `AddDataMessageChan`)
  to be insulated from future `SECS2Endpoint` growth.
  `FakeEndpoint` does not implement `Connection`;
  a hand-rolled `Connection` needs a stub, e.g.
  `func (...) SubscribeLifecycle(func(hsms.LifecycleEvent)) func() { return func() {} }`.
  `hsmsss` and `secs1` are unaffected: both build on the engine this package returns.

### Fixed

- `hsms`, `hsmsss`: **a transport goroutine that outlives its connection generation can no longer
  act on the generation that replaced it.**
  Teardown abandons goroutines wedged past `WithCloseTimeout`;
  one resuming after a reconnect could, until now:
  drop the new link with a false lifecycle cause,
  mark it Selected without a handshake (or drop it out of Selected),
  attach its dead socket to it,
  disarm its auto-linktest while leaving a stale T7 dwell behind,
  answer the new peer with responses to transactions that peer never opened,
  or open a Select / linktest transaction on the new link's socket and reply registry.
  Every disconnect report, state commit, socket publication, and control frame now carries the
  identity of the generation it belongs to,
  and is applied or sent only while that generation is still current —
  anything arriving out of an ended generation is discarded,
  and a refused socket is closed instead of leaking with a goroutine parked on it.
  The working generation is never affected.
  Present since v2.0.0; not a v2.4 regression.
  `secs1` is unchanged and keeps the previous behavior.
- `hsmsss`: `ConnectionMetrics.RejectSentCount` and `LinktestReqRecvCount` no longer count
  responses the generation binding deliberately dropped;
  `LinktestSendCount`'s documentation now says plainly that it counts attempts, not emitted frames.

## [2.3.1] - 2026-08-12

Security and performance patch.
No public API changes, and one deliberate behavior narrowing: SML input nesting deeper than
`secs2.MaxListDepth` is now rejected (see Changed).

Everything here came out of a QA review of the v2 packages and the per-commit review loop that
followed it.

### Security

- `sml`: the parser sized allocations from the declared item-count token (`[n]` / `[n..m]`), which
  is attacker-controlled and bounded only by `math.MaxInt32`. A 29-byte message could demand
  multiple gigabytes before a single payload byte was inspected, and strict-mode ASCII reached
  ~1.9 GB from 25 bytes by growing its string builder the same way. Every such hint is now clamped
  to the bytes actually left to parse.
- `sml`: strict mode accumulated each unquoted numeric token one rune at a time. Go strings are
  immutable, so that copied quadratically — a 195 KB token reached roughly 19 GB before the token
  was even validated. The token is now sliced from the input at its delimiter, which is linear.
- `sml`: list parsing recursed once per nesting level with no budget, so nesting in the text mapped
  onto stack depth. Around two million levels — a 14 MB message — aborted the process with a stack
  overflow, which Go treats as a fatal error rather than a panic, so callers could not defend
  themselves with `recover`. Nesting is now bounded.

### Changed

- `sml`: list nesting is capped at `secs2.MaxListDepth` (64), the same ceiling the wire decoder has
  always enforced, and deeper input returns a `*ParseError` carrying its position. This narrows
  which inputs parse. Messages nested deeper than 64 could never round-trip — `secs2.Decode`
  already rejected them — so they were unusable on the wire regardless. Real SECS-II messages nest
  a handful of levels; nothing in this repository builds deeper than four.
- `secs1`: block transmission appends into a send buffer owned by the line engine instead of
  building each block from a nil slice, removing 792 B/op and 4 allocs/op from the hot send path.
- `secs2`: `Equal` compares the underlying item fields directly instead of going through accessors
  that clone their value slices only to discard them, removing 160 B/op and 2 allocs/op per
  comparison on a path `hsms.Equal` uses per message.

### Removed

- `internal/pool`: `GetTimer` no longer guards a channel drain on a condition that could not hold,
  since timers only enter the pool through `PutTimer`, which stops them first.

### Tests

- `hsms`: fuzz targets for the payload decode entry points, including the zero-copy path that
  adopts the caller's buffer.
- `hsmsss`, `secs1`: the sealed-start abort on both HSMS-SS roles and on SECS-I passive start,
  where a stop racing a start must close the socket it just opened, plus the listen-failure paths.
- `logger` coverage rose from 31% to 92%, `logger/loggertest` from 9% to 100%, and
  `hsms/hsmstest` from 74% to 85%.

### Known follow-ups

- A caller that programmatically builds a list tree deeper than 64 can still reach the recursive
  SML and SECS-II encoders. Untrusted input can no longer produce such a tree, since the parser and
  the wire decoder both cap at 64, so this is follow-up hardening rather than a live exposure.

## [2.3.0] - 2026-08-12

Conformance release, widening scope from v2.2.0's HSMS-SS (SEMI E37.1) axis to the shared `hsms` engine
against SEMI E37 generic and `secs2` against SEMI E5.
Full findings are in `docs/specs/secs2-hsms-conformance-audit.md`.

**This release does not achieve full conformance, and does not claim to.**
The default reconnect backoff still seeds at 100 ms rather than honoring the T5 connect-separation floor
SEMI E37 §9.2.1.1 asks for after a terminated active connect.
That default is unchanged here, deliberately: it is a documented product choice
(`hsms.WithT5`, `hsms.WithReconnectBackoff` godoc), not an oversight, and
`WithReconnectBackoff(t5, 1.0)` remains the conformant setting for anyone who wants it today.

**Read this before upgrading if you send or receive HSMS data messages.**
Several send-side changes reject constructs that used to reach the wire silently — see "Changed" below for the
exact call that now returns an error.
Two of those changes break the build, not just the behavior: `NewRejectReq`, `NewRejectReqRaw`, and
`secs2.DecodeOwnedFrame` all gained a return value.
Receive-side leniency is intentionally **not** tightened by default:
the equipment on the other end of most of these connections cannot be patched,
so this release adds observation (new counters and accessors) while leaving delivery exactly as it was,
and puts the conformant behavior behind an opt-in.
Read the `WithStrictReplyMatching` entry under "Added" carefully before turning it on —
its failure mode is a hang, not an error.

### Fixed

- **`hsms`: an outbound data message that would exceed the maximum on-wire frame size is now rejected
  instead of transmitted unbounded.**
  Nothing previously stopped a caller from building a message whose encoded frame exceeded the same
  16,777,215-byte ceiling (`MaxByteSize`/`MaxMessageSize`) the receive path has always enforced —
  the send path had no check at all.
  `SendDataMessage`, `SendSECS2Message`, and `ForwardDataMessage` now return the new
  `hsms.ErrMessageTooLarge` before an oversized frame reaches the wire.
  `SendDataMessageAsync` and `ForwardDataMessageAsync` enqueue onto the async send channel and cannot return
  the error synchronously; the same check runs when the framing layer drains the queue, so an oversized
  message is still rejected before it reaches the wire, surfaced through the async-send error counter and,
  if configured, `WithAsyncSendErrorHandler` — not through the call's return value.
  This was not a v2 regression: v1 had no outbound cap either.
- **`hsms`: `GetRejectReasonCode` now accepts the full legal SEMI E37 Reject reason-code range, `[1, 255]`,**
  instead of incorrectly rejecting the standard-reserved and local-entity range `5–255` as if it were undefined.
  `ControlMessage.RejectReasonCode()` inherits the fix.
  `ErrInvalidRejectReason`'s message is corrected to match.
- **`hsms`: primary-send APIs now refuse an even function code.**
  `SendDataMessage`, `SendDataMessageAsync`, and `SendSECS2Message` return the new
  `hsms.ErrEvenFunctionPrimary` instead of putting an even-function, W-clear "primary" on the wire.
  SEMI E5 §7.2 reserves odd function codes for primaries.
  `ReplyDataMessage` and `ForwardDataMessage` are unaffected — both legitimately carry even functions.
- **`hsms`: the exported frame decoders now reject a control frame that carries a body.**
  `DecodeHSMSMessage`, `DecodeHSMSPayload`, and `DecodeOwnedHSMSPayload` return the new
  `hsms.ErrControlFrameWithBody` for a non-zero-SType frame whose length is not exactly 10, per SEMI E37
  §9.3.3.1.
  The live `hsmsss` receive path already rejected this on the wire (with a Reject.req, to keep the link);
  the gap was in the standalone decode APIs a proxy, log replayer, or test harness would use directly.
- **`hsmsss`: a second dialer while a session is already live is now refused per SEMI E37 §9.2.4.1.1's
  preferred option**, instead of being accepted and then silently closed.
  The extra connection is accepted, given one bounded chance to send a bare `Select.req`, answered
  `Select.rsp` status 1 (Communication Already Active), and closed.
  Previously the peer's connect procedure completed successfully and the socket then closed with no Select
  attempted and no status returned — indistinguishable, from the peer's side, from a random dropped
  connection.
- **`hsmsss`: `ConnectionMetrics.LinktestErrCount` no longer counts a linktest aborted by connection
  teardown.**
  A `Close` (or a Deselect/drop) racing an in-flight auto-linktest could abort its reply wait before the
  teardown was observably in progress, and the old classification could not tell that abort apart from a
  genuine round-trip failure, so it was counted as one.
  Only a genuine round-trip failure — a T6 timeout, or a write error on a still-live link — counts now.

### Added

- **`hsms.MaxMessageSize`** — the exported name for the on-wire frame-size ceiling
  (10-byte header + body) that the receive path already enforced and the send path now enforces too.
- **`ConnectionMetrics.ReplyMismatchCount`** — counts a reply whose Stream or Function does not match its
  primary's, per SEMI E37 §9.4.1, even though (by default) the reply is still delivered unchanged.
  A nonzero count means a peer on this connection is not echoing Stream/Function correctly.
- **`DataMessage.TrailingBytes()` / `DataMessageCodec.TrailingBytes()`** — the number of bytes past the first
  complete SECS-II item in a message body.
  Delivery is unchanged: the first item is still decoded and delivered normally.
  A nonzero count means a peer is sending bodies with more than one item's worth of bytes,
  which SEMI E5 §10.3.1.3(2) forbids but which this library has never rejected and still does not.
- **`hsms.WithStrictReplyMatching`** — opt-in enforcement of the SEMI E37 §9.4.1 Stream/Function check that
  `ReplyMismatchCount` only observes by default.
  With it enabled, a mismatched candidate is a **miss** rather than a delivered reply: the message falls
  through to the session's data handlers as unsolicited, and the original transaction keeps waiting for a
  conforming reply until it hits **T3**.
  **Enabling this against a peer that is sloppy about Stream/Function turns its previously-working (if wrong)
  replies into T3 stalls — the first symptom is a hang, not an error.**
  Soak under the default first and watch `ReplyMismatchCount`; enable strict mode only once it holds at zero.
  Full SEMI E37 §9.4.1 enforcement is this option plus the existing `WithSessionIDValidation`.
- **`hsms.ErrMessageTooLarge`, `ErrEvenFunctionPrimary`, `ErrAsyncReplyExpected`, `ErrControlFrameWithBody`**
  — new error sentinels for the four send/decode rejections above and below.

### Changed

- **`hsms.NewRejectReq` and `hsms.NewRejectReqRaw` now return `(*ControlMessage, error)`** instead of a bare
  `*ControlMessage` — **a compile-time break**; every call site needs a second return value.
  Both now reject a zero reason code with `ErrInvalidRejectReason`, since SEMI E37 §8.3.21.3 never permits one.
- **`secs2.DecodeOwnedFrame` now returns `(Item, int, error)`** instead of `(Item, error)` — **a compile-time
  break**; the added `int` is the new trailing-byte count described under `TrailingBytes()` above.
- **`hsms.SendDataMessageAsync` now refuses `replyExpected: true`**, returning the new
  `hsms.ErrAsyncReplyExpected`.
  The asynchronous send path never began a T3 reply timer for a W-bit primary, which SEMI E37 §9.4.1.2 requires
  unconditionally; callers that need a reply-bearing transaction must use `SendDataMessage` instead.
- **`ConnectionMetrics.DataMsgErrCount` no longer counts a send rejected by the new `MaxMessageSize` check.**
  An oversized-message rejection never touches the wire — it is a caller-side condition, not a transport
  failure — so counting it would have inflated a transport/protocol-health signal with an application bug.

## [2.2.0] - 2026-08-11

Conformance release.
HSMS-SS control-message handling is brought in line with SEMI E37.1, the subsidiary standard,
after a field report that the active Select.req carried the wrong session ID.
Two of the changes are visible on the wire;
read the `Reject.req` and `Separate.req` entries before upgrading.

### Fixed

- **`hsmsss`: HSMS-SS control messages now always carry SessionID `0xFFFF`.**
  The active-role `Select.req` and the graceful-close farewell `Separate.req` were both sent with the configured session ID (`hsms.WithSessionID`).
  SEMI E37.1 §7.1.1, §7.6, and §8.1 require `0xFFFF` for *every* HSMS-SS control message;
  the configured session ID is the **device ID** and belongs in data messages only (§7.2).
  Equipment that enforces the rule rejected the `Select.req` and closed the socket whenever the configured session ID was not `0xFFFF` (e.g. a host using device ID `0`),
  producing an endless `NotSelected` → `NotConnected` reconnect loop.
  This was a regression from v1.17.1, which hard-coded `0xFFFF` at both sites.
  Data messages, `Select.rsp` (which echoes the request's session ID per E37 §8.3.7.1), and `Linktest.req` were never affected.
  `Reject.req` was affected too, and is covered by its own entry below.
- **`hsmsss`: `Reject.req` now carries SessionID `0xFFFF` instead of the rejected message's.**
  All three reject paths echoed the offending frame's session ID, per SEMI E37 generic §8.3.21.1
  (unsupported PType/SType, data-while-not-Selected, and orphan control response).
  E37.1 §8.1 admits no exception for HSMS-SS: every control message carries `0xFFFF`.
  This is wire-visible.
  A peer that correlated our Rejects by session ID will now see `0xFFFF` rather than its own device ID.
  Correlation by System Bytes is unchanged: they are still echoed verbatim (§8.3.21.4),
  as is the offending PType/SType in header byte 2 (§8.3.21.2).
- **`hsmsss`: a peer `Separate.req` on an already-cancelled generation no longer injects a disconnect.**
  `connection.TCPDown` resolves the current epoch and supervisor at call time,
  so a Separate read by a generation whose teardown had already begun could inject `evDisconnect` into the *next* generation
  and knock it out of `NotSelected`.
  The receive loop already guarded read errors this way; the dispatch path did not.
  Honoring E37.1 §7.6 in every substate is what made it reachable.
  **This narrows the window; it does not close it.**
  The guard checks the generation context and then calls `TCPDown` as two separate steps,
  so a cancellation landing between them can still reach a successor.
  Closing that requires queued FSM events to carry epoch identity and be revalidated when the supervisor processes them.
  That affects every `TCPDown`/`T7Expired` producer and is tracked as separate work;
  the same check-then-call shape has always been the receive loop's read-error guard.
  See `docs/specs/e37-1-hsms-ss-conformance-audit.md`, Gap 2.
- **`hsmsss`: a `Separate.req` received while NotSelected now closes the connection.**
  It was ignored, following SEMI E37 generic §7.9.2.3 ("If the responding entity is not in the SELECTED state, the Separate.req is ignored").
  E37.1 §7.6 overrides that for HSMS-SS:
  the Separate.req is valid in the TCP/IP CONNECTED state *and its substates*,
  and after receiving one the entity "shall immediately close the TCP/IP connection".
  NOT SELECTED is such a substate.
  In practice the peer usually closed its socket right after, so the link dropped on EOF anyway;
  a peer that sent Separate and held the socket open was previously reaped only by the T7 dwell.
  `ConnectionMetrics.SeparateRecvCount` now counts a Separate received in any substate, not only while Selected.

### Added

- **`hsms.ControlSessionID`** — the `0xFFFF` session ID that SEMI E37.1 §8.1 mandates for HSMS-SS control messages.
  Exported so callers constructing control messages directly do not have to repeat the literal.

## [2.1.0] - 2026-08-01

### Added

- `hsms`: `WithLinktestSuppression` (default enabled) — activity-based suppression of the
  automatic linktest. Each probe opportunity is skipped when it observes recent line traffic or
  an outstanding data reply, and a probe timeout is not counted toward the linktest fail
  threshold when the failure evaluation observes signs of life (liveness credit). This restores
  and extends v1's linktest suppression (v1 commit `77262a6`). See the `WithLinktestSuppression`
  godoc for the precise guarantees and bounds.
- `hsmsss`: `ConnectionMetrics` gains `LinktestSuppressedCount` and `LinktestCreditedCount` to
  observe suppression and credit activity.

### Changed

- Behavior (on by default): on busy connections, `LinktestSendCount`/`LinktestRecvCount` stop
  climbing (probes are suppressed while traffic flows), and `LinktestErrCount` can grow without a
  disconnect when failures are credited. Dashboards or alerts built on the old always-probing
  cadence must account for this or set `WithLinktestSuppression(false)`.
- Detection bound: a silent dead link is still dropped within approximately
  `threshold × (interval + T6)` of its last sign of life; with a data-message reply outstanding,
  T3 applies first. Rare races can extend this: up to one extra probe cycle, and up to double
  immediately after a reconnect — see the `WithLinktestSuppression` godoc for the precise bounds.

## [2.0.1] - 2026-07-11

Performance-only patch release. No public API changes; behavior is
byte-identical to `2.0.0` across SECS-II wire encoding, error semantics, and
item construction — every change in this release is validated against a
parity oracle or an existing behavior-preservation test suite.

### Changed

- `secs2`: `ListItem.Error()` is now O(1) for valid built-in item trees via a
  known-clean flag computed at construction, replacing a per-call recursive
  walk.
- `secs2`: decoded scalar numeric items (`IntItem`/`UintItem`/`FloatItem`,
  single-value) and decoded `ASCIIItem`/`JIS8Item`/`LocalizedStrItem`/
  `BinaryItem`/scalar `BooleanItem` are now carved from geometrically-sized
  slab chunks instead of individually heap-allocated, cutting decode-side
  allocation count substantially on large item trees.
- `secs2`: `NewIntItem` gained a scalar fast path that skips an intermediate
  slice allocation for single-value construction.
- `secs2`: the SECS-II item header encoder no longer looks up a string-keyed
  type map on the encode and construction hot paths, removing a measurable
  CPU cost on large item trees.

On a profiled 32-way concurrent HSMS-SS round trip with a 100,000-leaf
payload, this release's combined changes measured -33.7% ns/op and -74.6%
allocs/op against `2.0.0`, with no regression on small-message benchmarks.

## [2.0.0] - 2026-07-09

First stable release of the **v2 major version** — a ground-up redesign around immutable messages
and items, published at a new module path (`github.com/arloliu/go-secs/v2`) so v1
(`github.com/arloliu/go-secs`) keeps working unchanged. This is not an in-place upgrade; it is a
new major-version module you opt into. See
[`docs/migration-v1-to-v2.md`](docs/migration-v1-to-v2.md) for the full symbol-by-symbol migration
guide and worked examples.

This GA is identical in library behavior to `2.0.0-rc6`; it promotes the release candidate line to a
stable, semver-guaranteed public API. The complete set of changes that make up v2 is recorded in the
release-candidate entries below:

- **[2.0.0-rc1]** — the redesign itself: immutable/GC-owned messages and items, connection-centric
  API (`hsms.Connection` as the SECS-II endpoint, no `Session`/`AddSession`), context-first sends,
  typed item access, `ID()` → `SessionID()`, and the split metrics taxonomy.
- **[2.0.0-rc2]–[2.0.0-rc5]** — gap-closure from downstream migrations: restored v1-parity helpers,
  cold-peer active-connect retry and a bounded active-connect timeout, undecodable-inbound signalling
  (`hsms.DecodeErrorHandler`), codec read-duality, `secs2` deferred-error ergonomics, and the
  generated GEM (SEMI E30) builder surface (package `gem`, `tools/gemgen`).
- **[2.0.0-rc6]** — final ergonomics round: `(*hsms.DataMessage).WithID` / `WithID` builder wither,
  a canonical `Is*`/`To*` type-dispatch example, and test/CI hardening.

There are no library changes since rc6.

## [2.0.0-rc6] - 2026-07-09

Sixth release candidate of the v2 major version. A small ergonomics round surfaced by a second,
independent downstream migration of the same device: one additive builder method plus a canonical
dispatch reference. There are no library behavior changes; the remaining commits since rc5 are
test/CI hardening.

### Added

- **`(*hsms.DataMessage).WithID(id uint32)`** and **`(*hsms.DataMessageBuilder).WithID(id uint32)`** —
  the immutable-wither counterpart to the `ID()` accessor (exactly
  `WithSystemBytes(ToSystemBytes(id))`), completing the wither surface alongside `WithSessionID` and
  `WithSystemBytes`. This is the immutable replacement for v1's removed `(*DataMessage).SetID`: the
  message's System Bytes are set from `id` (big-endian), sharing the body with no re-encode.

### Documentation

- **`secs2.ExampleItem_typeDispatch`** — a canonical, exhaustive `Is*`/`To*` type-dispatch example for
  turning an arbitrary `secs2.Item` into a concrete Go value. Every branch reads through the typed
  `To*` accessor and propagates that accessor's deferred error (never discards it), and every string
  family — ASCII, JIS8, and LocalizedStr — is covered, giving hand-rolled dispatch code an
  authoritative reference to diff against.

### Changed

- Internal test/CI hardening only, with no library-behavior change: a lifecycle state-notification
  test was relaxed to match the engine's documented best-effort (drop-OLDEST coalescing) contract for
  intermediate state changes; `make stress-test` again includes the Fuzz seed corpora (the stale
  `-skip '^Fuzz'` was removed after confirming the current harness cannot hang); and v1↔v2 benchmark
  compatibility tests were added.

## [2.0.0-rc5] - 2026-07-08

Fifth release candidate of the v2 major version. Introduces a generated GEM (SEMI E30) message
builder surface produced by a new code generator, and closes six gaps plus documentation drift
identified during a downstream migration: signalling for undecodable inbound messages, a bounded
active-connect timeout, codec read-duality, deferred-error ergonomics on `secs2` accessors, and
SECS-I documentation corrections.

### Added

- **Generated GEM message builders (package `gem`).** Over 130 pure value builders covering SEMI
  E30 role messages for Streams 1, 2, 5, 6, and 9 (e.g. `gem.S1F1()`, `gem.S2F31(...)`,
  `gem.S6F11(...)`), each returning a `secs2.SECS2Message` sendable via
  `Connection.SendSECS2Message` on either transport. Equipment-defined identifiers are passed as
  `secs2.Item` values so callers control the SECS-II type. The builders are generated from an
  E5 data dictionary and a message DSL by the new **`tools/gemgen`** generator (its own Go module,
  wired into `go:generate`, lint, test, and CI).
- **`hsms.DecodeErrorHandler`** and **`SECS2Endpoint.AddDecodeErrorHandler(...)`** — opt-in
  handling for an inbound data message whose SECS-II body fails to decode. When at least one
  handler is registered, an undecodable primary is diverted to it (and counted, see below) instead
  of silently reaching the normal `DataMessageHandler`s; with none registered, body decoding stays
  lazy and routing is unchanged. The handler runs inline on the receive goroutine and must not
  block, matching the `DataMessageHandler` contract.
- **`hsms.ConnectionMetrics.BodyDecodeErrCount()`** — counts inbound data messages that framed and
  were received but whose lazy SECS-II body failed to decode and were diverted to a
  `DecodeErrorHandler`. Intentionally disjoint from `DecodeErrCount` (frame-level failures), so
  neither double-counts the other.
- **`hsmsss.WithConnectTimeout(d)`** / **`secs1.WithConnectTimeout(d)`** — bound each active-role
  dial attempt with a per-attempt deadline, including background reconnect attempts. The default
  (`0`) leaves the dial unbounded (prior behavior); the option composes with `WithDialer`.
- **`*hsms.DataMessageCodec` read delegators** — `Stream`, `Function`, `WaitBit`, `SessionID`,
  `ID`, `SystemBytes`, `HeaderBytes`, `ToBytes`, `Type`, `DecodeErr`, `Item`, plus a nil-safe
  `ToDataMessage()`, so a codec can be inspected without unwrapping `.Message`. Scalar/byte reads
  on a nil `Message` return the zero value (panic-free); `Item()`/`DecodeErr()` return
  `ErrNilMessage` rather than a misleading nil.
- **`hsms/hsmstest.MalformedDataMessage(stream, function, waitBit)`** — a test helper producing a
  `*hsms.DataMessage` with valid HSMS framing but an undecodable body, for exercising decode-error
  handling without hand-forging wire bytes.

### Changed

- **`SendDataMessage` / `SendSECS2Message` now surface an undecodable reply.** When the reply
  frames but its SECS-II body fails to decode, they return `(reply, decodeErr)` — the message
  alongside the error — instead of `(nil, nil)`, so a malformed reply no longer unblocks the
  caller as a clean success. A timeout or control-message reply is unaffected, and a well-formed
  reply still returns `(reply, nil)`. `hsms/hsmstest.FakeEndpoint`'s scripted-reply path mirrors
  this behavior.
- **The exported `hsms.SECS2Endpoint` interface gained `AddDecodeErrorHandler`.** Code that merely
  consumes a `SECS2Endpoint`/`Connection` is unaffected; any external type that *implements* the
  interface must add the method.

### Fixed

- **`secs1` package documentation** corrected: the `WriteTimeout` paragraph now states that
  `UpdateConfigOptions` re-forces `WithWriteTimeout(0)` on every live update (matching what
  `NewConfig` forces at construction), rather than claiming it is not intercepted. The
  block-assembly mismatch metrics are now documented, and the note clarifies that the
  assembler-violation notifications (S9F1 for a device-ID mismatch, S9F7 for block/header/
  first-block violations) are sent for the equipment role only.

### Documentation

- **`secs2` typed accessors and predicates** (`To*` / `Is*`) now warn in their Godoc that they do
  not surface an item's deferred error: `Is*` reflects the declared type only, and a passing
  predicate does not imply a usable item. Callers should gate value extraction on `Error()` (or the
  `To*` accessor's returned error). A runnable example demonstrates the safe pattern.

## [2.0.0-rc4] - 2026-07-06

Fourth release candidate of the v2 major version. A correctness fix for SECS-I live config
updates, plus small API symmetry additions found during downstream migration.

### Fixed

- **`secs1.UpdateConfigOptions` now re-forces `WriteTimeout=0` on every live update**, matching
  what `NewConfig` already forces at construction. Previously, a runtime
  `WithConnectionOption(hsms.WithWriteTimeout(d))` could arm a nonzero write deadline that fires
  mid-transaction, since SECS-I's own T1/T2/T4 timers — not the socket write deadline — are meant
  to govern block-transfer pacing.

### Added

- **`hsmsss.WithListener(ListenFunc)`** / **`secs1.WithListener(ListenFunc)`**, symmetric to the
  existing `WithDialer(DialFunc)` — override how a passive-role connection listens for inbound
  connections (e.g. routing through a test proxy or a non-default network), the listener-side
  counterpart to the dialer override.
- **`(*hsms.DataMessageBuilder).WithSessionID`/`WithSystemBytes`** — restamp the session ID or
  system bytes on a message mid-derivation, before `Build()`, without a separate restamp call on
  the built result.
- **`hsms/hsmstest.RequireBuild(t, builder)`** — calls a `*DataMessageBuilder`'s `Build()`, fails
  the test via `require` on error, and returns the built `*DataMessage`, for tests that don't need
  to handle the build error themselves.
- **`hsms/hsmstest.HeaderOnly()`** — `RequireDataMessageEqual` option comparing only the header
  (stream, function, wait bit, session ID, System Bytes) and skipping the body entirely; the mirror
  image of the existing `BodyOnly()`.

## [2.0.0-rc3] - 2026-07-06

Third release candidate of the v2 major version. Closes four more gaps identified during a
downstream migration, plus a small API convenience addition.

### Added

- **`hsms.WithReconnectBackoff(initial, multiplier)`** configures the reconnect loop's per-attempt
  backoff curve (initial delay and growth multiplier); attempts still ramp up to no more than the
  `T5` ceiling.
- **`secs1.WithT5`**, for symmetry with `WithT1`/`WithT2`/`WithT4` — SECS-I now has a build-time
  option for the reconnect-backoff ceiling; previously only the runtime-wrapper form
  `WithConnectionOption(hsms.WithT5(d))` was available.
- **`hsms.WithAsyncSendErrorHandler`** and **`hsms.ConnectionMetrics.AsyncSendErrCount()`** surface
  transport write failures on the fire-and-forget async send path (`SendAsync` /
  `ForwardDataMessageAsync`, plus internal control-message async sends such as Reject/Select.rsp),
  which were previously silent.
- **`(*hsms.DataMessage).Codec()`** wraps a message in a `*hsms.DataMessageCodec`, for storage
  layers requiring `encoding.BinaryMarshaler`/`BinaryUnmarshaler` — avoids every consumer
  hand-rolling `&hsms.DataMessageCodec{Message: msg}`.

### Changed

- **An active connection's `Open(ctx, hsms.OpenBackground)` no longer fails when the peer is
  unreachable at Open time** (TCP never came up) — it now retries in the background instead,
  matching v1 behavior. `OpenWaitSelected` is unaffected: a first-dial failure is still returned
  synchronously.
- **The reconnect loop's per-attempt wait is now an exponential backoff capped at `T5`**, not a
  flat `T5` sleep, so recovery after a brief drop starts sooner while the long-run retry cadence
  still respects the configured `T5` ceiling.

## [2.0.0-rc2] - 2026-07-05

Second release candidate of the v2 major version. Restores six behavioral capabilities
identified as gaps between v1.17.1 and 2.0.0-rc1 during a downstream migration, closing out
the rc1 → rc2 gap list:

### Added

- **Equipment-role auto-notification on reply timeout.** `hsmsss.WithEquipRole()` /
  `hsmsss.WithHostRole()` (mirroring `secs1.WithEquipment()` / `secs1.WithHost()`, which now
  also enable/disable this) restore v1's behavior: a synchronous data-message send whose reply
  times out (T3) on an equipment-role connection automatically notifies the peer with an S9F9
  (SEMI E5 §10.13), on both transports. Backed by the new shared primitive
  `hsms.WithAutoS9F9(bool)`.
- **SECS-I inbound assembler protocol-violation notifications.** The multi-block reassembler now
  validates the inbound block's device ID against the connection's configured device ID (SEMI E4
  §9.4.1) and, on an equipment-role connection, answers a device-ID mismatch with S9F1 and a
  block-number/header-mismatch or invalid-first-block violation with S9F7. New metrics:
  `secs1.ConnectionMetrics.DeviceIDMismatchCount()`, `BlockNumberMismatchCount()`,
  `InvalidFirstBlockCount()`.
- **`hsms.WithTraceTraffic(bool)`** enables per-frame wire-level tracing (hex dump, Debug level)
  for both outbound sends and inbound receives, including decode failures — usable by both
  transports (HSMS-SS additionally traces inbound control frames).
- **`hsms.ConnectionMetrics.DecodeErrCount()`** counts inbound frames that were read successfully
  but failed to decode. **`hsmsss.ConnectionMetrics.ReadErrCount()`** counts socket read /
  frame-length errors in the HSMS-SS receive loop.
- **`hsms.DataMessageCodec`** implements `encoding.BinaryMarshaler` / `encoding.BinaryUnmarshaler`
  by wrapping a `*hsms.DataMessage`, for storage layers whose contract requires those interfaces
  (v1's `*DataMessage` satisfied them directly; v2's `DataMessage` stays fully immutable, so this
  is a separate wrapper rather than new mutating methods).
- **`hsms.GenerateMsgSystemBytes()`** and **`hsms.Message.ToDataMessage()`**, both dropped in the
  rc1 rewrite, are restored (same semantics as v1; `GenerateMsgSystemBytes` now returns `[4]byte`).

## [2.0.0-rc1] - 2026-07-05

First release candidate of the v2 major version. **v2 is a ground-up redesign
around immutable messages and items**,
published at a new module path (`github.com/arloliu/go-secs/v2`) so v1
(`github.com/arloliu/go-secs`) keeps working unchanged for existing consumers — this
is not an in-place upgrade, it's a new major-version module you opt into. See
[`docs/migration-v1-to-v2.md`](docs/migration-v1-to-v2.md) for the full
symbol-by-symbol migration guide and worked examples; the highlights:

### Changed

- **Every `hsms.Message` / `secs2.Item` is immutable and GC-owned.** No mutation
  after construction, safe to share across goroutines without locking, cloning, or
  reference counting. Structural changes go through a validating builder:
  `msg.Derive().WithStream(s).WithFunction(f).WithWaitBit(w).Build() (msg, error)`.
  Envelope-only restamps (`WithSessionID`, `WithSystemBytes`) return a new message
  that shares the original body at zero re-encode cost.
- **`hsms.Connection` is the SECS-II endpoint** — no more `Session` /
  `AddSession`. `hsmsss.New` / `secs1.New` return `hsmsss.Connection` /
  `secs1.Connection`, both embedding `hsms.Connection`. Send/reply methods are
  context-first (`conn.SendDataMessage(ctx, ...)`); `Open(ctx,
  hsms.OpenWaitSelected|hsms.OpenBackground)`; handlers are
  `func(*hsms.DataMessage, hsms.SECS2Endpoint)`.
- **Identity renamed `ID()` → `SessionID()`** across `hsms`/`hsmsss`/`secs1`;
  `secs1.Connection.SessionID()` now reports the wire device ID.
- **SECS-II item access is typed**, not `Values() any` — `ToASCII()`, `ToInt()`,
  `IntAt(i)`, `Ints()`, and friends. Item shortcuts (`secs2.A`, `secs2.L`, `secs2.J`,
  `secs2.W`, `secs2.B`, `secs2.BOOLEAN`) are now functions, not package variables.
- **Metrics taxonomy split by ownership.** `conn.Metrics()` returns the shared
  `hsms.ConnectionMetrics`; HSMS-SS-only counters (linktest, Select/Separate/Reject)
  moved to `hsmsss.ConnectionMetrics` via `conn.ControlMetrics()`; SECS-I block-level
  counters are new, via `conn.BlockMetrics()` on `secs1.ConnectionMetrics`.
- **SECS-I T1/T2/T4 timers unified into the shared `hsms.TimerConfig`**, so they're
  live-updatable through the same config-swap path as T3/T5–T8.
- Go floor raised to `1.26.0`; `sml.Parse`/`ParseStrict` replace `sml.ParseHSMS`;
  `logger.MockLogger` moved to `logger/loggertest.MockLogger`; `logger.GetLogger()`
  → `logger.Default()`; `secs1.WithIsEquip` → `secs1.WithEquipment()` /
  `secs1.WithHost()`.
- **Binary (`B`) items render as hex by default in SML**, matching v1's exact
  `0x%02X` format (`secs2.BinaryItem.ToSML()` and `sml.Encoder`'s default). The
  previous binary-literal (`0b...`) form is opt-in via `sml.WithBinaryStyle(sml.BinaryLiteral)`.
  The parser reads both forms regardless of the encoder's chosen style.
- **Single-value `IntItem`/`UintItem`/`FloatItem`/`BooleanItem` decode without allocating a backing
  slice.** A scalar fast path in `secs2.Decode`/`DecodeOwned` stores the lone value inline instead of
  building a one-element `[]T`: ~18-20% faster and one fewer allocation per scalar item, up to ~42%
  fewer allocations on payloads dominated by single-value items. No API change. Hot atomic counters
  in `hsms.ConnectionMetrics`/`secs1.ConnectionMetrics` are also cache-line padded to prevent false
  sharing under concurrent access.

### Added

- **`secs2.DecodeOwned`** — public zero-copy decode for callers that already own
  the input buffer and won't mutate it after the call.
- **`hsms.FromSystemBytes(b [4]byte) uint32`** (the inverse of `ToSystemBytes`) and
  **`DataMessage.ID()` / `ControlMessage.ID() uint32`** — a numeric message-ID
  accessor decoding System Bytes, restoring v1's `msg.ID()` ergonomics.
- **`hsms.NewEmptyDataMessage()`** — a zero-value baseline `*DataMessage` (stream
  0, function 0, no wait bit, session ID 0, empty body) for tests that don't need
  the full `NewDataMessage` argument list.
- **`secs2.Equal(a, b Item) bool`** — semantic SECS-II item equality: same
  declared type/width and decoded logical value, immune to internal wire-encoding
  differences (non-canonical length fields, F4/F8 storage precision) that make
  `reflect.DeepEqual`/`ToBytes()` comparison unsafe.
- **`(*hsms.DataMessage).Equal(other *DataMessage) bool`** — header + semantic
  body equality (via `secs2.Equal`), immune to the lazy-decode `sync.Once`
  ordering hazard that makes `reflect.DeepEqual` on two `*DataMessage` unsafe.
- **`hsms.DecodeHSMSPayload` / `DecodeOwnedHSMSPayload`** — decode a `[header ||
  body]` payload with no 4-byte length prefix (copying and zero-copy variants,
  mirroring `secs2.Decode`/`DecodeOwned`), and **`hsms.NewDataMessageFromHeader`**
  — build a `*DataMessage` from an already-formed 10-byte header plus a decoded
  item.
- **`sml.EncodeMessage` / `sml.MustEncodeMessage`** — package-level shortcuts for
  `NewEncoder().EncodeMessage(msg)`; `MustEncodeMessage` returns a
  `"<!sml encode error: ...>"` diagnostic string on error instead of panicking, so
  it's always safe to embed in a log line.
- **`hsms/hsmstest` package** — `FakeEndpoint`, an in-memory `hsms.SECS2Endpoint`
  for tests (records sends, scripts replies, delivers inbound messages to
  registered handlers, no TCP connection), plus `RequireDataMessageEqual` /
  `IgnoreSystemBytes` / `IgnoreSessionID` / `BodyOnly` assertion helpers built on
  `DataMessage.Equal`. Import only in test code, like `logger/loggertest`.
- **`hsms.WithSessionIDValidation`** (default off) — opt-in inbound SessionID
  mismatch check at the connection's chokepoint, replying `S9F1` and dropping the
  frame on mismatch.
- **`hsms.SECS2Endpoint`: `ForwardDataMessage` / `ForwardDataMessageAsync`** — send
  an already-built `*DataMessage` verbatim (System Bytes, W-bit, session ID, and
  stream/function preserved) without registering a reply expectation, so the
  correlated secondary is delivered to the registered `DataMessageHandler`s instead
  of being consumed by the send. For routers/proxies that forward messages carrying
  externally-assigned System Bytes and own reply correlation themselves; ordinary
  request/reply code keeps using `SendDataMessage`.
- **`gem` package expanded** to a bounded SEMI E30/E5 builder set returning
  `secs2.SECS2Message`: `S1F1/2` (+ host forms), `S1F13/14` (+ host forms),
  `S2F17/18`, `S2F31/32`, `S2F37/38`, `S5F1/2`, `S6F11/12`, and a `Report` helper.
  `S9Fx` error-report builders are now E5-faithful (carry the offending message
  header / system bytes).
- **`hsmsss`/`secs1`: `WithDialer(DialFunc)`** for a custom active-side dialer
  (e.g. routing through a test proxy or a non-default network).
- A deterministic, network-free integration test suite (`integration/`) driving
  real HSMS-SS and SECS-I clients over `net.Pipe`, and a standalone `benchmarks/`
  module comparing v1 against v2 (headline: every full-connection round trip is
  15%–61% faster in v2).
- Top-level `README.md` rewritten for the v2 API, with every code example
  compile-checked against the module.

### Removed

- **The entire v1 pooling API**: `Free`, `UsePool`/`IsUsePool`,
  `GetMessageBuffer`/`PutMessageBuffer`, `Clone`, `CloneCodec`, `SnapshotForRelay`.
  Messages and items are ordinary GC-owned values — the whole class of
  use-after-free / double-free / retain-past-`Free` bugs this history kept
  surfacing (see the 1.15.0–1.18.0 entries below) is now structurally impossible.
- `Session` / `AddSession` and the mutable `Set*` methods on messages and items
  (`SetSessionID`, `SetStreamCode`, `item.SetValues`, ...).

## [1.18.0] - 2026-06-27

Adds `DataMessage.CloneCodec` and `DataMessage.SnapshotForRelay` (an opt-in,
relay-optimized snapshot clone), and fixes a data race in `secs2.ListItem.Clone`
exposed on the concurrent fan-out (multi-handler / broadcast) path — closing the
related `ASCIIItem` aliasing hazard and a stale serialization-cache bug. Also
decodes JIS8 and localized-string items, which previously failed to decode.

### Added

- **`hsms`: `DataMessage.CloneCodec`** — returns an independent deep copy of the
  message typed only as the `encoding.BinaryMarshaler`/`BinaryUnmarshaler` pair.
  This lets external code obtain a fan-out-safe deep copy through a capability
  interface it declares itself, without importing this package or depending on
  the concrete `*DataMessage` type. Thin wrapper over `Clone`.
- **`hsms`: `DataMessage.SnapshotForRelay`** — opt-in, relay-optimized snapshot
  that serializes the message once and serves those bytes from `ToBytes` without
  re-encoding, decoding the item structure lazily only if a structural method
  (e.g. `Item`, `ToDataMessage`) is called on the result. For the
  serialize-and-forward (relay / broadcast / shadow) path it is markedly cheaper
  than `Clone` (measured ~4.6× faster for a single large-leaf payload; ~110–200×
  faster for multi-item / nested lists; 2–5 allocations vs 12–2024). It is a
  trade-off, not a free win: if the consumer accesses items, `SnapshotForRelay`
  is *slower* than `Clone` (serialize-at-clone plus lazy-decode-at-access), so
  `Clone` remains the right choice for structural consumers (including the
  in-process handler fan-out path). `Clone`/`CloneCodec` are unchanged
  (structural deep copy). `Item()` on the returned message yields a
  lazily-decoded item — use the `secs2.Item` interface, not concrete-type
  assertions, to remain compatible with future implementations.

### Fixed

- **`hsms`: decode JIS8 and localized-string items.** `DecodeSECS2Item` /
  `DecodeHSMSMessage` previously returned "invalid format" for JIS8 (0o21) and
  localized-string (0o22) payloads; both are now decoded correctly.
- **`secs2`: `ListItem.Clone` now performs a recursive deep copy.** Previously it
  shared child items with the original, which caused a data race when a cloned
  message and its source were serialized concurrently (the fan-out / multi-handler
  path). Clones are now fully independent.
- **`secs2`: `ASCIIItem.SetValues` no longer retains a caller-supplied `[]byte`
  (it now copies the input).** Combined with the deep `ListItem.Clone`, ASCII
  leaves can safely share their now-owned/immutable backing, so common payloads
  (including large ASCII recipes built from strings) still clone without copying
  the data.
- **`secs2`: `SetValues` now invalidates the cached serialization.** Calling
  `SetValues` on an item that had already been serialized via `ToBytes` left the
  stale `rawBytes` cache in place, so the next `ToBytes` returned the previous
  value's bytes. Affects `ASCIIItem`, `BinaryItem`, `BooleanItem`, and `ListItem`.

## [1.17.1] - 2026-06-03

Patch follow-up to v1.17.0 that makes the `DataMsgDropNotSelectedCount`
accounting introduced there exact across every send gate, rate-limits the
not-Selected drop log so a send flood across a disconnect cannot spam it, and
tightens a `secs1` non-data input edge case. No public API additions.

### Fixed

- **`hsmsss` / `secs1`: `DataMsgDropNotSelectedCount` is now incremented exactly
  once on every not-Selected data drop.** v1.17.0 counted the drop at some send
  gates but not others — the `hsmsss` `sendMsg` entry gate and the `secs1`
  `sendMsg` / `sendMsgSync` entry gates dropped data without counting, and the
  `hsmsss` increment lived in the sender task so direct `SendMessageSync` callers
  were uncounted. All not-Selected data drops now route through a single
  per-connection chokepoint, so the metric can neither under- nor double-count
  regardless of which path (sync, async, or the sender-task write boundary)
  rejects the message.
- **`secs1`: a non-data message passed to `SendMessage` / `SendMessageSync` while
  not Selected is now rejected as `ErrNotDataMsg` and no longer increments
  `DataMsgDropNotSelectedCount`.** The message type is validated before the
  Selected-state gate, so an invalid (non-data) input is not counted as a dropped
  data message. Metric-accuracy only — the previously missing `Free` on that path
  was a no-op, since control messages are not pooled.

### Changed

- **`hsmsss` / `secs1`: the not-Selected data-drop log is rate-limited.** A flood
  of sends across a disconnect previously emitted one log line per dropped message,
  at inconsistent severities. Drops are now logged at `Warn` at most once per 5s
  window — the first drop logs immediately, with a periodic heartbeat during a
  sustained outage — while `DataMsgDropNotSelectedCount` still counts every drop
  exactly. `secs1`, which previously logged nothing on these drops, now emits the
  same throttled `Warn`.

### Tests

- Exact-count, teeth-checked regression tests for the single counting chokepoint
  in both packages (every send gate counts exactly once; `DataMsgErrCount` stays
  clean), the `secs1` non-data-before-gate behavior, and the log throttle (a flood
  emits exactly one `Warn` while the metric counts all drops). Adds an internal
  `throttle` helper with unit tests, including a concurrent-burst
  permit-exactly-once check.

## [1.17.0] - 2026-06-02

Fixes a `NotSelected → NotConnected` reconnect loop in active HSMS-SS sessions,
with the same gate gap back-ported to SECS-I. The async send path
(`SendMessageAsync`, and `SendDataMessageAsync` / response sends) enqueued data
messages regardless of connection state, unlike the already-gated synchronous
paths; while not Selected this filled the sender queue with undeliverable data,
choking the sender task and starving the `Select.req` enqueue so the link could
never re-Select. Async data sends while not Selected now return
`ErrNotSelectedState` without enqueuing, with a defense-in-depth backstop in the
`hsmsss` sender task. Adds a dedicated `DataMsgDropNotSelectedCount` metric kept
out of `DataMsgErrCount`. One observable behavior change: `SendMessageAsync` now
returns `ErrNotSelectedState` while not Selected (see **Changed**).

### Fixed

- **`hsmsss` / `secs1`: async sends are now gated on the Selected state.**
  `sendMsgAsync` (used by `SendMessageAsync`, and by `SendDataMessageAsync` /
  response sends) previously enqueued data messages regardless of connection
  state, unlike the already-gated `sendMsg` / `sendMsgSync`. While the link was
  not Selected this let undeliverable data pile into the sender queue and choke
  the sender task / starve the `Select.req` enqueue, producing a
  `NotSelected → NotConnected` reconnect loop that never reached Selected. Async
  data sends while not Selected now return `ErrNotSelectedState` without
  enqueuing. As defense-in-depth, the `hsmsss` sender task no longer treats
  `ErrNotSelectedState` on an already-queued message (the residual
  check-then-enqueue race) as fatal — it drops the message and stays alive
  instead of tearing the connection down.
- **`secs1`: `sendMsgSync` now frees the message on its not-Selected gate**,
  mirroring `sendMsg`, fixing a pooled-item leak when sending while not Selected.

### Added

- **`ConnectionMetrics.DataMsgDropNotSelectedCount`** (`hsmsss` and `secs1`):
  counts outbound data messages dropped because the connection was not Selected
  (expected backpressure, e.g. an application that keeps sending across a
  disconnect). These drops are intentionally **not** counted under
  `DataMsgErrCount`, which remains a clean protocol / I-O fault signal.

### Changed

- Behavior change: `SendMessageAsync` (and the async / response send paths) now
  return `ErrNotSelectedState` when invoked while the connection is not Selected,
  matching the long-standing contract of the synchronous send paths. Callers that
  relied on the previous silent enqueue should handle this error (retry once
  Selected, buffer, or drop).

## [1.16.2] - 2026-05-24

Follow-up to v1.16.1 closing residual flakes that surfaced under
post-merge stress runs and adding a focused performance pass on the
HSMS `DataMessage` hot path. The dominant **Fixed** item is a
`send on closed channel` panic window plus five orphan-leak defects in
`hsmsss`'s reply-channel teardown (`replyMsgChans` / `replyErrs`),
followed by a ~0.4% close-timeout flake that required coupled fixes in
both the passive-side reconnect guard (`hsmsss`, `secs1`) and
`hsms.ConnStateMgr.Stop`'s desired-state reset. On the **Changed**
side, `hsms.DataMessage.systemBytes` is now an inline `[4]byte`,
removing one heap allocation per message on both the send-construction
and decode paths while keeping the struct in one 64-byte cache line;
`BaseSession`'s four send constructors take the ID as `uint32` via a
new package-internal `newDataMessageWithID`. The pooled `hsmsDecoder`
is now scrubbed before return so spiky load no longer pins the
most-recently-decoded body in memory. The item-ownership contract on
`Session`'s send methods is documented explicitly in Godoc.

No public API additions. The only observable behavior change is the
`ConnStateMgr.Stop` desired-state reset; existing callers that follow
the (now-explicit) ownership contract are unaffected by the
`systemBytes` representation change.

### Changed

- `hsms`: `DataMessage.systemBytes` is now an inline `[4]byte` instead
  of a heap-allocated `[]byte`. `SystemBytes()` returns a slice that
  aliases the struct's array — callers must not retain it past `Free`
  (the new Godoc lifetime note makes this explicit). All public
  constructors and `Clone` preserve prior nil/short-input zero-fill
  semantics; one heap allocation per message is removed on both send
  and decode.
- `hsms`: new package-internal `newDataMessageWithID(stream, function,
  wbit, id, item)` writes the ID directly into the inline array via
  `binary.BigEndian.PutUint32`, skipping the 4-byte slice that
  `GenerateMsgSystemBytes` would otherwise materialize. `BaseSession`'s
  `SendDataMessage`, `SendDataMessageAsync`, `SendSECS2Message`, and
  `SendSECS2MessageAsync` now use it. `NewDataMessage` and
  `GenerateMsgSystemBytes` remain for back-compat.
- `hsmsss`: data-message send hot path — `queueSendRequest` attempts a
  non-blocking enqueue first and only acquires the `SendTimeout`
  pooled timer when `senderMsgChan` is full; `sendMsg`'s W-bit branch
  defers the `hsms.MsgInfo` `[]any` allocation to the T3/T6 timeout
  branch (built via the new `hsms.MsgInfoFromFields` from primitives
  captured before ownership transfer); `recvDataMsg` adds a
  single-handler fast path that skips the snapshot + clone loop for
  the overwhelmingly common N==1 case. The analogous
  `replyToSender` fast path is deferred — see *Known follow-ups* at
  the end of this entry.
- `hsms`: the pooled `hsmsDecoder` is now scrubbed before return —
  `decoder.input` is cleared and the bool/int/uint/float scratch
  buffers are resliced to `:0` — so a pooled slot no longer pins the
  most-recently-decoded body and scratch contents in memory. Scratch
  capacity is retained for warm reuse.
- `hsms`: `Session.SendDataMessage`, `SendDataMessageAsync`,
  `SendSECS2Message`, `SendSECS2MessageAsync`, and `ReplyDataMessage`
  now document the item-ownership contract explicitly in Godoc — the
  caller transfers ownership of `dataItem` on call and must not retain
  or `Free` the item, regardless of the returned error. The behavior
  has always been this way; the docs make it unambiguous so callers do
  not invent retry idioms that race the library. The `hsmsss/doc.go`
  echo example and `hsmsss/session.go` `AddDataMessageHandler` example
  now `Clone` `msg.Item()` before passing to `ReplyDataMessage`, and
  the same fix is applied to `examples/device` and
  `examples/secs1_device` echo handlers.
- `secs2`: `StringToBytes` and `BytesToString` now carry SAFETY
  documentation describing the zero-copy invariants the decoder
  depends on — the decoder uses `BytesToString` to expose decoded
  ASCII items as views into the inbound message buffer, which is safe
  today because that buffer is freshly allocated per message and never
  written after decode. Documented so future buffer-pooling work does
  not silently break it.

### Fixed

#### hsmsss — reply-channel teardown panic & orphan leaks

- Replace `close(replyChan)` in `dropAllReplyMsgs` with a
  drain-then-`nil`-send pattern. The previous close raced with
  `replyToSender` / `replyErrToSender`, which Load the channel before
  the send: a Clear-Close interleave produced a `send on closed
  channel` panic. `sendMsg` already treats a `nil` receive as terminal
  (`conn.go:840-852`), so the wake-up semantic is preserved.
- Close five orphan-leak vectors uncovered by the same investigation,
  each independently regression-tested in `reply_close_race_test.go`:
  (1) `replyToSender` post-send: identity-compare against
  `replyMsgChans[id]` after a successful send, drain the orphan and
  `Free` it if `dropAllReplyMsgs.Clear()` interleaved between Load and
  send. (2)/(3) `replyErrToSender`: `Delete(id)` from `replyErrs` in
  both the `ctx.Done` and send-branch wins paths if the Store landed
  in a cleared map. (4) `sendMsg` terminal-branch TOCTOU: a new
  package-internal `drainOrphanReply` helper, deferred at the top of
  `sendMsg` right after `addReplyExpectedMsg`, catches the orphan on
  every terminal exit (`ctx.Done` / `sendErr` / T3-T6 / dropped) and
  is a no-op on the reply-success path. (5) `drainOrphanReply`
  additionally `Delete`s `replyErrs[id]` when it consumes a `nil`
  sentinel, eliminating the symmetric strand where `replyErrToSender`
  stored an err that `sendMsg`'s terminal branch ate before the
  reply-case fired.

#### hsms, hsmsss, secs1 — close-timeout flake under -race

- `TestConnection_CloseTCPIdempotent` was failing ~0.4% of the time
  under `-race -p $(nproc)` with `desiredState=connecting` observed
  after `Close`. Two coupled defects, both fixed:
  - `hsmsss/conn_passive.go`, `secs1/conn_passive.go`: the passive
    `ConnectingState` handler called `doOpen()` unconditionally. The
    `NotConnected` handler only enqueues `ToConnectingAsync` when
    `!c.shutdown` at handler-time, but a `Close()` arriving between
    handler-time and the dispatcher consuming the queued event let
    `doOpen` flip `opState` back to `Opening` during teardown. Fixed
    by re-checking `c.shutdown.Load()` in the `ConnectingState`
    handler.
  - `hsms/conn_state.go`: `processAsyncStateChange` sets
    `desiredState=Connecting` before `invokeHandlers`, so the racing
    event leaves `desiredState=Connecting` even after the handler
    skips. `Close()`'s polling loop requires
    `desiredState=NotConnected`, stranding the close for the full
    `CloseConnTimeout`. Fixed by having `ConnStateMgr.Stop()` reset
    `desiredState=NotConnected` (it already resets `state`).
  - Both fixes verified with isolated teeth-checks (reverting each
    individually reproduces the flake at the original rate) and a
    `-count=2000 -race -p $(nproc)` clean run.

#### examples, tests — use-after-free / leak in reply handling

- `tests/active_host`, `tests/passive_host`: the reply-message
  handler called `replyMsg.Free()` twice and then read
  `replyMsg.ToSML()`. The first `Free` returns the `DataMessage` to
  its pool; `Free` itself is idempotent under CAS, but the post-`Free`
  `ToSML` is a use-after-free on a struct that another goroutine may
  have recycled.
- `examples/device`: same shape — `Free` called before logging
  `StreamCode` / `FunctionCode` / `ToSML`.
- `examples/secs1_device`: `replyMsg` was never `Free`'d at all —
  a pure leak of one `DataMessage` per second for the lifetime of the
  example.
- All four sites reordered so logging runs while `replyMsg` is still
  caller-owned, with `Free` last. Non-library code paths; no
  behavior impact on `hsms` / `hsmsss` / `secs1` / `secs2`.

### Tests

- `hsmsss`: `reply_close_race_test.go` adds six deterministic
  regression tests gated on package-internal hook seams
  (`replyToSenderPreSendHook`, `replyErrToSenderPreStoreHook`,
  `nil` in production, installed via `t.Cleanup` in tests) so the
  reply-channel teardown defects can be reproduced without
  `t.Parallel` hazards. Teeth verified per defect.
- `hsmsss`: `testMsgSuccess` now `Clone`s `dataItem` per retry. The
  library `Free`s `dataItem` on early-rejection branches (e.g.
  `ErrNotSelectedState`), so reusing the same item across retry
  attempts wrapped a stale pointer that had already been returned to
  `secs2.asciiItemPool`; under `-race`, a peer-side
  `NewASCIIItemWithBytes` could then race the retry's later `Free`.
  Stopgap until the library-side contract can be made symmetric — see
  the Codex post-impl note referenced in 19985b6's commit message.
- `hsms`: `data_msg_lifetime_test.go` (build-tagged `debugfree`)
  documents by example why callers must not retain `SystemBytes()`
  past `Free` — the slice aliases the struct's `[4]byte` field, so
  `SetID` after capture is observable through the cached slice. The
  best-effort pool-reuse leg logs-and-skips when `sync.Pool` does not
  hand back the same pointer.
- `hsmsss`: corrected `Benchmark_ActiveHost_PassiveEQP_SmallItem` so
  the payload is built fresh per iteration. The prior version shared
  one `secs2.Item` across iterations; `processSendRequest`'s
  `defer msg.Free()` returned that item to its `sync.Pool` with
  cleared values, so iterations 2..N were silently sending an empty
  list rather than the list-of-three the test name advertised. Also
  frees the returned reply per iteration and fixes the
  off-by-one loop bound (`i <= b.N` → `i < b.N`). Numbers from before
  this fix should not be referenced.

### Known follow-ups

- The non-blocking-first optimization in `queueSendRequest` was
  considered and deferred for `replyToSender`. `closeConn` step 10
  calls `dropAllReplyMsgs` which can close reply channels while
  receiver tasks are still alive (intentional on the
  close-timeout path), so a fast-path send there would race the
  teardown. A clean lifecycle fix is owed before the optimization can
  apply symmetrically.

## [1.16.1] - 2026-05-24

Stability release focused on connection-lifecycle correctness in HSMS-SS and
back-porting the same fixes to SECS-I. The dominant themes are: closing the
race window between `Close` and producers still holding `senderMsgChan`,
making `UpdateConfigOptions` transactional, tightening SEMI-E37 conformance
on the wire (Reject.req for unsupported PType/SType, T8 across the length
header, S9F7 for malformed continuation blocks), and stopping a handful of
goroutine / channel / pool leaks that accumulated across reconnect cycles.
Adds direct chaos coverage for the `hsms` dispatcher and a post-close
leak-detection harness; the dispatcher work surfaced a latent panic-
propagation bug that's now recovered.

No public API additions; behavior changes are noted under **Changed**.

### Build

- `Makefile`: `stress-test` skips Fuzz targets and the per-package
  timeout is bumped to 45 m. The skip works around a Go-runtime cgo
  DNS pile-up that hangs `FuzzConnectionLifecycle` at high count;
  fuzz coverage stays in `make fuzz-test`. The timeout bump absorbs
  ~7 m of new chaos-test runtime in `tests/hsmsss_integration`.

### Changed

- `hsmsss`, `secs1`: `Connection.UpdateConfigOptions` is now a single
  transaction — every option is validated against a snapshot first, errors
  are aggregated via `errors.Join`, and live config is only mutated if every
  option succeeds. Previously, the first failure left earlier options
  half-applied. Non-runtime options (mode, role, queue sizes, logger
  identity) are now explicitly rejected with a clear error instead of being
  silently accepted into a half-updated state. Idempotent same-value calls
  for non-runtime options (e.g. `WithActive()` on an already-active
  connection) succeed; real value transitions are still flagged.
- `hsmsss`: `Connection.Close` now returns the first non-nil error from
  teardown (captured via an atomic `closeErr`), so callers can distinguish a
  clean close from one where task teardown exceeded `CloseConnTimeout`.
  Previous behavior returned `nil` from the second-caller path even when the
  owning `closeConn` had timed out.
- `hsms`: async connection-state handlers no longer observe idempotent
  `(prev == next)` events. The forced `ToNotConnected()` cleanup path used
  to deliver `(NotConnected, NotConnected)` to async subscribers; sync
  handlers are unaffected so internal forced-cleanup invariants are
  preserved.

### Fixed

#### hsms — async dispatcher robustness

- `ConnStateMgr.dispatchAsyncEvent` now wraps each handler call in
  `recover()`. Previously, a panicking user-supplied handler
  (registered via `AddAsyncHandler` or `Session.AddConnStateChangeHandler`)
  crashed the dispatcher goroutine and silently stopped all future
  state-event delivery. Panics are now logged with the `(prev, next)`
  pair and the dispatcher continues with sibling handlers / follow-up
  transitions.

#### hsmsss — connection close & teardown

- Gate sends during connection close via an `RWMutex`-guarded `sendClosed`
  flag, so external producers can no longer strand a frame in
  `senderMsgChan` past `closeConn`'s drain. The gate is closed in
  `closeConn` and re-opened in `doOpen` / `renewConnCtx`.
- Drain `senderMsgChan` a second time after `taskMgr.Wait()` returns. A
  send queued between the step-3 drain and `connCtx` cancellation could
  otherwise survive `closeConn` and replay on the next connection with
  stale system bytes and session ID.
- Join `selectSession` via `taskMgr` instead of launching it as a raw
  goroutine. Without this, `closeConn`'s `taskMgr.Wait()` could return
  while `selectSession` still held a producer reference to
  `senderMsgChan`, breaking the post-Wait drain's "every producer has
  exited" invariant.
- Complete teardown on host-initiated `Deselect.rsp(success)`: mark the
  connection as deselected and transition to `NotConnected`. Without this,
  an active-initiated graceful Deselect left the connection in `Selected`
  until the peer eventually closed TCP, at which point a stray
  `Separate.req` was sent on a just-gracefully-torn-down link, violating
  SEMI E37 §7.7. Path is currently unreachable from user code; the fix is
  defensive for any future public API exposing active-initiated Deselect.
- Reporting `Close` teardown errors is now safe under concurrent callers.
  The previous `CompareAndSwap`-from-nil guard let a racing `Close()` read
  `nil` if it arrived before the owning `closeConn` stored the error;
  concurrent callers now spin on `isClosed()` (which requires `opState`
  Closed) and always observe the stored result.
- Clear stranded `replyErrs` entries in `dropAllReplyMsgs`. A transaction
  error stranded by a `connCtx`-done race in `replyErrToSender` could
  outlive the connection and surface on the next send.

#### hsmsss — SEMI-E37 wire conformance

- `messageReader` now enforces T8 across the 4-byte length header, not just
  the payload. Per SEMI E37 §9.2.3.1, T8 must govern the gap between any two
  successive bytes once reception of a message has begun; the per-iteration
  deadline now switches from `idleTimeout` to `t8Timeout` after the first
  header byte arrives.
- T8 payload-read timeout is wrapped with `hsms.ErrT8Timeout` so
  `errors.Is(err, hsms.ErrT8Timeout)` succeeds for payload-phase T8 expiry,
  matching the header-phase behavior.
- Reply `Reject.req` instead of disconnecting on unsupported PType
  (reason 2) and undefined SType (reason 1) per SEMI E37 §7.10.3.
  `messageReader` inspects the 10-byte header before `DecodeMessage` and
  returns a typed `headerRejectError`; `receiverTask` peels it off,
  sends the `Reject.req` via the existing `senderTask` path, and keeps
  the connection alive.

#### hsmsss — pool / channel leaks & accounting

- Close `replyMsgChans` entries on every terminal path of `sendMsg`.
  Receiving `replyMsg == nil` on the peer `Reject.req` path previously
  returned without `removeReplyExpectedMsg`, leaking the transaction
  entry for the connection lifetime.
- Stop double-decrementing `DataMsgInflightCount` on reply.
  `replyToSender` called `decDataMsgInflightCount()` independently of
  `sendMsg`, driving the gauge to -1 after one W-bit round trip.
- Locked hot-path getters (`IsActive`, `IsEquip`, `SendTimeout`,
  `TraceTraffic`, `ValidateDataMessage`) eliminate the data races
  `-race` flagged in `sendMsg` / `sendMsgSync` / `receiverTask` /
  `validateMsg`.

#### secs1 — back-ports of the above

- Gate sends during connection close (`sendClosed` / `sendMu`). Without
  this, a frame queued between the pre-cancel drain and `connCtx`
  cancellation survived into the next connection generation and was
  transmitted as its first frame instead of the expected protocol-init
  exchange. Gate is reset under `sendMu.Lock()` outside `createContext()`
  to avoid `ctxMutex`/`sendMu` lock-order inversion.
- Join the active reconnect loop via a `connectLoopWg` `WaitGroup` before
  installing fresh contexts in `Open`. The pre-existing atomic-CAS guard
  could race with a rapid Close→Open cycle when the old goroutine's
  deferred `Store(false)` had not yet run, causing the replacement
  retry loop to be skipped and the reconnect to stall.
- Transition to `NotConnected` *before* stopping the state manager on
  `Close`. SECS-I had inherited the wrong ordering from an earlier
  HSMS-SS revision, so the final state change fired after the async
  handler dispatcher had already exited via `stateMgr.Stop()` and was
  never observed by user-registered `ConnStateChangeHandler`s.
- Send `S9F7` (Illegal Data) when `ErrHeaderMismatch` is raised by the
  multi-block assembler. A host sending a malformed continuation block
  (mismatched W-bit, stream, or function) previously never learned the
  message was rejected, and any outstanding W-bit transaction hung until
  T3 expiry.

### Tests

- `hsmsss`: regression tests for the close/send race
  (`TestConnection_CloseDrainsStrandedSenderFrame`), the gate
  (`TestConnection_CloseGateRefusesSendsAfterClose`), the Close vs in-flight
  `selectSessionTask` race (`TestHSMS_SelectCloseRace_NoPanicNoZombie`,
  ×100), the partial-header T8 deadline
  (`TestMessageReader_PartialHeader_T8Timeout`), the peer-Reject reply-chan
  leak (`TestConnection_SendMsg_PeerReject_NoReplyChanLeak`), the W-bit
  in-flight balance (`TestConnection_DataMsgInflightCountBalanced`), and a
  property test for idempotent-async transition suppression.
- `hsmsss` integration: P0 round-trip coverage for `Reject.req` reason
  codes 3 (TransactionNotOpen) and 4 (NotSelected) — including
  `SendMessage` surfacing `ErrRejectNotSelected` — plus decode-layer
  coverage for codes 1 (STypeNotSupported) and 2 (PTypeNotSupported), and
  a mid-sequence-recovery test for the auto-linktest fail-threshold.
- `secs1`: a 30-cycle wire-observable regression
  (`TestSECS1_CloseReopen_NoStaleBlockAcrossGeneration`) guards the
  send-gate fix by asserting the gen-2 peer never sees a stale gen-1
  block; a 20-iteration end-to-end cycle plus a unit-level invariant
  guard the connect-loop `WaitGroup` join; a 5-cycle lifecycle test
  asserts user handlers receive all three anchor transitions per
  open/close, mirroring the HSMS-SS coverage.
- `secs1` integration: new `tests/secs1_integration/` package with a
  raw-block peer helper and table-driven coverage for multi-block error
  paths (`BlockNumberGap`, `OutOfOrder`, `HeaderMismatch_{Wbit,Stream,
  Function}` → S9F7), T4 inter-block timeout abort, and clean recovery
  for the next message.
- `secs1`: ownership-contract regression guard. Fixed
  `TestConnection_SendGateReopensAfterCloseAndReopen` which violated
  `queueSendRequest`'s success-transfers-ownership invariant by
  Free-ing `msg` unconditionally; the resulting double-Free poisoned
  `hsms.DataMessage`'s `sync.Pool` and surfaced as cross-test races
  under `-count=50 -race -p $(nproc)`. Added
  `TestSession_MultiHandlerDispatch_ConcurrentSendStressor` for multi-
  handler dispatch coverage; the corrected
  `TestConnection_SendGateReopensAfterCloseAndReopen` exercised under
  `make stress-test` is the primary regression guard.
- `hsms`: architectural invariant tests
  (`TestDataMessage_Free_PutResetsFreedAndNilsDataItem`,
  `TestDataMessage_Free_IdempotentWithinSingleCycle`) lock in the two
  pool properties — `freed=0` reset and `dataItem=nil` before
  `pool.Put` — that the new build-tagged content-aware stale-Free
  detector (`hsms/pool_debug.go`, activated via `-tags debugfree`)
  depends on. The detector is zero-cost in default builds and is
  retained as a tool for future hard-to-reproduce pool double-Free
  investigations.
- `hsms`: direct chaos coverage for `ConnStateMgr`
  (`connstate_chaos_test.go`) — handler panic survival, `Stop` while
  a handler is mid-execution, and concurrent handler registration
  during dispatch. A randomized property variant is gated behind
  `//go:build stress`. Fills the gap of previously reaching the
  dispatcher only indirectly through `hsmsss` / `secs1` integration
  tests.
- `hsmsss`: `assertCleanShutdown(t, conn)` polls the five
  authoritative post-close internals (`replyMsgChans`, `replyErrs`,
  `DataMsgInflightCount`, `taskMgr.TaskCount`, `senderMsgChan`) in
  one shot, so chaos / lifecycle tests gate on one line instead of
  five. Teeth verified via a temporary in-tree revert of the
  duplicate-`decDataMsgInflightCount` fix, which made the smoke test
  fail with `DataMsgInflight=-2` as expected.
- `tests/hsmsss_integration`: advisory `snapshotLeaks` /
  `assertSettled` helper for coarse goroutine + (Linux-only) fd
  comparison via `t.Cleanup`. Drift logs `t.Logf` warnings rather
  than failing the test; the package-internal counter gate above is
  authoritative, this layer is defense-in-depth against scheduler /
  fd noise.
- `tests/hsmsss_integration`: `ByteChaosProxy` parallel to the
  existing `ChaosProxy` for faults the filter-driven pump cannot
  express — partial length-header writes, partial payload writes,
  `payload[4]` (PType) / `payload[5]` (SType) substitution, and
  length-header override. Five scenarios drive the matching SEMI-E37
  conformance fixes above (per-byte T8, unsupported-PType/SType
  Reject.req, length-too-large rejection) end-to-end through the
  live wire.

## [1.16.0] - 2026-04-18

Connection-state handler dispatch is split into synchronous and asynchronous
models so that user handlers can safely perform reply-expecting I/O, and the
HSMS-SS Select.req path commits to `SelectedState` before responding on both
sides of the connection.

### Added

- `hsms.ConnStateMgr.AddAsyncHandler` — register connection-state handlers
  that run on a dedicated dispatcher goroutine after the transition commits.
  Async handlers may perform blocking work, including `SendDataMessage` with
  the W-bit set, and may call the synchronous `ToX` state-change methods
  without deadlocking. The existing `AddHandler` remains for library-internal,
  invariant-preserving bookkeeping and is unchanged.
- `make help`, `make fmt`, `make vet`, `make test-all` targets; lint tool
  installation is now gated on availability.

### Changed

- Public `Session.AddConnStateChangeHandler` now dispatches handlers on the
  async path. Handlers that previously observed `cs.State()` inline during
  the transition should rely on the `(prev, new)` arguments they receive —
  live state may have advanced by the time the handler runs.

### Fixed

- `hsmsss` passive Select.req now commits to `SelectedState` synchronously
  before sending Select.rsp. Previously, data messages arriving immediately
  after Select.rsp could be rejected with `RejectNotSelected` while the
  async transition was still in flight.
- `hsmsss` active simultaneous-select branch (SEMI E37 §7.4.3) applies the
  same synchronous commit; on transition failure the peer now receives
  `SelectStatusNotReady` instead of a falsely successful reply.

### Tests

- Async handler dispatcher now has regression coverage for ordering,
  FIFO delivery across transitions, handler-initiated `ToX` calls, and the
  buffer-overflow drop-newest path.
- Added coverage for the already-Selected SelectReq branch on both
  active and passive sides.

## [1.15.1] - 2026-03-09

### Fixed

- `hsmsss`, `secs1`: hardened `isNetError` classification and cleaned up the
  shared error helpers to avoid misclassifying expected shutdown-path errors
  as connection faults.
- Flaky `testAsyncMsgSuccess` made resilient to stale replies arriving from
  a prior iteration.

## [1.15.0] - 2026-03-08

Concurrency hardening release. Focused on eliminating races and deadlocks
around `ConnStateMgr`, message pooling, and reconnect paths, with new chaos
infrastructure to keep the guarantees honest. One new SECS-II type and one
new HSMS-SS tunable.

### Added

- `secs2`: Localized Character String support (FormatCode `0o22`).
- `hsmsss`: configurable linktest failure threshold so operators can tune
  how many consecutive linktest errors trigger a disconnect.
- `hsmsss`: chaos-testing proxy and edge-case scenarios exercising
  partial reads, slow peers, and mid-handshake teardown.

### Changed

- `hsmsss`: replaced `sync.RWMutex`-guarded context fields with
  `atomic.Pointer`, removing a contention hotspot on the hot receive path.
- Upgraded `golangci-lint` to v2 and addressed the new warnings.

### Fixed

- `hsms.ConnStateMgr`: resolved a race/deadlock between `Stop()` and
  in-flight `changeStateAsync` callers, plus a flaky exponential-backoff
  timing test.
- `hsms.DataMessage.Free` is now idempotent; prevents a double-free race
  when a message is returned to the pool along multiple paths.
- `hsmsss` / `secs1`: pooled messages are now freed on every
  drop / reject / drain / queue-fail path to stop the leak of pool-backed
  buffers under error conditions.
- `hsmsss`: `DataMessage` is cloned per handler so concurrent subscribers
  can no longer race over a shared pooled pointer after one of them frees.
- `hsmsss`, `secs1`: fixed a data race in the `sendMsg` timeout handling.
- `hsmsss`: prevented overlapping connect loops with a dedicated
  `connectLoopWg`; a rapid reconnect cycle could previously start a
  second connect loop before the first had exited.
- Multiple smaller fixes for timing flakes and fuzz-test lifecycle issues.

## [1.14.0] - 2026-02-28

HSMS-SS / SECS-I reconnect stability release. Active and passive
connections were reworked to share a single state model, TCP half-open
detection was added, and the decoding edge cases surfaced by last
release's fuzz work were closed out.

### Changed

- `hsmsss`: active and passive connection flows now share the same
  state-machine shape as `secs1`, simplifying reconnect logic and
  eliminating drift between the two sides.
- Documented `opState` and `stateMgr` architecture.

### Fixed

- `hsmsss`: TCP half-open detection via periodic read deadlines and
  TCP keep-alive; a peer that silently disappeared would previously
  leave the connection stuck in `Selected` forever.
- `hsmsss`: resolved reconnect deadlocks surfaced by the active/passive
  alignment refactor.
- `hsms`, `hsmsss`, `secs1`: `loopCtx` accessed under the proper mutex;
  addresses a handful of review-fix items.
- Connection and decoding edge cases caught by the new fuzz / integration
  suites.

## [1.13.2] - 2026-02-15

### Fixed

- `secs1`: improved disconnect detection and added support for runtime
  configuration updates without tearing the connection down.

## [1.13.1] - 2026-02-14

### Added

- `hsmsss`: extracted `messageReader` and added fuzz + integration tests
  around its framing logic.

### Fixed

- `hsmsss`: remaining SEMI E37 compliance gaps identified after the
  v1.13.0 release (deselect and control-message edge cases).

## [1.13.0] - 2026-02-14

### Added

- `hsmsss`: SEMI E37 deselect support and control-message handling —
  Deselect.req / Deselect.rsp / Separate.req are now honoured end-to-end
  and take the session through the documented state transitions.

[2.7.0]: https://github.com/arloliu/go-secs/releases/tag/v2.7.0
[2.6.0]: https://github.com/arloliu/go-secs/releases/tag/v2.6.0
[2.5.0]: https://github.com/arloliu/go-secs/releases/tag/v2.5.0
[2.4.2]: https://github.com/arloliu/go-secs/releases/tag/v2.4.2
[2.4.1]: https://github.com/arloliu/go-secs/releases/tag/v2.4.1
[2.4.0]: https://github.com/arloliu/go-secs/releases/tag/v2.4.0
[2.3.1]: https://github.com/arloliu/go-secs/releases/tag/v2.3.1
[2.3.0]: https://github.com/arloliu/go-secs/releases/tag/v2.3.0
[2.2.0]: https://github.com/arloliu/go-secs/releases/tag/v2.2.0
[2.1.0]: https://github.com/arloliu/go-secs/releases/tag/v2.1.0
[2.0.1]: https://github.com/arloliu/go-secs/releases/tag/v2.0.1
[2.0.0]: https://github.com/arloliu/go-secs/releases/tag/v2.0.0
[2.0.0-rc6]: https://github.com/arloliu/go-secs/releases/tag/v2.0.0-rc6
[2.0.0-rc5]: https://github.com/arloliu/go-secs/releases/tag/v2.0.0-rc5
[2.0.0-rc4]: https://github.com/arloliu/go-secs/releases/tag/v2.0.0-rc4
[2.0.0-rc3]: https://github.com/arloliu/go-secs/releases/tag/v2.0.0-rc3
[2.0.0-rc2]: https://github.com/arloliu/go-secs/releases/tag/v2.0.0-rc2
[2.0.0-rc1]: https://github.com/arloliu/go-secs/releases/tag/v2.0.0-rc1
[1.18.0]: https://github.com/arloliu/go-secs/releases/tag/v1.18.0
[1.17.1]: https://github.com/arloliu/go-secs/releases/tag/v1.17.1
[1.17.0]: https://github.com/arloliu/go-secs/releases/tag/v1.17.0
[1.16.2]: https://github.com/arloliu/go-secs/releases/tag/v1.16.2
[1.16.1]: https://github.com/arloliu/go-secs/releases/tag/v1.16.1
[1.16.0]: https://github.com/arloliu/go-secs/releases/tag/v1.16.0
[1.15.1]: https://github.com/arloliu/go-secs/releases/tag/v1.15.1
[1.15.0]: https://github.com/arloliu/go-secs/releases/tag/v1.15.0
[1.14.0]: https://github.com/arloliu/go-secs/releases/tag/v1.14.0
[1.13.2]: https://github.com/arloliu/go-secs/releases/tag/v1.13.2
[1.13.1]: https://github.com/arloliu/go-secs/releases/tag/v1.13.1
[1.13.0]: https://github.com/arloliu/go-secs/releases/tag/v1.13.0

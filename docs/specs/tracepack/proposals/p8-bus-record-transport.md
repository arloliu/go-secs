# Proposal P8 — carrying records over the durable bus: the message unit and large records

Status: draft (2026-09-28) — awaiting the owner.
Source: owner discussion of 2026-09-28 on the equipment gateway → log service path.
Nothing here is a decided rule.

## 1. Questions and where the outcome lands

[STO §4] "Recorder over a durable bus" fixes the duties of the producer and of the consumer:
producers publish records to NATS JetStream,
and the log service is a set of stateless consumers sharing one work-queue consumer (G5-86).
It does not fix what one bus message carries,
nor how a record larger than the bus's message limit crosses the bus;
G5-80 places the producer-to-service transport outside the tracepack specification.
This proposal answers two questions for the equipment gateway → log service path:

- **Q1.** Does the producer collect records for a period (for example 5 minutes or N MiB) and publish them as a pack,
  or does it publish each record?
- **Q2.** How does a record larger than the bus message limit cross the bus,
  when any of the N consumer instances may take any message?

P8 closes as a decision-log entry plus a non-normative note in [tracepack-go.md](../tracepack-go.md) §2,
next to the equipment-gateway producer row.
Only the claim-check of §3.3 touches normative text:
the rule of [STO §4] that an accepted message is removed only by acknowledgement extends to the object bucket.

## 2. Q1: one record per bus message

Proposed: the producer publishes each record as its own bus message as soon as it observes it.
No pack crosses the bus, and the producer does not batch;
the log service stays the only pack writer.

Reasons, from [STO §4]:

- The bus's acknowledgement is a record's durability point.
  A producer that holds records for 5 minutes moves that point into its own memory:
  a crash loses up to 5 minutes of traffic, which [STO §5] Completeness reports as a seq gap.
- A pack of 5 minutes of busy traffic easily exceeds the bus message limit even compressed,
  so every flush would become the large-message case of Q2.
- A producer that writes packs takes back the writer role G5-86 gave the service:
  it would own `writer`, `classifier`, `max_frame_len`, the flush timing and the block layout,
  and the service would admit packs instead of consuming records.

### 2.1 Message layout (proposed)

- **Subject:** `<prefix>.<tool_id>.<capture_id>`, one subject per capture as [STO §4] requires.
  The tool token costs nothing now;
  it is the token a later ingest partition by tool (G5-87, second phase) maps with a `{{partition(n, …)}}` subject transform.
  `tool_id` is encoded as a valid subject token (no `.`, `*`, `>` or whitespace), and `capture_id` as lowercase hex.
- **Body:** the record header of [FMT §7.1] (44 bytes) as one record's gathered bytes, in field order ([FMT §6]),
  followed by the `payload_len` bytes of payload of [FMT §8].
  Reusing the record header layout avoids a second schema, and the consumer copies the producer-owned bytes as received.
  The producer fills the fields it owns:
  `seq`, `ts_utc_ns`, `mono_ns`, `record_flags.mono_present`, `epoch`, `kind`, `dir`, `fidelity`, the declared `quality` bits and `payload_len`.
  The consumer owns `classifier` ([STO §4]),
  so it computes `decode_status` and `trailing_bytes` ([FMT I-11]),
  and `field_validity`, from the payload;
  the producer writes those fields as zero.
- **Headers:**
  - `Tpk-Layout: 1` names the body layout, so a later layout (§2.3) can coexist with this one.
  - `Nats-Msg-Id: <capture_id>.<seq>` makes the stream store a producer's retry once
    while it falls within the duplicate window (2 minutes by default).
    A duplicate outside the window is still harmless:
    [STO §4] allows a record to be written twice, and [FMT I-12] deduplicates it.
  - `Tpk-Capture-Descriptor`, only on the message of the capture's `start` record:
    the producer-owned pack-metadata entries of the capture descriptor ([STO §4]) in the TLV encoding of [FMT §5], base64-encoded.

### 2.2 Stream and consumer settings the layout depends on

- **Traffic stream:** work-queue retention and file storage,
  with no `max_age`, `max_bytes` or `max_msgs` limit ([STO §4]).
  Compression is the bus's job, not the message's:
  the stream stores messages S2-compressed (`compression: s2`, NATS server 2.10 and later).
- **Acknowledgement wait:** a consumer holds each record unacknowledged until the segment holding it is durable in `staging/`,
  which takes up to F plus the segment's upload.
  Either `ack_wait` exceeds that, or the consumer sends in-progress acknowledgements (`+WPI`) while it holds records;
  in both cases a message reaches a second consumer only after the first one stopped.
- **Pending budget:** `max_ack_pending` (1000 by default) bounds the messages all instances together hold unacknowledged.
  It must exceed the peak record rate × (F + upload time), or delivery stalls until a flush acknowledges;
  with one message per record this is large,
  for example 120 000 at 2 000 records per second and F = 60 s.

### 2.3 Deferred: micro-batching

A message carrying several records cuts the message count and the pending budget of §2.2.
It stays within [STO §4] under two conditions:
a batch holds records of one capture with contiguous seqs,
and a `clock-step` record ends its batch, which is acknowledged before the next batch is published.
It is deferred until measured traffic shows per-record publishing to be a bottleneck;
it would be a new `Tpk-Layout` value.

## 3. Q2: records larger than the bus message limit

### 3.1 Sizes

- go-secs accepts HSMS messages up to `hsms.MaxMessageSize` = 2^24 − 1 bytes, about 16 MiB,
  and recipe bodies (S7F3, S7F6) and large event reports can reach several MiB.
- NATS `max_payload` is 1 MiB by default and counts headers and payload together.
  It may be raised to 64 MiB,
  but the NATS documentation recommends against values above 8 MiB.

### 3.2 Why chunking on the work queue does not work

Any consumer instance takes any message ([STO §4]), so the chunks of one record reach different instances.
An instance cannot acknowledge a chunk before the record's segment is durable,
and it cannot write the record without the chunks another instance holds.
Each instance naks or times out, the bus redelivers to arbitrary instances again,
and a record completes only when one instance happens to hold all of its chunks at once.
The `seqId`/`seqCount` chunking of the equipment gateway's EAP bus adapter works today
because its receiver is a single subscriber per tool subject,
not a pool of stateless consumers.

### 3.3 Proposed: inline below the limit, claim-check above it

A record travels inline when its body stays at least 4 KiB under `max_payload`;
the margin covers the headers of §2.1 and the capture descriptor on the `start` message,
and `max_payload` is the only knob.
Above that, the producer:

1. puts the payload into an Object Store bucket as the object `<capture_id>.<seq>`
   (Object Store splits it into 128 KiB chunks and records its SHA-256 digest),
2. waits until the put completes,
3. publishes the record's message with a header-only body, the record header carrying the true `payload_len`,
   and the headers `Tpk-Payload-Ref: <bucket>/<object>` and `Tpk-Payload-Digest: SHA-256=<digest>`.

The order matters:
a reference published before its object is durable could reach a consumer that finds nothing.

The consumer fetches the object, checks its length against `payload_len` and its digest,
and then writes the record like an inline one.
It never writes a record header without its payload:
for a missing object or a failed check it naks the message with a delay and raises an alert,
as it defers a record whose capture descriptor is not yet registered ([STO §4]).
It deletes the object only after a confirmed (synchronous) acknowledgement of the message.
A work-queue stream removes an acknowledged message,
and under the settings of §2.2 a message reaches a second consumer only after the first stopped without acknowledging,
so a missing object on a message that is still pending points at a nonconforming component.
One case remains:
a consumer that stalled past `ack_wait` without sending `+WPI` can find the object gone for a message another instance has since acknowledged,
which costs an alert and no record.

The bucket's stream follows the traffic stream's rule, with no `max_age`, `max_bytes` or `max_msgs` limit:
a deferred reference can outlive any fixed age.

Two kinds of object lose their reference:

- an **orphan**, when the producer stopped between the put and the publish;
  nothing references it, and its seq is a gap ([STO §5] Completeness);
- a **leftover**, when the consumer stopped between the acknowledgement and the delete;
  its record is staged.

A sweeper removes both; its rule is open (§5).

### 3.4 Alternatives

| Alternative | Why not now |
|---|---|
| The producer batches records into a pack for a period (Q1) | moves the durability point into the producer, makes every flush a large message, and moves the writer role back to the producer (§2) |
| Raise `max_payload` to cover `hsms.MaxMessageSize` (just over 16 MiB with the record header, so a 17 MiB setting) | above the 8 MiB NATS recommends; it is a server- or account-wide setting; one large message delays every other message on the same connection, and stream replication multiplies it |
| Chunk on the traffic stream, with the capture's messages routed to one instance by a subject partition | needs the ingest partitioned by tool (G5-87, second phase), with leases and rebalancing; chunking can be reconsidered when that phase is designed, since one instance then receives every message of a capture |
| Micro-batching | deferred (§2.3) |

## 4. Consequences for the first release

- The equipment gateway, through the recorder device or the record emission in the EAP bus adapter of G5-85,
  publishes one message per record, inline or with a payload reference.
- The log service's consumer handles both message forms, the acknowledgement settings of §2.2 and the sweeper.
- The format is unchanged: the bus carries record headers and payloads as [FMT §7] and [FMT §8] define them.

## 5. Open questions for the owner

1. Where the outcome lands:
   the decision log and a note in [tracepack-go.md](../tracepack-go.md) (§1),
   or a service design document outside the specification.
2. `max_payload`: keep the 1 MiB default (proposed), or raise it toward 8 MiB so fewer records take the claim-check path.
3. The sweeper's rule.
   Leftover: an object older than a grace period whose record a staged segment holds.
   Orphan: an object older than a grace period whose capture's subject holds no message,
   which meets the publish-fence question of proposal P7 §2 point 1.
4. Whether the tracepack Go module exports the record-header encoding for use outside a block,
   so that the equipment gateway and the log service share one implementation of §2.1.
5. Outside P8's scope, noted because §2.1 places classification on the consumer:
   during a rolling upgrade, two consumers with different classifier versions can write the same (`capture_id`, `seq`) with different derived fields,
   while [FMT I-12] requires them to be byte-identical within the active view.

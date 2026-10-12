package tracepack

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// busCapture is the capture every bus test message belongs to.
var busCapture = UUID{0x01, 0x8f, 0x3a, 0x6b, 0x7c, 0x2d, 0x7e, 0x0f, 0x8a, 0x1b, 0x2c, 0x3d, 0x4e, 0x5f, 0x60, 0x71}

// busFrame returns an HSMS data frame: the length prefix, a 10-byte header of stream and function, and text.
func busFrame(stream, function uint8, text []byte) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(10+len(text)))
	b = append(b, 0x00, 0x01, stream|0x80, function, 0x00, 0x00, 0x01, 0x02, 0x03, 0x04)

	return append(b, text...)
}

// busEventPayload returns the payload of a transport-event record carrying ev.
func busEventPayload(t testing.TB, ev *TransportEvent) []byte {
	t.Helper()

	b, err := ev.MarshalBinary()
	require.NoError(t, err)

	return b
}

// busStartRecord returns the start boundary record of the capture at seq 0, as a producer publishes it.
func busStartRecord(t testing.TB) Record {
	t.Helper()

	return Record{
		Seq:      0,
		TSUTCNs:  1_700_000_000_000_000_000,
		Kind:     KindTransportEvent,
		Dir:      DirLocal,
		Fidelity: FidelityNotApplicable,
		Payload: busEventPayload(t, &TransportEvent{
			Event:        EventCaptureBoundary,
			BoundaryKind: new(BoundaryKindStart),
		}),
	}
}

// busEncode encodes r inline, as a producer does.
func busEncode(t testing.TB, r *Record) []byte {
	t.Helper()

	br := NewBusRecord(r)
	b, err := EncodeBusBody(&br)
	require.NoError(t, err)

	return b
}

// busEnvelope returns the headers of an inline message of seq.
func busEnvelope(t testing.TB, seq uint64) BusEnvelope {
	t.Helper()

	id, err := BusMsgID(busCapture, seq)
	require.NoError(t, err)

	return BusEnvelope{Layout: "1", MsgID: id}
}

// startBody returns the inline body of the capture's start record.
func startBody(t testing.TB) []byte {
	t.Helper()

	r := busStartRecord(t)

	return busEncode(t, &r)
}

// startEnvelope returns the headers of the start message, with descriptor as the Tpk-Capture-Descriptor value.
func startEnvelope(t testing.TB, descriptor string) BusEnvelope {
	t.Helper()

	env := busEnvelope(t, 0)
	env.Descriptor = descriptor

	return env
}

// claimCheck returns the header-only body of r and the headers of its claim-check message in bucket.
func claimCheck(t testing.TB, r *Record, bucket string) ([]byte, BusEnvelope) {
	t.Helper()

	br := NewBusRecord(r)
	br.ClaimCheck = true
	br.Bucket = bucket
	br.Digest = sha256.Sum256(r.Payload)
	body, err := EncodeBusBody(&br)
	require.NoError(t, err)
	require.Len(t, body, 44)

	env := busEnvelope(t, r.Seq)
	env.PayloadRef = bucket + "/" + env.MsgID
	env.PayloadDigest = BusPayloadDigest(r.Payload)

	return body, env
}

func TestBusBodyRoundTrip(t *testing.T) {
	t.Parallel()

	text := []byte{0x41, 0x02, 'o', 'k', 0xFF, 0xFF, 0xFF}
	unobservedText := make([]byte, 12)
	tests := []struct {
		name string
		rec  Record
		cls  *fakeClassifier
		// want holds the derived values: decode_status, trailing_bytes and field_validity.
		wantStatus   DecodeStatus
		wantTrailing uint32
		wantValidity FieldValidity
		wantCalls    int
	}{
		{
			name:       "start",
			rec:        busStartRecord(t),
			wantStatus: DecodeStatusNotApplicable,
		},
		{
			name: "socket event",
			rec: Record{Seq: 1, TSUTCNs: 5, Epoch: 1, Kind: KindTransportEvent, Dir: DirLocal, Fidelity: FidelityNotApplicable,
				Payload: busEventPayload(t, &TransportEvent{Event: EventSocketConnect, SocketRole: new(SocketRoleActive)})},
			wantStatus: DecodeStatusNotApplicable,
		},
		{
			name: "clock-step",
			rec: Record{Seq: 2, TSUTCNs: 6, Kind: KindTransportEvent, Dir: DirLocal, Fidelity: FidelityNotApplicable,
				Payload: busEventPayload(t, &TransportEvent{Event: EventClockStep, ClockStepNs: new(int64(-3_000_000_000))})},
			wantStatus: DecodeStatusNotApplicable,
		},
		{
			name: "orderly stop",
			rec: Record{Seq: 3, TSUTCNs: 7, Kind: KindTransportEvent, Dir: DirLocal, Fidelity: FidelityNotApplicable,
				Payload: busEventPayload(t, &TransportEvent{Event: EventCaptureBoundary, BoundaryKind: new(BoundaryKindStop)})},
			wantStatus: DecodeStatusNotApplicable,
		},
		{
			name: "annotation",
			rec: Record{Seq: 4, TSUTCNs: 8, Kind: KindAnnotation, Dir: DirLocal, Fidelity: FidelityNotApplicable,
				Payload: func() []byte {
					b, err := (&Annotation{AnnotationKind: AnnotationKindNote, Text: new("hello")}).MarshalBinary()
					require.NoError(t, err)

					return b
				}()},
			wantStatus: DecodeStatusNotApplicable,
		},
		{
			name: "data frame",
			rec: Record{Seq: 5, TSUTCNs: 9, MonoNs: 77, MonoPresent: true, Epoch: 2, Kind: KindData, Dir: DirEquipmentToHost,
				Fidelity: FidelityWireExact, Quality: QualityDirectionInferred, Payload: busFrame(6, 11, text),
				// The producer's derived values never travel: the encoder writes zero.
				DecodeStatus: DecodeStatusShortFrame, TrailingBytes: 9, FieldValidity: FieldValiditySessionID},
			cls:          &fakeClassifier{name: "c", status: DecodeStatusOKWithTrailing, trailing: 3},
			wantStatus:   DecodeStatusOKWithTrailing,
			wantTrailing: 3,
			wantValidity: fieldValidityMask,
			wantCalls:    1,
		},
		{
			name:         "short data frame",
			rec:          Record{Seq: 6, TSUTCNs: 10, Epoch: 2, Kind: KindData, Dir: DirHostToEquipment, Fidelity: FidelityWireExact, Payload: []byte{0, 0, 0, 10, 0, 1, 0x86, 11}},
			cls:          &fakeClassifier{name: "c", status: DecodeStatusShortFrame, trailing: 5},
			wantStatus:   DecodeStatusShortFrame,
			wantValidity: FieldValiditySessionID | FieldValidityStreamAndW | FieldValidityFunction,
			wantCalls:    1,
		},
		{
			name:         "control frame",
			rec:          Record{Seq: 7, TSUTCNs: 11, Epoch: 2, Kind: KindControl, Dir: DirHostToEquipment, Fidelity: FidelityWireExact, Payload: busFrame(0, 0, nil)},
			cls:          &fakeClassifier{name: "c", status: DecodeStatusOK},
			wantStatus:   DecodeStatusOK,
			wantValidity: fieldValidityMask,
			wantCalls:    1,
		},
		{
			name:         "data frame without a classifier",
			rec:          Record{Seq: 8, TSUTCNs: 12, Epoch: 2, Kind: KindData, Dir: DirHostToEquipment, Fidelity: FidelityReEncoded, Payload: busFrame(1, 1, nil)},
			wantStatus:   DecodeStatusNotAttempted,
			wantValidity: fieldValidityMask,
		},
		{
			name: "unobserved frame",
			rec: Record{Seq: 9, TSUTCNs: 13, Epoch: 2, Kind: KindData, Dir: DirEquipmentToHost, Fidelity: FidelityReEncoded,
				Quality: QualityUnobserved, Payload: busFrame(1, 4, unobservedText)},
			cls:          &fakeClassifier{name: "c", status: DecodeStatusOK},
			wantStatus:   DecodeStatusNotAttempted,
			wantValidity: fieldValidityMask,
		},
		{
			name: "wire-exact frame in a decoded-message capture",
			rec: Record{Seq: 10, TSUTCNs: 14, Epoch: 3, Kind: KindData, Dir: DirEquipmentToHost, Fidelity: FidelityWireExact,
				Payload: busFrame(6, 11, text)},
			cls:          &fakeClassifier{name: "c", status: DecodeStatusOK},
			wantStatus:   DecodeStatusOK,
			wantValidity: fieldValidityMask,
			wantCalls:    1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			body := busEncode(t, &tt.rec)
			require.Len(t, body, 44+len(tt.rec.Payload))
			assert.Equal(t, []byte{0, 0, 0, 0}, body[32:36], "trailing_bytes travels as zero")
			assert.Zero(t, body[41], "decode_status travels as zero")
			assert.Zero(t, body[42], "field_validity travels as zero")

			env := busEnvelope(t, tt.rec.Seq)
			if tt.name == "start" {
				env.Descriptor = base64.StdEncoding.EncodeToString(descriptorBytes(t))
			}
			br, err := CheckBusMessage(env, body)
			require.NoError(t, err)
			assert.Equal(t, busCapture, br.CaptureID)
			assert.Equal(t, uint32(len(tt.rec.Payload)), br.PayloadLen)
			assert.False(t, br.ClaimCheck)

			want := tt.rec
			want.DecodeStatus, want.TrailingBytes, want.FieldValidity = 0, 0, 0
			assert.Equal(t, want, br.Record, "every producer field kept, fidelity included")

			again, err := EncodeBusBody(&br)
			require.NoError(t, err)
			assert.Equal(t, body, again, "the encoder's output round-trips exactly")

			var cls Classifier
			if tt.cls != nil {
				cls = tt.cls
			}
			require.NoError(t, DeriveBusRecord(&br.Record, cls))
			assert.Equal(t, tt.wantStatus, br.Record.DecodeStatus)
			assert.Equal(t, tt.wantTrailing, br.Record.TrailingBytes)
			assert.Equal(t, tt.wantValidity, br.Record.FieldValidity)
			if tt.cls != nil {
				assert.Len(t, tt.cls.seen, tt.wantCalls)
			}
		})
	}
}

func TestBusClaimCheckStartRoundTrip(t *testing.T) {
	t.Parallel()

	start := busStartRecord(t)
	desc := descriptorBytes(t)
	body, env := claimCheck(t, &start, "tpk-objects")
	env.Descriptor = base64.StdEncoding.EncodeToString(desc)

	br, err := CheckBusMessage(env, body)
	require.NoError(t, err)
	assert.True(t, br.ClaimCheck)
	assert.Equal(t, "tpk-objects", br.Bucket)
	assert.Equal(t, sha256.Sum256(start.Payload), br.Digest)
	assert.Equal(t, uint32(len(start.Payload)), br.PayloadLen)
	assert.Nil(t, br.Record.Payload, "a claim-check record has no payload until it is fetched")
	assert.Nil(t, br.Descriptor, "a claim-check start's descriptor is checked once its payload is known")

	again, err := EncodeBusBody(&br)
	require.NoError(t, err)
	assert.Equal(t, body, again)

	require.NoError(t, CheckBusPayload(&br, start.Payload))
	assert.Equal(t, start.Payload, br.Record.Payload)
	assert.Equal(t, desc, br.Descriptor)
	m, err := DecodeDescriptor(br.Descriptor)
	require.NoError(t, err)
	assert.Equal(t, descriptorMeta(), m)

	again, err = EncodeBusBody(&br)
	require.NoError(t, err)
	assert.Equal(t, body, again, "a fetched claim-check record still encodes header-only")

	require.NoError(t, DeriveBusRecord(&br.Record, nil))
	assert.Equal(t, DecodeStatusNotApplicable, br.Record.DecodeStatus)
}

func TestBusClaimCheckStartDescriptor(t *testing.T) {
	t.Parallel()

	start := busStartRecord(t)
	tests := []struct {
		name       string
		descriptor string
	}{
		{"missing", ""},
		{"undecodable", base64.StdEncoding.EncodeToString(descriptorBytes(t)[:10])},
		{"unpadded", base64.RawStdEncoding.EncodeToString(descriptorBytes(t))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			body, env := claimCheck(t, &start, "b")
			env.Descriptor = tt.descriptor
			br, err := CheckBusMessage(env, body)
			require.NoError(t, err, "the record is not known to be a start before its payload is fetched")

			err = CheckBusPayload(&br, start.Payload)
			require.ErrorIs(t, err, ErrBusNotAccepted)
			assert.NotErrorIs(t, err, ErrBusPayloadMismatch)
		})
	}

	t.Run("ignored on another record", func(t *testing.T) {
		t.Parallel()

		r := Record{Seq: 4, TSUTCNs: 1, Kind: KindData, Fidelity: FidelityWireExact, Payload: busFrame(1, 1, []byte{1, 2, 3})}
		body, env := claimCheck(t, &r, "b")
		env.Descriptor = "!!not base64!!"
		br, err := CheckBusMessage(env, body)
		require.NoError(t, err)
		require.NoError(t, CheckBusPayload(&br, r.Payload))
		assert.Nil(t, br.Descriptor)
	})
}

func TestCheckBusMessage(t *testing.T) {
	t.Parallel()

	const seq = 42
	data := Record{Seq: seq, TSUTCNs: 1, Epoch: 1, Kind: KindData, Fidelity: FidelityWireExact, Payload: busFrame(1, 3, []byte{1, 2})}
	body := busEncode(t, &data)
	env := busEnvelope(t, seq)
	id := env.MsgID
	uuidText := busCapture.String()
	ccBody, ccEnv := claimCheck(t, &data, "bucket")
	otherCapture := busCapture
	otherCapture[15] ^= 1

	bodyAt := func(s uint64) []byte {
		r := data
		r.Seq = s
		b := slicesClone(body)
		binary.LittleEndian.PutUint64(b[0:8], r.Seq)

		return b
	}

	tests := []struct {
		name string
		env  BusEnvelope
		body []byte
	}{
		{"layout missing", BusEnvelope{MsgID: id}, body},
		{"layout 2", BusEnvelope{Layout: "2", MsgID: id}, body},
		{"layout 01", BusEnvelope{Layout: "01", MsgID: id}, body},
		{"layout with space", BusEnvelope{Layout: " 1", MsgID: id}, body},
		{"body of 43 bytes", env, body[:43]},
		{"empty body", env, nil},
		{"inline one byte short", env, body[:len(body)-1]},
		{"inline one byte long", env, append(slicesClone(body), 0)},
		{"inline header only", env, body[:44]},
		{"msg id missing", BusEnvelope{Layout: "1"}, body},
		{"msg id seq with a leading zero", BusEnvelope{Layout: "1", MsgID: uuidText + ".042"}, body},
		{"msg id seq with a plus sign", BusEnvelope{Layout: "1", MsgID: uuidText + ".+42"}, body},
		{"msg id seq with a minus sign", BusEnvelope{Layout: "1", MsgID: uuidText + ".-42"}, body},
		{"msg id seq empty", BusEnvelope{Layout: "1", MsgID: uuidText + "."}, body},
		{"msg id seq with a space", BusEnvelope{Layout: "1", MsgID: uuidText + ". 42"}, body},
		{"msg id seq not decimal", BusEnvelope{Layout: "1", MsgID: uuidText + ".0x2A"}, body},
		{"msg id without seq", BusEnvelope{Layout: "1", MsgID: uuidText}, body},
		{"msg id with two seqs", BusEnvelope{Layout: "1", MsgID: id + ".1"}, body},
		{"msg id uppercase capture", BusEnvelope{Layout: "1", MsgID: strings.ToUpper(uuidText) + ".42"}, body},
		{"msg id capture without hyphens", BusEnvelope{Layout: "1", MsgID: strings.ReplaceAll(uuidText, "-", "") + ".42"}, body},
		{"msg id capture in braces", BusEnvelope{Layout: "1", MsgID: "{" + uuidText + "}.42"}, body},
		{"msg id disagreeing with the body", BusEnvelope{Layout: "1", MsgID: uuidText + ".43"}, body},
		{"msg id seq 2^63-1", BusEnvelope{Layout: "1", MsgID: uuidText + ".9223372036854775807"}, bodyAt(math.MaxInt64)},
		{"msg id seq above u64", BusEnvelope{Layout: "1", MsgID: uuidText + ".18446744073709551616"}, body},
		{"body seq above 2^63-1", BusEnvelope{Layout: "1", MsgID: uuidText + ".9223372036854775808"}, bodyAt(1 << 63)},
		{"claim-check with a payload", ccEnv, body},
		{"digest without ref", BusEnvelope{Layout: "1", MsgID: id, PayloadDigest: ccEnv.PayloadDigest}, ccBody},
		{"digest without ref, inline", BusEnvelope{Layout: "1", MsgID: id, PayloadDigest: ccEnv.PayloadDigest}, body},
		{"ref without digest", withRef(ccEnv, ccEnv.PayloadRef, ""), ccBody},
		{"ref without bucket", withRef(ccEnv, "/"+id, ccEnv.PayloadDigest), ccBody},
		{"ref without slash", withRef(ccEnv, "bucket"+id, ccEnv.PayloadDigest), ccBody},
		{"ref without object", withRef(ccEnv, "bucket/", ccEnv.PayloadDigest), ccBody},
		{"ref naming another capture", withRef(ccEnv, "bucket/"+otherCapture.String()+".42", ccEnv.PayloadDigest), ccBody},
		{"ref naming another seq", withRef(ccEnv, "bucket/"+uuidText+".41", ccEnv.PayloadDigest), ccBody},
		{"ref with a leading zero", withRef(ccEnv, "bucket/"+uuidText+".042", ccEnv.PayloadDigest), ccBody},
		{"ref with an uppercase capture", withRef(ccEnv, "bucket/"+strings.ToUpper(uuidText)+".42", ccEnv.PayloadDigest), ccBody},
		{"ref with a nested path", withRef(ccEnv, "bucket/x/"+id, ccEnv.PayloadDigest), ccBody},
		{"digest of another prefix", withRef(ccEnv, ccEnv.PayloadRef, "SHA-512="+ccEnv.PayloadDigest[len("SHA-256="):]), ccBody},
		{"digest of a lowercase prefix", withRef(ccEnv, ccEnv.PayloadRef, "sha-256="+ccEnv.PayloadDigest[len("SHA-256="):]), ccBody},
		{"digest without prefix", withRef(ccEnv, ccEnv.PayloadRef, ccEnv.PayloadDigest[len("SHA-256="):]), ccBody},
		{"digest of 31 bytes", withRef(ccEnv, ccEnv.PayloadRef, "SHA-256="+base64.URLEncoding.EncodeToString(make([]byte, 31))), ccBody},
		{"digest of 33 bytes", withRef(ccEnv, ccEnv.PayloadRef, "SHA-256="+base64.URLEncoding.EncodeToString(make([]byte, 33))), ccBody},
		{"digest unpadded", withRef(ccEnv, ccEnv.PayloadRef, "SHA-256="+base64.RawURLEncoding.EncodeToString(make([]byte, 32))), ccBody},
		{"digest in the standard alphabet", withRef(ccEnv, ccEnv.PayloadRef, "SHA-256="+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xFF}, 32))), ccBody},
		{"digest with a line break", withRef(ccEnv, ccEnv.PayloadRef, ccEnv.PayloadDigest[:20]+"\n"+ccEnv.PayloadDigest[20:]), ccBody},
		{"claim-check body with a payload byte", ccEnv, append(slicesClone(ccBody), 0)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			br, err := CheckBusMessage(tt.env, tt.body)
			require.ErrorIs(t, err, ErrBusMalformed)
			assert.NotErrorIs(t, err, ErrBusNotAccepted)
			assert.Equal(t, BusRecord{}, br)
		})
	}

	t.Run("valid inline and claim-check", func(t *testing.T) {
		t.Parallel()

		_, err := CheckBusMessage(env, body)
		require.NoError(t, err)
		br, err := CheckBusMessage(ccEnv, ccBody)
		require.NoError(t, err)
		assert.Equal(t, "bucket", br.Bucket)
	})
}

// withRef returns env with the claim-check headers ref and digest.
func withRef(env BusEnvelope, ref, digest string) BusEnvelope {
	env.PayloadRef, env.PayloadDigest = ref, digest

	return env
}

// slicesClone returns a copy of b that never aliases it.
func slicesClone(b []byte) []byte { return append([]byte(nil), b...) }

// largeClaimCheck returns a claim-check message declaring a payload of 16 MiB, its body and headers.
func largeClaimCheck(t *testing.T) ([]byte, BusEnvelope) {
	t.Helper()

	header := Record{Seq: 7, TSUTCNs: 1, Kind: KindData, Fidelity: FidelityWireExact}
	br := NewBusRecord(&header)
	br.ClaimCheck, br.PayloadLen, br.Bucket = true, 16<<20, "big"
	body, err := EncodeBusBody(&br)
	require.NoError(t, err)
	env := busEnvelope(t, 7)
	env.PayloadRef = "big/" + env.MsgID
	env.PayloadDigest = BusPayloadDigest(nil)

	return body, env
}

// TestBusLargeClaimCheckAllocs is not parallel: testing.Benchmark counts the allocations of every goroutine.
func TestBusLargeClaimCheckAllocs(t *testing.T) {
	body, env := largeClaimCheck(t)
	got, err := CheckBusMessage(env, body)
	require.NoError(t, err)

	check := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			if _, err := CheckBusMessage(env, body); err != nil {
				b.Fatal(err)
			}
		}
	})
	encode := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			if _, err := EncodeBusBody(&got); err != nil {
				b.Fatal(err)
			}
		}
	})
	t.Logf("CheckBusMessage %d B/op, EncodeBusBody %d B/op for a declared 16 MiB", check.AllocedBytesPerOp(), encode.AllocedBytesPerOp())
	assert.Less(t, check.AllocedBytesPerOp(), int64(1024), "CheckBusMessage does not scale with the declared length")
	assert.Less(t, encode.AllocedBytesPerOp(), int64(1024), "EncodeBusBody does not scale with the declared length")
}

func TestBusLargeClaimCheck(t *testing.T) {
	t.Parallel()

	const declared = 16 << 20
	header := Record{Seq: 7, TSUTCNs: 1, Kind: KindData, Fidelity: FidelityWireExact}
	br := NewBusRecord(&header)
	br.ClaimCheck, br.PayloadLen, br.Bucket = true, declared, "big"
	body, err := EncodeBusBody(&br)
	require.NoError(t, err)
	require.Len(t, body, 44)
	assert.Equal(t, uint32(declared), binary.LittleEndian.Uint32(body[28:32]))

	env := busEnvelope(t, 7)
	env.PayloadRef = "big/" + env.MsgID
	env.PayloadDigest = BusPayloadDigest(nil)

	got, err := CheckBusMessage(env, body)
	require.NoError(t, err)
	assert.Equal(t, uint32(declared), got.PayloadLen)
	again, err := EncodeBusBody(&got)
	require.NoError(t, err)
	assert.Equal(t, body, again, "re-encoded without fetching anything")

	t.Run("inline length mismatch", func(t *testing.T) {
		t.Parallel()

		r := Record{Seq: 7, Kind: KindData, Payload: busFrame(1, 1, nil)}
		inline := NewBusRecord(&r)
		inline.PayloadLen++
		_, err := EncodeBusBody(&inline)
		require.ErrorIs(t, err, ErrBusMalformed)
	})

	t.Run("fetched object", func(t *testing.T) {
		t.Parallel()

		r := Record{Seq: 9, TSUTCNs: 1, Kind: KindData, Fidelity: FidelityWireExact, Payload: busFrame(1, 3, bytes.Repeat([]byte{7}, 100))}
		body, env := claimCheck(t, &r, "b")
		for _, tc := range []struct {
			name    string
			payload []byte
			err     error
		}{
			{"matching", r.Payload, nil},
			{"shorter", r.Payload[:len(r.Payload)-1], ErrBusPayloadMismatch},
			{"longer", append(slicesClone(r.Payload), 7), ErrBusPayloadMismatch},
		} {
			br, err := CheckBusMessage(env, body)
			require.NoError(t, err)
			err = CheckBusPayload(&br, tc.payload)
			if tc.err == nil {
				require.NoError(t, err, tc.name)
				assert.Equal(t, r.Payload, br.Record.Payload)

				continue
			}
			require.ErrorIs(t, err, tc.err, tc.name)
			assert.NotErrorIs(t, err, ErrBusMalformed, tc.name)
			assert.Nil(t, br.Record.Payload, tc.name)
		}
	})
}

func TestBusDescriptorHeader(t *testing.T) {
	t.Parallel()

	// A descriptor whose base64 differs between the two alphabets and needs padding.
	var desc []byte
	for i := 0; ; i++ {
		m := descriptorMeta()
		m.Recorder = fmt.Sprintf("gateway/%d", i)
		b, err := EncodeDescriptor(m)
		require.NoError(t, err)
		if base64.StdEncoding.EncodeToString(b) != base64.URLEncoding.EncodeToString(b) && len(b)%3 != 0 {
			desc = b

			break
		}
	}
	std := base64.StdEncoding.EncodeToString(desc)
	tests := []struct {
		name       string
		descriptor string
	}{
		{"missing", ""},
		{"url alphabet", base64.URLEncoding.EncodeToString(desc)},
		{"unpadded", base64.RawStdEncoding.EncodeToString(desc)},
		{"with a line break", std[:16] + "\n" + std[16:]},
		{"with a carriage return", std[:16] + "\r" + std[16:]},
		{"with a space", " " + std},
		{"not base64", "!!!!"},
		{"refused descriptor", base64.StdEncoding.EncodeToString(desc[:len(desc)-1])},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			br, err := CheckBusMessage(startEnvelope(t, tt.descriptor), startBody(t))
			require.ErrorIs(t, err, ErrBusNotAccepted)
			assert.NotErrorIs(t, err, ErrBusMalformed)
			assert.Equal(t, BusRecord{}, br)
		})
	}

	t.Run("accepted", func(t *testing.T) {
		t.Parallel()

		br, err := CheckBusMessage(startEnvelope(t, std), startBody(t))
		require.NoError(t, err)
		assert.Equal(t, desc, br.Descriptor)
	})

	t.Run("ignored on a data record", func(t *testing.T) {
		t.Parallel()

		r := Record{Seq: 1, TSUTCNs: 1, Kind: KindData, Fidelity: FidelityWireExact, Payload: busFrame(1, 1, nil)}
		for _, d := range []string{std, "!!!!", base64.StdEncoding.EncodeToString(desc[:3])} {
			env := busEnvelope(t, 1)
			env.Descriptor = d
			br, err := CheckBusMessage(env, busEncode(t, &r))
			require.NoError(t, err)
			assert.Nil(t, br.Descriptor)
		}
	})

	t.Run("ignored on a stop", func(t *testing.T) {
		t.Parallel()

		r := Record{Seq: 1, TSUTCNs: 1, Kind: KindTransportEvent, Fidelity: FidelityNotApplicable,
			Payload: busEventPayload(t, &TransportEvent{Event: EventCaptureBoundary, BoundaryKind: new(BoundaryKindStop)})}
		env := busEnvelope(t, 1)
		env.Descriptor = "!!!!"
		br, err := CheckBusMessage(env, busEncode(t, &r))
		require.NoError(t, err)
		assert.Nil(t, br.Descriptor)
	})

	t.Run("a transport event that does not decode is not a start", func(t *testing.T) {
		t.Parallel()

		r := Record{Seq: 1, TSUTCNs: 1, Kind: KindTransportEvent, Fidelity: FidelityNotApplicable, Payload: []byte{1, 2, 3}}
		_, err := CheckBusMessage(busEnvelope(t, 1), busEncode(t, &r))
		require.NoError(t, err)
	})
}

// natsHeaderBlockLen returns the length of the NATS header block that carries headers:
// the version line, one "name: value" line per header and the blank line.
func natsHeaderBlockLen(headers [][2]string) int {
	n := len("NATS/1.0\r\n") + len("\r\n")
	for _, h := range headers {
		n += len(h[0]) + len(": ") + len(h[1]) + len("\r\n")
	}

	return n
}

func TestBusSizing(t *testing.T) {
	t.Parallel()

	t.Run("max_payload", func(t *testing.T) {
		t.Parallel()

		require.Error(t, CheckBusStart(100, 44, BusMinMaxPayload-1))
		require.ErrorIs(t, CheckBusStart(100, 44, 4139), ErrBusMalformed)
		require.NoError(t, CheckBusStart(100, 44, 4140))
		require.NoError(t, CheckBusStart(100, 44, 4141))
		require.NoError(t, CheckBusStart(BusHeaderBlockLimit, 44, BusMinMaxPayload), "every header-only message fits the minimum")
		require.Error(t, CheckBusStart(BusHeaderBlockLimit, 45, BusMinMaxPayload), "the whole message counts against max_payload")
		require.Error(t, CheckBusStart(-1, 44, 1<<20))
		require.Error(t, CheckBusStart(10, 43, 1<<20), "a body holds the 44-byte header")
		require.ErrorIs(t, CheckBusStart(BusHeaderBlockLimit, math.MaxInt, BusMinMaxPayload), ErrBusMalformed, "a body length near the int limit")
		require.ErrorIs(t, CheckBusStart(BusHeaderBlockLimit, math.MaxInt, math.MaxInt), ErrBusMalformed, "header block and body exceed even the largest max_payload")
		require.NoError(t, CheckBusStart(BusHeaderBlockLimit, math.MaxInt-BusHeaderBlockLimit, math.MaxInt))
	})

	t.Run("inline bound", func(t *testing.T) {
		t.Parallel()

		for _, maxPayload := range []int{4140, 4141, 1 << 20} {
			assert.True(t, BusInline(maxPayload-4096, maxPayload))
			assert.False(t, BusInline(maxPayload-4096+1, maxPayload))
		}
		assert.True(t, BusInline(44, 4140))
		assert.False(t, BusInline(45, 4140))
		assert.False(t, BusInline(44, 4139), "a max_payload below 4140 admits no message")
		assert.False(t, BusInline(0, math.MinInt), "no wrap below the int limit")
		assert.True(t, BusInline(math.MaxInt-BusHeaderBlockLimit, math.MaxInt))
	})

	t.Run("header block", func(t *testing.T) {
		t.Parallel()

		id, err := BusMsgID(busCapture, 0)
		require.NoError(t, err)
		ccHeaders := [][2]string{
			{BusHeaderPayloadRef, "bucket/" + id},
			{BusHeaderPayloadDigest, BusPayloadDigest(nil)},
		}

		// A descriptor large enough that its header block passes 4096 bytes with and without the claim-check headers.
		m := descriptorMeta()
		m.Recorder = strings.Repeat("r", 3100)
		desc, err := EncodeDescriptor(m)
		require.NoError(t, err)
		headers := make([][2]string, 0, 3+len(ccHeaders))
		headers = append(headers,
			[2]string{BusHeaderLayout, strconv.Itoa(BusLayout1)},
			[2]string{BusHeaderMsgID, id},
			[2]string{BusHeaderDescriptor, base64.StdEncoding.EncodeToString(desc)},
		)
		plain := natsHeaderBlockLen(headers)
		withCC := natsHeaderBlockLen(append(headers, ccHeaders...))
		require.Greater(t, plain, BusHeaderBlockLimit)
		require.ErrorIs(t, CheckBusStart(plain, 44+200, 1<<20), ErrBusMalformed)
		require.ErrorIs(t, CheckBusStart(withCC, 44, 1<<20), ErrBusMalformed)

		// A data message whose long bucket name takes its block over the bound.
		dataHeaders := [][2]string{
			{BusHeaderLayout, "1"},
			{BusHeaderMsgID, id},
			{BusHeaderPayloadRef, strings.Repeat("b", 4000) + "/" + id},
			{BusHeaderPayloadDigest, BusPayloadDigest(nil)},
		}
		require.ErrorIs(t, CheckBusStart(natsHeaderBlockLen(dataHeaders), 44, 1<<20), ErrBusMalformed)
		require.NoError(t, CheckBusStart(natsHeaderBlockLen(dataHeaders[:2]), 44, 1<<20))
	})
}

func TestBusToolToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		tool  string
		token string
	}{
		{"tool-A_1~", "tool-A_1~"},
		{"a.b", "a%2Eb"},
		{"a*b>c", "a%2Ab%3Ec"},
		{"50%", "50%25"},
		{"x y\tz", "x%20y%09z"},
		{"café", "caf%C3%A9"},
		{"a/b", "a%2Fb"},
		{"...", "%2E%2E%2E"},
	}
	for _, tt := range tests {
		got, err := BusToolToken(tt.tool)
		require.NoError(t, err, tt.tool)
		assert.Equal(t, tt.token, got)
		assert.NotContainsf(t, got, ".", "%q", tt.tool)
		back, err := ParseBusToolToken(got)
		require.NoError(t, err, tt.token)
		assert.Equal(t, tt.tool, back)
	}

	for _, tool := range []string{"", ".", ".."} {
		_, err := BusToolToken(tool)
		require.ErrorIs(t, err, ErrBusMalformed, "%q", tool)
	}
	for _, token := range []string{"", "%2E", "%2E%2E", "a.b", "a*b", "a>b", "a b", "%2e", "%41", "%2", "%G0", "a%", "café"} {
		_, err := ParseBusToolToken(token)
		require.ErrorIs(t, err, ErrBusMalformed, "%q", token)
	}

	subject, err := BusSubject("tpk.traffic", "tool.A", busCapture)
	require.NoError(t, err)
	assert.Equal(t, "tpk.traffic.tool%2EA."+busCapture.String(), subject)
	for _, prefix := range []string{"", ".", "a.", ".a", "a..b", "a b", "a*", "a.>", "a\tb"} {
		_, err := BusSubject(prefix, "tool", busCapture)
		require.ErrorIs(t, err, ErrBusMalformed, "%q", prefix)
	}
	_, err = BusSubject("p", "..", busCapture)
	require.ErrorIs(t, err, ErrBusMalformed)
}

func TestBusMsgID(t *testing.T) {
	t.Parallel()

	for _, seq := range []uint64{0, 1, 42, math.MaxInt64 - 1} {
		id, err := BusMsgID(busCapture, seq)
		require.NoError(t, err)
		assert.Equal(t, fmt.Sprintf("%s.%d", busCapture, seq), id)
		c, s, err := ParseBusMsgID(id)
		require.NoError(t, err)
		assert.Equal(t, busCapture, c)
		assert.Equal(t, seq, s)
	}
	for _, seq := range []uint64{math.MaxInt64, 1 << 63, math.MaxUint64} {
		_, err := BusMsgID(busCapture, seq)
		require.ErrorIs(t, err, ErrBusMalformed, "%d", seq)
	}
	_, _, err := ParseBusMsgID(busCapture.String() + ".00")
	require.ErrorIs(t, err, ErrBusMalformed)
}

func TestBusPayloadCheck(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "SHA-256=47DEQpj8HBSa-_TImW-5JCeuQeRkm5NMpJWZG3hSuFU=", BusPayloadDigest(nil), "the digest of no bytes")
	d := BusPayloadDigest([]byte("payload"))
	assert.Len(t, d, len("SHA-256=")+44)
	assert.True(t, strings.HasPrefix(d, "SHA-256="))

	r := Record{Seq: 3, TSUTCNs: 1, Kind: KindData, Fidelity: FidelityWireExact, Payload: busFrame(1, 3, []byte("abcdefgh"))}
	body, env := claimCheck(t, &r, "b")

	corrupted := slicesClone(r.Payload)
	corrupted[len(corrupted)-1] ^= 0x01
	for _, tc := range []struct {
		name    string
		payload []byte
	}{
		{"equal-length corrupted", corrupted},
		{"shorter", r.Payload[:5]},
		{"longer", append(slicesClone(r.Payload), 0)},
		{"empty", nil},
	} {
		br, err := CheckBusMessage(env, body)
		require.NoError(t, err)
		err = CheckBusPayload(&br, tc.payload)
		require.ErrorIs(t, err, ErrBusPayloadMismatch, tc.name)
		assert.Nil(t, br.Record.Payload, tc.name)
	}

	br, err := CheckBusMessage(env, body)
	require.NoError(t, err)
	require.NoError(t, CheckBusPayload(&br, r.Payload))
	assert.Equal(t, r.Payload, br.Record.Payload)

	inline, err := CheckBusMessage(busEnvelope(t, 3), busEncode(t, &r))
	require.NoError(t, err)
	require.Error(t, CheckBusPayload(&inline, r.Payload), "an inline record has no object to check")
}

// busRawBody returns a 44-byte header of seq with every byte the normalization concerns set,
// followed by payload.
func busRawBody(seq uint64, quality uint16, payload []byte) []byte {
	b := binary.LittleEndian.AppendUint64(nil, seq)
	b = binary.LittleEndian.AppendUint64(b, uint64(1_700_000_000_000_000_123))
	b = binary.LittleEndian.AppendUint64(b, 555)
	b = binary.LittleEndian.AppendUint32(b, 9)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(payload)))
	b = binary.LittleEndian.AppendUint32(b, 7) // trailing_bytes
	b = binary.LittleEndian.AppendUint16(b, quality)
	b = append(b, byte(KindData), byte(DirEquipmentToHost), byte(FidelityWireExact),
		5,    // decode_status
		0xFF, // field_validity
		0xFF, // record_flags
	)

	return append(b, payload...)
}

func TestBusNormalization(t *testing.T) {
	t.Parallel()

	payload := busFrame(1, 3, []byte{1, 2, 3})
	// Every quality bit but redacted, which the producer never sets: bit 3 retired, bits 8-15 reserved.
	body := busRawBody(12, 0xFFDF, payload)
	env := busEnvelope(t, 12)

	br, err := CheckBusMessage(env, body)
	require.NoError(t, err)
	assert.Equal(t, Record{
		Seq:         12,
		TSUTCNs:     1_700_000_000_000_000_123,
		MonoNs:      555,
		MonoPresent: true,
		Epoch:       9,
		Kind:        KindData,
		Dir:         DirEquipmentToHost,
		Fidelity:    FidelityWireExact,
		Quality:     Quality(0xFFDF) & qualityMask,
		Payload:     payload,
	}, br.Record)
	assert.Equal(t, uint32(len(payload)), br.PayloadLen)
	assert.Equal(t, busCapture, br.CaptureID)

	enc, err := EncodeBusBody(&br)
	require.NoError(t, err)
	want := slicesClone(body)
	copy(want[32:36], []byte{0, 0, 0, 0})
	binary.LittleEndian.PutUint16(want[36:38], 0xFFDF&0x00B7)
	want[41], want[42], want[43] = 0, 0, 0x02
	assert.Equal(t, want, enc, "re-encoding gives the normalized bytes")

	again, err := CheckBusMessage(env, enc)
	require.NoError(t, err)
	assert.Equal(t, br, again)
	enc2, err := EncodeBusBody(&again)
	require.NoError(t, err)
	assert.Equal(t, enc, enc2, "a second encode gives the same bytes")

	// mono_ns is kept as decoded when mono_present is clear.
	cleared := slicesClone(body)
	cleared[43] = 0xFD
	br, err = CheckBusMessage(env, cleared)
	require.NoError(t, err)
	assert.False(t, br.Record.MonoPresent)
	assert.Equal(t, int64(555), br.Record.MonoNs)
	enc, err = EncodeBusBody(&br)
	require.NoError(t, err)
	assert.Equal(t, int64(555), int64(binary.LittleEndian.Uint64(enc[16:24])))
	assert.Equal(t, byte(0), enc[43])
}

func TestEncodeBusBodyRefuses(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		rec  Record
	}{
		{"redacted", Record{Seq: 1, Kind: KindData, Quality: QualityRedacted, Payload: busFrame(1, 1, nil)}},
		{"seq 2^63-1", Record{Seq: math.MaxInt64, Kind: KindTransportEvent}},
		{"seq 2^63", Record{Seq: 1 << 63, Kind: KindTransportEvent}},
	} {
		br := NewBusRecord(&tc.rec)
		b, err := EncodeBusBody(&br)
		require.ErrorIs(t, err, ErrBusMalformed, tc.name)
		assert.Nil(t, b, tc.name)

		br.ClaimCheck = true
		_, err = EncodeBusBody(&br)
		require.ErrorIs(t, err, ErrBusMalformed, tc.name)
	}

	// A well-framed body with redacted set is accepted, and its record refused by the encoder.
	payload := busFrame(1, 3, nil)
	br, err := CheckBusMessage(busEnvelope(t, 5), busRawBody(5, uint16(QualityRedacted), payload))
	require.NoError(t, err)
	assert.True(t, br.Record.Quality.Has(QualityRedacted))
	_, err = EncodeBusBody(&br)
	require.ErrorIs(t, err, ErrBusMalformed)
}

func TestDeriveBusRecordRefuses(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		rec  Record
		cls  Classifier
	}{
		{"unknown kind", Record{Kind: KindUnknown}, nil},
		{"kind out of range", Record{Kind: Kind(9)}, nil},
		{"trailing bytes out of range", Record{Kind: KindData, Payload: busFrame(1, 1, nil)},
			&fakeClassifier{status: DecodeStatusOKWithTrailing, trailing: -1}},
	} {
		require.ErrorIs(t, DeriveBusRecord(&tc.rec, tc.cls), ErrInvalidRecord, tc.name)
	}
}

// TestBusInlineBodyLen checks that an inline body's length is computed without overflow:
// a payload whose body would pass the int limit, as on a 32-bit platform near 2^31 bytes, is refused before any allocation.
func TestBusInlineBodyLen(t *testing.T) {
	t.Parallel()

	const busTestHeaderLen = 44 // the record header of the format specification §6

	n, err := busInlineBodyLen(0)
	require.NoError(t, err)
	require.Equal(t, busTestHeaderLen, n)

	n, err = busInlineBodyLen(math.MaxInt - busTestHeaderLen)
	require.NoError(t, err)
	require.Equal(t, math.MaxInt, n)

	_, err = busInlineBodyLen(math.MaxInt - busTestHeaderLen + 1)
	require.ErrorIs(t, err, ErrBusMalformed)
}

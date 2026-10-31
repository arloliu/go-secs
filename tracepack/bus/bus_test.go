package bus

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// busCapture is the capture every bus test message belongs to.
var busCapture = tracepack.UUID{0x01, 0x8f, 0x3a, 0x6b, 0x7c, 0x2d, 0x7e, 0x0f, 0x8a, 0x1b, 0x2c, 0x3d, 0x4e, 0x5f, 0x60, 0x71}

// busFrame returns an HSMS data frame: the length prefix, a 10-byte header of stream and function, and text.
func busFrame(stream, function uint8, text []byte) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(10+len(text)))
	b = append(b, 0x00, 0x01, stream|0x80, function, 0x00, 0x00, 0x01, 0x02, 0x03, 0x04)

	return append(b, text...)
}

// busEventPayload returns the payload of a transport-event record carrying ev.
func busEventPayload(t testing.TB, ev *tracepack.TransportEvent) []byte {
	t.Helper()

	b, err := ev.MarshalBinary()
	require.NoError(t, err)

	return b
}

// busStartRecord returns the start boundary record of the capture at seq 0, as a producer publishes it.
func busStartRecord(t testing.TB) tracepack.Record {
	t.Helper()

	return tracepack.Record{
		Seq:      0,
		TSUTCNs:  1_700_000_000_000_000_000,
		Kind:     tracepack.KindTransportEvent,
		Dir:      tracepack.DirLocal,
		Fidelity: tracepack.FidelityNotApplicable,
		Payload: busEventPayload(t, &tracepack.TransportEvent{
			Event:        tracepack.EventCaptureBoundary,
			BoundaryKind: new(tracepack.BoundaryKindStart),
		}),
	}
}

// busEncode encodes r inline, as a producer does.
func busEncode(t testing.TB, r *tracepack.Record) []byte {
	t.Helper()

	br := NewRecord(r)
	b, err := EncodeBody(&br)
	require.NoError(t, err)

	return b
}

// busEnvelope returns the headers of an inline message of seq.
func busEnvelope(t testing.TB, seq uint64) Envelope {
	t.Helper()

	id, err := MsgID(busCapture, seq)
	require.NoError(t, err)

	return Envelope{Layout: "1", MsgID: id}
}

// startBody returns the inline body of the capture's start record.
func startBody(t testing.TB) []byte {
	t.Helper()

	r := busStartRecord(t)

	return busEncode(t, &r)
}

// startEnvelope returns the headers of the start message, with descriptor as the Tpk-Capture-Descriptor value.
func startEnvelope(t testing.TB, descriptor string) Envelope {
	t.Helper()

	env := busEnvelope(t, 0)
	env.Descriptor = descriptor

	return env
}

// claimCheck returns the header-only body of r and the headers of its claim-check message in bucket.
func claimCheck(t testing.TB, r *tracepack.Record, bucket string) ([]byte, Envelope) {
	t.Helper()

	br := NewRecord(r)
	br.ClaimCheck = true
	br.Bucket = bucket
	br.Digest = sha256.Sum256(r.Payload)
	body, err := EncodeBody(&br)
	require.NoError(t, err)
	require.Len(t, body, 44)

	env := busEnvelope(t, r.Seq)
	env.PayloadRef = bucket + "/" + env.MsgID
	env.PayloadDigest = PayloadDigest(r.Payload)

	return body, env
}

func TestBodyRoundTrip(t *testing.T) {
	t.Parallel()

	text := []byte{0x41, 0x02, 'o', 'k', 0xFF, 0xFF, 0xFF}
	unobservedText := make([]byte, 12)
	tests := []struct {
		name string
		rec  tracepack.Record
		cls  *fakeClassifier
		// want holds the derived values: decode_status, trailing_bytes and field_validity.
		wantStatus   tracepack.DecodeStatus
		wantTrailing uint32
		wantValidity tracepack.FieldValidity
		wantCalls    int
	}{
		{
			name:       "start",
			rec:        busStartRecord(t),
			wantStatus: tracepack.DecodeStatusNotApplicable,
		},
		{
			name: "socket event",
			rec: tracepack.Record{Seq: 1, TSUTCNs: 5, Epoch: 1, Kind: tracepack.KindTransportEvent, Dir: tracepack.DirLocal, Fidelity: tracepack.FidelityNotApplicable,
				Payload: busEventPayload(t, &tracepack.TransportEvent{Event: tracepack.EventSocketConnect, SocketRole: new(tracepack.SocketRoleActive)})},
			wantStatus: tracepack.DecodeStatusNotApplicable,
		},
		{
			name: "clock-step",
			rec: tracepack.Record{Seq: 2, TSUTCNs: 6, Kind: tracepack.KindTransportEvent, Dir: tracepack.DirLocal, Fidelity: tracepack.FidelityNotApplicable,
				Payload: busEventPayload(t, &tracepack.TransportEvent{Event: tracepack.EventClockStep, ClockStepNs: new(int64(-3_000_000_000))})},
			wantStatus: tracepack.DecodeStatusNotApplicable,
		},
		{
			name: "orderly stop",
			rec: tracepack.Record{Seq: 3, TSUTCNs: 7, Kind: tracepack.KindTransportEvent, Dir: tracepack.DirLocal, Fidelity: tracepack.FidelityNotApplicable,
				Payload: busEventPayload(t, &tracepack.TransportEvent{Event: tracepack.EventCaptureBoundary, BoundaryKind: new(tracepack.BoundaryKindStop)})},
			wantStatus: tracepack.DecodeStatusNotApplicable,
		},
		{
			name: "annotation",
			rec: tracepack.Record{Seq: 4, TSUTCNs: 8, Kind: tracepack.KindAnnotation, Dir: tracepack.DirLocal, Fidelity: tracepack.FidelityNotApplicable,
				Payload: func() []byte {
					b, err := (&tracepack.Annotation{AnnotationKind: tracepack.AnnotationKindNote, Text: new("hello")}).MarshalBinary()
					require.NoError(t, err)

					return b
				}()},
			wantStatus: tracepack.DecodeStatusNotApplicable,
		},
		{
			name: "data frame",
			rec: tracepack.Record{Seq: 5, TSUTCNs: 9, MonoNs: 77, MonoPresent: true, Epoch: 2, Kind: tracepack.KindData, Dir: tracepack.DirEquipmentToHost,
				Fidelity: tracepack.FidelityWireExact, Quality: tracepack.QualityDirectionInferred, Payload: busFrame(6, 11, text),
				// The producer's derived values never travel: the encoder writes zero.
				DecodeStatus: tracepack.DecodeStatusShortFrame, TrailingBytes: 9, FieldValidity: tracepack.FieldValiditySessionID},
			cls:          &fakeClassifier{name: "c", status: tracepack.DecodeStatusOKWithTrailing, trailing: 3},
			wantStatus:   tracepack.DecodeStatusOKWithTrailing,
			wantTrailing: 3,
			wantValidity: fieldValidityMask,
			wantCalls:    1,
		},
		{
			name:         "short data frame",
			rec:          tracepack.Record{Seq: 6, TSUTCNs: 10, Epoch: 2, Kind: tracepack.KindData, Dir: tracepack.DirHostToEquipment, Fidelity: tracepack.FidelityWireExact, Payload: []byte{0, 0, 0, 10, 0, 1, 0x86, 11}},
			cls:          &fakeClassifier{name: "c", status: tracepack.DecodeStatusShortFrame, trailing: 5},
			wantStatus:   tracepack.DecodeStatusShortFrame,
			wantValidity: tracepack.FieldValiditySessionID | tracepack.FieldValidityStreamAndW | tracepack.FieldValidityFunction,
			wantCalls:    1,
		},
		{
			name:         "control frame",
			rec:          tracepack.Record{Seq: 7, TSUTCNs: 11, Epoch: 2, Kind: tracepack.KindControl, Dir: tracepack.DirHostToEquipment, Fidelity: tracepack.FidelityWireExact, Payload: busFrame(0, 0, nil)},
			cls:          &fakeClassifier{name: "c", status: tracepack.DecodeStatusOK},
			wantStatus:   tracepack.DecodeStatusOK,
			wantValidity: fieldValidityMask,
			wantCalls:    1,
		},
		{
			name:         "data frame without a classifier",
			rec:          tracepack.Record{Seq: 8, TSUTCNs: 12, Epoch: 2, Kind: tracepack.KindData, Dir: tracepack.DirHostToEquipment, Fidelity: tracepack.FidelityReEncoded, Payload: busFrame(1, 1, nil)},
			wantStatus:   tracepack.DecodeStatusNotAttempted,
			wantValidity: fieldValidityMask,
		},
		{
			name: "unobserved frame",
			rec: tracepack.Record{Seq: 9, TSUTCNs: 13, Epoch: 2, Kind: tracepack.KindData, Dir: tracepack.DirEquipmentToHost, Fidelity: tracepack.FidelityReEncoded,
				Quality: tracepack.QualityUnobserved, Payload: busFrame(1, 4, unobservedText)},
			cls:          &fakeClassifier{name: "c", status: tracepack.DecodeStatusOK},
			wantStatus:   tracepack.DecodeStatusNotAttempted,
			wantValidity: fieldValidityMask,
		},
		{
			name: "wire-exact frame in a decoded-message capture",
			rec: tracepack.Record{Seq: 10, TSUTCNs: 14, Epoch: 3, Kind: tracepack.KindData, Dir: tracepack.DirEquipmentToHost, Fidelity: tracepack.FidelityWireExact,
				Payload: busFrame(6, 11, text)},
			cls:          &fakeClassifier{name: "c", status: tracepack.DecodeStatusOK},
			wantStatus:   tracepack.DecodeStatusOK,
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
			br, err := CheckMessage(env, body)
			require.NoError(t, err)
			assert.Equal(t, busCapture, br.CaptureID)
			assert.Equal(t, uint32(len(tt.rec.Payload)), br.PayloadLen)
			assert.False(t, br.ClaimCheck)

			want := tt.rec
			want.DecodeStatus, want.TrailingBytes, want.FieldValidity = 0, 0, 0
			assert.Equal(t, want, br.Record, "every producer field kept, fidelity included")

			again, err := EncodeBody(&br)
			require.NoError(t, err)
			assert.Equal(t, body, again, "the encoder's output round-trips exactly")

			var cls tracepack.Classifier
			if tt.cls != nil {
				cls = tt.cls
			}
			require.NoError(t, DeriveRecord(&br.Record, cls))
			assert.Equal(t, tt.wantStatus, br.Record.DecodeStatus)
			assert.Equal(t, tt.wantTrailing, br.Record.TrailingBytes)
			assert.Equal(t, tt.wantValidity, br.Record.FieldValidity)
			if tt.cls != nil {
				assert.Len(t, tt.cls.seen, tt.wantCalls)
			}
		})
	}
}

// TestStartRefusedDescriptor checks that a start whose descriptor DecodeDescriptor refuses is not accepted, never malformed:
// every truncation, a trailing byte, each entry dropped, each entry repeated, and an unknown tag.
func TestStartRefusedDescriptor(t *testing.T) {
	t.Parallel()

	b := descriptorBytes(t)
	entries := descriptorEntries(t)
	cases := make([][]byte, 0, len(b)+1+2*len(entries)+1)
	for n := range len(b) {
		cases = append(cases, b[:n])
	}
	cases = append(cases, append(slicesClone(b), 0x01))
	for i := range entries {
		dropped := append(append([]tlv.Entry{}, entries[:i]...), entries[i+1:]...)
		repeated := append(append([]tlv.Entry{}, entries...), entries[i])
		cases = append(cases, encodeTLV(dropped), encodeTLV(repeated))
	}
	cases = append(cases, encodeTLV(append(append([]tlv.Entry{}, entries...), tlv.U64Entry(0x7F00, 1))))

	refused := 0
	for _, desc := range cases {
		if _, err := tracepack.DecodeDescriptor(desc); err == nil {
			continue
		}
		refused++
		br, err := CheckMessage(startEnvelope(t, base64.StdEncoding.EncodeToString(desc)), startBody(t))
		require.ErrorIs(t, err, ErrNotAccepted, "%x", desc)
		assert.NotErrorIs(t, err, ErrMalformed, "%x", desc)
		assert.Equal(t, Record{}, br)
	}
	// Nearly every truncation, the trailing byte, each required entry dropped, each entry that may not repeat repeated,
	// and the unknown tag: far more cases than the descriptor has bytes.
	assert.Greater(t, refused, len(b)+len(entries))
}

func TestClaimCheckStartRoundTrip(t *testing.T) {
	t.Parallel()

	start := busStartRecord(t)
	desc := descriptorBytes(t)
	body, env := claimCheck(t, &start, "tpk-objects")
	env.Descriptor = base64.StdEncoding.EncodeToString(desc)

	br, err := CheckMessage(env, body)
	require.NoError(t, err)
	assert.True(t, br.ClaimCheck)
	assert.Equal(t, "tpk-objects", br.Bucket)
	assert.Equal(t, sha256.Sum256(start.Payload), br.Digest)
	assert.Equal(t, uint32(len(start.Payload)), br.PayloadLen)
	assert.Nil(t, br.Record.Payload, "a claim-check record has no payload until it is fetched")
	assert.Nil(t, br.Descriptor, "a claim-check start's descriptor is checked once its payload is known")

	again, err := EncodeBody(&br)
	require.NoError(t, err)
	assert.Equal(t, body, again)

	require.NoError(t, CheckPayload(&br, start.Payload))
	assert.Equal(t, start.Payload, br.Record.Payload)
	assert.Equal(t, desc, br.Descriptor)
	m, err := tracepack.DecodeDescriptor(br.Descriptor)
	require.NoError(t, err)
	assert.Equal(t, descriptorMeta(), m)

	again, err = EncodeBody(&br)
	require.NoError(t, err)
	assert.Equal(t, body, again, "a fetched claim-check record still encodes header-only")

	require.NoError(t, DeriveRecord(&br.Record, nil))
	assert.Equal(t, tracepack.DecodeStatusNotApplicable, br.Record.DecodeStatus)
}

func TestClaimCheckStartDescriptor(t *testing.T) {
	t.Parallel()

	start := busStartRecord(t)
	tests := []struct {
		name       string
		descriptor string
	}{
		{"missing", ""},
		{"undecodable", base64.StdEncoding.EncodeToString(descriptorBytes(t)[:10])},
		{"unpadded", base64.RawStdEncoding.EncodeToString(descriptorBytes(t))},
		{"unknown timer", base64.StdEncoding.EncodeToString(encodeTLV(replaceDescriptorEntry(descriptorEntries(t), tagHSMSTimers,
			tlv.NestedEntry(tagHSMSTimers, []tlv.Entry{tlv.U64Entry(9, 1)}))))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			body, env := claimCheck(t, &start, "b")
			env.Descriptor = tt.descriptor
			br, err := CheckMessage(env, body)
			require.NoError(t, err, "the record is not known to be a start before its payload is fetched")

			err = CheckPayload(&br, start.Payload)
			require.ErrorIs(t, err, ErrNotAccepted)
			assert.NotErrorIs(t, err, ErrPayloadMismatch)
		})
	}

	t.Run("ignored on another record", func(t *testing.T) {
		t.Parallel()

		r := tracepack.Record{Seq: 4, TSUTCNs: 1, Kind: tracepack.KindData, Fidelity: tracepack.FidelityWireExact, Payload: busFrame(1, 1, []byte{1, 2, 3})}
		body, env := claimCheck(t, &r, "b")
		env.Descriptor = "!!not base64!!"
		br, err := CheckMessage(env, body)
		require.NoError(t, err)
		require.NoError(t, CheckPayload(&br, r.Payload))
		assert.Nil(t, br.Descriptor)
	})
}

func TestCheckMessage(t *testing.T) {
	t.Parallel()

	const seq = 42
	data := tracepack.Record{Seq: seq, TSUTCNs: 1, Epoch: 1, Kind: tracepack.KindData, Fidelity: tracepack.FidelityWireExact, Payload: busFrame(1, 3, []byte{1, 2})}
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
		env  Envelope
		body []byte
	}{
		{"layout missing", Envelope{MsgID: id}, body},
		{"layout 2", Envelope{Layout: "2", MsgID: id}, body},
		{"layout 01", Envelope{Layout: "01", MsgID: id}, body},
		{"layout with space", Envelope{Layout: " 1", MsgID: id}, body},
		{"body of 43 bytes", env, body[:43]},
		{"empty body", env, nil},
		{"inline one byte short", env, body[:len(body)-1]},
		{"inline one byte long", env, append(slicesClone(body), 0)},
		{"inline header only", env, body[:44]},
		{"msg id missing", Envelope{Layout: "1"}, body},
		{"msg id seq with a leading zero", Envelope{Layout: "1", MsgID: uuidText + ".042"}, body},
		{"msg id seq with a plus sign", Envelope{Layout: "1", MsgID: uuidText + ".+42"}, body},
		{"msg id seq with a minus sign", Envelope{Layout: "1", MsgID: uuidText + ".-42"}, body},
		{"msg id seq empty", Envelope{Layout: "1", MsgID: uuidText + "."}, body},
		{"msg id seq with a space", Envelope{Layout: "1", MsgID: uuidText + ". 42"}, body},
		{"msg id seq not decimal", Envelope{Layout: "1", MsgID: uuidText + ".0x2A"}, body},
		{"msg id without seq", Envelope{Layout: "1", MsgID: uuidText}, body},
		{"msg id with two seqs", Envelope{Layout: "1", MsgID: id + ".1"}, body},
		{"msg id uppercase capture", Envelope{Layout: "1", MsgID: strings.ToUpper(uuidText) + ".42"}, body},
		{"msg id capture without hyphens", Envelope{Layout: "1", MsgID: strings.ReplaceAll(uuidText, "-", "") + ".42"}, body},
		{"msg id capture in braces", Envelope{Layout: "1", MsgID: "{" + uuidText + "}.42"}, body},
		{"msg id disagreeing with the body", Envelope{Layout: "1", MsgID: uuidText + ".43"}, body},
		{"msg id seq 2^63-1", Envelope{Layout: "1", MsgID: uuidText + ".9223372036854775807"}, bodyAt(math.MaxInt64)},
		{"msg id seq above u64", Envelope{Layout: "1", MsgID: uuidText + ".18446744073709551616"}, body},
		{"body seq above 2^63-1", Envelope{Layout: "1", MsgID: uuidText + ".9223372036854775808"}, bodyAt(1 << 63)},
		{"claim-check with a payload", ccEnv, body},
		{"digest without ref", Envelope{Layout: "1", MsgID: id, PayloadDigest: ccEnv.PayloadDigest}, ccBody},
		{"digest without ref, inline", Envelope{Layout: "1", MsgID: id, PayloadDigest: ccEnv.PayloadDigest}, body},
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

			br, err := CheckMessage(tt.env, tt.body)
			require.ErrorIs(t, err, ErrMalformed)
			assert.NotErrorIs(t, err, ErrNotAccepted)
			assert.Equal(t, Record{}, br)
		})
	}

	t.Run("valid inline and claim-check", func(t *testing.T) {
		t.Parallel()

		_, err := CheckMessage(env, body)
		require.NoError(t, err)
		br, err := CheckMessage(ccEnv, ccBody)
		require.NoError(t, err)
		assert.Equal(t, "bucket", br.Bucket)
	})
}

// withRef returns env with the claim-check headers ref and digest.
func withRef(env Envelope, ref, digest string) Envelope {
	env.PayloadRef, env.PayloadDigest = ref, digest

	return env
}

// slicesClone returns a copy of b that never aliases it.
func slicesClone(b []byte) []byte { return append([]byte(nil), b...) }

// largeClaimCheck returns a claim-check message declaring a payload of 16 MiB, its body and headers.
func largeClaimCheck(t *testing.T) ([]byte, Envelope) {
	t.Helper()

	header := tracepack.Record{Seq: 7, TSUTCNs: 1, Kind: tracepack.KindData, Fidelity: tracepack.FidelityWireExact}
	br := NewRecord(&header)
	br.ClaimCheck, br.PayloadLen, br.Bucket = true, 16<<20, "big"
	body, err := EncodeBody(&br)
	require.NoError(t, err)
	env := busEnvelope(t, 7)
	env.PayloadRef = "big/" + env.MsgID
	env.PayloadDigest = PayloadDigest(nil)

	return body, env
}

// TestLargeClaimCheckAllocs is not parallel: testing.Benchmark counts the allocations of every goroutine.
func TestLargeClaimCheckAllocs(t *testing.T) {
	body, env := largeClaimCheck(t)
	got, err := CheckMessage(env, body)
	require.NoError(t, err)

	check := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			if _, err := CheckMessage(env, body); err != nil {
				b.Fatal(err)
			}
		}
	})
	encode := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			if _, err := EncodeBody(&got); err != nil {
				b.Fatal(err)
			}
		}
	})
	t.Logf("CheckMessage %d B/op, EncodeBody %d B/op for a declared 16 MiB", check.AllocedBytesPerOp(), encode.AllocedBytesPerOp())
	assert.Less(t, check.AllocedBytesPerOp(), int64(1024), "CheckMessage does not scale with the declared length")
	assert.Less(t, encode.AllocedBytesPerOp(), int64(1024), "EncodeBody does not scale with the declared length")
}

func TestLargeClaimCheck(t *testing.T) {
	t.Parallel()

	const declared = 16 << 20
	header := tracepack.Record{Seq: 7, TSUTCNs: 1, Kind: tracepack.KindData, Fidelity: tracepack.FidelityWireExact}
	br := NewRecord(&header)
	br.ClaimCheck, br.PayloadLen, br.Bucket = true, declared, "big"
	body, err := EncodeBody(&br)
	require.NoError(t, err)
	require.Len(t, body, 44)
	assert.Equal(t, uint32(declared), binary.LittleEndian.Uint32(body[28:32]))

	env := busEnvelope(t, 7)
	env.PayloadRef = "big/" + env.MsgID
	env.PayloadDigest = PayloadDigest(nil)

	got, err := CheckMessage(env, body)
	require.NoError(t, err)
	assert.Equal(t, uint32(declared), got.PayloadLen)
	again, err := EncodeBody(&got)
	require.NoError(t, err)
	assert.Equal(t, body, again, "re-encoded without fetching anything")

	t.Run("inline length mismatch", func(t *testing.T) {
		t.Parallel()

		r := tracepack.Record{Seq: 7, Kind: tracepack.KindData, Payload: busFrame(1, 1, nil)}
		inline := NewRecord(&r)
		inline.PayloadLen++
		_, err := EncodeBody(&inline)
		require.ErrorIs(t, err, ErrMalformed)
	})

	t.Run("fetched object", func(t *testing.T) {
		t.Parallel()

		r := tracepack.Record{Seq: 9, TSUTCNs: 1, Kind: tracepack.KindData, Fidelity: tracepack.FidelityWireExact, Payload: busFrame(1, 3, bytes.Repeat([]byte{7}, 100))}
		body, env := claimCheck(t, &r, "b")
		for _, tc := range []struct {
			name    string
			payload []byte
			err     error
		}{
			{"matching", r.Payload, nil},
			{"shorter", r.Payload[:len(r.Payload)-1], ErrPayloadMismatch},
			{"longer", append(slicesClone(r.Payload), 7), ErrPayloadMismatch},
		} {
			br, err := CheckMessage(env, body)
			require.NoError(t, err)
			err = CheckPayload(&br, tc.payload)
			if tc.err == nil {
				require.NoError(t, err, tc.name)
				assert.Equal(t, r.Payload, br.Record.Payload)

				continue
			}
			require.ErrorIs(t, err, tc.err, tc.name)
			assert.NotErrorIs(t, err, ErrMalformed, tc.name)
			assert.Nil(t, br.Record.Payload, tc.name)
		}
	})
}

func TestDescriptorHeader(t *testing.T) {
	t.Parallel()

	// A descriptor whose base64 differs between the two alphabets and needs padding.
	var desc []byte
	for i := 0; ; i++ {
		m := descriptorMeta()
		m.Recorder = fmt.Sprintf("gateway/%d", i)
		b, err := tracepack.EncodeDescriptor(m)
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

			br, err := CheckMessage(startEnvelope(t, tt.descriptor), startBody(t))
			require.ErrorIs(t, err, ErrNotAccepted)
			assert.NotErrorIs(t, err, ErrMalformed)
			assert.Equal(t, Record{}, br)
		})
	}

	t.Run("accepted", func(t *testing.T) {
		t.Parallel()

		br, err := CheckMessage(startEnvelope(t, std), startBody(t))
		require.NoError(t, err)
		assert.Equal(t, desc, br.Descriptor)
	})

	t.Run("ignored on a data record", func(t *testing.T) {
		t.Parallel()

		r := tracepack.Record{Seq: 1, TSUTCNs: 1, Kind: tracepack.KindData, Fidelity: tracepack.FidelityWireExact, Payload: busFrame(1, 1, nil)}
		for _, d := range []string{std, "!!!!", base64.StdEncoding.EncodeToString(desc[:3])} {
			env := busEnvelope(t, 1)
			env.Descriptor = d
			br, err := CheckMessage(env, busEncode(t, &r))
			require.NoError(t, err)
			assert.Nil(t, br.Descriptor)
		}
	})

	t.Run("ignored on a stop", func(t *testing.T) {
		t.Parallel()

		r := tracepack.Record{Seq: 1, TSUTCNs: 1, Kind: tracepack.KindTransportEvent, Fidelity: tracepack.FidelityNotApplicable,
			Payload: busEventPayload(t, &tracepack.TransportEvent{Event: tracepack.EventCaptureBoundary, BoundaryKind: new(tracepack.BoundaryKindStop)})}
		env := busEnvelope(t, 1)
		env.Descriptor = "!!!!"
		br, err := CheckMessage(env, busEncode(t, &r))
		require.NoError(t, err)
		assert.Nil(t, br.Descriptor)
	})

	t.Run("a transport event that does not decode is not a start", func(t *testing.T) {
		t.Parallel()

		r := tracepack.Record{Seq: 1, TSUTCNs: 1, Kind: tracepack.KindTransportEvent, Fidelity: tracepack.FidelityNotApplicable, Payload: []byte{1, 2, 3}}
		_, err := CheckMessage(busEnvelope(t, 1), busEncode(t, &r))
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

func TestSizing(t *testing.T) {
	t.Parallel()

	t.Run("max_payload", func(t *testing.T) {
		t.Parallel()

		require.Error(t, CheckStart(100, 44, MinMaxPayload-1))
		require.ErrorIs(t, CheckStart(100, 44, 4139), ErrMalformed)
		require.NoError(t, CheckStart(100, 44, 4140))
		require.NoError(t, CheckStart(100, 44, 4141))
		require.NoError(t, CheckStart(HeaderBlockLimit, 44, MinMaxPayload), "every header-only message fits the minimum")
		require.Error(t, CheckStart(HeaderBlockLimit, 45, MinMaxPayload), "the whole message counts against max_payload")
		require.Error(t, CheckStart(-1, 44, 1<<20))
		require.Error(t, CheckStart(10, 43, 1<<20), "a body holds the 44-byte header")
		require.ErrorIs(t, CheckStart(HeaderBlockLimit, math.MaxInt, MinMaxPayload), ErrMalformed, "a body length near the int limit")
		require.ErrorIs(t, CheckStart(HeaderBlockLimit, math.MaxInt, math.MaxInt), ErrMalformed, "header block and body exceed even the largest max_payload")
		require.NoError(t, CheckStart(HeaderBlockLimit, math.MaxInt-HeaderBlockLimit, math.MaxInt))
	})

	t.Run("inline bound", func(t *testing.T) {
		t.Parallel()

		for _, maxPayload := range []int{4140, 4141, 1 << 20} {
			assert.True(t, Inline(maxPayload-4096, maxPayload))
			assert.False(t, Inline(maxPayload-4096+1, maxPayload))
		}
		assert.True(t, Inline(44, 4140))
		assert.False(t, Inline(45, 4140))
		assert.False(t, Inline(44, 4139), "a max_payload below 4140 admits no message")
		assert.False(t, Inline(0, math.MinInt), "no wrap below the int limit")
		assert.True(t, Inline(math.MaxInt-HeaderBlockLimit, math.MaxInt))
	})

	t.Run("header block", func(t *testing.T) {
		t.Parallel()

		id, err := MsgID(busCapture, 0)
		require.NoError(t, err)
		ccHeaders := [][2]string{
			{HeaderPayloadRef, "bucket/" + id},
			{HeaderPayloadDigest, PayloadDigest(nil)},
		}

		// A descriptor large enough that its header block passes 4096 bytes with and without the claim-check headers.
		m := descriptorMeta()
		m.Recorder = strings.Repeat("r", 3100)
		desc, err := tracepack.EncodeDescriptor(m)
		require.NoError(t, err)
		headers := make([][2]string, 0, 3+len(ccHeaders))
		headers = append(headers,
			[2]string{HeaderLayout, Layout1},
			[2]string{HeaderMsgID, id},
			[2]string{HeaderDescriptor, base64.StdEncoding.EncodeToString(desc)},
		)
		plain := natsHeaderBlockLen(headers)
		withCC := natsHeaderBlockLen(append(headers, ccHeaders...))
		require.Greater(t, plain, HeaderBlockLimit)
		require.ErrorIs(t, CheckStart(plain, 44+200, 1<<20), ErrMalformed)
		require.ErrorIs(t, CheckStart(withCC, 44, 1<<20), ErrMalformed)

		// A data message whose long bucket name takes its block over the bound.
		dataHeaders := [][2]string{
			{HeaderLayout, "1"},
			{HeaderMsgID, id},
			{HeaderPayloadRef, strings.Repeat("b", 4000) + "/" + id},
			{HeaderPayloadDigest, PayloadDigest(nil)},
		}
		require.ErrorIs(t, CheckStart(natsHeaderBlockLen(dataHeaders), 44, 1<<20), ErrMalformed)
		require.NoError(t, CheckStart(natsHeaderBlockLen(dataHeaders[:2]), 44, 1<<20))
	})
}

func TestToolToken(t *testing.T) {
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
		got, err := ToolToken(tt.tool)
		require.NoError(t, err, tt.tool)
		assert.Equal(t, tt.token, got)
		assert.NotContainsf(t, got, ".", "%q", tt.tool)
		back, err := ParseToolToken(got)
		require.NoError(t, err, tt.token)
		assert.Equal(t, tt.tool, back)
	}

	for _, tool := range []string{"", ".", ".."} {
		_, err := ToolToken(tool)
		require.ErrorIs(t, err, ErrMalformed, "%q", tool)
	}
	for _, token := range []string{"", "%2E", "%2E%2E", "a.b", "a*b", "a>b", "a b", "%2e", "%41", "%2", "%G0", "a%", "café"} {
		_, err := ParseToolToken(token)
		require.ErrorIs(t, err, ErrMalformed, "%q", token)
	}

	subject, err := Subject("tpk.traffic", "tool.A", busCapture)
	require.NoError(t, err)
	assert.Equal(t, "tpk.traffic.tool%2EA."+busCapture.String(), subject)
	for _, prefix := range []string{"", ".", "a.", ".a", "a..b", "a b", "a*", "a.>", "a\tb"} {
		_, err := Subject(prefix, "tool", busCapture)
		require.ErrorIs(t, err, ErrMalformed, "%q", prefix)
	}
	_, err = Subject("p", "..", busCapture)
	require.ErrorIs(t, err, ErrMalformed)
}

func TestMsgID(t *testing.T) {
	t.Parallel()

	for _, seq := range []uint64{0, 1, 42, math.MaxInt64 - 1} {
		id, err := MsgID(busCapture, seq)
		require.NoError(t, err)
		assert.Equal(t, fmt.Sprintf("%s.%d", busCapture, seq), id)
		c, s, err := ParseMsgID(id)
		require.NoError(t, err)
		assert.Equal(t, busCapture, c)
		assert.Equal(t, seq, s)
	}
	for _, seq := range []uint64{math.MaxInt64, 1 << 63, math.MaxUint64} {
		_, err := MsgID(busCapture, seq)
		require.ErrorIs(t, err, ErrMalformed, "%d", seq)
	}
	_, _, err := ParseMsgID(busCapture.String() + ".00")
	require.ErrorIs(t, err, ErrMalformed)
}

func TestPayloadCheck(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "SHA-256=47DEQpj8HBSa-_TImW-5JCeuQeRkm5NMpJWZG3hSuFU=", PayloadDigest(nil), "the digest of no bytes")
	d := PayloadDigest([]byte("payload"))
	assert.Len(t, d, len("SHA-256=")+44)
	assert.True(t, strings.HasPrefix(d, "SHA-256="))

	r := tracepack.Record{Seq: 3, TSUTCNs: 1, Kind: tracepack.KindData, Fidelity: tracepack.FidelityWireExact, Payload: busFrame(1, 3, []byte("abcdefgh"))}
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
		br, err := CheckMessage(env, body)
		require.NoError(t, err)
		err = CheckPayload(&br, tc.payload)
		require.ErrorIs(t, err, ErrPayloadMismatch, tc.name)
		assert.Nil(t, br.Record.Payload, tc.name)
	}

	br, err := CheckMessage(env, body)
	require.NoError(t, err)
	require.NoError(t, CheckPayload(&br, r.Payload))
	assert.Equal(t, r.Payload, br.Record.Payload)

	inline, err := CheckMessage(busEnvelope(t, 3), busEncode(t, &r))
	require.NoError(t, err)
	require.Error(t, CheckPayload(&inline, r.Payload), "an inline record has no object to check")
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
	b = append(b, byte(tracepack.KindData), byte(tracepack.DirEquipmentToHost), byte(tracepack.FidelityWireExact),
		5,    // decode_status
		0xFF, // field_validity
		0xFF, // record_flags
	)

	return append(b, payload...)
}

func TestNormalization(t *testing.T) {
	t.Parallel()

	payload := busFrame(1, 3, []byte{1, 2, 3})
	// Every quality bit but redacted, which the producer never sets: bit 3 retired, bits 8-15 reserved.
	body := busRawBody(12, 0xFFDF, payload)
	env := busEnvelope(t, 12)

	br, err := CheckMessage(env, body)
	require.NoError(t, err)
	assert.Equal(t, tracepack.Record{
		Seq:         12,
		TSUTCNs:     1_700_000_000_000_000_123,
		MonoNs:      555,
		MonoPresent: true,
		Epoch:       9,
		Kind:        tracepack.KindData,
		Dir:         tracepack.DirEquipmentToHost,
		Fidelity:    tracepack.FidelityWireExact,
		Quality:     tracepack.Quality(0xFFDF) & qualityMask,
		Payload:     payload,
	}, br.Record)
	assert.Equal(t, uint32(len(payload)), br.PayloadLen)
	assert.Equal(t, busCapture, br.CaptureID)

	enc, err := EncodeBody(&br)
	require.NoError(t, err)
	want := slicesClone(body)
	copy(want[32:36], []byte{0, 0, 0, 0})
	binary.LittleEndian.PutUint16(want[36:38], 0xFFDF&0x00B7)
	want[41], want[42], want[43] = 0, 0, 0x02
	assert.Equal(t, want, enc, "re-encoding gives the normalized bytes")

	again, err := CheckMessage(env, enc)
	require.NoError(t, err)
	assert.Equal(t, br, again)
	enc2, err := EncodeBody(&again)
	require.NoError(t, err)
	assert.Equal(t, enc, enc2, "a second encode gives the same bytes")

	// mono_ns is kept as decoded when mono_present is clear.
	cleared := slicesClone(body)
	cleared[43] = 0xFD
	br, err = CheckMessage(env, cleared)
	require.NoError(t, err)
	assert.False(t, br.Record.MonoPresent)
	assert.Equal(t, int64(555), br.Record.MonoNs)
	enc, err = EncodeBody(&br)
	require.NoError(t, err)
	assert.Equal(t, int64(555), int64(binary.LittleEndian.Uint64(enc[16:24])))
	assert.Equal(t, byte(0), enc[43])
}

func TestEncodeBodyRefuses(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		rec  tracepack.Record
	}{
		{"redacted", tracepack.Record{Seq: 1, Kind: tracepack.KindData, Quality: tracepack.QualityRedacted, Payload: busFrame(1, 1, nil)}},
		{"seq 2^63-1", tracepack.Record{Seq: math.MaxInt64, Kind: tracepack.KindTransportEvent}},
		{"seq 2^63", tracepack.Record{Seq: 1 << 63, Kind: tracepack.KindTransportEvent}},
	} {
		br := NewRecord(&tc.rec)
		b, err := EncodeBody(&br)
		require.ErrorIs(t, err, ErrMalformed, tc.name)
		assert.Nil(t, b, tc.name)

		br.ClaimCheck = true
		_, err = EncodeBody(&br)
		require.ErrorIs(t, err, ErrMalformed, tc.name)
	}

	// A well-framed body with redacted set is accepted, and its record refused by the encoder.
	payload := busFrame(1, 3, nil)
	br, err := CheckMessage(busEnvelope(t, 5), busRawBody(5, uint16(tracepack.QualityRedacted), payload))
	require.NoError(t, err)
	assert.True(t, br.Record.Quality.HasAny(tracepack.QualityRedacted))
	_, err = EncodeBody(&br)
	require.ErrorIs(t, err, ErrMalformed)
}

func TestDeriveRecordRefuses(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		rec  tracepack.Record
		cls  tracepack.Classifier
	}{
		{"unknown kind", tracepack.Record{Kind: tracepack.KindUnknown}, nil},
		{"kind out of range", tracepack.Record{Kind: tracepack.Kind(9)}, nil},
		{"trailing bytes out of range", tracepack.Record{Kind: tracepack.KindData, Payload: busFrame(1, 1, nil)},
			&fakeClassifier{status: tracepack.DecodeStatusOKWithTrailing, trailing: -1}},
	} {
		require.ErrorIs(t, DeriveRecord(&tc.rec, tc.cls), tracepack.ErrInvalidRecord, tc.name)
	}
}

// TestInlineBodyLen checks that an inline body's length is computed without overflow:
// a payload whose body would pass the int limit, as on a 32-bit platform near 2^31 bytes, is refused before any allocation.
func TestInlineBodyLen(t *testing.T) {
	t.Parallel()

	const busTestHeaderLen = 44 // the record header of the format specification §6

	n, err := inlineBodyLen(0)
	require.NoError(t, err)
	require.Equal(t, busTestHeaderLen, n)

	n, err = inlineBodyLen(math.MaxInt - busTestHeaderLen)
	require.NoError(t, err)
	require.Equal(t, math.MaxInt, n)

	_, err = inlineBodyLen(math.MaxInt - busTestHeaderLen + 1)
	require.ErrorIs(t, err, ErrMalformed)
}

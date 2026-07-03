package integration

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// maxFuzzBody caps each fuzz body so a single round-trip stays bounded over both transports. It
// sits far below the SECS-II item-size limit and small enough that the half-duplex SECS-I line
// reassembles the multi-block message quickly, keeping every iteration fast.
const maxFuzzBody = 4 << 10 // 4 KiB

// fuzzSendTimeout bounds each W-bit send so a fuzz input can never wedge on a missing reply. It sits
// far above the deterministic round-trip cost over the in-memory pipe, so it only fires on a genuine
// stall — which, together with the per-input connection close, is what guarantees no hang.
const fuzzSendTimeout = 2 * time.Second

// FuzzParityRoundTrip drives a normalized, always-valid W-bit primary through EACH real transport
// over the in-memory net.Pipe harness and asserts the scripted peer echoes the sent body verbatim.
//
// The raw fuzz input is normalized into a legal primary before the send: a primary must have an odd
// function to carry the wait-bit, and the stream field is 7 bits wide (its top bit is the wait-bit
// position on the wire). With those two invariants held, every iteration is a well-formed W-bit send
// that the peer echoes, and a binary item round-trips unchanged because the peer returns the raw
// SECS-II body verbatim.
//
// Each iteration opens both transports fresh, bounds every send with a short context deadline, and
// closes each connection before the next — so no input can leak a peer or pipe, and the target is
// safe under -race. The two transports must AGREE on the outcome: a normalized legal primary either
// round-trips with a verbatim echo on BOTH, or fails on BOTH. A one-sided error (one transport
// rejecting an input the other accepts), a corrupted echo, a panic, or a hang is a failure.
func FuzzParityRoundTrip(f *testing.F) {
	f.Add(byte(1), byte(1), []byte{0x01, 0x02})                     // the canonical short body
	f.Add(byte(1), byte(1), []byte{})                               // empty body: nil-vs-empty echo
	f.Add(byte(6), byte(11), bytes.Repeat([]byte{0xAB, 0xCD}, 200)) // 400 bytes: multi-block over SECS-I

	f.Fuzz(func(t *testing.T, stream, function byte, body []byte) {
		// Normalize the raw input into a legal W-bit primary.
		function |= 0x01 // force an odd (primary) function so the wait-bit is legal
		stream &= 0x7F   // a 7-bit stream field; the top bit is the wait-bit position on the wire

		// Keep every iteration bounded over both transports.
		if len(body) > maxFuzzBody {
			body = body[:maxFuzzBody]
		}

		// roundTrip sends the normalized primary through one transport and reports whether it echoed
		// the body back: (true, echo) on a successful round-trip, (false, nil) on a send error. The
		// send is bounded by a short context and the connection is always closed before returning, so
		// one input can never hold two live connections, leak a peer, or wedge.
		roundTrip := func(fac transportFactory) (bool, []byte) {
			conn, _ := fac.open(t)
			defer fac.close(t, conn)

			ctx, cancel := context.WithTimeout(context.Background(), fuzzSendTimeout)
			defer cancel()

			reply, err := conn.SendDataMessage(ctx, stream, function, true, secs2.NewBinaryItem(body))
			if err != nil {
				return false, nil
			}

			item, err := reply.Item()
			require.NoError(t, err)

			got, err := item.ToBinary()
			require.NoError(t, err)

			return true, got
		}

		// Drive the same normalized input through BOTH transports and require they AGREE: either both
		// round-trip with a verbatim echo, or both fail. A transport that silently rejects an input the
		// other accepts (or echoes different bytes) is a parity regression — accepting an arbitrary
		// one-sided error would make the target vacuous. bytes.Equal treats nil and empty as equal, so
		// an empty body round-trips cleanly.
		factories := parityFactories()

		wantOK, firstEcho := roundTrip(factories[0])
		if wantOK {
			require.True(t, bytes.Equal(body, firstEcho), "%s: echoed body must equal the sent body", factories[0].name)
		}

		for _, fac := range factories[1:] {
			ok, echo := roundTrip(fac)
			require.Equal(t, wantOK, ok, "%s vs %s: transports disagree on whether the normalized send succeeded", factories[0].name, fac.name)
			if ok {
				require.True(t, bytes.Equal(body, echo), "%s: echoed body must equal the sent body", fac.name)
			}
		}
	})
}

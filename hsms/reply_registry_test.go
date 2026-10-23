package hsms

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReplyRegistry_RouteHit(t *testing.T) {
	r := newReplyRegistry()
	key := [4]byte{1, 2, 3, 4}
	ch := r.registerControl(key)
	defer r.deregister(key)
	delivered, mismatched := r.route(key, replyResult{msg: nil, err: nil}, false)
	require.True(t, delivered)
	require.False(t, mismatched)
	select {
	case <-ch:
	default:
		t.Fatal("routed result not delivered to sender channel")
	}
}

func TestReplyRegistry_RouteMiss(t *testing.T) {
	r := newReplyRegistry()
	delivered, mismatched := r.route([4]byte{9, 9, 9, 9}, replyResult{}, false)
	require.False(t, delivered, "unknown key must miss")
	require.False(t, mismatched, "an absent key is never a field mismatch")
}

func TestReplyRegistry_LateReplyAfterDeregisterIsDropped(t *testing.T) {
	r := newReplyRegistry()
	key := [4]byte{5, 5, 5, 5}
	_ = r.registerControl(key)
	r.deregister(key) // sender gave up (ctx/T3)
	delivered, _ := r.route(key, replyResult{}, false)
	require.False(t, delivered, "late reply after deregister must drop, never panic")
	require.Equal(t, 0, r.len())
}

func TestReplyRegistry_RouteNeverBlocks(t *testing.T) {
	r := newReplyRegistry()
	key := [4]byte{7, 7, 7, 7}
	_ = r.registerControl(key) // cap-1 channel, nobody draining
	delivered, _ := r.route(key, replyResult{}, false)
	require.True(t, delivered) // fills buffer
	done := make(chan struct{})
	go func() { defer close(done); r.route(key, replyResult{}, false) }() // 2nd hits default, never blocks
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("route must never block (non-blocking send)")
	}
}

// replyDM builds a bare *DataMessage carrying only the header fields route cares about
// (stream, function, W-bit clear) — a white-box helper mirroring TestIsSecondaryReply's approach
// in connection_runtime_test.go, since route only ever inspects these three header bytes.
func replyDM(stream, function uint8) *DataMessage {
	dm := &DataMessage{}
	dm.header[2] = stream & 0x7F // W-bit (0x80) stays clear — a reply is always W-clear
	dm.header[3] = function

	return dm
}

// sentHeader builds the 10-byte header a W-bit data primary of stream and function is sent with under System Bytes sb,
// with session ID 0xFFFF: the header registerData records for the wait.
func sentHeader(sb [4]byte, stream, function uint8) [10]byte {
	return [10]byte{0xFF, 0xFF, 0x80 | stream&0x7F, function, 0, 0, sb[0], sb[1], sb[2], sb[3]}
}

// TestReplyRegistry_FieldMatching is the field-comparison matrix (Task 6 Design Reference): a
// compared candidate is an entry registered for a DATA primary (isData true) answered by a
// *DataMessage result.
// Stream must equal the registered primary's stream; function must equal
// primary+1 OR 0 (the SxF0 abort secondary, E5 §7.2/§10.4.1) — the F0 exception is function-only,
// so a wrong-stream F0 still mismatches. strict selects whether a mismatch is delivered anyway
// (default/observe) or missed (strict/enforce); mismatched is independent of strict — every
// compared mismatch is reported so the caller can count it exactly once.
func TestReplyRegistry_FieldMatching(t *testing.T) {
	// The registered primary is S1F1 (stream 1, function 1) throughout; primary+1 = F2.
	tests := []struct {
		name           string
		replyStream    uint8
		replyFunction  uint8
		wantMismatched bool
	}{
		{"correct reply S1F2", 1, 2, false},
		{"same-stream F0 accepted (E5 §7.2/§10.4.1 abort secondary)", 1, 0, false},
		{"wrong stream, correct function", 2, 2, true},
		{"correct stream, wrong function (neither primary+1 nor 0)", 1, 4, true},
		{"wrong stream AND wrong function — counts once, not twice", 2, 4, true},
		{"wrong-stream F0 — the F0 exception is function-only", 2, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name+"/default", func(t *testing.T) {
			r := newReplyRegistry()
			key := [4]byte{0, 0, 0, 1}
			ch := r.registerData(sentHeader(key, 1, 1))
			defer r.deregister(key)

			reply := replyDM(tt.replyStream, tt.replyFunction)
			delivered, mismatched := r.route(key, replyResult{msg: reply}, false)

			require.True(t, delivered, "default (observe) mode always delivers, mismatch or not")
			require.Equal(t, tt.wantMismatched, mismatched)
			select {
			case res := <-ch:
				require.Same(t, reply, res.msg)
			default:
				t.Fatal("default mode must deliver the candidate to the sender channel")
			}
		})

		t.Run(tt.name+"/strict", func(t *testing.T) {
			r := newReplyRegistry()
			key := [4]byte{0, 0, 0, 2}
			ch := r.registerData(sentHeader(key, 1, 1))
			defer r.deregister(key)

			reply := replyDM(tt.replyStream, tt.replyFunction)
			delivered, mismatched := r.route(key, replyResult{msg: reply}, true)

			require.Equal(t, tt.wantMismatched, mismatched)
			if tt.wantMismatched {
				require.False(t, delivered, "strict mode misses a mismatched candidate")
				select {
				case <-ch:
					t.Fatal("a strict-mode miss must not deliver to the sender channel")
				default:
				}
			} else {
				require.True(t, delivered, "strict mode still delivers a conforming reply")
				select {
				case res := <-ch:
					require.Same(t, reply, res.msg)
				default:
					t.Fatal("a conforming reply must be delivered even under strict mode")
				}
			}
		})
	}
}

// TestReplyRegistry_ControlExemption_RegistrationLeg proves the exemption's first leg: an entry
// registered with isData=false (a control primary — Select/Deselect/Linktest) is never compared,
// regardless of how wildly the routed result's fields would diverge if it WERE a data candidate.
// Control messages have no Stream()/Function() at all, so route must not even attempt the type
// assertion's field comparison path for these entries.
func TestReplyRegistry_ControlExemption_RegistrationLeg(t *testing.T) {
	r := newReplyRegistry()
	key := [4]byte{1, 1, 1, 1}
	ch := r.registerControl(key) // isData false: a control primary (e.g. Select.req)
	defer r.deregister(key)

	// Even a *DataMessage result with wildly different fields must deliver uncompared: a
	// control-registered entry is never a compared candidate.
	reply := replyDM(9, 9)
	delivered, mismatched := r.route(key, replyResult{msg: reply}, true) // strict: would matter if compared

	require.True(t, delivered, "a control-registered entry always delivers uncompared")
	require.False(t, mismatched, "a control-registered entry never counts as a field mismatch")
	select {
	case <-ch:
	default:
		t.Fatal("the routed result must reach the sender channel")
	}
}

// TestReplyRegistry_DataRegistration_ControlResultMustMiss guards against a control response (Select.rsp/Linktest.rsp) completing a DATA primary's transaction.
// A data-registered entry (isData true) can only be answered by a *DataMessage secondary (E37 §9.4.1);
// a *ControlMessage carries no Stream()/Function() to compare in the first place,
// so it must never be handed to the waiting sender as if it were that primary's reply — regardless of strict mode.
// The registration must survive the miss so a later genuine data reply can still complete it.
func TestReplyRegistry_DataRegistration_ControlResultMustMiss(t *testing.T) {
	linktestReq := NewLinktestReq([4]byte{0, 0, 0, 20})
	linktestRsp, err := NewLinktestRsp(linktestReq)
	require.NoError(t, err)

	selectReq := NewSelectReq(0xFFFF, [4]byte{0, 0, 0, 21})
	selectRsp, err := NewSelectRsp(selectReq, SelectStatusSuccess)
	require.NoError(t, err)

	deselectReq := NewDeselectReq(0xFFFF, [4]byte{0, 0, 0, 22})
	deselectRsp, err := NewDeselectRsp(deselectReq, DeselectStatusSuccess)
	require.NoError(t, err)

	tests := []struct {
		name    string
		control *ControlMessage
	}{
		{"Linktest.rsp", linktestRsp},
		{"Select.rsp", selectRsp},
		{"Deselect.rsp", deselectRsp},
	}

	for _, tt := range tests {
		for _, strict := range []bool{false, true} {
			t.Run(tt.name+"/"+map[bool]string{false: "default", true: "strict"}[strict], func(t *testing.T) {
				r := newReplyRegistry()
				key := [4]byte{0, 0, 0, 30}
				ch := r.registerData(sentHeader(key, 1, 1)) // isData true: registered for a data primary (S1F1)
				defer r.deregister(key)

				control := tt.control.WithSystemBytes(key)
				delivered, mismatched := r.route(key, replyResult{msg: control}, strict)

				require.False(t, delivered, "a control response must never complete a data primary's transaction")
				require.False(t, mismatched, "a control result is not a field-comparable candidate at all")
				select {
				case <-ch:
					t.Fatal("the control result must not reach the sender channel")
				default:
				}
				require.Equal(t, 1, r.len(), "the miss must not consume the registration")

				// A subsequent GENUINE data reply for the same key must still be delivered.
				reply := replyDM(1, 2)
				delivered, mismatched = r.route(key, replyResult{msg: reply}, strict)
				require.True(t, delivered, "the still-open registration must accept the real reply")
				require.False(t, mismatched)
				select {
				case res := <-ch:
					require.Same(t, reply, res.msg)
				default:
					t.Fatal("the genuine reply must reach the sender channel")
				}
			})
		}
	}
}

// TestReplyRegistry_ControlExemption_ResultLeg proves the exemption's second leg: a
// data-registered entry (isData true) answered by a field-less result (an inbound Reject.req,
// surfaced as replyResult{err: &RejectError{...}}, res.msg == nil) always delivers uncompared —
// the *DataMessage type assertion fails, so neither leg of the "if want.isData { if dm, ok :=
// result.msg.(*DataMessage); ok { ... } }" guard fires.
// A *RejectError misclassified as a
// mismatch would strand a strict-mode sender until T3 over a legitimate rejection (E37 §8.3.11).
func TestReplyRegistry_ControlExemption_ResultLeg(t *testing.T) {
	r := newReplyRegistry()
	key := [4]byte{2, 2, 2, 2}
	ch := r.registerData(sentHeader(key, 1, 1)) // isData true: registered for a data primary
	defer r.deregister(key)

	rejectResult := replyResult{err: &RejectError{Reason: 3}}
	delivered, mismatched := r.route(key, rejectResult, true) // strict: would matter if compared

	require.True(t, delivered, "a field-less RejectError result always delivers uncompared")
	require.False(t, mismatched, "a field-less RejectError result never counts as a field mismatch")
	select {
	case res := <-ch:
		var rejectErr *RejectError
		require.ErrorAs(t, res.err, &rejectErr)
	default:
		t.Fatal("the RejectError result must reach the sender channel")
	}
}

// s9Result is the result an S9 error message correlated to the wait for header completes it with.
func s9Result(header [10]byte) replyResult {
	return replyResult{err: &S9Error{Function: 5, MHEAD: header}}
}

// requireOneResult receives the one result buffered on ch and fails if there is none.
func requireOneResult(t *testing.T, ch chan replyResult) replyResult {
	t.Helper()

	select {
	case res := <-ch:
		return res
	default:
		t.Fatal("no result reached the sender channel")

		return replyResult{}
	}
}

// requireNoResult fails if a result is buffered on ch.
func requireNoResult(t *testing.T, ch chan replyResult) {
	t.Helper()

	select {
	case res := <-ch:
		t.Fatalf("unexpected result on the sender channel: %+v", res)
	default:
	}
}

// TestReplyRegistry_StrictMismatchThenConformingReply proves a strict-mode mismatch does not claim the wait:
// it misses before the claim,
// so the conforming reply that follows still completes the transaction.
func TestReplyRegistry_StrictMismatchThenConformingReply(t *testing.T) {
	r := newReplyRegistry()
	key := [4]byte{0, 0, 0, 40}
	ch := r.registerData(sentHeader(key, 1, 1))
	defer r.deregister(key)

	delivered, mismatched := r.route(key, replyResult{msg: replyDM(2, 2)}, true)
	require.False(t, delivered, "strict mode misses a mismatched candidate")
	require.True(t, mismatched)
	requireNoResult(t, ch)

	reply := replyDM(1, 2)
	delivered, mismatched = r.route(key, replyResult{msg: reply}, true)
	require.True(t, delivered, "the strict miss must leave the wait open for the conforming reply")
	require.False(t, mismatched)
	require.Same(t, reply, requireOneResult(t, ch).msg)
}

// TestReplyRegistry_CandidateAfterClaim is the matrix for a candidate that arrives after the wait already has its result,
// won either by a reply or by an S9 error message.
//
// Only the winner ever reaches the sender channel.
// After a reply win the candidate is a duplicate: reported delivered and dropped, as before the claim existed.
// After an S9 win it misses, so a genuine reply falls through to the data handlers rather than vanishing.
// A strict-mode mismatch misses before the claim is even consulted.
// mismatched is reported in every case, so the caller still counts the mismatch.
//
// Each case runs with the winner still buffered and with the sender having drained it but not yet deregistered:
// the outcome must not depend on the buffer, because the claim alone records that the wait is over.
//
// TEETH: report every lost claim as delivered (the shape before the claim existed)
// and every S9 row fails except the strict mismatched candidates, which miss before the claim;
// drop the claim entirely and the drained rows deliver the candidate to the sender.
func TestReplyRegistry_CandidateAfterClaim(t *testing.T) {
	candidates := []struct {
		name           string
		res            replyResult
		wantMismatched bool
	}{
		{"conforming reply", replyResult{msg: replyDM(1, 2)}, false},
		{"mismatched reply", replyResult{msg: replyDM(2, 4)}, true},
		{"peer Reject.req", replyResult{err: &RejectError{Reason: 3}}, false},
	}

	for _, winner := range []string{"reply", "S9"} {
		for _, strict := range []bool{false, true} {
			for _, drained := range []bool{false, true} {
				for _, cand := range candidates {
					name := fmt.Sprintf("%s won/strict=%t/drained=%t/%s", winner, strict, drained, cand.name)
					t.Run(name, func(t *testing.T) {
						r := newReplyRegistry()
						key := [4]byte{0, 0, 0, 41}
						ch := r.registerData(sentHeader(key, 1, 1))
						defer r.deregister(key)

						var won replyResult
						if winner == "reply" {
							won = replyResult{msg: replyDM(1, 2)}
							delivered, mismatched := r.route(key, won, strict)
							require.True(t, delivered)
							require.False(t, mismatched)
						} else {
							won = s9Result(sentHeader(key, 1, 1))
							require.True(t, r.failS9(sentHeader(key, 1, 1), mheadExact, won))
						}

						if drained {
							requireOneResult(t, ch)
						}

						delivered, mismatched := r.route(key, cand.res, strict)

						wantDelivered := winner == "reply"
						if cand.wantMismatched && strict {
							wantDelivered = false
						}
						require.Equal(t, wantDelivered, delivered)
						require.Equal(t, cand.wantMismatched, mismatched, "the claim must never clear mismatched")

						if !drained {
							got := requireOneResult(t, ch)
							require.Equal(t, won, got, "only the winner reaches the sender channel")
						}
						requireNoResult(t, ch)
						require.Equal(t, 1, r.len(), "cleanup stays with the sender's deregister")
					})
				}
			}
		}
	}
}

// TestReplyRegistry_ConcurrentCandidatesSendOnce races many conforming replies and one S9 claim at a single open wait.
// Exactly one of them must reach the sender channel,
// and every reply must agree with the claim's winner:
// all delivered when a reply won, all missed when the S9 won.
func TestReplyRegistry_ConcurrentCandidatesSendOnce(t *testing.T) {
	const replies = 16

	r := newReplyRegistry()
	key := [4]byte{0, 0, 0, 42}
	ch := r.registerData(sentHeader(key, 1, 1))
	defer r.deregister(key)

	start := make(chan struct{})
	delivered := make([]bool, replies)
	var s9Won bool

	var wg sync.WaitGroup
	for i := range replies {
		wg.Go(func() {
			<-start
			delivered[i], _ = r.route(key, replyResult{msg: replyDM(1, 2)}, false)
		})
	}
	wg.Go(func() {
		<-start
		s9Won = r.failS9(sentHeader(key, 1, 1), mheadExact, s9Result(sentHeader(key, 1, 1)))
	})

	close(start)
	wg.Wait()

	got := requireOneResult(t, ch)
	requireNoResult(t, ch)

	if s9Won {
		require.Error(t, got.err, "the S9 won, so its result is the one sent")
	} else {
		require.NotNil(t, got.msg, "a reply won, so it is the one sent")
	}
	for i, d := range delivered {
		require.Equal(t, !s9Won, d, "reply %d disagrees with the claim's winner", i)
	}
}

// TestReplyRegistry_S9AfterReply sends an S9 for the wait after a reply already completed it,
// with the reply still buffered and with the sender having drained it but not yet deregistered.
// The S9 must not complete the wait a second time:
// failS9 reports false, sends nothing, and leaves the reply's claim in place,
// so the S9 is left to the data handlers.
//
// TEETH: drop failS9's claim (send whenever the MHEAD matches) and both rows fail.
func TestReplyRegistry_S9AfterReply(t *testing.T) {
	for _, drained := range []bool{false, true} {
		t.Run(fmt.Sprintf("drained=%t", drained), func(t *testing.T) {
			r := newReplyRegistry()
			key := [4]byte{0, 0, 0, 43}
			header := sentHeader(key, 1, 1)
			ch := r.registerData(header)
			defer r.deregister(key)

			reply := replyResult{msg: replyDM(1, 2)}
			delivered, _ := r.route(key, reply, false)
			require.True(t, delivered)

			if drained {
				require.Equal(t, reply, requireOneResult(t, ch))
			}

			require.False(t, r.failS9(header, mheadExact, s9Result(header)), "the reply already completed the wait")

			if !drained {
				require.Equal(t, reply, requireOneResult(t, ch), "only the reply reaches the sender channel")
			}
			requireNoResult(t, ch)

			w, ok := r.m.Load(key)
			require.True(t, ok, "cleanup stays with the sender's deregister")
			require.Equal(t, replyClaimedByReply, w.state.Load())
		})
	}
}

// TestReplyRegistry_FailS9Match covers what failS9 matches:
// only a data waiter whose sent header the MHEAD identifies under the policy, keyed on the MHEAD's own System Bytes.
func TestReplyRegistry_FailS9Match(t *testing.T) {
	key := [4]byte{0, 0, 0, 44}
	header := sentHeader(key, 6, 11)

	withByte := func(i int, b byte) [10]byte {
		m := header
		m[i] = b

		return m
	}

	tests := []struct {
		name  string
		mhead [10]byte
		want  bool
	}{
		{"exact header", header, true},
		{"other System Bytes", withByte(9, 45), false},
		{"other session ID", withByte(1, 0xFE), false},
		{"other stream", withByte(2, 0x80|2), false},
		{"W-bit clear", withByte(2, 1), false},
		{"other function", withByte(3, 5), false},
		{"PType or SType set", withByte(5, 1), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newReplyRegistry()
			ch := r.registerData(header)
			defer r.deregister(key)

			res := s9Result(tt.mhead)
			require.Equal(t, tt.want, r.failS9(tt.mhead, mheadExact, res))

			if tt.want {
				require.Equal(t, res, requireOneResult(t, ch))
			}
			requireNoResult(t, ch)
		})
	}

	t.Run("control waiter with the same System Bytes", func(t *testing.T) {
		r := newReplyRegistry()
		ch := r.registerControl(key)
		defer r.deregister(key)

		require.False(t, r.failS9(header, mheadExact, s9Result(header)), "an S9 never ends a control transaction")
		requireNoResult(t, ch)

		delivered, _ := r.route(key, replyResult{}, false)
		require.True(t, delivered, "the control wait must still be open")
	})

	// Zero System Bytes make a control waiter's key equal its all-zero header's,
	// so only the waiter's kind keeps an all-zero MHEAD from matching it.
	t.Run("control waiter with an MHEAD equal to its zero header", func(t *testing.T) {
		r := newReplyRegistry()
		zero := [4]byte{}
		ch := r.registerControl(zero)
		defer r.deregister(zero)

		require.False(t, r.failS9([10]byte{}, mheadExact, s9Result([10]byte{})), "an S9 never ends a control transaction")
		requireNoResult(t, ch)
	})

	t.Run("no waiter", func(t *testing.T) {
		r := newReplyRegistry()
		require.False(t, r.failS9(header, mheadExact, s9Result(header)))
	})
}

// TestMHEADPolicy_Matches is the comparison table for both policies.
// The exact policy compares all 10 bytes.
// The block policy ignores the R-bit (the top bit of byte 0) and bytes 4 and 5 (E-bit and block number),
// and compares the device ID, W-bit with stream, function, and System Bytes.
func TestMHEADPolicy_Matches(t *testing.T) {
	header := [10]byte{0x01, 0x23, 0x81, 0x03, 0, 0, 0, 0, 0, 7}

	with := func(f func(m *[10]byte)) [10]byte {
		m := header
		f(&m)

		return m
	}

	tests := []struct {
		name      string
		mhead     [10]byte
		wantExact bool
		wantBlock bool
	}{
		{"identical", header, true, true},
		{"R-bit set", with(func(m *[10]byte) { m[0] |= 0x80 }), false, true},
		{"E-bit and block number", with(func(m *[10]byte) { m[4], m[5] = 0x80, 0x01 }), false, true},
		{"R-bit, E-bit, and block number", with(func(m *[10]byte) { m[0] |= 0x80; m[4], m[5] = 0x80, 0x02 }), false, true},
		{"other device ID high byte", with(func(m *[10]byte) { m[0] = 0x02 }), false, false},
		{"other device ID low byte", with(func(m *[10]byte) { m[1] = 0x24 }), false, false},
		{"W-bit clear", with(func(m *[10]byte) { m[2] = 0x01 }), false, false},
		{"other stream", with(func(m *[10]byte) { m[2] = 0x82 }), false, false},
		{"other function", with(func(m *[10]byte) { m[3] = 0x05 }), false, false},
		{"other System Bytes", with(func(m *[10]byte) { m[6] = 1 }), false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.wantExact, mheadExact.matches(tt.mhead, header), "exact")
			require.Equal(t, tt.wantBlock, mheadBlock.matches(tt.mhead, header), "block")
		})
	}
}

package hsms

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReplyRegistry_RouteHit(t *testing.T) {
	r := newReplyRegistry()
	key := [4]byte{1, 2, 3, 4}
	ch := r.register(key, 0, 0, false)
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
	_ = r.register(key, 0, 0, false)
	r.deregister(key) // sender gave up (ctx/T3)
	delivered, _ := r.route(key, replyResult{}, false)
	require.False(t, delivered, "late reply after deregister must drop, never panic")
	require.Equal(t, 0, r.len())
}

func TestReplyRegistry_RouteNeverBlocks(t *testing.T) {
	r := newReplyRegistry()
	key := [4]byte{7, 7, 7, 7}
	_ = r.register(key, 0, 0, false) // cap-1 channel, nobody draining
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
			ch := r.register(key, 1, 1, true)
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
			ch := r.register(key, 1, 1, true)
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
	ch := r.register(key, 0, 0, false) // isData false: a control primary (e.g. Select.req)
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
	ch := r.register(key, 1, 1, true) // isData true: registered for a data primary
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

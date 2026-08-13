package hsms

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// errPlain is an opaque error the classifiers must not recognize.
// It implements neither marker interface and matches none of the built-in sentinels.
var errPlain = errors.New("classify_test: opaque unrecognized error")

// markedTransientError is a caller-defined error implementing TransientError directly,
// proving the extensible path (rule 1) works for a type this package has never heard of.
type markedTransientError struct{ transient bool }

func (markedTransientError) Error() string     { return "classify_test: marked transient" }
func (m markedTransientError) Transient() bool { return m.transient }

var _ TransientError = markedTransientError{}

// realNetTimeout returns a genuine *net.OpError with Timeout() true, produced by a real TCP read against an already-expired deadline.
// It is not a hand-rolled stub.
// The connection pair is closed via t.Cleanup.
func realNetTimeout(t *testing.T) error {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	conn, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(-time.Second)))

	buf := make([]byte, 1)
	_, readErr := conn.Read(buf)
	require.Error(t, readErr)

	var ne net.Error
	require.ErrorAs(t, readErr, &ne, "test setup must produce a genuine net.Error")
	require.True(t, ne.Timeout(), "test setup must produce a genuine timeout")

	return readErr
}

// TestIsTransient_TableDriven covers every reachable hsms sentinel's transient classification,
// plain and wrapped/joined, plus the marker-interface path and the unknown-error default.
func TestIsTransient_TableDriven(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"ErrNotSelectedState", ErrNotSelectedState, true},
		{"ErrNotSelectedState wrapped", fmt.Errorf("send: %w", ErrNotSelectedState), true},
		{"ErrNotSelectedState joined", errors.Join(errPlain, ErrNotSelectedState), true},
		{"errors.Join of a permanent sentinel and a transient one: any-branch-wins, not vetoed", errors.Join(ErrMessageTooLarge, ErrNotSelectedState), true},
		{"ErrConnClosed", ErrConnClosed, true},
		{"ErrConnClosed wrapped", fmt.Errorf("write: %w", ErrConnClosed), true},
		{"ErrT3Timeout", ErrT3Timeout, true},
		{"ErrT3Timeout wrapped", fmt.Errorf("reply: %w", ErrT3Timeout), true},
		{"ErrT6Timeout", ErrT6Timeout, true},
		{"ErrCloseTimeout is timeout, not transient", ErrCloseTimeout, false},
		{"ErrMessageTooLarge is not transient", ErrMessageTooLarge, false},
		{"ErrAlreadyOpen is not transient", ErrAlreadyOpen, false},
		{"ErrNotOpen is not transient", ErrNotOpen, false},
		{"ErrNilMessage is not transient", ErrNilMessage, false},
		{"ErrEvenFunctionPrimary is not transient", ErrEvenFunctionPrimary, false},
		{"ErrAsyncReplyExpected is not transient", ErrAsyncReplyExpected, false},
		{"RejectError falls to the default (unclassified)", &RejectError{Reason: RejectNotSelected}, false},
		{"marker interface true wins over no sentinel match", markedTransientError{transient: true}, true},
		{"marker interface false is honored verbatim", markedTransientError{transient: false}, false},
		{"context.Canceled is not transient (caller gave up)", context.Canceled, false},
		{"context.DeadlineExceeded alone is not transient", context.DeadlineExceeded, false},
		{"plain unknown error", errPlain, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, IsTransient(tt.err))
		})
	}
}

// TestIsTimeout_TableDriven covers every reachable hsms sentinel's timeout classification, plain
// and wrapped/joined, plus the raw stdlib fallback (context.DeadlineExceeded, a real net.Error) and
// the unknown-error default.
func TestIsTimeout_TableDriven(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"ErrT3Timeout", ErrT3Timeout, true},
		{"ErrT3Timeout wrapped", fmt.Errorf("reply: %w", ErrT3Timeout), true},
		{"ErrT3Timeout joined", errors.Join(errPlain, ErrT3Timeout), true},
		{"errors.Join of a non-timeout sentinel and a timeout one: any-branch-wins", errors.Join(ErrMessageTooLarge, ErrT3Timeout), true},
		{"ErrT6Timeout", ErrT6Timeout, true},
		{"ErrCloseTimeout", ErrCloseTimeout, true},
		{"ErrCloseTimeout wrapped", fmt.Errorf("close: %w", ErrCloseTimeout), true},
		{"ErrNotSelectedState is not a timeout", ErrNotSelectedState, false},
		{"ErrConnClosed is not a timeout", ErrConnClosed, false},
		{"ErrMessageTooLarge is not a timeout", ErrMessageTooLarge, false},
		{"context.DeadlineExceeded (raw stdlib fallback)", context.DeadlineExceeded, true},
		{"context.DeadlineExceeded wrapped", fmt.Errorf("ctx: %w", context.DeadlineExceeded), true},
		{"context.Canceled is not a timeout", context.Canceled, false},
		{"plain unknown error", errPlain, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, IsTimeout(tt.err))
		})
	}

	t.Run("real net.Error timeout (raw stdlib fallback)", func(t *testing.T) {
		require.True(t, IsTimeout(realNetTimeout(t)))
	})

	t.Run("real net.Error timeout wrapped", func(t *testing.T) {
		require.True(t, IsTimeout(fmt.Errorf("read: %w", realNetTimeout(t))))
	})
}

// TestClassify_JoinPolicy_AnyMatchWins pins the errors.Join policy documented on both functions.
// A matching branch wins even when another branch in the same tree is explicitly permanent (for IsTransient)
// or explicitly not a timeout (for IsTimeout) —
// a permanent or non-timeout branch does not veto a match found elsewhere in the tree.
func TestClassify_JoinPolicy_AnyMatchWins(t *testing.T) {
	// Sanity check first: the permanent branch alone classifies false/false.
	// The true results below come from the OTHER branch, not from ErrMessageTooLarge itself.
	require.False(t, IsTransient(ErrMessageTooLarge))
	require.False(t, IsTimeout(ErrMessageTooLarge))

	transientJoin := errors.Join(ErrMessageTooLarge, ErrNotSelectedState)
	require.True(t, IsTransient(transientJoin),
		"a permanent branch (ErrMessageTooLarge) must not veto a transient one (ErrNotSelectedState) elsewhere in the Join tree")

	timeoutJoin := errors.Join(ErrMessageTooLarge, ErrT3Timeout)
	require.True(t, IsTimeout(timeoutJoin),
		"a non-timeout branch (ErrMessageTooLarge) must not veto a timeout one (ErrT3Timeout) elsewhere in the Join tree")
}

// TestIsTransient_Integration_NotSelectedSend proves the classification end-to-end through the
// real SECS2Endpoint surface, not just against the bare sentinel:
// a primary send into a NotSelected connection is refused by the B1 gate with ErrNotSelectedState,
// and IsTransient reports true for the error SendDataMessage actually returns.
func TestIsTransient_Integration_NotSelectedSend(t *testing.T) {
	c, _ := newTestSendConn(t, NotSelectedState)

	_, err := c.SendDataMessage(t.Context(), 1, 1, false, secs2.NewASCIIItem("PING"))
	require.ErrorIs(t, err, ErrNotSelectedState)
	require.True(t, IsTransient(err), "a NotSelected send must classify as transient end-to-end")
	require.False(t, IsTimeout(err))
}

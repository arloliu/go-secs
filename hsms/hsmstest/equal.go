package hsmstest

import (
	"testing"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// equalOpts accumulates the fields RequireDataMessageEqual should skip.
type equalOpts struct {
	ignoreSessionID  bool
	ignoreSystemByte bool
	bodyOnly         bool
}

// EqualOption tunes which fields RequireDataMessageEqual compares.
type EqualOption func(*equalOpts)

// IgnoreSystemBytes excludes System Bytes (header bytes 6-9) from the comparison.
func IgnoreSystemBytes() EqualOption {
	return func(o *equalOpts) { o.ignoreSystemByte = true }
}

// IgnoreSessionID excludes the session ID (header bytes 0-1) from the comparison.
func IgnoreSessionID() EqualOption {
	return func(o *equalOpts) { o.ignoreSessionID = true }
}

// BodyOnly compares only the decoded SECS-II item, ignoring the whole header (stream, function,
// wait bit, session ID, and System Bytes).
func BodyOnly() EqualOption {
	return func(o *equalOpts) { o.bodyOnly = true }
}

// RequireDataMessageEqual fails t (via testify require) unless want and got are equal under opts.
//
// With no options it is exact equality (header + body), equivalent to want.Equal(got). It forces
// the body decode on both sides and compares items with secs2.Equal, so it is immune to the
// lazy-decode sync.Once ordering hazard; a decode error on either side fails the assertion with a
// clear message rather than panicking.
func RequireDataMessageEqual(t *testing.T, want, got *hsms.DataMessage, opts ...EqualOption) {
	t.Helper()

	var o equalOpts
	for _, opt := range opts {
		opt(&o)
	}

	if len(opts) == 0 {
		require.True(t, want.Equal(got), "DataMessage mismatch:\nwant: %+v\ngot:  %+v", want, got)

		return
	}

	if !o.bodyOnly {
		require.Equal(t, want.Stream(), got.Stream(), "Stream mismatch")
		require.Equal(t, want.Function(), got.Function(), "Function mismatch")
		require.Equal(t, want.WaitBit(), got.WaitBit(), "WaitBit mismatch")

		if !o.ignoreSessionID {
			require.Equal(t, want.SessionID(), got.SessionID(), "SessionID mismatch")
		}
		if !o.ignoreSystemByte {
			require.Equal(t, want.SystemBytes(), got.SystemBytes(), "SystemBytes mismatch")
		}
	}

	wantItem, wantErr := want.Item()
	require.NoError(t, wantErr, "want message body decode error")
	gotItem, gotErr := got.Item()
	require.NoError(t, gotErr, "got message body decode error")

	require.True(t, secs2.Equal(wantItem, gotItem), "item mismatch:\nwant: %s\ngot:  %s", wantItem.ToSML(), gotItem.ToSML())
}

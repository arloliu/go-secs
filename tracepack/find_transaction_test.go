package tracepack

import (
	"context"
	"errors"
	"math"
	"math/big"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFindTransactionInvalid calls FindTransaction with arguments it must reject before observing anything,
// and with the edges it must accept:
// a rejected call returns the zero TxResult and an error wrapping ErrInvalidQuery, and never calls the source.
func TestFindTransactionInvalid(t *testing.T) {
	t.Parallel()

	key := TxKey{Capture: captureLow, Seq: 1, Hour: memTestHour}
	at := func(hour int64) TxKey {
		k := key
		k.Hour = hour

		return k
	}
	tests := []struct {
		name    string
		nilSrc  bool
		key     TxKey
		opts    TxOptions
		invalid string
	}{
		{name: "nil source", nilSrc: true, key: key, invalid: "src is nil"},
		{name: "zero capture", key: TxKey{Seq: 1, Hour: memTestHour}, invalid: "capture_id is zero"},
		{name: "hour below MinTxHour", key: at(MinTxHour - 1), invalid: "outside"},
		{name: "hour above MaxTxHour", key: at(MaxTxHour + 1), invalid: "outside"},
		{name: "hour at math.MinInt64", key: at(math.MinInt64), invalid: "outside"},
		{name: "default scopes past MaxTxHour", key: at(MaxTxHour), invalid: "2 scopes from hour"},
		{name: "scopes past MaxTxHour", key: at(MaxTxHour - 1), opts: TxOptions{MaxScopes: 3}, invalid: "3 scopes from hour"},
		{name: "scopes far past MaxTxHour", key: at(0), opts: TxOptions{MaxScopes: math.MaxInt}, invalid: "scopes from hour"},
		{name: "hour 0", key: at(0)},
		{name: "negative hour", key: at(-1)},
		{name: "MinTxHour", key: at(MinTxHour), opts: TxOptions{MaxScopes: 1}},
		{name: "MaxTxHour, one scope", key: at(MaxTxHour), opts: TxOptions{MaxScopes: 1}},
		{name: "last hour MaxTxHour", key: at(MaxTxHour - 2), opts: TxOptions{MaxScopes: 3}},
		{name: "negative limits", key: key, opts: TxOptions{MaxScopes: -1, MaxHeldBytes: -1, MaxConflicts: -1, MaxStateBytes: -1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newMemSource()
			var src PackSource = s
			if tt.nilSrc {
				src = nil
			}
			res, err := FindTransaction(t.Context(), src, tt.key, tt.opts)
			assert.Equal(t, TxResult{}, res)
			if tt.invalid != "" {
				require.ErrorIs(t, err, ErrInvalidQuery)
				require.ErrorContains(t, err, tt.invalid)
			} else {
				require.ErrorIs(t, err, errTxNotImplemented)
			}
			observes, _, _ := s.counts()
			assert.Zero(t, observes)
		})
	}
}

// TestFindTransactionCancelled calls FindTransaction with ctx done: valid arguments return ctx's error, wrapped;
// invalid ones are rejected first.
func TestFindTransactionCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := FindTransaction(ctx, newMemSource(), TxKey{Capture: captureLow, Hour: memTestHour}, TxOptions{})
	require.ErrorIs(t, err, context.Canceled)
	_, err = FindTransaction(ctx, nil, TxKey{Capture: captureLow, Hour: memTestHour}, TxOptions{})
	require.ErrorIs(t, err, ErrInvalidQuery)
}

// TestCheckFindTransactionDefaults applies the defaults of TxOptions: every zero or negative field takes its default,
// every positive field is kept.
func TestCheckFindTransactionDefaults(t *testing.T) {
	t.Parallel()

	key := TxKey{Capture: captureLow, Hour: memTestHour}
	got, err := checkFindTransaction(newMemSource(), key, TxOptions{MaxHeldBytes: -1, MaxConflicts: -1})
	require.NoError(t, err)
	assert.Equal(t, TxOptions{
		MaxScopes: DefaultTxMaxScopes, MaxHeldBytes: DefaultMaxHeldBytes,
		MaxConflicts: DefaultMaxConflicts, MaxStateBytes: DefaultTxMaxStateBytes,
	}, got)

	set := TxOptions{MaxScopes: 5, MaxHeldBytes: 6, MaxConflicts: 7, MaxStateBytes: 8}
	got, err = checkFindTransaction(newMemSource(), key, set)
	require.NoError(t, err)
	assert.Equal(t, set, got)
}

// TestTxHourRange checks MinTxHour and MaxTxHour against exact arithmetic:
// the start and end of every hour in the range fit an int64 in nanoseconds, and those of the hours just outside do not.
func TestTxHourRange(t *testing.T) {
	t.Parallel()

	fits := func(h int64) bool {
		v := new(big.Int).Mul(big.NewInt(h), big.NewInt(hourNs))
		return v.IsInt64()
	}
	assert.True(t, fits(MinTxHour))
	assert.True(t, fits(MinTxHour+1))
	assert.False(t, fits(MinTxHour-1))
	assert.True(t, fits(MaxTxHour))
	assert.True(t, fits(MaxTxHour+1))
	assert.False(t, fits(MaxTxHour+2), "the end of MaxTxHour + 1 does not fit")
	assert.Equal(t, int64(-2562047), MinTxHour)
	assert.Equal(t, int64(2562046), MaxTxHour)
}

// TestTxClassNames formats TxClass values: zero, each bit, two bits, a known bit with an undefined one, undefined bits alone.
func TestTxClassNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		c     TxClass
		names []string
		str   string
	}{
		{c: 0, names: nil, str: "none"},
		{c: TxPrimary, names: []string{"primary"}, str: "primary"},
		{c: TxCandidate, names: []string{"candidate"}, str: "candidate"},
		{c: TxPossibleReply, names: []string{"possible-reply"}, str: "possible-reply"},
		{c: TxSameKeyPrimary, names: []string{"same-key-primary"}, str: "same-key-primary"},
		{c: TxPossiblePrimary, names: []string{"possible-primary"}, str: "possible-primary"},
		{c: TxClosing, names: []string{"closing"}, str: "closing"},
		{c: TxOutcomeRecord, names: []string{"outcome"}, str: "outcome"},
		{
			c:     TxPossibleReply | TxPossiblePrimary,
			names: []string{"possible-reply", "possible-primary"}, str: "possible-reply|possible-primary",
		},
		{c: TxCandidate | 0x0100, names: []string{"candidate", "unknown(0x100)"}, str: "candidate|unknown(0x100)"},
		{c: 0x8080, names: []string{"unknown(0x8080)"}, str: "unknown(0x8080)"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.names, tt.c.Names(), "%#x", uint16(tt.c))
		assert.Equal(t, tt.str, tt.c.String(), "%#x", uint16(tt.c))
	}
	assert.True(t, (TxPossibleReply | TxPossiblePrimary).Has(TxPossiblePrimary))
	assert.True(t, TxClosing.Has(TxClosing|TxPrimary))
	assert.False(t, TxClosing.Has(TxPrimary))
	assert.False(t, TxClass(0).Has(TxPrimary))
}

// TestTxEnumStrings formats every TxOutcome, TxGapReason and EndState value, and one undefined value of each.
func TestTxEnumStrings(t *testing.T) {
	t.Parallel()

	outcomes := map[TxOutcome]string{
		0: "none", TxMatched: "matched", TxAmbiguous: "ambiguous", TxUnmatched: "unmatched", TxIncomplete: "incomplete",
		5: "unknown(5)",
	}
	for o, want := range outcomes {
		assert.Equal(t, want, o.String())
	}

	reasons := map[TxGapReason]string{
		0: "unknown(0)", TxGapNoKey: "no-key", TxGapConflict: "conflict", TxGapIndex: "index", TxGapCold: "cold",
		TxGapRead: "read", TxGapCoverage: "coverage", TxGapUnevaluated: "unevaluated", TxGapEvidence: "evidence",
		TxGapSeqGap: "seq-gap", TxGapOpenWindow: "open-window", TxGapBarrier: "barrier",
		TxGapCaptureBoundary: "capture-boundary", TxGapOrderingUncertain: "ordering-uncertain",
		TxGapCorrelation: "correlation", TxGapUnavailable: "unavailable", TxGapContradiction: "contradiction",
		17: "unknown(17)",
	}
	for r, want := range reasons {
		assert.Equal(t, want, r.String())
	}

	ends := map[EndState]string{EndOpen: "open", EndStopped: "stopped", EndStoppedUnclean: "stopped-unclean", 3: "unknown(3)"}
	for e, want := range ends {
		assert.Equal(t, want, e.String())
	}
}

// TestTxPackError formats a TxPackError with its hour and pack_id and unwraps it to its Err.
func TestTxPackError(t *testing.T) {
	t.Parallel()

	inner := errors.New("footer CRC mismatch")
	e := TxPackError{Hour: 12, Pack: seg0, Err: inner}
	assert.Equal(t, "tracepack: hour 12 pack "+seg0.String()+": footer CRC mismatch", e.Error())
	require.ErrorIs(t, e, inner)
}

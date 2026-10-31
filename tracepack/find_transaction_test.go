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

// TestFindTransactionArguments calls FindTransaction with arguments it must reject before reading anything,
// and with the edges of the valid ones:
// a rejected call returns the zero TxResult and an error wrapping ErrInvalidQuery, and never calls the source;
// an accepted call asks the empty source for the readers of its hour alone, reads that hour,
// and finds the primary missing where nothing explains it: ErrNotPrimary.
func TestFindTransactionArguments(t *testing.T) {
	t.Parallel()

	key := TxKey{CaptureID: captureLow, Seq: 1, Hour: memTestHour}
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
		{name: "seq above the format's largest", key: TxKey{CaptureID: captureLow, Seq: 1 << 63, Hour: memTestHour}, invalid: "seq"},
		{name: "seq at math.MaxUint64", key: TxKey{CaptureID: captureLow, Seq: math.MaxUint64, Hour: memTestHour}, invalid: "seq"},
		{name: "hour below MinTxHour", key: at(MinTxHour - 1), invalid: "outside"},
		{name: "hour above MaxTxHour", key: at(MaxTxHour + 1), invalid: "outside"},
		{name: "hour at math.MinInt64", key: at(math.MinInt64), invalid: "outside"},
		{name: "default scopes past MaxTxHour", key: at(MaxTxHour), invalid: "2 scopes from hour"},
		{name: "scopes past MaxTxHour", key: at(MaxTxHour - 1), opts: TxOptions{MaxHours: 3}, invalid: "3 scopes from hour"},
		{name: "scopes far past MaxTxHour", key: at(0), opts: TxOptions{MaxHours: math.MaxInt}, invalid: "scopes from hour"},
		{name: "the format's largest seq", key: TxKey{CaptureID: captureLow, Seq: 1<<63 - 1, Hour: memTestHour}},
		{name: "hour 0", key: at(0)},
		{name: "negative hour", key: at(-1)},
		{name: "MinTxHour", key: at(MinTxHour), opts: TxOptions{MaxHours: 1}},
		{name: "MaxTxHour, one scope", key: at(MaxTxHour), opts: TxOptions{MaxHours: 1}},
		{name: "last hour MaxTxHour", key: at(MaxTxHour - 2), opts: TxOptions{MaxHours: 3}},
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
			if tt.invalid != "" {
				require.ErrorIs(t, err, ErrInvalidQuery)
				require.ErrorContains(t, err, tt.invalid)
				assert.Equal(t, TxResult{}, res)
				assert.Empty(t, s.counts())

				return
			}

			require.ErrorIs(t, err, ErrNotPrimary)
			assert.Zero(t, res.Outcome)
			assert.Empty(t, res.Incomplete)
			assert.Equal(t, []TxHour{{Hour: tt.key.Hour}}, res.Searched)
			assert.Equal(t, map[int64]int{tt.key.Hour: 1}, s.counts())
		})
	}
}

// TestFindTransactionCancelled calls FindTransaction with ctx done: valid arguments return ctx's error, wrapped;
// invalid ones are rejected first.
func TestFindTransactionCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := FindTransaction(ctx, newMemSource(), TxKey{CaptureID: captureLow, Hour: memTestHour}, TxOptions{})
	require.ErrorIs(t, err, context.Canceled)
	_, err = FindTransaction(ctx, nil, TxKey{CaptureID: captureLow, Hour: memTestHour}, TxOptions{})
	require.ErrorIs(t, err, ErrInvalidQuery)
}

// TestCheckFindTransactionDefaults applies the defaults of TxOptions: every zero field takes its default,
// every positive field is kept.
func TestCheckFindTransactionDefaults(t *testing.T) {
	t.Parallel()

	key := TxKey{CaptureID: captureLow, Hour: memTestHour}
	got, err := checkFindTransaction(newMemSource(), key, TxOptions{})
	require.NoError(t, err)
	assert.Equal(t, TxOptions{
		MaxHours: DefaultTxMaxHours, MaxHeldBytes: DefaultMaxHeldBytes,
		MaxConflicts: DefaultMaxConflicts, MaxStateBytes: DefaultTxMaxStateBytes,
	}, got)

	set := TxOptions{MaxHours: 5, MaxHeldBytes: 6, MaxConflicts: 7, MaxStateBytes: 8}
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
}

// TestTxClassHasAnyHasAll checks both methods, the zero argument and the zero receiver included.
func TestTxClassHasAnyHasAll(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		c, bits  TxClass
		any, all bool
	}{
		{"zero argument", TxClosing, 0, false, true},
		{"zero receiver", 0, TxPrimary, false, false},
		{"zero both", 0, 0, false, true},
		{"one set", TxPossibleReply | TxPossiblePrimary, TxPossiblePrimary, true, true},
		{"one clear", TxClosing, TxPrimary, false, false},
		{"both set", TxPossibleReply | TxPossiblePrimary, TxPossibleReply | TxPossiblePrimary, true, true},
		{"one set, one clear", TxClosing, TxClosing | TxPrimary, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.any, tt.c.HasAny(tt.bits))
			assert.Equal(t, tt.all, tt.c.HasAll(tt.bits))
		})
	}
}

// TestTxEnumStrings formats every TxOutcome and TxGapReason value, and one undefined value of each,
// and pins their contiguous numbering.
func TestTxEnumStrings(t *testing.T) {
	t.Parallel()

	outcomes := map[TxOutcome]string{
		0: "none", TxMatched: "matched", TxAmbiguous: "ambiguous", TxNotFound: "not-found", 4: "unknown(4)",
	}
	for o, want := range outcomes {
		assert.Equal(t, want, o.String())
	}
	assert.Equal(t, []TxOutcome{1, 2, 3}, []TxOutcome{TxMatched, TxAmbiguous, TxNotFound})

	reasons := map[TxGapReason]string{
		0: "unknown(0)", TxGapNoKey: "no-key", TxGapConflict: "conflict", TxGapIndex: "index",
		TxGapRead: "read", TxGapCoverage: "coverage", TxGapUnevaluated: "unevaluated",
		TxGapSeqGap: "seq-gap", TxGapOpenWindow: "open-window", TxGapCrashGap: "crash-gap",
		TxGapCaptureBoundary: "capture-boundary", TxGapOrderingUncertain: "ordering-uncertain",
		TxGapCorrelation: "correlation", TxGapUnavailable: "unavailable", TxGapScopeBreach: "scope-breach", 15: "unknown(15)",
	}
	for r, want := range reasons {
		assert.Equal(t, want, r.String())
	}
	assert.Equal(t, []TxGapReason{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14}, []TxGapReason{
		TxGapNoKey, TxGapConflict, TxGapIndex, TxGapRead, TxGapCoverage, TxGapUnevaluated, TxGapSeqGap, TxGapOpenWindow,
		TxGapCrashGap, TxGapCaptureBoundary, TxGapOrderingUncertain, TxGapCorrelation, TxGapUnavailable, TxGapScopeBreach,
	})
}

// TestTxPackError formats a TxPackError with its hour and pack_id and unwraps it to its Err.
func TestTxPackError(t *testing.T) {
	t.Parallel()

	inner := errors.New("footer CRC mismatch")
	e := TxPackError{Hour: 12, Pack: seg0, Err: inner}
	assert.Equal(t, "tracepack: hour 12 pack "+seg0.String()+": footer CRC mismatch", e.Error())
	require.ErrorIs(t, e, inner)
}

// TestTxResultHelpers derives Complete, Matches and PrimaryVersions from constructed results:
// Complete follows Incomplete alone, so a conflict reported there as a TxGapConflict gap makes the lookup incomplete,
// while a conflict listed in Conflicts only, or a footer error, does not;
// Matches and PrimaryVersions point into Records at the versions with Valid and with TxPrimary, in Records order, nil when none.
func TestTxResultHelpers(t *testing.T) {
	t.Parallel()

	// The primary in its own hour and a later one, a valid candidate, an invalid one,
	// a valid candidate in conflict and the other version of its seq.
	mixed := []TxRecord{
		{Record: Record{Seq: 12}, Hour: 1, Class: TxPrimary},
		{Record: Record{Seq: 12}, Hour: 2, Class: TxPrimary},
		{Record: Record{Seq: 13}, Hour: 1, Class: TxCandidate | TxClosing, Decidable: true, Eligible: true, Valid: true},
		{Record: Record{Seq: 14}, Hour: 1, Class: TxCandidate, Decidable: true},
		{Record: Record{Seq: 15}, Hour: 2, Class: TxCandidate, Conflict: true, Decidable: true, Eligible: true, Valid: true},
		{Record: Record{Seq: 15}, Hour: 2, Conflict: true},
	}
	tests := []struct {
		name      string
		res       TxResult
		complete  bool
		matches   []int
		primaries []int
	}{
		{name: "the zero result", complete: true},
		{name: "a conflict on a seq the lookup kept, a gap", complete: false, res: TxResult{
			Incomplete: []TxGap{{Reason: TxGapConflict, Hours: []int64{1}, Block: -1, Offset: -1, Seq: new(uint64(13))}},
			Conflicts:  []Conflict{{CaptureID: captureLow, Seq: 13}},
		}},
		{name: "a conflict below the primary, not a gap", complete: true, res: TxResult{
			Conflicts: []Conflict{{CaptureID: captureLow, Seq: 11}},
		}},
		{name: "a conflict of another capture, not a gap", complete: true, res: TxResult{
			Conflicts: []Conflict{{CaptureID: captureHigh, Seq: 13}},
		}},
		{name: "a footer error alone", complete: true, res: TxResult{
			FooterErrs: []TxPackError{{Hour: 1, Pack: seg0, Err: errors.New("footer CRC mismatch")}},
		}},
		{name: "another gap", complete: false, res: TxResult{Incomplete: []TxGap{{Reason: TxGapOpenWindow, Block: -1, Offset: -1}}}},
		{name: "no version qualifies", complete: true, res: TxResult{Records: []TxRecord{
			{Record: Record{Seq: 13}, Class: TxSameKeyPrimary},
			{Record: Record{Seq: 14}, Class: TxCandidate, Decidable: true},
			{Record: Record{Seq: 15}, Conflict: true},
		}}},
		{name: "versions interleaved", complete: true, res: TxResult{Records: mixed}, matches: []int{2, 4}, primaries: []int{0, 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			res := tt.res
			assert.Equal(t, tt.complete, res.Complete())
			requireTxRecordsAt(t, &res, tt.matches, res.Matches())
			requireTxRecordsAt(t, &res, tt.primaries, res.PrimaryVersions())
		})
	}
}

// TestTxResultHelpersAlias writes through the pointers Matches returns, which changes the elements of Records,
// and reallocates Records, which detaches them from it.
func TestTxResultHelpersAlias(t *testing.T) {
	t.Parallel()

	res := TxResult{Records: []TxRecord{
		{Record: Record{Seq: 12}, Class: TxPrimary},
		{Record: Record{Seq: 13}, Class: TxCandidate, Valid: true},
	}}
	m := res.Matches()
	require.Len(t, m, 1)
	m[0].Hour = 7
	assert.Equal(t, int64(7), res.Records[1].Hour, "written through the pointer")

	res.Records = append([]TxRecord(nil), res.Records...)
	m[0].Hour = 8
	assert.Equal(t, int64(7), res.Records[1].Hour, "detached by the reallocation")
	assert.Same(t, &res.Records[1], res.Matches()[0], "a new call points into the new array")
}

// TestTxResultHelpersLookup runs real lookups of txRecord's primary at 12, answered at 13 and bounded by a same-key primary at 14,
// over two packs of one hour:
// a conflict below the primary leaves the lookup complete and is listed in Conflicts only,
// while a conflict on the reply's seq is a TxGapConflict gap that makes it incomplete;
// in both, the helpers select from Records exactly what filtering Records by hand selects.
func TestTxResultHelpersLookup(t *testing.T) {
	t.Parallel()

	other := txFrame(func(p []byte) { p[fieldSystemBytesOff] = 0 })
	tests := []struct {
		name     string
		second   []Record
		complete bool
		gaps     []TxGapReason
		conflict uint64
	}{
		{name: "a conflict below the primary", second: []Record{txRecord(11, other), txRecord(12, nil), txReply(13, nil), txRecord(14, nil)},
			complete: true, gaps: []TxGapReason{}, conflict: 11},
		{name: "a conflict on the reply's seq", second: []Record{txRecord(11, nil), txRecord(12, nil), txReply(13, other), txRecord(14, nil)},
			complete: false, gaps: []TxGapReason{TxGapConflict}, conflict: 13},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			first := txPack(t, seg0, nil, txBlock(txRecord(11, nil), txRecord(12, nil), txReply(13, nil), txRecord(14, nil)))
			second := txPack(t, seg1, nil, txBlock(tt.second...))
			res, err := findTx(t, t.Context(), txSource(t, first, second), txKeyAt(12), TxOptions{MaxHours: 1})
			require.NoError(t, err)
			assert.Equal(t, TxMatched, res.Outcome)
			assert.Equal(t, tt.gaps, gapReasons(res.Incomplete))
			assert.Equal(t, []uint64{tt.conflict}, conflictSeqs(res.Conflicts))
			assert.Equal(t, tt.complete, res.Complete())

			var matches, primaries []int
			for i := range res.Records {
				if res.Records[i].Valid {
					matches = append(matches, i)
				}
				if res.Records[i].Class.HasAny(TxPrimary) {
					primaries = append(primaries, i)
				}
			}
			require.NotEmpty(t, matches, "the reply is a valid match")
			require.Len(t, primaries, 1, "one version of the primary")
			requireTxRecordsAt(t, &res, matches, res.Matches())
			requireTxRecordsAt(t, &res, primaries, res.PrimaryVersions())
		})
	}
}

// requireTxRecordsAt requires that got points at the elements of res.Records at the indices want, in order,
// and that it is nil when want is empty.
func requireTxRecordsAt(t *testing.T, res *TxResult, want []int, got []*TxRecord) {
	t.Helper()

	if len(want) == 0 {
		require.Nil(t, got)

		return
	}
	require.Len(t, got, len(want))
	for k, i := range want {
		require.Same(t, &res.Records[i], got[k], "index %d", i)
	}
}

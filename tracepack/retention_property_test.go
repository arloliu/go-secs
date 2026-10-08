package tracepack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The reference of a read under a retention boundary (retentionFacts)
// checks a read against an event schedule chosen apart from the read's own checks:
// a scriptedRun moves the boundary at the n-th event among the block reads and the calls of fn, as the seed chooses,
// and logs every block read, call of fn and call of RetainedFrom in the order they happen.
// From the log alone, with the hours each pack and block covers (the tracepack storage specification §5, Retention),
// the reference knows when the read must have found a covered hour removed, and which hours it lists.
// The same schedule's faults, a failed block read, an error from fn and a failing provider,
// run again without a boundary, give the read the boundary must not change.

// retentionEventKind is what a retentionEvent logs.
type retentionEventKind uint8

const (
	// eventRead is a full read of a block, logged as the read starts.
	eventRead retentionEventKind = iota + 1
	// eventCall is a call of fn, with the Item's pack and block.
	eventCall
	// eventCheck is a call of RetainedFrom, with the boundary it returned.
	eventCheck
)

// retentionEvent is one event of a scripted read.
type retentionEvent struct {
	kind        retentionEventKind
	pack, block int
	// from is the boundary a check returned.
	from int64
	// failed reports a block read failing with errInjected, or a check failing with errRetention.
	failed bool
}

// key returns the event's block, by pack and block index.
func (e retentionEvent) key() [2]int {
	return [2]int{e.pack, e.block}
}

// boundaryScript is the schedule of a scripted read.
type boundaryScript struct {
	// provider gives the read the scriptedRun as its Retention; without it, the read has no boundary.
	provider bool
	// from is the boundary before the read; at, the index among the read's block reads and calls of fn,
	// counting from 0, at which it becomes to: before the block is read, or while fn runs; -1 for never.
	from, to int64
	at       int
	// failRead is the block read, counting from 0, that fails with errInjected;
	// stopCall the call of fn that returns errStopIterate;
	// failCheck the call of RetainedFrom from which every call fails with errRetention; -1 for none.
	failRead, stopCall, failCheck int
}

// noScript is the schedule of a read with no boundary and no fault.
var noScript = boundaryScript{at: -1, failRead: -1, stopCall: -1, failCheck: -1}

// faults returns s without its boundary: the read the boundary must not change.
func (s boundaryScript) faults() boundaryScript {
	return boundaryScript{at: -1, failRead: s.failRead, stopCall: s.stopCall, failCheck: -1}
}

// scriptedRun is a Retention that follows a boundaryScript and logs the events of the read it serves.
type scriptedRun struct {
	mu     sync.Mutex
	script boundaryScript
	from   int64
	// steps counts the block reads and calls of fn so far; reads, calls and checks each kind.
	steps, reads, calls, checks int
	events                      []retentionEvent
}

var _ Retention = (*scriptedRun)(nil)

// newScriptedRun returns a scriptedRun following s.
func newScriptedRun(s boundaryScript) *scriptedRun {
	return &scriptedRun{script: s, from: s.from}
}

// RetainedFrom implements Retention.
func (s *scriptedRun) RetainedFrom(context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := s.checks
	s.checks++
	if s.script.failCheck >= 0 && n >= s.script.failCheck {
		s.events = append(s.events, retentionEvent{kind: eventCheck, failed: true})

		return 0, errRetention
	}
	s.events = append(s.events, retentionEvent{kind: eventCheck, from: s.from})

	return s.from, nil
}

// step counts a block read or a call of fn, moving the boundary when the schedule says.
// The caller holds s.mu.
func (s *scriptedRun) step() {
	if s.steps == s.script.at {
		s.from = s.script.to
	}
	s.steps++
}

// blockRead logs the read of block of pack, the boundary moving first when the schedule says,
// and returns errInjected when the read fails.
func (s *scriptedRun) blockRead(pack, block int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := s.reads
	s.reads++
	s.step()
	fail := n == s.script.failRead
	s.events = append(s.events, retentionEvent{kind: eventRead, pack: pack, block: block, failed: fail})
	if fail {
		return errInjected
	}

	return nil
}

// call logs a call of fn with a record of block of pack, the boundary moving during the call when the schedule says,
// and returns the call's error.
func (s *scriptedRun) call(pack, block int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := s.calls
	s.calls++
	s.events = append(s.events, retentionEvent{kind: eventCall, pack: pack, block: block})
	s.step()
	if n == s.script.stopCall {
		return errStopIterate
	}

	return nil
}

// log returns the events logged so far.
func (s *scriptedRun) log() []retentionEvent {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.events)
}

// scriptedReaderAt is the io.ReaderAt of a pack of a scripted read:
// once a run is set, every read at a block's offset is a block read of that run.
type scriptedReaderAt struct {
	data []byte
	pack int
	// blocks maps the offset of each block to its index, once the pack is open.
	blocks map[int64]int
	run    *scriptedRun
	// stray counts the reads of a run at no block's offset.
	stray int
}

// ReadAt implements io.ReaderAt.
func (a *scriptedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if a.run != nil {
		i, ok := a.blocks[off]
		if !ok {
			a.stray++
		} else if err := a.run.blockRead(a.pack, i); err != nil {
			return 0, err
		}
	}

	return bytes.NewReader(a.data).ReadAt(p, off)
}

// openScripted opens data, the pack at index pack of a read, through a scriptedReaderAt.
//
// Returns:
//   - *Reader: the open pack; nil with an error.
//   - *scriptedReaderAt: its io.ReaderAt, with no run set.
//   - error: Open's error.
func openScripted(ctx context.Context, data []byte, pack int, opts ReaderOptions) (*Reader, *scriptedReaderAt, error) {
	a := &scriptedReaderAt{data: data, pack: pack}
	r, err := Open(ctx, a, int64(len(data)), opts)
	if err != nil {
		return nil, nil, err
	}
	a.blocks = make(map[int64]int, len(r.blocks))
	for i, b := range r.Blocks() {
		a.blocks[int64(b.Offset)] = i
	}

	return r, a, nil
}

// openScriptedAll opens files, each through a scriptedReaderAt, requiring each to open.
func openScriptedAll(t testing.TB, files ...[]byte) ([]*Reader, []*scriptedReaderAt) {
	t.Helper()

	readers := make([]*Reader, len(files))
	ats := make([]*scriptedReaderAt, len(files))
	for i, f := range files {
		var err error
		readers[i], ats[i], err = openScripted(t.Context(), f, i, ReaderOptions{})
		require.NoError(t, err)
	}

	return readers, ats
}

// refHourOf returns the UTC hour of ts, a ts_utc_ns, numbered as HourOf numbers it.
func refHourOf(ts int64) int64 {
	h := ts / hourNs
	if ts%hourNs < 0 {
		h--
	}

	return h
}

// retentionFacts is what a read's packs cover,
// as the reference takes it from what Open found and from each block's records.
type retentionFacts struct {
	// initial holds the hours known before any block is read:
	// the scope hour of each pack but an extract, the F-2 ts_min and ts_max hours of each indexed block of an extract.
	initial []int64
	extract []bool
	// blockHours holds the distinct hours of the records of each usable block of an extract, by pack and block.
	blockHours map[[2]int][]int64
}

// retentionFactsOf returns the facts of readers, reading each block of an extract once.
// The readers' scriptedReaderAts must have no run set.
func retentionFactsOf(t testing.TB, readers []*Reader) *retentionFacts {
	t.Helper()

	f := &retentionFacts{extract: make([]bool, len(readers)), blockHours: map[[2]int][]int64{}}
	for p, r := range readers {
		if r.meta.PackRole != PackRoleExtract {
			f.initial = append(f.initial, refHourOf(r.meta.PeriodStart))

			continue
		}
		f.extract[p] = true
		for i, b := range r.Blocks() {
			if b.Indexed {
				f.initial = append(f.initial, refHourOf(b.TSMin), refHourOf(b.TSMax))
			}
			var buf blockBuf
			d, _, err := r.readBlock(i, &buf)
			require.NoError(t, err)
			if d == nil {
				continue
			}
			var hours []int64
			for j := range d.count() {
				hours = append(hours, refHourOf(d.header(j).TSUTCNs))
			}
			slices.Sort(hours)
			f.blockHours[[2]int{p, i}] = slices.Compact(hours)
		}
	}

	return f
}

// hours returns every hour the facts know, ascending, each once.
func (f *retentionFacts) hours() []int64 {
	out := slices.Clone(f.initial)
	for _, hs := range f.blockHours {
		out = append(out, hs...)
	}
	slices.Sort(out)

	return slices.Compact(out)
}

// replay follows the events of a read and finds the first at which the read knows a covered hour removed:
// a check returning a boundary past an hour known covered, or the read of an extract's block adding a covered hour
// before the largest boundary returned so far.
//
// Returns:
//   - int: the index of that event; -1 when there is none.
//   - []int64: the hours known covered then before the largest boundary returned, ascending; nil when there is none.
func (f *retentionFacts) replay(events []retentionEvent) (int, []int64) {
	known := map[int64]bool{}
	for _, h := range f.initial {
		known[h] = true
	}
	var boundary int64
	seen := false
	for i, e := range events {
		if e.kind == eventRead && !e.failed && f.extract[e.pack] {
			for _, h := range f.blockHours[e.key()] {
				known[h] = true
			}
		}
		if e.kind == eventCheck && !e.failed && (!seen || e.from > boundary) {
			boundary, seen = e.from, true
		}
		if !seen || e.kind == eventCall {
			continue
		}
		var removed []int64
		for h := range known {
			if h < boundary {
				removed = append(removed, h)
			}
		}
		if removed != nil {
			slices.Sort(removed)

			return i, removed
		}
	}

	return -1, nil
}

// newBoundaryScript returns the schedule seed draws for a read with reads block reads and calls calls of fn,
// its boundaries chosen around the hours facts knows:
// mostly retaining every hour before the read, now and then removing some, or every hour known before the read,
// or from math.MinInt64;
// moving at a random event, now and then never, to an hour around them or to math.MaxInt64;
// now and then failing a block read, returning an error from fn, or failing the provider from one of its calls on.
func newBoundaryScript(seed uint64, facts *retentionFacts, reads, calls int) boundaryScript {
	rng := rand.New(rand.NewPCG(seed, 0x7265_7461_696e))
	var lo, hi int64
	if hours := facts.hours(); len(hours) > 0 {
		lo, hi = hours[0], hours[len(hours)-1]
	}
	near := func() int64 { return lo - 1 + rng.Int64N(hi-lo+3) }

	s := boundaryScript{provider: true, from: lo - rng.Int64N(2), at: -1, failRead: -1, stopCall: -1, failCheck: -1}
	switch rng.IntN(8) {
	case 0:
		s.from = math.MinInt64
	case 1:
		s.from = near()
	case 2:
		// Retaining every hour known before the read, so the first hour of a record below them ends it.
		s.from = hi
		if len(facts.initial) > 0 {
			s.from = slices.Min(facts.initial)
		}
	default:
		// The boundary before the read retains every hour.
	}
	if rng.IntN(5) != 0 {
		s.at, s.to = rng.IntN(reads+calls+1), near()
		if rng.IntN(10) == 0 {
			s.to = math.MaxInt64
		}
	}
	if reads > 0 && rng.IntN(5) == 0 {
		s.failRead = rng.IntN(reads)
	}
	if calls > 0 && rng.IntN(5) == 0 {
		s.stopCall = rng.IntN(calls)
	}
	if rng.IntN(10) == 0 {
		s.failCheck = rng.IntN(calls + 3)
	}

	return s
}

// scriptedEnd is how a scripted read ended.
type scriptedEnd uint8

const (
	// endKept is a read the boundary did not end.
	endKept scriptedEnd = iota
	// endRemovedCheck is a read ended by a check finding a covered hour removed,
	// endRemovedHours by the hours of an extract's block read, endRemovedRecheck by the check after a failed block read.
	endRemovedCheck
	endRemovedHours
	endRemovedRecheck
	// endProviderErr is a read ended by the provider's error.
	endProviderErr
	scriptedEnds
)

// String names the end.
func (e scriptedEnd) String() string {
	return [...]string{"kept", "removed at a check", "removed by a block's hours", "removed after a failed read", "provider error"}[e]
}

// scriptedRead runs one read under a schedule, with its readers' scriptedReaderAts set to the schedule's run,
// and returns what it read with the events it logged.
type scriptedRead func(s boundaryScript) (checkedRead, []retentionEvent)

// mergeScripted returns the scriptedRead of q over readers, opened through ats, with opts (readCheckedWith).
func mergeScripted(t *testing.T, readers []*Reader, ats []*scriptedReaderAt, q Query, opts MergeIterateOptions) scriptedRead {
	return func(s boundaryScript) (checkedRead, []retentionEvent) {
		t.Helper()

		run := newScriptedRun(s)
		for _, a := range ats {
			a.run = run
		}
		defer func() {
			for _, a := range ats {
				a.run = nil
			}
		}()
		rq := q
		if s.provider {
			rq.Retention = run
		}
		out := readCheckedWith(t, readers, rq, opts, func(it *Item) error { return run.call(it.Pack, it.Block) })
		for _, a := range ats {
			require.Zero(t, a.stray, "pack %d: every read of the read is a block read", a.pack)
		}

		return out, run.log()
	}
}

// iterateScripted returns the scriptedRead of q over r, opened through a.
func iterateScripted(t *testing.T, r *Reader, a *scriptedReaderAt, q Query) scriptedRead {
	return func(s boundaryScript) (checkedRead, []retentionEvent) {
		t.Helper()

		run := newScriptedRun(s)
		a.run = run
		defer func() { a.run = nil }()
		rq := q
		if s.provider {
			rq.Retention = run
		}
		var out checkedRead
		out.res, out.err = r.Iterate(t.Context(), rq, func(it *Item) error {
			out.items = append(out.items, copyItem(it))

			return run.call(0, it.Block)
		})
		require.Zero(t, a.stray, "every read of the read is a block read")

		return out, run.log()
	}
}

// checkScripted runs read without a boundary to count its block reads and calls of fn,
// then under the schedule each of seeds draws, requiring each to agree with the reference (requireScriptedRead).
// It returns how many of the reads ended each way.
func checkScripted(t *testing.T, facts *retentionFacts, read scriptedRead, seeds ...uint64) [scriptedEnds]int {
	t.Helper()

	_, events := read(noScript)
	reads := len(slices.DeleteFunc(slices.Clone(events), func(e retentionEvent) bool { return e.kind != eventRead }))
	calls := len(slices.DeleteFunc(slices.Clone(events), func(e retentionEvent) bool { return e.kind != eventCall }))

	var ends [scriptedEnds]int
	for _, seed := range seeds {
		s := newBoundaryScript(seed, facts, reads, calls)
		base, baseEvents := read(s.faults())
		out, events := read(s)
		ends[requireScriptedRead(t, facts, s, base, out, baseEvents, events)]++
	}

	return ends
}

// withoutChecks returns the block reads and calls of fn of events, in order.
func withoutChecks(events []retentionEvent) []retentionEvent {
	return nilIfEmpty(slices.DeleteFunc(slices.Clone(events), func(e retentionEvent) bool { return e.kind == eventCheck }))
}

// checkedBetween reports whether events[from:to] holds a check.
func checkedBetween(events []retentionEvent, from, to int) bool {
	return slices.ContainsFunc(events[max(from, 0):to], func(e retentionEvent) bool { return e.kind == eventCheck })
}

// requireCheckOrder requires the read logging events to ask the boundary before it reads a block;
// before every call of fn with a record of another block than the last call's,
// and between a block's read and its first call of fn after it;
// after a failed block read, ending the read; and, when the provider fails, last.
func requireCheckOrder(t *testing.T, events []retentionEvent, msg string) {
	t.Helper()

	require.NotEmpty(t, events, msg)
	require.Equal(t, eventCheck, events[0].kind, "the boundary is asked before any block is read; %s", msg)

	lastRead := map[[2]int]int{}
	lastCall := -1
	for i, e := range events {
		switch e.kind {
		case eventRead:
			lastRead[e.key()] = i
			if e.failed {
				require.Len(t, events, i+2, "event %d: one check follows a failed block read, the last; %s", i, msg)
				require.Equal(t, eventCheck, events[i+1].kind, "event %d: a check follows a failed block read; %s", i, msg)
			}
		case eventCall:
			if lastCall < 0 || events[lastCall].key() != e.key() {
				require.True(t, checkedBetween(events, lastCall+1, i),
					"event %d: a check precedes a call of fn with a record of another block than the last call's; %s", i, msg)
			}
			r, ok := lastRead[e.key()]
			require.True(t, ok, "event %d: fn is passed a record of a block read; %s", i, msg)
			calledSince := slices.ContainsFunc(events[r+1:i], func(c retentionEvent) bool { return c.kind == eventCall && c.key() == e.key() })
			if !calledSince {
				require.True(t, checkedBetween(events, r+1, i),
					"event %d: a check follows the read of a block before its first call of fn; %s", i, msg)
			}
			lastCall = i
		case eventCheck:
			if e.failed {
				require.Len(t, events, i+1, "event %d: a failing provider ends the read; %s", i, msg)
			}
		default:
			require.Failf(t, "an event of no kind", "event %d; %s", i, msg)
		}
	}
}

// requireScriptedRead requires out, a read under s logging events, to agree with the reference of facts
// and with base, the read under s's faults alone, logging baseEvents:
//   - the read asks the boundary before it reads a block;
//     before every call of fn with a record of another block than the last call's,
//     and between a block's read and its first call of fn after it;
//     after a failed block read, ending the read; and before it returns nil;
//   - its block reads and calls of fn are base's, or a prefix of them when the boundary ended the read;
//   - when the reference knows a covered hour removed, nothing follows that event,
//     the read ends with ErrRemoved, joined after the read error when the event is the check after a failed read,
//     and Result.Removed lists the hours the reference knows removed;
//   - a failing provider ends the read with its error,
//     joined after the read error when it fails the check after a failed read;
//   - otherwise the read returns what base returned, the error of fn, ErrReadLimit or a read error included;
//   - a read the boundary ended passed fn a prefix of base's records, and reports a prefix of its conflicts and defects.
//
// It returns how the read ended.
func requireScriptedRead(t *testing.T, facts *retentionFacts, s boundaryScript, base, out checkedRead,
	baseEvents, events []retentionEvent,
) scriptedEnd {
	t.Helper()

	msg := fmt.Sprintf("schedule %+v, events %v", s, events)
	requireCheckOrder(t, events, msg)

	last := len(events) - 1
	afterFailedRead := last > 0 && events[last].kind == eventCheck && events[last-1].kind == eventRead && events[last-1].failed
	requireReadErrFirst := func() {
		t.Helper()

		require.ErrorIs(t, out.err, errInjected, msg)
		assert.ErrorIs(t, unwrapJoined(t, out.err)[0], errInjected, "the read error comes first; %s", msg)
	}

	end := endKept
	at, removed := facts.replay(events)
	switch {
	case at >= 0:
		require.Equal(t, last, at, "nothing follows the event that found a covered hour removed; %s", msg)
		require.ErrorIs(t, out.err, ErrRemoved, msg)
		require.Equal(t, removed, out.res.Removed, "the hours known covered before the largest boundary returned; %s", msg)
		assert.False(t, out.res.Complete(), msg)
		switch {
		case events[at].kind == eventRead:
			end = endRemovedHours
		case afterFailedRead:
			requireReadErrFirst()
			end = endRemovedRecheck
		default:
			end = endRemovedCheck
		}
	case events[last].kind == eventCheck && events[last].failed:
		require.ErrorIs(t, out.err, errRetention, msg)
		require.NotErrorIs(t, out.err, ErrRemoved, msg)
		require.Nil(t, out.res.Removed, msg)
		if afterFailedRead {
			requireReadErrFirst()
		}
		end = endProviderErr
	default:
		require.NotErrorIs(t, out.err, ErrRemoved, msg)
		require.Nil(t, out.res.Removed, msg)
		require.Equal(t, withoutChecks(baseEvents), withoutChecks(events), "the block reads and calls of fn of the read without a boundary; %s", msg)
		require.Equal(t, base.items, out.items, msg)
		require.Equal(t, base.res, out.res, msg)
		require.Equal(t, base.err == nil, out.err == nil, "errors %v and %v; %s", base.err, out.err, msg)
		if base.err != nil {
			require.EqualError(t, out.err, base.err.Error(), msg)
		}
		if errors.Is(base.err, errStopIterate) {
			require.Equal(t, errStopIterate, out.err, "the error of fn as is; %s", msg) //nolint:testifylint // identity, not errors.Is
		}
		if out.err == nil {
			require.Equal(t, eventCheck, events[last].kind, "the boundary is asked before the read returns nil; %s", msg)
		}

		return end
	}

	requirePrefix(t, withoutChecks(baseEvents), withoutChecks(events), "the block reads and calls of fn")
	requirePrefix(t, base.items, out.items, "items")
	requirePrefix(t, base.res.Conflicts, out.res.Conflicts, "conflicts")
	requirePrefix(t, defectKeys(base.res.Incomplete), defectKeys(out.res.Incomplete), "defects")
	require.Equal(t, base.res.FooterErrs, out.res.FooterErrs, msg)

	return end
}

// scriptedSchedules is the number of schedules TestMergeIterateRetentionScripted and TestIterateRetentionScripted draw
// for each read.
const scriptedSchedules = 4

// scriptedSeeds returns the seeds of the n schedules of the read generated from seed.
func scriptedSeeds(seed uint64, n int) []uint64 {
	out := make([]uint64, n)
	for i := range out {
		out[i] = seed<<8 | uint64(i)
	}

	return out
}

// requireScriptedEnds requires each of want, summed over the reads in ends, to have been exercised.
func requireScriptedEnds(t *testing.T, reads int, ends [scriptedEnds]int, want ...scriptedEnd) {
	t.Helper()

	counts := map[string]int{}
	for e := range scriptedEnds {
		counts[e.String()] = ends[e]
	}
	t.Logf("over %d reads: %v", reads, counts)
	for _, e := range want {
		assert.Positive(t, ends[e], e.String())
	}
}

// TestMergeIterateRetentionScripted reads the reads genRetentionReadOf generates, in both orders,
// under schedules a seed draws (newBoundaryScript),
// and requires each read to agree with the reference (requireScriptedRead)
// and nothing to stay held after it, its block buffers dropped (readCheckedWith);
// the read without a boundary agrees with the reference of a read over several packs (requireReference).
// Together the reads end in every way but by the hours of an extract's block, which TestRetentionScriptedExtracts exercises;
// it checks so when every read ran, not when a -run pattern selects some of them.
func TestMergeIterateRetentionScripted(t *testing.T) {
	t.Parallel()

	n := genReadsLong / 2
	if testing.Short() {
		n = genReads
	}
	var mu sync.Mutex
	var sum [scriptedEnds]int
	ran := 0
	t.Cleanup(func() {
		if ran == n {
			requireScriptedEnds(t, n, sum, endKept, endRemovedCheck, endRemovedRecheck, endProviderErr)
		}
	})
	for seed := range uint64(n) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()

			g := genRetentionReadOf(t, seed)
			readers, ats := openScriptedAll(t, g.files...)
			ref := newMergeIterateReference(t, readers, g.q)
			facts := retentionFactsOf(t, readers)
			var ends [scriptedEnds]int
			for k, order := range []Order{OrderCapture, OrderTime} {
				opts := MergeIterateOptions{Order: order, MaxHeldBytes: g.heldLimit}
				requireReference(t, ref, opts, readChecked(t, readers, g.q, opts))
				got := checkScripted(t, facts, mergeScripted(t, readers, ats, g.q, opts), scriptedSeeds(seed<<1|uint64(k), scriptedSchedules)...)
				for e := range ends {
					ends[e] += got[e]
				}
			}

			mu.Lock()
			defer mu.Unlock()
			ran++
			for e := range sum {
				sum[e] += ends[e]
			}
		})
	}
}

// TestIterateRetentionScripted iterates each pack of the reads genRetentionReadOf generates
// under schedules a seed draws (newBoundaryScript),
// and requires each read to agree with the reference (requireScriptedRead).
func TestIterateRetentionScripted(t *testing.T) {
	t.Parallel()

	n := genReads
	var mu sync.Mutex
	var sum [scriptedEnds]int
	ran := 0
	t.Cleanup(func() {
		if ran == n {
			requireScriptedEnds(t, n, sum, endKept, endRemovedCheck, endRemovedHours, endRemovedRecheck, endProviderErr)
		}
	})
	for seed := range uint64(n) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()

			g := genRetentionReadOf(t, seed)
			var ends [scriptedEnds]int
			for i, file := range g.files {
				r, a, err := openScripted(t.Context(), file, 0, ReaderOptions{})
				require.NoError(t, err)
				facts := retentionFactsOf(t, []*Reader{r})
				got := checkScripted(t, facts, iterateScripted(t, r, a, g.q), scriptedSeeds(seed<<4|uint64(i), scriptedSchedules)...)
				for e := range ends {
					ends[e] += got[e]
				}
			}

			mu.Lock()
			defer mu.Unlock()
			ran++
			for e := range sum {
				sum[e] += ends[e]
			}
		})
	}
}

// TestRetentionScriptedExtracts reads, through MergeIterate in both orders and through Iterate pack by pack,
// a walked extract whose blocks hold records of the hours P−3, P and P+2,
// an indexed extract whose block 0 holds records of P−5, its F-2 entry saying P,
// and a segment of P+1,
// under schedules seeds draw (newBoundaryScript), requiring each read to agree with the reference (requireScriptedRead).
// Together the reads end at a check and by the hours of an extract's block read.
func TestRetentionScriptedExtracts(t *testing.T) {
	t.Parallel()

	walked := writeReaderPack(t, retentionPackConfig(0, true, true), hourBlockRecords(retentionHour-3, retentionHour, retentionHour+2)).file
	misindexed := reindexedWith(t, writeReaderPack(t, retentionPackConfig(0, true, false), hourBlockRecords(retentionHour-5, retentionHour)).file,
		func(i int, s *blockSummary) {
			if i == 0 {
				s.tsMin, s.tsMax = blockTestHour, blockTestHour+1
				s.epochs[0].tsMin, s.epochs[0].tsMax = blockTestHour, blockTestHour+1
			}
		})
	h := blockTestHour + hourNs
	files := [][]byte{
		withIDs(t, walked, seg0, captureLow),
		withIDs(t, misindexed, seg1, captureHigh),
		retentionMergePack(t, seg2, UUID{0xC2}, retentionHour+1, false, recordsAt(1, h, h+1, h+2)),
	}
	const schedules = 128

	var mergeEnds [scriptedEnds]int
	readers, ats := openScriptedAll(t, files...)
	facts := retentionFactsOf(t, readers)
	for k, order := range []Order{OrderCapture, OrderTime} {
		read := mergeScripted(t, readers, ats, Query{Payloads: true}, MergeIterateOptions{Order: order})
		got := checkScripted(t, facts, read, scriptedSeeds(uint64(k), schedules)...)
		for e := range mergeEnds {
			mergeEnds[e] += got[e]
		}
	}
	requireScriptedEnds(t, 2*schedules, mergeEnds, endKept, endRemovedCheck, endRemovedHours)

	var iterateEnds [scriptedEnds]int
	for i, file := range files {
		r, a, err := openScripted(t.Context(), file, 0, ReaderOptions{})
		require.NoError(t, err)
		got := checkScripted(t, retentionFactsOf(t, []*Reader{r}), iterateScripted(t, r, a, Query{}), scriptedSeeds(uint64(2+i), schedules)...)
		for e := range iterateEnds {
			iterateEnds[e] += got[e]
		}
	}
	requireScriptedEnds(t, len(files)*schedules, iterateEnds, endKept, endRemovedCheck, endRemovedHours)
}

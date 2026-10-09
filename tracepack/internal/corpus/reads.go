package corpus

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/arloliu/go-secs/tracepack"
)

// Values of a read's order (the tracepack corpus specification §5.10).
const (
	ReadOrderCapture = "capture"
	ReadOrderTime    = "time"
)

// ErrorConflictLimit is the error of a read that finds one conflict more than the bound it states
// (the tracepack corpus specification §5.10).
const ErrorConflictLimit = "conflict-limit"

// ReadVector is one read over several packs of reads.json (the tracepack corpus specification §5.10).
type ReadVector struct {
	ID    string   `json:"id"`
	Cites []string `json:"cites"`
	// Packs holds the packs read, by number, in the order the read is given them.
	Packs []int  `json:"packs"`
	Order string `json:"order"`
	// Payloads is written only when true.
	Payloads bool `json:"payloads,omitempty"`
	// MaxConflicts is the read's conflict bound, written only when the read states one; 0 for none.
	MaxConflicts int        `json:"max_conflicts,omitempty"`
	Filter       Filter     `json:"filter"`
	Expect       ReadExpect `json:"expect"`
}

// Reads is reads.json: the reads over several packs of a multi-pack vector.
type Reads []ReadVector

// ReadExpect is what a read over several packs yields: the success form, {items, incomplete, conflicts, footer_errors},
// or the error form, {error, conflicts}.
// The success form holds Items, Incomplete and FooterErrors, each non-nil, and no Error;
// the error form holds Error and none of the three.
type ReadExpect struct {
	// Error is the error form's error, ErrorConflictLimit; "" in the success form.
	Error        string            `json:"error,omitempty"`
	Items        *[]ReadItem       `json:"items,omitempty"`
	Incomplete   *[]PackIncomplete `json:"incomplete,omitempty"`
	Conflicts    []ConflictEntry   `json:"conflicts"`
	FooterErrors *[]int            `json:"footer_errors,omitempty"`
}

// ReadItem is one version a read over several packs yielded:
// the record's seq, the pack and block of the version's representative, and whether the record's copies differ.
type ReadItem struct {
	Seq      U64  `json:"seq"`
	Pack     int  `json:"pack"`
	Block    int  `json:"block"`
	Conflict bool `json:"conflict"`
}

// PackIncomplete is one reason a read over several packs is incomplete:
// the pack's number, then the keys of the tracepack corpus specification §5.4.
type PackIncomplete struct {
	Pack int `json:"pack"`
	Incomplete
}

// ConflictEntry is one conflict a read listed: the record's capture and seq,
// and for each version, in version order, the numbers of the packs holding it.
type ConflictEntry struct {
	CaptureID string  `json:"capture_id"`
	Seq       U64     `json:"seq"`
	Versions  [][]int `json:"versions"`
}

// ReadRun is a read over several packs as tracepack.MergeIterate returned it, with what its projection needs.
type ReadRun struct {
	// Packs holds the number of each reader's pack, in the order the readers were given.
	Packs []int
	// IDs holds the pack_id of each reader's pack, in the same order.
	IDs []tracepack.UUID
	// Coverage holds the coverage entries of each reader's pack metadata, in stored order, in the same order.
	Coverage [][]tracepack.Coverage
	// MaxConflicts is the conflict bound the read states; 0 for none.
	MaxConflicts int
	// Items holds the items the read yielded, in yield order;
	// only their Record.Seq, Pack, Block and Conflict are read, so a shallow copy of each item suffices.
	Items []tracepack.Item
	// Result and Err are what MergeIterate returned.
	Result tracepack.Result
	Err    error
}

// readOrders maps the corpus names of a read's order to the tracepack orders.
var readOrders = map[string]tracepack.Order{ReadOrderCapture: tracepack.OrderCapture, ReadOrderTime: tracepack.OrderTime}

// Query returns the query and the options of MergeIterate that r states: its filter, payloads, order and conflict bound.
//
// Returns:
//   - tracepack.Query: the query.
//   - tracepack.MergeIterateOptions: the order, and MaxConflicts when the read states a bound.
//   - error: an order other than capture and time, a negative bound, or the error of ToFilter.
func (r *ReadVector) Query() (tracepack.Query, tracepack.MergeIterateOptions, error) {
	order, ok := readOrders[r.Order]
	if !ok {
		return tracepack.Query{}, tracepack.MergeIterateOptions{}, fmt.Errorf("corpus: read %q: order %q is not defined", r.ID, r.Order)
	}
	if r.MaxConflicts < 0 {
		return tracepack.Query{}, tracepack.MergeIterateOptions{}, fmt.Errorf("corpus: read %q: max_conflicts %d is below 1", r.ID, r.MaxConflicts)
	}
	f, err := r.Filter.ToFilter()
	if err != nil {
		return tracepack.Query{}, tracepack.MergeIterateOptions{}, fmt.Errorf("corpus: read %q: %w", r.ID, err)
	}

	return tracepack.Query{Filter: f, Payloads: r.Payloads}, tracepack.MergeIterateOptions{Order: order, MaxConflicts: r.MaxConflicts}, nil
}

// ReadExpectFrom returns the expect of the read run (the tracepack corpus specification §5.10):
// the items in yield order, the incomplete reasons of each pack in the order of the readers,
// each pack's as ExpectFrom orders one pack's, the conflicts in discovery order,
// and the packs whose footer was not used, in the order of the readers;
// or, for a read that states a bound and fails with exactly that many conflicts, the error form.
//
// Returns:
//   - ReadExpect: the projection.
//   - error: run's error other than the conflict bound, a reader limit, a removed hour,
//     a defect the decision table does not hold, a pack index or pack_id that names no reader,
//     a version naming a pack twice or out of reader order, or footer errors out of reader order.
func ReadExpectFrom(run *ReadRun) (ReadExpect, error) {
	n := len(run.Packs)
	if len(run.IDs) != n || len(run.Coverage) != n {
		return ReadExpect{}, fmt.Errorf("corpus: %d pack numbers for %d pack_ids and %d coverage lists", n, len(run.IDs), len(run.Coverage))
	}
	res := &run.Result
	if len(res.Removed) > 0 {
		return ReadExpect{}, errors.New("corpus: a read over several packs reported removed hours")
	}
	conflicts, err := conflictEntries(res.Conflicts, run.IDs, run.Packs)
	if err != nil {
		return ReadExpect{}, err
	}
	if run.Err != nil {
		if run.MaxConflicts > 0 && errors.Is(run.Err, tracepack.ErrReadLimit) && len(res.Conflicts) == run.MaxConflicts {
			return ReadExpect{Error: ErrorConflictLimit, Conflicts: conflicts}, nil
		}

		return ReadExpect{}, fmt.Errorf("corpus: the read failed: %w", run.Err)
	}

	items := make([]ReadItem, 0, len(run.Items))
	for i := range run.Items {
		it := &run.Items[i]
		if it.Pack < 0 || it.Pack >= n || it.Block < 0 {
			return ReadExpect{}, fmt.Errorf("corpus: item %d names reader %d, block %d", i, it.Pack, it.Block)
		}
		items = append(items, ReadItem{Seq: U64(it.Record.Seq), Pack: run.Packs[it.Pack], Block: it.Block, Conflict: it.Conflict})
	}

	perPack := make([][]tracepack.Defect, n)
	for _, d := range res.Incomplete {
		if d.Pack < 0 || d.Pack >= n {
			return ReadExpect{}, fmt.Errorf("corpus: a %s defect names reader %d", d.Reason, d.Pack)
		}
		perPack[d.Pack] = append(perPack[d.Pack], d)
	}
	incomplete := []PackIncomplete{}
	for i, ds := range perPack {
		in, err := sortedIncomplete(ds, run.Coverage[i])
		if err != nil {
			return ReadExpect{}, fmt.Errorf("corpus: pack %d: %w", run.Packs[i], err)
		}
		for _, x := range in {
			incomplete = append(incomplete, PackIncomplete{Pack: run.Packs[i], Incomplete: x})
		}
	}

	footer := []int{}
	last := -1
	for _, fe := range res.FooterErrs {
		if fe.Pack <= last || fe.Pack >= n {
			return ReadExpect{}, fmt.Errorf("corpus: a footer error of reader %d is out of reader order", fe.Pack)
		}
		last = fe.Pack
		footer = append(footer, run.Packs[fe.Pack])
	}

	return ReadExpect{Items: &items, Incomplete: &incomplete, Conflicts: conflicts, FooterErrors: &footer}, nil
}

// conflictEntries renders cs, naming each pack by its number:
// ids holds the pack_id of each pack the conflicts may name, in the order the versions list them,
// and numbers the number of each.
//
// Returns:
//   - []ConflictEntry: the entries, in the order of cs; [] for none.
//   - error: a pack_id ids does not hold, a version without a pack, or a version whose packs are not in the order of ids.
func conflictEntries(cs []tracepack.Conflict, ids []tracepack.UUID, numbers []int) ([]ConflictEntry, error) {
	out := make([]ConflictEntry, 0, len(cs))
	for i := range cs {
		c := &cs[i]
		e := ConflictEntry{CaptureID: c.CaptureID.String(), Seq: U64(c.Seq), Versions: make([][]int, 0, len(c.Versions))}
		if len(c.Versions) == 0 {
			return nil, fmt.Errorf("corpus: the conflict at seq %d lists no version", c.Seq)
		}
		for _, v := range c.Versions {
			if len(v) == 0 {
				return nil, fmt.Errorf("corpus: a version of the conflict at seq %d lists no pack", c.Seq)
			}
			packs := make([]int, 0, len(v))
			last := -1
			for _, id := range v {
				k := slices.Index(ids, id)
				if k <= last {
					return nil, fmt.Errorf("corpus: the conflict at seq %d names pack %s out of order or not read", c.Seq, id)
				}
				last = k
				packs = append(packs, numbers[k])
			}
			e.Versions = append(e.Versions, packs)
		}
		out = append(out, e)
	}

	return out, nil
}

// Marshal returns reads.json in the canonical form:
// the reads ascending by id, which must be unique kebab-case ids, each filter's arrays in the schema's orders.
// rs itself is not reordered.
//
// Returns:
//   - []byte: the file's bytes.
//   - error: a read that check refuses.
func (rs Reads) Marshal() ([]byte, error) {
	out := slices.Clone(rs)
	slices.SortFunc(out, func(a, b ReadVector) int { return strings.Compare(a.ID, b.ID) })
	for i := range out {
		if i > 0 && out[i].ID == out[i-1].ID {
			return nil, fmt.Errorf("corpus: read %q is listed twice", out[i].ID)
		}
		if err := out[i].check(); err != nil {
			return nil, err
		}
		f, err := out[i].Filter.ToFilter()
		if err != nil {
			return nil, fmt.Errorf("corpus: read %q: %w", out[i].ID, err)
		}
		out[i].Filter = FilterFrom(&f)
	}
	if out == nil {
		out = Reads{}
	}

	return Marshal([]ReadVector(out))
}

// check checks r against the schema of the tracepack corpus specification §5.10.
func (r *ReadVector) check() error {
	if !kebabID.MatchString(r.ID) || len(r.Cites) == 0 {
		return fmt.Errorf("corpus: read %q needs a kebab-case id and at least one cite", r.ID)
	}
	if len(r.Packs) == 0 || slices.ContainsFunc(r.Packs, func(n int) bool { return n < 0 }) || hasDuplicate(r.Packs) {
		return fmt.Errorf("corpus: read %q: packs %v is empty, names a negative pack or names one twice", r.ID, r.Packs)
	}
	if _, _, err := r.Query(); err != nil {
		return err
	}

	return r.checkExpect()
}

// checkExpect checks r's expect: the error form or the success form, its packs among the packs read and in their order.
func (r *ReadVector) checkExpect() error {
	e := &r.Expect
	if e.Conflicts == nil {
		return fmt.Errorf("corpus: read %q: expect has no conflicts", r.ID)
	}
	for _, c := range e.Conflicts {
		if err := checkConflict(&c, r.Packs); err != nil {
			return fmt.Errorf("corpus: read %q: %w", r.ID, err)
		}
	}
	if e.Error != "" {
		switch {
		case e.Error != ErrorConflictLimit || r.MaxConflicts == 0:
			return fmt.Errorf("corpus: read %q: error %q without a conflict bound, or not %s", r.ID, e.Error, ErrorConflictLimit)
		case e.Items != nil || e.Incomplete != nil || e.FooterErrors != nil:
			return fmt.Errorf("corpus: read %q: the error form holds only error and conflicts", r.ID)
		case len(e.Conflicts) != r.MaxConflicts:
			return fmt.Errorf("corpus: read %q: the error form lists %d conflicts, not the bound %d", r.ID, len(e.Conflicts), r.MaxConflicts)
		default:
			return nil
		}
	}
	switch {
	case e.Items == nil || e.Incomplete == nil || e.FooterErrors == nil:
		return fmt.Errorf("corpus: read %q: the success form needs items, incomplete and footer_errors", r.ID)
	case r.MaxConflicts > 0 && len(e.Conflicts) > r.MaxConflicts:
		return fmt.Errorf("corpus: read %q: %d conflicts pass the bound %d", r.ID, len(e.Conflicts), r.MaxConflicts)
	case slices.ContainsFunc(*e.Items, func(it ReadItem) bool { return !slices.Contains(r.Packs, it.Pack) || it.Block < 0 }):
		return fmt.Errorf("corpus: read %q: an item names a pack not read or a negative block", r.ID)
	case !inPackOrder(r.Packs, packsOf(*e.Incomplete), false):
		return fmt.Errorf("corpus: read %q: incomplete names a pack not read or out of the order of packs", r.ID)
	case !inPackOrder(r.Packs, *e.FooterErrors, true):
		return fmt.Errorf("corpus: read %q: footer_errors names a pack not read, twice or out of the order of packs", r.ID)
	default:
		return nil
	}
}

// checkConflict checks that every version of c names packs of order, each once, in the order of order.
func checkConflict(c *ConflictEntry, order []int) error {
	if _, err := parseUUID(c.CaptureID); err != nil {
		return fmt.Errorf("conflict at seq %d: %w", c.Seq, err)
	}
	if len(c.Versions) == 0 {
		return fmt.Errorf("conflict at seq %d lists no version", c.Seq)
	}
	for _, v := range c.Versions {
		if len(v) == 0 || !inPackOrder(order, v, true) {
			return fmt.Errorf("a version of the conflict at seq %d names no pack, a pack not read, or packs out of order", c.Seq)
		}
	}

	return nil
}

// inPackOrder reports whether every number of got is in order and got follows the order of order,
// strictly when strict, so that no number repeats.
func inPackOrder(order, got []int, strict bool) bool {
	last := -1
	for _, n := range got {
		k := slices.Index(order, n)
		if k < 0 || k < last || (strict && k == last) {
			return false
		}
		last = k
	}

	return true
}

// packsOf returns the pack of each reason of in.
func packsOf(in []PackIncomplete) []int {
	out := make([]int, 0, len(in))
	for _, x := range in {
		out = append(out, x.Pack)
	}

	return out
}

package corpus

import (
	"cmp"
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/arloliu/go-secs/tracepack"
)

// systemBytesLen is the length of the System Bytes a filter compares.
const systemBytesLen = 4

// QueryVector is one query vector of queries.json (the tracepack corpus specification §5.4).
type QueryVector struct {
	ID     string   `json:"id"`
	Cites  []string `json:"cites"`
	Filter Filter   `json:"filter"`
	Expect Expect   `json:"expect"`
}

// Queries is queries.json: the query vectors over a vector's pack.
type Queries []QueryVector

// Filter is the selection of a query vector, one key per tracepack.Filter predicate;
// an absent key places no constraint, and an array is never empty.
type Filter struct {
	TimeFrom    *I64     `json:"time_from,omitempty"`
	TimeTo      *I64     `json:"time_to,omitempty"`
	Kinds       []string `json:"kinds,omitempty"`
	Dirs        []string `json:"dirs,omitempty"`
	Epochs      []uint32 `json:"epochs,omitempty"`
	SF          []SF     `json:"sf,omitempty"`
	SessionIDs  []uint16 `json:"session_ids,omitempty"`
	SystemBytes []byte   `json:"system_bytes,omitempty"`
	// IncludeUnavailable is written only when true.
	IncludeUnavailable bool `json:"include_unavailable,omitempty"`
}

// SF is a stream and function pair of a filter.
type SF struct {
	Stream   uint8 `json:"stream"`
	Function uint8 `json:"function"`
}

// Expect is what a query vector yields:
// the seqs of the selected records in yield order, and the read's incomplete reasons.
type Expect struct {
	Seqs       []U64        `json:"seqs"`
	Incomplete []Incomplete `json:"incomplete"`
}

// Incomplete is one reason a query's read is incomplete, by the decision table of the tracepack corpus specification §5.4.
type Incomplete struct {
	Reason   string    `json:"reason"`
	Block    *int      `json:"block,omitempty"`
	Offset   *U64      `json:"offset,omitempty"`
	Coverage *Coverage `json:"coverage,omitempty"`
}

// FilterFrom returns the query-vector form of f: arrays sorted into the schema's orders, empty predicates omitted.
// Kinds and directions are named as Kind.String and Dir.String name them, "unknown(<n>)" for a value without a name.
// Duplicate values are kept, so ToFilter gives back f with its arrays sorted.
func FilterFrom(f *tracepack.Filter) Filter {
	var out Filter
	if f.TimeFrom != nil {
		out.TimeFrom = new(I64(*f.TimeFrom))
	}
	if f.TimeTo != nil {
		out.TimeTo = new(I64(*f.TimeTo))
	}
	for _, k := range slices.Sorted(slices.Values(f.Kinds)) {
		out.Kinds = append(out.Kinds, k.String())
	}
	for _, d := range slices.Sorted(slices.Values(f.Dirs)) {
		out.Dirs = append(out.Dirs, d.String())
	}
	if len(f.Epochs) > 0 {
		out.Epochs = slices.Sorted(slices.Values(f.Epochs))
	}
	for _, sf := range slices.SortedFunc(slices.Values(f.SF), compareSF) {
		out.SF = append(out.SF, SF{Stream: sf.Stream, Function: sf.Function})
	}
	if len(f.SessionIDs) > 0 {
		out.SessionIDs = slices.Sorted(slices.Values(f.SessionIDs))
	}
	if f.SystemBytes != nil {
		out.SystemBytes = append([]byte{}, f.SystemBytes[:]...)
	}
	out.IncludeUnavailable = f.IncludeUnavailable

	return out
}

// ExpectFrom returns the expect of a read with result res
// that yielded the records at seqs, block by block as blocks gives them.
// The seqs are listed blocks in file order, each block's records in the order the read gave them;
// incomplete lists the coverage entries in the order of coverage, the pack metadata's entries,
// then the pack-level truncated, then the block defects ascending by block (the tracepack corpus specification §5.4).
//
// Parameters:
//   - blocks, seqs: the block index and the seq of each record the read yielded, in yield order.
//   - res: the read's result.
//   - coverage: the coverage entries of the pack's metadata, in stored order.
//
// Returns:
//   - Expect: the projection.
//   - error: a reader limit, a removed hour or a conflict, which are never goldens;
//     a defect of a reason or block the decision table does not hold; two pack-level truncations;
//     a coverage defect whose entry the pack metadata does not hold.
func ExpectFrom(blocks []int, seqs []uint64, res *tracepack.Result, coverage []tracepack.Coverage) (Expect, error) {
	if len(blocks) != len(seqs) {
		return Expect{}, fmt.Errorf("corpus: %d blocks for %d seqs", len(blocks), len(seqs))
	}
	if len(res.Removed) > 0 || len(res.Conflicts) > 0 {
		return Expect{}, fmt.Errorf("corpus: a read of one pack reported removed hours or conflicts")
	}

	order := make([]int, len(seqs))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(blocks[a], blocks[b]) })
	out := Expect{Seqs: make([]U64, 0, len(seqs))}
	for _, i := range order {
		out.Seqs = append(out.Seqs, U64(seqs[i]))
	}

	incomplete, err := sortedIncomplete(res.Incomplete, coverage)
	if err != nil {
		return Expect{}, err
	}
	out.Incomplete = incomplete

	return out, nil
}

// RunQuery reads r with the selection f and returns what it yields, as ExpectFrom projects it.
//
// Returns:
//   - Expect: the projection.
//   - error: the error of ToFilter, of the read, or of ExpectFrom.
func RunQuery(ctx context.Context, r *tracepack.Reader, f *Filter) (Expect, error) {
	tf, err := f.ToFilter()
	if err != nil {
		return Expect{}, err
	}

	var (
		blocks []int
		seqs   []uint64
	)
	res, err := r.Iterate(ctx, tracepack.Query{Filter: tf}, func(it *tracepack.Item) error {
		blocks = append(blocks, it.Block)
		seqs = append(seqs, it.Record.Seq)

		return nil
	})
	if err != nil {
		return Expect{}, err
	}

	return ExpectFrom(blocks, seqs, &res, r.Header().Meta.Coverage)
}

// compareSF orders S/F pairs by stream, then function.
func compareSF(a, b tracepack.SF) int {
	return cmp.Or(cmp.Compare(a.Stream, b.Stream), cmp.Compare(a.Function, b.Function))
}

// sortedIncomplete renders the defects ds of a read in the order of the tracepack corpus specification §5.4.
// A coverage defect takes the position of the first entry of coverage that renders alike and is not yet matched,
// so entries that render alike, which are interchangeable, keep their count.
func sortedIncomplete(ds []tracepack.Defect, coverage []tracepack.Coverage) ([]Incomplete, error) {
	type ranked struct {
		group, key int
		in         Incomplete
	}
	rs := make([]ranked, 0, len(ds))
	rendered := make([]Coverage, len(coverage))
	for i := range coverage {
		rendered[i] = CoverageFrom(&coverage[i])
	}
	used := make([]bool, len(coverage))
	truncated := false
	for i := range ds {
		d := &ds[i]
		switch {
		case d.Reason == tracepack.ReasonLimit:
			return nil, fmt.Errorf("corpus: a reader limit was reached: %w", d.Err)
		case d.Reason == tracepack.ReasonCoverage && d.Coverage != nil:
			c := CoverageFrom(d.Coverage)
			k := matchCoverage(rendered, used, &c)
			if k < 0 {
				return nil, fmt.Errorf("corpus: a coverage defect names no unmatched coverage entry of the pack metadata")
			}
			used[k] = true
			rs = append(rs, ranked{group: 0, key: k, in: Incomplete{Reason: d.Reason.String(), Coverage: &c}})
		case d.Reason == tracepack.ReasonTruncated && d.Block < 0:
			if truncated {
				return nil, fmt.Errorf("corpus: the read reported two pack-level truncations")
			}
			truncated = true
			in := Incomplete{Reason: d.Reason.String()}
			if d.Offset >= 0 {
				in.Offset = new(U64(d.Offset))
			}
			rs = append(rs, ranked{group: 1, in: in})
		case d.Block >= 0 && slices.Contains([]tracepack.IncompleteReason{
			tracepack.ReasonCorruptBlock, tracepack.ReasonUnknownCodec, tracepack.ReasonIndexMismatch}, d.Reason):
			off, err := blockOffset(d)
			if err != nil {
				return nil, err
			}
			rs = append(rs, ranked{group: 2, key: d.Block, in: Incomplete{Reason: d.Reason.String(), Block: new(d.Block), Offset: &off}})
		default:
			return nil, fmt.Errorf("corpus: defect of reason %s and block %d is not in the decision table", d.Reason, d.Block)
		}
	}
	slices.SortStableFunc(rs, func(a, b ranked) int { return cmp.Or(cmp.Compare(a.group, b.group), cmp.Compare(a.key, b.key)) })

	out := make([]Incomplete, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.in)
	}

	return out, nil
}

// Marshal returns queries.json in the canonical form:
// the queries ascending by id, which must be unique kebab-case ids, each filter's arrays in the schema's orders.
// qs itself is not reordered.
//
// Returns:
//   - []byte: the file's bytes.
//   - error: an id that is not a unique kebab-case id, a query without cites, or a filter ToFilter refuses.
func (qs Queries) Marshal() ([]byte, error) {
	out := slices.Clone(qs)
	slices.SortFunc(out, func(a, b QueryVector) int { return strings.Compare(a.ID, b.ID) })
	for i := range out {
		if !kebabID.MatchString(out[i].ID) || (i > 0 && out[i].ID == out[i-1].ID) || len(out[i].Cites) == 0 {
			return nil, fmt.Errorf("corpus: query %q needs a unique kebab-case id and at least one cite", out[i].ID)
		}
		f, err := out[i].Filter.ToFilter()
		if err != nil {
			return nil, fmt.Errorf("corpus: query %q: %w", out[i].ID, err)
		}
		out[i].Filter = FilterFrom(&f)
	}
	if out == nil {
		out = Queries{}
	}

	return Marshal([]QueryVector(out))
}

// ToFilter returns the tracepack.Filter f selects with, its arrays in f's order.
//
// Returns:
//   - tracepack.Filter: the filter.
//   - error: an empty array, a kind or direction name no value has, or System Bytes of a length other than 4.
func (f *Filter) ToFilter() (tracepack.Filter, error) {
	var out tracepack.Filter
	if f.TimeFrom != nil {
		out.TimeFrom = new(int64(*f.TimeFrom))
	}
	if f.TimeTo != nil {
		out.TimeTo = new(int64(*f.TimeTo))
	}

	for _, a := range []struct {
		name  string
		empty bool
	}{
		{"kinds", f.Kinds != nil && len(f.Kinds) == 0},
		{"dirs", f.Dirs != nil && len(f.Dirs) == 0},
		{"epochs", f.Epochs != nil && len(f.Epochs) == 0},
		{"sf", f.SF != nil && len(f.SF) == 0},
		{"session_ids", f.SessionIDs != nil && len(f.SessionIDs) == 0},
	} {
		if a.empty {
			return tracepack.Filter{}, fmt.Errorf("corpus: filter %s is an empty array", a.name)
		}
	}

	var err error
	if out.Kinds, err = parseNames[tracepack.Kind]("kind", f.Kinds); err != nil {
		return tracepack.Filter{}, err
	}
	if out.Dirs, err = parseNames[tracepack.Dir]("dir", f.Dirs); err != nil {
		return tracepack.Filter{}, err
	}
	out.Epochs = slices.Clone(f.Epochs)
	for _, sf := range f.SF {
		out.SF = append(out.SF, tracepack.SF{Stream: sf.Stream, Function: sf.Function})
	}
	out.SessionIDs = slices.Clone(f.SessionIDs)
	if f.SystemBytes != nil {
		if len(f.SystemBytes) != systemBytesLen {
			return tracepack.Filter{}, fmt.Errorf("corpus: filter system_bytes holds %d bytes, not %d", len(f.SystemBytes), systemBytesLen)
		}
		out.SystemBytes = new([systemBytesLen]byte(f.SystemBytes))
	}
	out.IncludeUnavailable = f.IncludeUnavailable

	return out, nil
}

// matchCoverage returns the index of the first entry of rendered equal to c and not yet used, or -1.
func matchCoverage(rendered []Coverage, used []bool, c *Coverage) int {
	for k := range rendered {
		if !used[k] && reflect.DeepEqual(&rendered[k], c) {
			return k
		}
	}

	return -1
}

// parseNames returns the enum values of names, each the String of exactly one value of E.
func parseNames[E interface {
	~uint8
	fmt.Stringer
}](what string, names []string) ([]E, error) {
	var out []E
	for _, name := range names {
		v, ok := enumByName[E](name)
		if !ok {
			return nil, fmt.Errorf("corpus: %q names no %s", name, what)
		}
		out = append(out, v)
	}

	return out, nil
}

// enumByName returns the value of E whose String is name.
func enumByName[E interface {
	~uint8
	fmt.Stringer
}](name string) (E, bool) {
	for i := range 256 {
		if v := E(i); v.String() == name {
			return v, true
		}
	}

	return 0, false
}

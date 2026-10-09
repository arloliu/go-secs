package corpus

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/arloliu/go-secs/tracepack"
)

// Truncation is truncation.json (the tracepack corpus specification §5.7).
type Truncation struct {
	Size U64 `json:"size"`
	// RepairOptions is present iff the rows hold repair results.
	RepairOptions *RepairOptions  `json:"repair_options,omitempty"`
	Rows          []TruncationRow `json:"rows"`
}

// TruncationRow is one row of truncation.json: the expectation every cut length from From to To shares.
type TruncationRow struct {
	From U64 `json:"from"`
	To   U64 `json:"to"`
	Cut
}

// Cut is the expectation of one cut of a truncation vector's pack:
// its rejection code, or its outcome, export and verification and, in a table with repair options, its repair.
type Cut struct {
	Rejection       string     `json:"rejection,omitempty"`
	Outcome         string     `json:"outcome,omitempty"`
	ExportedRecords *U64       `json:"exported_records,omitempty"`
	Verify          *CutVerify `json:"verify,omitempty"`
	Repair          *CutRepair `json:"repair,omitempty"`
}

// CutVerify is the verification part of a cut's expectation, as verify.json states it.
type CutVerify struct {
	BlocksValidated int  `json:"blocks_validated"`
	Records         U64  `json:"records"`
	PrefixEnd       U64  `json:"prefix_end"`
	WalkStop        *U64 `json:"walk_stop,omitempty"`
}

// CutRepair is the repair part of a cut's expectation, as repair.json states it.
type CutRepair struct {
	Result string `json:"result"`
	// Blocks and CoverageAdded are present iff Result is patched.
	Blocks        *int        `json:"blocks,omitempty"`
	CoverageAdded *[]Coverage `json:"coverage_added,omitempty"`
}

// TruncationTable builds truncation.json one cut length at a time,
// merging consecutive lengths whose expectations are equal into one row.
type TruncationTable struct {
	size   uint64
	repair *RepairOptions
	rows   []TruncationRow
	// next is the cut length the next Add describes.
	next uint64
	// last is the encoding of the last row's expectation.
	last []byte
}

// NewTruncationTable returns an empty table for a pack of size bytes;
// repair holds the repair options when every cut that is not rejected also states its repair, else nil.
func NewTruncationTable(size uint64, repair *RepairOptions) *TruncationTable {
	return &TruncationTable{size: size, repair: repair}
}

// RejectedCut returns the expectation of a cut rejected at bootstrap with code.
func RejectedCut(code string) Cut {
	return Cut{Rejection: code}
}

// ReadCut returns the expectation of a cut that opens, verified as rep, whose export holds exported records.
//
// Returns:
//   - Cut: the expectation.
//   - error: the error of VerifyFrom.
func ReadCut(rep *tracepack.VerifyReport, exported uint64) (Cut, error) {
	v, err := VerifyFrom(rep)
	if err != nil {
		return Cut{}, err
	}

	return Cut{
		Outcome: v.Outcome, ExportedRecords: new(U64(exported)),
		Verify: &CutVerify{BlocksValidated: v.BlocksValidated, Records: v.Records, PrefixEnd: v.PrefixEnd, WalkStop: v.WalkStop},
	}, nil
}

// WithRepair returns c with the repair part of r, a repair.json of the cut.
func (c Cut) WithRepair(r *Repair) Cut {
	c.Repair = &CutRepair{Result: r.Result, Blocks: r.Blocks, CoverageAdded: r.CoverageAdded}

	return c
}

// Add records the expectation of the next cut length, from 0 upwards, merging it into the last row when equal.
//
// Returns:
//   - error: every cut already given; a cut both rejected and read, or neither;
//     a cut that opens without its repair in a table with repair options, or with one in a table without.
func (t *TruncationTable) Add(c Cut) error {
	if t.next >= t.size {
		return fmt.Errorf("corpus: every cut of the %d-byte pack is already given", t.size)
	}
	rejected := c.Rejection != ""
	read := c.Outcome != "" && c.ExportedRecords != nil && c.Verify != nil
	switch {
	case rejected == read || (rejected && (c.Outcome != "" || c.ExportedRecords != nil || c.Verify != nil || c.Repair != nil)):
		return fmt.Errorf("corpus: cut %d must be either rejected or read", t.next)
	case read && (c.Repair != nil) != (t.repair != nil):
		return fmt.Errorf("corpus: cut %d states its repair iff the table has repair options", t.next)
	}

	key, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if n := len(t.rows); n > 0 && slices.Equal(key, t.last) {
		t.rows[n-1].To = U64(t.next)
	} else {
		t.rows = append(t.rows, TruncationRow{From: U64(t.next), To: U64(t.next), Cut: c})
		t.last = key
	}
	t.next++

	return nil
}

// Table returns truncation.json once every cut length below the size is given.
func (t *TruncationTable) Table() (Truncation, error) {
	if t.next != t.size {
		return Truncation{}, fmt.Errorf("corpus: %d of %d cuts given", t.next, t.size)
	}

	return Truncation{Size: U64(t.size), RepairOptions: t.repair, Rows: append([]TruncationRow{}, t.rows...)}, nil
}

// Marshal returns truncation.json in the canonical form.
func (t *Truncation) Marshal() ([]byte, error) {
	return Marshal(t)
}

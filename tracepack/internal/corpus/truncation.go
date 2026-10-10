package corpus

import (
	"fmt"
	"reflect"

	"github.com/arloliu/go-secs/tracepack"
)

// Truncation is truncation.json (the tracepack corpus specification §5.7).
type Truncation struct {
	Size U64             `json:"size"`
	Rows []TruncationRow `json:"rows"`
}

// TruncationRow is one row of truncation.json: the expectation every cut length from From to To shares,
// its rejection code, or the records its export holds.
type TruncationRow struct {
	From            U64    `json:"from"`
	To              U64    `json:"to"`
	Rejection       string `json:"rejection,omitempty"`
	ExportedRecords *U64   `json:"exported_records,omitempty"`
}

// Cut is the expectation of one cut of a truncation vector's pack:
// its rejection code, or the records its export holds and its verification.
// truncation.json states the code or the records; the verification is checked in memory only.
type Cut struct {
	Rejection       string
	ExportedRecords *U64
	Verify          *CutVerify
}

// CutVerify is the verification of a cut that opens: its outcome, the blocks and records validated,
// where its validated prefix ends, and where its walk stopped, nil when it did not.
type CutVerify struct {
	Outcome         string
	BlocksValidated int
	Records         U64
	PrefixEnd       U64
	WalkStop        *U64
}

// CutRow is the expectation every cut length from From to To shares, verification included.
type CutRow struct {
	From, To U64
	Cut
}

// TruncationTable builds the expectations of every cut of a pack one cut length at a time,
// merging consecutive lengths whose expectations are equal into one row.
type TruncationTable struct {
	size uint64
	rows []CutRow
	// next is the cut length the next Add describes.
	next uint64
}

// NewTruncationTable returns an empty table for a pack of size bytes.
func NewTruncationTable(size uint64) *TruncationTable {
	return &TruncationTable{size: size}
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
		ExportedRecords: new(U64(exported)),
		Verify: &CutVerify{
			Outcome: v.Outcome, BlocksValidated: v.BlocksValidated, Records: v.Records, PrefixEnd: v.PrefixEnd, WalkStop: v.WalkStop,
		},
	}, nil
}

// Add records the expectation of the next cut length, from 0 upwards, merging it into the last row when equal.
//
// Returns:
//   - error: every cut already given; a cut both rejected and read, or neither.
func (t *TruncationTable) Add(c Cut) error {
	if t.next >= t.size {
		return fmt.Errorf("corpus: every cut of the %d-byte pack is already given", t.size)
	}
	rejected := c.Rejection != ""
	read := c.ExportedRecords != nil && c.Verify != nil
	if rejected == read || (rejected && (c.ExportedRecords != nil || c.Verify != nil)) {
		return fmt.Errorf("corpus: cut %d must be either rejected or read", t.next)
	}

	if n := len(t.rows); n > 0 && reflect.DeepEqual(t.rows[n-1].Cut, c) {
		t.rows[n-1].To = U64(t.next)
	} else {
		t.rows = append(t.rows, CutRow{From: U64(t.next), To: U64(t.next), Cut: c})
	}
	t.next++

	return nil
}

// Cuts returns the rows of the expectations of every cut given so far, verification included.
func (t *TruncationTable) Cuts() []CutRow {
	return append([]CutRow{}, t.rows...)
}

// Table returns truncation.json once every cut length below the size is given:
// the rows of Cuts without their verification, consecutive rows that are then equal merged into one.
func (t *TruncationTable) Table() (Truncation, error) {
	if t.next != t.size {
		return Truncation{}, fmt.Errorf("corpus: %d of %d cuts given", t.next, t.size)
	}

	out := Truncation{Size: U64(t.size), Rows: []TruncationRow{}}
	for _, r := range t.rows {
		row := TruncationRow{From: r.From, To: r.To, Rejection: r.Rejection, ExportedRecords: r.ExportedRecords}
		if n := len(out.Rows); n > 0 && sameCutResult(&out.Rows[n-1], &row) {
			out.Rows[n-1].To = row.To

			continue
		}
		out.Rows = append(out.Rows, row)
	}

	return out, nil
}

// sameCutResult reports whether the rows a and b state the same rejection code and records exported.
func sameCutResult(a, b *TruncationRow) bool {
	return a.Rejection == b.Rejection && reflect.DeepEqual(a.ExportedRecords, b.ExportedRecords)
}

// Marshal returns truncation.json in the canonical form.
func (t *Truncation) Marshal() ([]byte, error) {
	return Marshal(t)
}

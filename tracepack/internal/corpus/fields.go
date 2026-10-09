package corpus

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/arloliu/go-secs/tracepack"
)

// FieldRows is fields.json: one row per data or control record.
type FieldRows []Fields

// Fields is one record of fields.json: the HSMS header fields of a data or control record's payload
// (the tracepack corpus specification §5.5).
// Each field is present iff available, and read at its position for data and control records alike.
type Fields struct {
	Seq U64 `json:"seq"`
	// Available holds the field_validity names of the available fields, ascending by bit.
	Available   []string `json:"available"`
	SessionID   *uint16  `json:"session_id,omitempty"`
	Stream      *uint8   `json:"stream,omitempty"`
	W           *bool    `json:"w,omitempty"`
	Function    *uint8   `json:"function,omitempty"`
	PType       *uint8   `json:"ptype,omitempty"`
	SType       *uint8   `json:"stype,omitempty"`
	SystemBytes []byte   `json:"system_bytes,omitempty"`
}

// FieldsOf returns the fields.json row of rec, which must carry its payload.
//
// Returns:
//   - Fields: the row.
//   - bool: false for a record that is neither data nor control, which fields.json does not list.
func FieldsOf(rec *tracepack.Record) (Fields, bool) {
	if rec.Kind != tracepack.KindData && rec.Kind != tracepack.KindControl {
		return Fields{}, false
	}

	h := rec.HSMSHeader()
	out := Fields{Seq: U64(rec.Seq), Available: append([]string{}, h.Available.Names()...)}
	if h.Available.Has(tracepack.FieldValiditySessionID) {
		out.SessionID = new(h.SessionID)
	}
	if h.Available.Has(tracepack.FieldValidityStreamAndW) {
		out.Stream, out.W = new(h.Stream), new(h.W)
	}
	if h.Available.Has(tracepack.FieldValidityFunction) {
		out.Function = new(h.Function)
	}
	if h.Available.Has(tracepack.FieldValidityPType) {
		out.PType = new(h.PType)
	}
	if h.Available.Has(tracepack.FieldValiditySType) {
		out.SType = new(h.SType)
	}
	if h.Available.Has(tracepack.FieldValiditySystemBytes) {
		out.SystemBytes = append([]byte{}, h.SystemBytes[:]...)
	}

	return out, true
}

// ReadFields reads every record of r with its payload
// and returns the fields.json rows of its data and control records, ascending by seq.
//
// Returns:
//   - FieldRows: the rows; empty, not nil, when there is none.
//   - error: the error of the read.
func ReadFields(ctx context.Context, r *tracepack.Reader) (FieldRows, error) {
	rows := FieldRows{}
	_, err := r.Iterate(ctx, tracepack.Query{Payloads: true}, func(it *tracepack.Item) error {
		if f, ok := FieldsOf(&it.Record); ok {
			rows = append(rows, f)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(rows, func(a, b Fields) int { return cmp.Compare(a.Seq, b.Seq) })

	return rows, nil
}

// Marshal returns fields.json in the canonical form, the rows ascending by seq; rows itself is not reordered.
//
// Returns:
//   - []byte: the file's bytes.
//   - error: a seq listed twice.
func (rows FieldRows) Marshal() ([]byte, error) {
	out := append([]Fields{}, rows...)
	slices.SortStableFunc(out, func(a, b Fields) int { return cmp.Compare(a.Seq, b.Seq) })
	for i := 1; i < len(out); i++ {
		if out[i].Seq == out[i-1].Seq {
			return nil, fmt.Errorf("corpus: seq %d is listed twice", out[i].Seq)
		}
	}

	return Marshal(out)
}

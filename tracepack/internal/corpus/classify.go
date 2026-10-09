package corpus

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/arloliu/go-secs/tracepack"
)

// bytePredicateStatuses holds the decode_status values that are byte predicates
// of the tracepack semantics specification §3, short-frame to ok:
// the statuses classify.json lists.
var bytePredicateStatuses = []tracepack.DecodeStatus{
	tracepack.DecodeStatusOK, tracepack.DecodeStatusOKWithTrailing, tracepack.DecodeStatusShortFrame,
	tracepack.DecodeStatusLengthMismatch, tracepack.DecodeStatusBadPType, tracepack.DecodeStatusBadSType,
	tracepack.DecodeStatusControlWithBody, tracepack.DecodeStatusOversized, tracepack.DecodeStatusItemDecodeError,
}

// Classify is classify.json (the tracepack corpus specification §5.9).
type Classify struct {
	MaxFrameLen U64             `json:"max_frame_len"`
	Frames      []ClassifyFrame `json:"frames"`
}

// ClassifyFrame is the expected classification of one frame:
// the record's seq, the id of the case it exercises, its decode_status name and its trailing_bytes.
type ClassifyFrame struct {
	Seq           U64    `json:"seq"`
	Rule          string `json:"rule"`
	DecodeStatus  string `json:"decode_status"`
	TrailingBytes uint32 `json:"trailing_bytes"`
}

// NewClassifyFrame returns the classify.json row of the record at seq, which exercises the case rule
// and is expected to classify as status with trailing excess bytes.
//
// Returns:
//   - ClassifyFrame: the row.
//   - error: a status that is not a byte predicate of the tracepack semantics specification §3,
//     or trailing bytes on a status other than ok-with-trailing, for which the field has no meaning.
func NewClassifyFrame(seq uint64, rule string, status tracepack.DecodeStatus, trailing uint32) (ClassifyFrame, error) {
	if !slices.Contains(bytePredicateStatuses, status) {
		return ClassifyFrame{}, fmt.Errorf("corpus: %s is not a byte-predicate status", status)
	}
	if trailing != 0 && status != tracepack.DecodeStatusOKWithTrailing {
		return ClassifyFrame{}, fmt.Errorf("corpus: %d trailing bytes on a %s frame", trailing, status)
	}
	if rule == "" {
		return ClassifyFrame{}, fmt.Errorf("corpus: the frame of seq %d names no case", seq)
	}

	return ClassifyFrame{Seq: U64(seq), Rule: rule, DecodeStatus: status.String(), TrailingBytes: trailing}, nil
}

// Marshal returns classify.json in the canonical form, frames ascending by seq; c itself is not reordered.
//
// Returns:
//   - []byte: the file's bytes.
//   - error: a seq listed twice.
func (c *Classify) Marshal() ([]byte, error) {
	out := Classify{MaxFrameLen: c.MaxFrameLen, Frames: append([]ClassifyFrame{}, c.Frames...)}
	slices.SortStableFunc(out.Frames, func(a, b ClassifyFrame) int { return cmp.Compare(a.Seq, b.Seq) })
	for i := 1; i < len(out.Frames); i++ {
		if out.Frames[i].Seq == out.Frames[i-1].Seq {
			return nil, fmt.Errorf("corpus: seq %d is listed twice", out.Frames[i].Seq)
		}
	}

	return Marshal(&out)
}

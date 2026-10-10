package corpus

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/arloliu/go-secs/tracepack"
)

// bytePredicateStatuses holds the decode_status values that are byte predicates
// of the tracepack semantics specification §3, short-frame to ok:
// the statuses a classifier expectation lists.
var bytePredicateStatuses = []tracepack.DecodeStatus{
	tracepack.DecodeStatusOK, tracepack.DecodeStatusOKWithTrailing, tracepack.DecodeStatusShortFrame,
	tracepack.DecodeStatusLengthMismatch, tracepack.DecodeStatusBadPType, tracepack.DecodeStatusBadSType,
	tracepack.DecodeStatusControlWithBody, tracepack.DecodeStatusOversized, tracepack.DecodeStatusItemDecodeError,
}

// Classify holds the classifier expectations of a vector's pack,
// written by hand from the tracepack semantics specification §3
// and the checklist of the tracepack corpus specification §7.
// They are tests of the classifier, not files of the corpus.
type Classify struct {
	// MaxFrameLen is the pack's single max_frame_len, under which every frame is classified.
	MaxFrameLen uint64
	// Frames are the expected classifications of the frames of the pack's cases,
	// one per data or control record that is a case.
	Frames []ClassifyFrame
	// FormatOnly are the seqs of the pack's other data and control records,
	// such as one that stores not-attempted, whose stored status only the export pins.
	FormatOnly []uint64
}

// ClassifyFrame is the expected classification of one frame:
// the record's seq, the id of the case it exercises, its decode_status and its trailing_bytes.
type ClassifyFrame struct {
	Seq           uint64
	Rule          string
	DecodeStatus  tracepack.DecodeStatus
	TrailingBytes uint32
}

// ClassifierVector is the classifier expectations of the vector of id.
type ClassifierVector struct {
	ID       string
	Classify Classify
}

// NewClassifyFrame returns the expected classification of the record at seq, which exercises the case rule
// and is expected to classify as status with trailing excess bytes.
//
// Returns:
//   - ClassifyFrame: the expectation.
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

	return ClassifyFrame{Seq: seq, Rule: rule, DecodeStatus: status, TrailingBytes: trailing}, nil
}

// ClassifierVectors returns the classifier expectations of every recipe that has them, ascending by vector id,
// each a copy whose frames and format-only seqs ascend by seq.
// It builds no pack: the expectations are the recipes' own code, never read from a pack or from the classifier.
//
// Returns:
//   - []ClassifierVector: the expectations.
//   - error: two recipes of one id, or a seq listed twice in one vector, as a frame or as format-only.
func ClassifierVectors() ([]ClassifierVector, error) {
	recipes, err := Recipes()
	if err != nil {
		return nil, err
	}

	return expectationsOf(recipes)
}

// expectationsOf returns the classifier expectations of the recipes that have them, in the recipes' order,
// as ClassifierVectors states them.
//
// Returns:
//   - []ClassifierVector: the expectations.
//   - error: a seq listed twice in one vector, as a frame or as format-only.
func expectationsOf(recipes []Recipe) ([]ClassifierVector, error) {
	var out []ClassifierVector
	for i := range recipes {
		c := recipes[i].Classify
		if c == nil {
			continue
		}
		v := ClassifierVector{ID: recipes[i].ID, Classify: Classify{
			MaxFrameLen: c.MaxFrameLen, Frames: slices.Clone(c.Frames), FormatOnly: slices.Clone(c.FormatOnly),
		}}
		slices.SortStableFunc(v.Classify.Frames, func(a, b ClassifyFrame) int { return cmp.Compare(a.Seq, b.Seq) })
		slices.Sort(v.Classify.FormatOnly)
		seqs := slices.Clone(v.Classify.FormatOnly)
		for _, f := range v.Classify.Frames {
			seqs = append(seqs, f.Seq)
		}
		if hasDuplicate(seqs) {
			return nil, fmt.Errorf("corpus: vector %s lists a seq twice", v.ID)
		}
		out = append(out, v)
	}

	return out, nil
}

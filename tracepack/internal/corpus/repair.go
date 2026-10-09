package corpus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/arloliu/go-secs/tracepack"
)

// Repair results (the tracepack corpus specification §5.6).
const (
	RepairPatched       = "patched"
	RepairNotNeeded     = "not-needed"
	RepairNotRepairable = "not-repairable"
)

// RepairOptions is the options object of repair.json and of a repair truncation table.
type RepairOptions struct {
	Writer string `json:"writer"`
	PackID string `json:"pack_id"`
	// PatchBase is present when the scope has a generation.
	PatchBase *string `json:"patch_base,omitempty"`
	// Codec is the patch's footer codec, always "none".
	Codec            string `json:"codec"`
	WriterStartUTCNs I64    `json:"writer_start_utc_ns"`
}

// Repair is repair.json (the tracepack corpus specification §5.6).
type Repair struct {
	Options RepairOptions `json:"options"`
	Result  string        `json:"result"`
	// Blocks, Records and CoverageAdded are present iff Result is patched.
	Blocks        *int        `json:"blocks,omitempty"`
	Records       *U64        `json:"records,omitempty"`
	CoverageAdded *[]Coverage `json:"coverage_added,omitempty"`
}

// RepairOptionsFrom returns the options object of opts, whose Now gives the time the patch stamps.
//
// Returns:
//   - RepairOptions: the options.
//   - error: a nil Now, a zero PackID, or a footer codec other than none.
func RepairOptionsFrom(opts *tracepack.RepairOptions) (RepairOptions, error) {
	if opts.Now == nil || opts.PackID.IsZero() || opts.Codec != tracepack.CodecNone {
		return RepairOptions{}, fmt.Errorf("corpus: repair options need a clock, a pack_id and codec none")
	}

	out := RepairOptions{
		Writer: opts.Writer, PackID: opts.PackID.String(), Codec: tracepack.CodecNone.String(),
		WriterStartUTCNs: I64(opts.Now().UnixNano()),
	}
	if opts.PatchBase != nil {
		out.PatchBase = new(opts.PatchBase.String())
	}

	return out, nil
}

// RepairFrom returns the repair.json of a repair under opts that returned rep and err.
//
// Returns:
//   - Repair: the projection, its coverage_added ascending by seq_first.
//   - error: err when it is neither nil nor a refusal; a new coverage entry without seq_first.
func RepairFrom(opts RepairOptions, rep *tracepack.RepairReport, err error) (Repair, error) {
	out := Repair{Options: opts}
	switch {
	case errors.Is(err, tracepack.ErrRepairNotNeeded):
		out.Result = RepairNotNeeded
	case errors.Is(err, tracepack.ErrNotRepairable):
		out.Result = RepairNotRepairable
	case err != nil:
		return Repair{}, err
	default:
		added, err := sortedCoverage(rep.Coverage)
		if err != nil {
			return Repair{}, err
		}
		out.Result = RepairPatched
		out.Blocks, out.Records, out.CoverageAdded = new(rep.Blocks), new(U64(rep.Records)), &added
	}

	return out, nil
}

// RunRepair repairs pack under opts and returns its repair.json and the patch, nil when none is written.
//
// Returns:
//   - Repair: the projection.
//   - []byte: the patch; nil unless the result is patched.
//   - error: the error of ToOptions, of the repair, or of RepairFrom.
func RunRepair(ctx context.Context, pack []byte, opts RepairOptions) (Repair, []byte, error) {
	o, err := opts.ToOptions()
	if err != nil {
		return Repair{}, nil, err
	}

	var patch bytes.Buffer
	rep, rerr := tracepack.Repair(ctx, bytes.NewReader(pack), int64(len(pack)), &patch, o)
	out, err := RepairFrom(opts, &rep, rerr)
	if err != nil {
		return Repair{}, nil, err
	}
	if out.Result != RepairPatched {
		return out, nil, nil
	}

	return out, patch.Bytes(), nil
}

// ToOptions returns the tracepack.RepairOptions o states, its Now returning writer_start_utc_ns.
//
// Returns:
//   - tracepack.RepairOptions: the options.
//   - error: a pack_id or patch_base that is not a canonical UUID, or a codec other than none.
func (o *RepairOptions) ToOptions() (tracepack.RepairOptions, error) {
	if o.Codec != tracepack.CodecNone.String() {
		return tracepack.RepairOptions{}, fmt.Errorf("corpus: repair codec %q is not none", o.Codec)
	}
	id, err := parseUUID(o.PackID)
	if err != nil {
		return tracepack.RepairOptions{}, err
	}

	at := time.Unix(0, int64(o.WriterStartUTCNs)).UTC()
	out := tracepack.RepairOptions{Writer: o.Writer, PackID: id, Codec: tracepack.CodecNone, Now: func() time.Time { return at }}
	if o.PatchBase != nil {
		base, err := parseUUID(*o.PatchBase)
		if err != nil {
			return tracepack.RepairOptions{}, err
		}
		out.PatchBase = &base
	}

	return out, nil
}

// Marshal returns repair.json in the canonical form.
func (r *Repair) Marshal() ([]byte, error) {
	return Marshal(r)
}

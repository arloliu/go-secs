package corpus

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
)

var testPatchID = tracepack.UUID{0x01, 0x9a, 0x2b, 0x3c, 0x4d, 0x5e, 0x70, 0x04, 0x80, 0, 0, 0, 0, 0, 0, 0x04}

// testRepairOptions returns the repair options object of a patch stamped at testHour.
func testRepairOptions(t *testing.T, base *tracepack.UUID) RepairOptions {
	t.Helper()

	o, err := RepairOptionsFrom(&tracepack.RepairOptions{
		Writer: "corpus-repair", PackID: testPatchID, PatchBase: base,
		Now: func() time.Time { return time.Unix(0, testHour).UTC() },
	})
	require.NoError(t, err)

	return o
}

func TestRepairOptionsRoundTrip(t *testing.T) {
	t.Parallel()

	base := testCaptureID
	o := testRepairOptions(t, &base)
	require.Equal(t, `{
  "writer": "corpus-repair",
  "pack_id": "019a2b3c-4d5e-7004-8000-000000000004",
  "patch_base": "019a2b3c-4d5e-7002-8000-000000000002",
  "codec": "none",
  "writer_start_utc_ns": "1791000000000000000"
}
`, marshalJSON(t, &o))

	opts, err := o.ToOptions()
	require.NoError(t, err)
	require.Equal(t, "corpus-repair", opts.Writer)
	require.Equal(t, testPatchID, opts.PackID)
	require.Equal(t, &base, opts.PatchBase)
	require.Equal(t, tracepack.CodecNone, opts.Codec)
	require.Equal(t, testHour, opts.Now().UnixNano())

	back, err := RepairOptionsFrom(&opts)
	require.NoError(t, err)
	require.Equal(t, o, back)

	noBase := testRepairOptions(t, nil)
	require.NotContains(t, marshalJSON(t, &noBase), "patch_base")

	for _, bad := range []RepairOptions{
		{Writer: "w", PackID: testPatchID.String(), Codec: "zstd"},
		{Writer: "w", PackID: "019A2B3C-4D5E-7004-8000-000000000004", Codec: "none"},
		{Writer: "w", PackID: testPatchID.String(), PatchBase: new("nope"), Codec: "none"},
	} {
		_, err := bad.ToOptions()
		require.Error(t, err)
	}
	for _, bad := range []tracepack.RepairOptions{
		{Writer: "w", PackID: testPatchID},
		{Writer: "w", Now: time.Now},
		{Writer: "w", PackID: testPatchID, Now: time.Now, Codec: tracepack.CodecZstd},
	} {
		_, err := RepairOptionsFrom(&bad)
		require.Error(t, err)
	}
}

func TestRepairFromConstructedResults(t *testing.T) {
	t.Parallel()

	o := testRepairOptions(t, nil)
	opts := `  "options": {
    "writer": "corpus-repair",
    "pack_id": "019a2b3c-4d5e-7004-8000-000000000004",
    "codec": "none",
    "writer_start_utc_ns": "1791000000000000000"
  },
`
	for _, tt := range []struct {
		err  error
		want string
	}{
		{tracepack.ErrRepairNotNeeded, "not-needed"},
		{fmt.Errorf("x: %w", tracepack.ErrNotRepairable), "not-repairable"},
	} {
		r, err := RepairFrom(o, &tracepack.RepairReport{Blocks: 3}, tt.err)
		require.NoError(t, err)
		require.Equal(t, "{\n"+opts+"  \"result\": \""+tt.want+"\"\n}\n", marshalJSON(t, &r))
	}

	r, err := RepairFrom(o, &tracepack.RepairReport{Blocks: 0, Records: 0}, nil)
	require.NoError(t, err)
	require.Equal(t, "{\n"+opts+`  "result": "patched",
  "blocks": 0,
  "records": "0",
  "coverage_added": []
}
`, marshalJSON(t, &r))

	r, err = RepairFrom(o, &tracepack.RepairReport{Blocks: 1, Records: 2, Coverage: []tracepack.Coverage{
		{SeqFirst: new(uint64(9))}, {SeqFirst: new(uint64(4)), SeqLast: new(uint64(5))},
	}}, nil)
	require.NoError(t, err)
	require.Equal(t, "{\n"+opts+`  "result": "patched",
  "blocks": 1,
  "records": "2",
  "coverage_added": [
    {
      "seq_first": "4",
      "seq_last": "5"
    },
    {
      "seq_first": "9"
    }
  ]
}
`, marshalJSON(t, &r))

	_, err = RepairFrom(o, &tracepack.RepairReport{}, errors.New("disk full"))
	require.Error(t, err)
	_, err = RepairFrom(o, &tracepack.RepairReport{Coverage: []tracepack.Coverage{{}}}, nil)
	require.Error(t, err, "a new entry without seq_first")
}

func TestRunRepair(t *testing.T) {
	t.Parallel()

	pack, blocks := gappedPack(t)
	o := testRepairOptions(t, nil)

	r, patch, err := RunRepair(t.Context(), pack, o)
	require.NoError(t, err)
	require.Nil(t, patch)
	require.Equal(t, RepairNotNeeded, r.Result)

	off1 := metaEnd(pack) + blockLen(blocks[0])
	damaged := flipByte(pack, int(off1+blockLen(blocks[1])-1))
	r, patch, err = RunRepair(t.Context(), damaged, o)
	require.NoError(t, err)
	require.NotEmpty(t, patch)
	rep := verifyPack(t, patch)
	require.Equal(t, tracepack.OutcomeFinalizedConsistent, rep.Outcome)
	require.True(t, bytes.Contains(patch, damaged[metaEnd(pack):off1]), "the first block is copied byte for byte")

	ts := func(seq int64) string { return strconv.FormatInt(testHour+seq*int64(time.Microsecond), 10) }
	added := func(first, last string) string {
		return `    {
      "capture_id": "019a2b3c-4d5e-7002-8000-000000000002",
      "seq_first": "` + first + `",
      "seq_last": "` + last + `",
      "time_start": "` + ts(3) + `",
      "time_end": "` + ts(10) + `"
    }`
	}
	require.Equal(t, `{
  "options": {
    "writer": "corpus-repair",
    "pack_id": "019a2b3c-4d5e-7004-8000-000000000004",
    "codec": "none",
    "writer_start_utc_ns": "1791000000000000000"
  },
  "result": "patched",
  "blocks": 2,
  "records": "5",
  "coverage_added": [
`+added("3", "4")+`,
`+added("7", "8")+`,
`+added("10", "10")+`
  ]
}
`, marshalJSON(t, &r))
}

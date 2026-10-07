package tracepack_test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/classify"
)

// ExampleSegmentWriter records a capture into a local directory laid out as a bucket.
// A classifier with a 32-byte ceiling records the 36-byte reply as oversized instead of decoding it.
func ExampleSegmentWriter() {
	ctx := context.Background()
	root, err := os.MkdirTemp("", "tracepack-example-")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer os.RemoveAll(root)

	sink, err := tracepack.NewDirSink(root, "")
	if err != nil {
		fmt.Println(err)
		return
	}

	// What the recorder persists across restarts:
	// its deployment's identity, generated once and never per capture,
	// and the CaptureID of its last capture, nil before the first.
	var state struct {
		instance tracepack.UUID
		previous *tracepack.UUID
	}
	state.instance = tracepack.UUID{0x01, 0x9a, 0x2b, 0x3c, 0x4d, 0x5e, 0x70, 0x00, 0x80, 0x00, 0, 0, 0, 0, 0, 1}

	// A live recorder takes its times from time.Now() or the go-secs observer events,
	// which carry the monotonic reading mono_ns is measured from;
	// fixed times keep this example's output stable.
	origin := time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC)
	w, err := tracepack.NewSegmentWriter(ctx, tracepack.SegmentWriterOptions{
		Capture: tracepack.CaptureDescriptor{
			ToolID:             "EQP-01",
			Transport:          tracepack.TransportHSMSSS,
			CaptureMethod:      tracepack.CaptureMethodRawStream,
			Vantage:            tracepack.VantageHost,
			Recorder:           "example-recorder/1.0",
			RecorderInstanceID: state.instance,
			PreviousCaptureID:  state.previous,
			TimeSource:         tracepack.TimeSourceCaptureClock,
			CaptureOrigin:      origin,
			LifecycleCoverage:  tracepack.LifecycleCoverageSubscribed,
			QualityEvaluated:   true,
		},
		Sink:       sink,
		Classifier: classify.New(32),
		Now:        func() time.Time { return origin.Add(10 * time.Minute) },
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	// Persist the id at once, so the next capture names this one even if the recorder crashes.
	state.previous = new(w.CaptureID())

	// A SegmentWriter is not safe for concurrent use: a recorder serializes its observer callbacks into it.
	// Epoch 1 is the first connection the recorder saw; 0 would mean the connection is unknown.
	body := []byte{0x01, 0x02, 0x41, 0x0b, 'M', 'O', 'D', 'E', 'L', '-', 'X', '-', '2', '0', '0', 0x41, 0x05, '4', '.', '2', '.', '0'}
	if err := w.AppendFrame(ctx, origin.Add(time.Second), tracepack.DirHostToEquipment, 1, s1Frame(1, true, 1, nil)); err != nil {
		fmt.Println(err)
		return
	}
	if err := w.AppendFrame(ctx, origin.Add(2*time.Second), tracepack.DirEquipmentToHost, 1, s1Frame(2, false, 1, body)); err != nil {
		fmt.Println(err)
		return
	}

	// Tick commits a segment whose period has ended, even while the connection is idle.
	if err := w.Tick(ctx, origin.Add(5*time.Minute)); err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println("segments after Tick:", len(segmentPaths(root)))

	if err := w.Close(ctx); err != nil {
		// Segments committed before the failure stay published, even ones this Close committed:
		// a stop in a new period commits the open segment first,
		// and with no seq left for the stop, Close commits the open segment without it and returns ErrSeqOrder.
		if errors.Is(err, tracepack.ErrPublishUncertain) {
			// The segment of the failed commit is visible under its key, but may not survive a system crash.
			fmt.Println("published, durability uncertain:", err)
		} else {
			// The segment the failure aborted, if any, is not visible: its records are lost, a seq gap.
			fmt.Println("failed:", err)
		}

		return
	}
	fmt.Println("segments after Close:", len(segmentPaths(root)))
	fmt.Println("previous capture persisted:", *state.previous == w.CaptureID())

	if err := printRecords(ctx, root); err != nil {
		fmt.Println(err)
	}
	// Output:
	// segments after Tick: 1
	// segments after Close: 2
	// previous capture persisted: true
	// seq 0 transport-event not-applicable
	// seq 1 data ok
	// seq 2 data oversized
	// seq 3 transport-event not-applicable
}

// ExampleOpen opens a committed segment and reads its records.
func ExampleOpen() {
	ctx := context.Background()
	root, err := os.MkdirTemp("", "tracepack-example-")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer os.RemoveAll(root)
	if _, err := recordExampleCapture(ctx, root); err != nil {
		fmt.Println(err)
		return
	}

	f, err := os.Open(segmentPaths(root)[0])
	if err != nil {
		fmt.Println(err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		fmt.Println(err)
		return
	}
	// The Reader reads through f, which stays open until the last read.
	r, err := tracepack.Open(ctx, f, st.Size(), tracepack.ReaderOptions{})
	if err != nil {
		fmt.Println(err)
		return
	}
	h := r.Header()
	fmt.Println(h.Meta.ToolID, h.Meta.PackRole, "finalized:", h.Finalized)

	res, err := r.Iterate(ctx, tracepack.Query{Payloads: true}, func(it *tracepack.Item) error {
		rec := &it.Record
		if rec.Kind != tracepack.KindData {
			fmt.Println(rec.Seq, rec.Kind)
			return nil
		}
		hdr := rec.HSMSHeader()
		fmt.Printf("%d %s S%dF%d W=%t\n", rec.Seq, rec.Dir, hdr.Stream, hdr.Function, hdr.W)

		return nil
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println("complete:", res.Complete())
	// Output:
	// EQP-01 segment finalized: true
	// 0 transport-event
	// 1 host-to-equipment S1F1 W=true
	// complete: true
}

// ExampleFindTransaction finds the reply to a primary message across an hour boundary,
// over the segments of a local recording.
func ExampleFindTransaction() {
	ctx := context.Background()
	root, err := os.MkdirTemp("", "tracepack-example-")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer os.RemoveAll(root)
	capture, err := recordExampleCapture(ctx, root)
	if err != nil {
		fmt.Println(err)
		return
	}

	// Open every committed segment, skipping the sink's temporary files under .partial/.
	// Each Reader reads through its file, which stays open until the lookup is done.
	var readers []*tracepack.Reader
	var files []*os.File
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".partial" {
				return filepath.SkipDir
			}

			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		files = append(files, f)
		st, err := f.Stat()
		if err != nil {
			return err
		}
		r, err := tracepack.Open(ctx, f, st.Size(), tracepack.ReaderOptions{})
		if err != nil {
			return err
		}
		readers = append(readers, r)

		return nil
	})
	if err != nil {
		fmt.Println(err)
		return
	}

	// Find the primary, the S1F1 W, and build its key.
	var key tracepack.TxKey
	for _, r := range readers {
		_, err := r.Iterate(ctx, tracepack.Query{Payloads: true}, func(it *tracepack.Item) error {
			hdr := it.Record.HSMSHeader()
			if it.Record.Kind == tracepack.KindData && hdr.Stream == 1 && hdr.Function == 1 {
				key = tracepack.TxKeyOf(capture, it.Record.Seq, time.Unix(0, it.Record.TSUTCNs))
			}

			return nil
		})
		if err != nil {
			fmt.Println(err)
			return
		}
	}

	// These readers are every pack of the tool, none removed and none rejected,
	// so the source may claim Complete; without it, no lookup is ever TxUnmatched.
	src, err := tracepack.NewReaderSource(readers, tracepack.ReaderSourceOptions{Complete: true})
	if err != nil {
		fmt.Println(err)
		return
	}
	res, err := tracepack.FindTransaction(ctx, src, key, tracepack.TxOptions{})
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(res.Outcome, "gaps:", len(res.Gaps))
	for _, v := range res.Records {
		if v.Valid {
			hdr := v.Record.HSMSHeader()
			fmt.Printf("reply S%dF%d, %d hour after the primary\n", hdr.Stream, hdr.Function, v.Hour-key.Hour)
		}
	}
	// Output:
	// matched gaps: 0
	// reply S1F2, 1 hour after the primary
}

// ExampleMergeIterate reads the segments of a recording as one read, in time order.
func ExampleMergeIterate() {
	ctx := context.Background()
	root, err := os.MkdirTemp("", "tracepack-example-")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer os.RemoveAll(root)
	if _, err := recordExampleCapture(ctx, root); err != nil {
		fmt.Println(err)
		return
	}

	readers, closeAll, err := openSegments(ctx, root)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer closeAll()

	opts := tracepack.MergeIterateOptions{Order: tracepack.OrderTime}
	res, err := tracepack.MergeIterate(ctx, readers, tracepack.Query{}, opts, func(it *tracepack.Item) error {
		rec := &it.Record
		at := time.Unix(0, rec.TSUTCNs).UTC().Format(time.TimeOnly)
		fmt.Println(at, "seq", rec.Seq, rec.Kind, "from segment", it.Pack)

		return nil
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println("complete:", res.Complete(), "conflicts:", len(res.Conflicts))
	// Output:
	// 08:59:00 seq 0 transport-event from segment 0
	// 08:59:50 seq 1 data from segment 0
	// 09:00:05 seq 2 data from segment 1
	// 09:00:10 seq 3 transport-event from segment 1
	// complete: true conflicts: 0
}

// s1Frame returns a stream 1 HSMS data message frame, length prefix included:
// SessionID 0, the function and W bit given, PType and SType 0, the System Bytes system, and body.
func s1Frame(function byte, wbit bool, system uint32, body []byte) []byte {
	f := make([]byte, 14, 14+len(body))
	binary.BigEndian.PutUint32(f[0:4], uint32(10+len(body)))
	f[6] = 1
	if wbit {
		f[6] |= 0x80
	}
	f[7] = function
	binary.BigEndian.PutUint32(f[10:14], system)

	return append(f, body...)
}

// recordExampleCapture records one capture of tool EQP-01 into a directory sink at root:
// it starts at 08:59:00 UTC, holds an S1F1 W primary at 08:59:50 and its S1F2 reply at 09:00:05, both on epoch 1,
// and stops at 09:00:10, so it spans two segments, one in each hour.
// It returns the capture's id.
func recordExampleCapture(ctx context.Context, root string) (tracepack.UUID, error) {
	sink, err := tracepack.NewDirSink(root, "")
	if err != nil {
		return tracepack.UUID{}, err
	}
	origin := time.Date(2026, 10, 6, 8, 59, 0, 0, time.UTC)
	w, err := tracepack.NewSegmentWriter(ctx, tracepack.SegmentWriterOptions{
		Capture: tracepack.CaptureDescriptor{
			ToolID:             "EQP-01",
			Transport:          tracepack.TransportHSMSSS,
			CaptureMethod:      tracepack.CaptureMethodRawStream,
			Vantage:            tracepack.VantageHost,
			Recorder:           "example-recorder/1.0",
			RecorderInstanceID: tracepack.UUID{0x01, 0x9a, 0x2b, 0x3c, 0x4d, 0x5e, 0x70, 0x00, 0x80, 0x00, 0, 0, 0, 0, 0, 1},
			TimeSource:         tracepack.TimeSourceCaptureClock,
			CaptureOrigin:      origin,
			LifecycleCoverage:  tracepack.LifecycleCoverageSubscribed,
			QualityEvaluated:   true,
		},
		Sink: sink,
		Now:  func() time.Time { return origin.Add(70 * time.Second) },
	})
	if err != nil {
		return tracepack.UUID{}, err
	}
	if err := w.AppendFrame(ctx, origin.Add(50*time.Second), tracepack.DirHostToEquipment, 1, s1Frame(1, true, 7, nil)); err != nil {
		return tracepack.UUID{}, err
	}
	if err := w.AppendFrame(ctx, origin.Add(65*time.Second), tracepack.DirEquipmentToHost, 1, s1Frame(2, false, 7, nil)); err != nil {
		return tracepack.UUID{}, err
	}

	return w.CaptureID(), w.Close(ctx)
}

// segmentPaths returns the paths of the committed segments under root, a directory sink's root, in key order;
// none when root cannot be walked.
func segmentPaths(root string) []string {
	var paths []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".partial" {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			paths = append(paths, path)
		}

		return nil
	})
	slices.Sort(paths)

	return paths
}

// openSegments opens every committed segment under root, a directory sink's root, in key order.
// closeAll closes the files the readers read through, after the last read.
func openSegments(ctx context.Context, root string) (readers []*tracepack.Reader, closeAll func(), err error) {
	var files []*os.File
	closeAll = func() {
		for _, f := range files {
			f.Close()
		}
	}
	for _, path := range segmentPaths(root) {
		f, err := os.Open(path)
		if err != nil {
			closeAll()
			return nil, nil, err
		}
		files = append(files, f)
		st, err := f.Stat()
		if err != nil {
			closeAll()
			return nil, nil, err
		}
		r, err := tracepack.Open(ctx, f, st.Size(), tracepack.ReaderOptions{})
		if err != nil {
			closeAll()
			return nil, nil, err
		}
		readers = append(readers, r)
	}

	return readers, closeAll, nil
}

// printRecords prints the seq, kind and decode_status of every record of the segments under root, in key order.
func printRecords(ctx context.Context, root string) error {
	readers, closeAll, err := openSegments(ctx, root)
	if err != nil {
		return err
	}
	defer closeAll()
	for _, r := range readers {
		_, err := r.Iterate(ctx, tracepack.Query{}, func(it *tracepack.Item) error {
			fmt.Println("seq", it.Record.Seq, it.Record.Kind, it.Record.DecodeStatus)
			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

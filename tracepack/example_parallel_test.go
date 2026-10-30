package tracepack_test

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/arloliu/go-secs/tracepack"
)

// ExampleOpen_parallel opens the segments of a recording on goroutines of their own, then reads them as one read.
// A Reader is safe for concurrent use and Open shares no state between Readers,
// so where every ReadAt waits on a network or object store, opening the packs at once removes most of a read's time.
// The errors are kept by position and the first one in that order is returned,
// so the error does not depend on which open finishes first.
func ExampleOpen_parallel() {
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

	paths := segmentPaths(root)
	files := make([]*os.File, len(paths))
	readers := make([]*tracepack.Reader, len(paths))
	errs := make([]error, len(paths))
	var wg sync.WaitGroup
	for i, path := range paths {
		wg.Go(func() {
			f, err := os.Open(path)
			if err != nil {
				errs[i] = err
				return
			}
			files[i] = f
			st, err := f.Stat()
			if err != nil {
				errs[i] = err
				return
			}
			readers[i], errs[i] = tracepack.Open(ctx, f, st.Size(), tracepack.ReaderOptions{})
		})
	}
	wg.Wait()
	// The files stay open while the Readers are read.
	defer func() {
		for _, f := range files {
			if f != nil {
				f.Close()
			}
		}
	}()
	for i, err := range errs {
		if err != nil {
			fmt.Println("segment", i, err)
			return
		}
	}

	records := 0
	opts := tracepack.MergeIterateOptions{Order: tracepack.OrderTime}
	res, err := tracepack.MergeIterate(ctx, readers, tracepack.Query{}, opts, func(*tracepack.Item) error {
		records++
		return nil
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println("segments:", len(readers), "records:", records, "complete:", res.Complete())
	// Output:
	// segments: 2 records: 4 complete: true
}

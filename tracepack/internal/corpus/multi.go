package corpus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/arloliu/go-secs/tracepack"
)

// storedKey names a record of a pack by its block and its seq.
type storedKey struct {
	block int
	seq   uint64
}

// storedRecord is a record of a pack as stored:
// its fields, its payload and its record header's extension area, both owned.
type storedRecord struct {
	record      tracepack.Record
	headerExtra []byte
}

// generateMulti checks the multi-pack vector built, rec's for seed, against rec's Expect,
// and returns its manifest entry and its files:
// the packs, each pack's verification report, and the results of its reads over several packs.
func generateMulti(ctx context.Context, rec *Recipe, seed string, built *Built) (Vector, *readOutputs, error) {
	switch {
	case built.Pack != nil || built.Queries != nil || built.Fields || built.Footer || built.Classify != nil || built.Repair != nil:
		return Vector{}, nil, errors.New("a multi-pack vector has no pack.tpk and none of its optional files")
	case len(built.Packs) == 0 || len(built.Reads) == 0:
		return Vector{}, nil, errors.New("a multi-pack vector needs its packs and its reads")
	}
	packs := make([][]byte, 0, len(built.Packs))
	for _, p := range built.Packs {
		packs = append(packs, p.Bytes)
	}

	out, err := readMultiVector(ctx, packs, built.Reads)
	if err != nil {
		return Vector{}, nil, err
	}
	if err := rec.Expect.checkMultiPack(out, packs, seed); err != nil {
		return Vector{}, nil, fmt.Errorf("disagrees with its Expect: %w", err)
	}

	return rec.vector(out)
}

// readMultiVector reads packs, the packs of a multi-pack vector, pack n at n, as a reader of the corpus does,
// and returns the files the reads give, in the order of the tracepack corpus specification §2:
// each pack-<n>.tpk and its pack-<n>.verify.json, then reads.json with the result of each read of reads.
// Each read over several packs checks every record it returns
// against the stored record its pack and block name (checkRecordContents).
// It is how the generator makes the goldens and how the conformance test reads the committed packs again.
func readMultiVector(ctx context.Context, packs [][]byte, reads []ReadSpec) (*readOutputs, error) {
	out := &readOutputs{}
	stored := make([]map[storedKey]storedRecord, len(packs))
	for n, pack := range packs {
		po, err := readPack(ctx, pack)
		if err != nil {
			return nil, fmt.Errorf("pack %d: %w", n, err)
		}
		if stored[n], err = storedRecords(ctx, pack); err != nil {
			return nil, fmt.Errorf("pack %d: %w", n, err)
		}
		out.packs = append(out.packs, po)
		out.files = append(out.files, file{PackFile(n), pack})
		if err := out.add(PackVerifyFile(n), &po.verify); err != nil {
			return nil, err
		}
	}

	for i := range reads {
		v, err := runRead(ctx, packs, stored, &reads[i])
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", reads[i].ID, err)
		}
		out.reads = append(out.reads, v)
	}
	slices.SortFunc(out.reads, func(a, b ReadVector) int { return strings.Compare(a.ID, b.ID) })
	if len(reads) > 0 {
		if err := out.add(FileReads, Reads(out.reads)); err != nil {
			return nil, err
		}
	}

	return out, nil
}

// readPack reads one pack of a multi-pack vector as readVector reads pack.tpk, without the files of a single pack:
// its export, whose records the expectation checks but which no file holds, its verification report and its statistics.
func readPack(ctx context.Context, pack []byte) (*readOutputs, error) {
	r, err := openBytes(ctx, pack)
	if err != nil {
		return nil, err
	}
	out := &readOutputs{}
	if out.export, out.seqs, err = exportPack(ctx, r); err != nil {
		return nil, err
	}
	if out.verify, err = verifyBytes(ctx, pack); err != nil {
		return nil, err
	}
	out.stats, out.statsOK = r.Stats()

	return out, nil
}

// storedRecords returns every record pack holds as stored, by block and seq, with its payload and header extension.
func storedRecords(ctx context.Context, pack []byte) (map[storedKey]storedRecord, error) {
	r, err := openBytes(ctx, pack)
	if err != nil {
		return nil, err
	}
	out := make(map[storedKey]storedRecord)
	_, err = r.Iterate(ctx, tracepack.Query{Payloads: true}, func(it *tracepack.Item) error {
		rec := it.Record
		rec.Payload = bytes.Clone(it.Record.Payload)
		out[storedKey{block: it.Block, seq: rec.Seq}] = storedRecord{record: rec, headerExtra: bytes.Clone(it.HeaderExtra)}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// runRead runs spec over packs with MergeIterate, keeping a copy of each item it returns,
// checks each against the stored records stored, by pack number, and returns the read with its result.
func runRead(ctx context.Context, packs [][]byte, stored []map[storedKey]storedRecord, spec *ReadSpec) (ReadVector, error) {
	v := ReadVector{
		ID: spec.ID, Cites: spec.Cites, Packs: spec.Packs, Order: spec.Order, Payloads: spec.Payloads, MaxConflicts: spec.MaxConflicts,
		Filter: spec.Filter,
	}
	q, opts, err := v.Query()
	if err != nil {
		return ReadVector{}, err
	}

	run := ReadRun{Packs: spec.Packs, MaxConflicts: spec.MaxConflicts}
	readers := make([]*tracepack.Reader, 0, len(spec.Packs))
	for _, n := range spec.Packs {
		if n < 0 || n >= len(packs) {
			return ReadVector{}, fmt.Errorf("the read names pack %d of %d", n, len(packs))
		}
		r, err := openBytes(ctx, packs[n])
		if err != nil {
			return ReadVector{}, fmt.Errorf("pack %d: %w", n, err)
		}
		h := r.Header()
		readers = append(readers, r)
		run.IDs = append(run.IDs, h.PackID)
		run.Coverage = append(run.Coverage, h.Meta.Coverage)
	}
	// An item's payload and header extension alias the read's buffers, so each is copied before the callback returns.
	run.Result, run.Err = tracepack.MergeIterate(ctx, readers, q, opts, func(it *tracepack.Item) error {
		c := *it
		c.Record.Payload = bytes.Clone(it.Record.Payload)
		c.HeaderExtra = bytes.Clone(it.HeaderExtra)
		run.Items = append(run.Items, c)

		return nil
	})
	if err := checkRecordContents(run.Items, spec.Packs, stored, spec.Payloads); err != nil {
		return ReadVector{}, err
	}
	if v.Expect, err = ReadExpectFrom(&run); err != nil {
		return ReadVector{}, err
	}

	return v, nil
}

// checkRecordContents checks that every item of a read is the record of its seq in the pack and block it names, as stored
// (the tracepack corpus specification §1): every field of its record header, its header extension byte for byte,
// and its payload byte for byte when the read returns payloads, nil when it does not.
// packs holds the number of each reader's pack, in the order of the readers,
// and stored the stored records of each pack, by number.
func checkRecordContents(items []tracepack.Item, packs []int, stored []map[storedKey]storedRecord, payloads bool) error {
	for i := range items {
		it := &items[i]
		if it.Pack < 0 || it.Pack >= len(packs) || packs[it.Pack] < 0 || packs[it.Pack] >= len(stored) {
			return fmt.Errorf("corpus: item %d names reader %d of %d", i, it.Pack, len(packs))
		}
		n := packs[it.Pack]
		s, ok := stored[n][storedKey{block: it.Block, seq: it.Record.Seq}]
		if !ok {
			return fmt.Errorf("corpus: item %d: block %d of pack %d holds no record of seq %d", i, it.Block, n, it.Record.Seq)
		}
		if err := sameRecord(it, &s, payloads); err != nil {
			return fmt.Errorf("corpus: item %d, seq %d of pack %d, block %d: %w", i, it.Record.Seq, n, it.Block, err)
		}
	}

	return nil
}

// sameRecord checks the record of the item it against the stored record s, the payload only when payloads.
func sameRecord(it *tracepack.Item, s *storedRecord, payloads bool) error {
	switch {
	case !bytes.Equal(it.HeaderExtra, s.headerExtra) || (it.HeaderExtra == nil) != (s.headerExtra == nil):
		return errors.New("its header extension is not the stored one")
	case payloads && !bytes.Equal(it.Record.Payload, s.record.Payload):
		return errors.New("its payload is not the stored one")
	case !payloads && it.Record.Payload != nil:
		return errors.New("it carries a payload the read did not request")
	}
	got, want := it.Record, s.record
	got.Payload, want.Payload = nil, nil
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("its record header %+v is not the stored %+v", got, want)
	}

	return nil
}

// readSpecOf returns the read r without its expect.
func readSpecOf(r *ReadVector) ReadSpec {
	return ReadSpec{
		ID: r.ID, Cites: r.Cites, Packs: r.Packs, Order: r.Order, Payloads: r.Payloads, MaxConflicts: r.MaxConflicts, Filter: r.Filter,
	}
}

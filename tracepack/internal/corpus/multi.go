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
// the packs, each pack's verification report, and the results of its reads over several packs and of its lookups.
func generateMulti(ctx context.Context, rec *Recipe, seed string, built *Built) (Vector, *readOutputs, error) {
	switch {
	case built.Pack != nil || built.Queries != nil || built.Fields || built.Footer || built.Classify != nil || built.Repair != nil:
		return Vector{}, nil, errors.New("a multi-pack vector has no pack.tpk and none of its optional files")
	case len(built.Packs) == 0 || len(built.Reads) == 0 && len(built.Lookups) == 0:
		return Vector{}, nil, errors.New("a multi-pack vector needs its packs and its reads or lookups")
	}
	packs := make([][]byte, 0, len(built.Packs))
	for _, p := range built.Packs {
		packs = append(packs, p.Bytes)
	}

	out, err := readMultiVector(ctx, packs, built.Reads, built.Lookups)
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
// each pack-<n>.tpk and its pack-<n>.verify.json, then reads.json with the result of each read of reads,
// then lookups.json with the result of each lookup of lookups.
// Each read over several packs and each lookup checks every record it returns
// against the stored record its pack and block name (checkRecordContents, checkTxRecordContents).
// It is how the generator makes the goldens and how the conformance test reads the committed packs again.
func readMultiVector(ctx context.Context, packs [][]byte, reads []ReadSpec, lookups []LookupSpec) (*readOutputs, error) {
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

	for i := range lookups {
		v, err := runLookup(ctx, packs, stored, &lookups[i])
		if err != nil {
			return nil, fmt.Errorf("lookup %s: %w", lookups[i].ID, err)
		}
		out.lookups = append(out.lookups, v)
	}
	slices.SortFunc(out.lookups, func(a, b LookupVector) int { return strings.Compare(a.ID, b.ID) })
	if len(lookups) > 0 {
		if err := out.add(FileLookups, Lookups(out.lookups)); err != nil {
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
// The reserved bits of record_flags are not part of tracepack.Record, so the check does not compare them:
// for copies differing in those bits alone, the item's pack and block alone identify its version.
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
		if err := sameRecord(&it.Record, it.HeaderExtra, &s, payloads); err != nil {
			return fmt.Errorf("corpus: item %d, seq %d of pack %d, block %d: %w", i, it.Record.Seq, n, it.Block, err)
		}
	}

	return nil
}

// sameRecord checks the record rec, with its header extension extra, against the stored record s, the payload only when payloads.
func sameRecord(rec *tracepack.Record, extra []byte, s *storedRecord, payloads bool) error {
	switch {
	case !bytes.Equal(extra, s.headerExtra) || (extra == nil) != (s.headerExtra == nil):
		return errors.New("its header extension is not the stored one")
	case payloads && !bytes.Equal(rec.Payload, s.record.Payload):
		return errors.New("its payload is not the stored one")
	case !payloads && rec.Payload != nil:
		return errors.New("it carries a payload the read did not request")
	}
	got, want := *rec, s.record
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

// runLookup runs spec over the packs of a multi-pack vector with FindTransaction,
// over the source NewReaderSource builds from the lookup's source packs (the tracepack corpus specification §8):
// the view packs in the order the source lists them, then the evidence packs, no other pack of the vector.
// It checks every version the lookup kept against the stored records stored, by pack number,
// and returns the lookup with its result.
func runLookup(ctx context.Context, packs [][]byte, stored []map[storedKey]storedRecord, spec *LookupSpec) (LookupVector, error) {
	v := LookupVector{ID: spec.ID, Cites: spec.Cites, Source: spec.Source, Key: spec.Key, MaxScopes: spec.MaxScopes}
	src := &v.Source
	all := slices.Concat(src.View, src.Evidence)
	switch {
	case len(src.View) == 0 || src.Evidence == nil:
		return LookupVector{}, errors.New("the source's view is empty or its evidence absent")
	case hasDuplicate(all):
		return LookupVector{}, fmt.Errorf("the source names a pack twice, in view or evidence: %v", all)
	case slices.ContainsFunc(all, func(n int) bool { return n < 0 || n >= len(packs) }):
		return LookupVector{}, fmt.Errorf("the source names a pack of none of the %d packs: %v", len(packs), all)
	}
	key, opts, err := v.Lookup()
	if err != nil {
		return LookupVector{}, err
	}

	run := LookupRun{Key: key, MaxScopes: opts.MaxScopes, Numbers: make(map[tracepack.UUID]int, len(all))}
	open := func(ns []int) ([]*tracepack.Reader, error) {
		readers := make([]*tracepack.Reader, 0, len(ns))
		for _, n := range ns {
			r, err := openBytes(ctx, packs[n])
			if err != nil {
				return nil, fmt.Errorf("pack %d: %w", n, err)
			}
			readers = append(readers, r)
			run.Numbers[r.Header().PackID] = n
		}

		return readers, nil
	}
	view, err := open(src.View)
	if err != nil {
		return LookupVector{}, err
	}
	evidence, err := open(src.Evidence)
	if err != nil {
		return LookupVector{}, err
	}
	source, err := tracepack.NewReaderSource(view, tracepack.ReaderSourceOptions{Complete: src.Complete, Evidence: evidence})
	if err != nil {
		return LookupVector{}, err
	}
	run.Result, run.Err = tracepack.FindTransaction(ctx, source, key, opts)
	if err := checkTxRecordContents(run.Result.Records, run.Numbers, stored); err != nil {
		return LookupVector{}, err
	}
	if v.Expect, err = LookupExpectFrom(&run); err != nil {
		return LookupVector{}, err
	}

	return v, nil
}

// checkTxRecordContents checks that every version a lookup kept is the record of its seq in the pack and block it names, as stored
// (the tracepack corpus specification §1): every field of its record header, its header extension and its payload, byte for byte,
// since a lookup's version always carries its payload.
// numbers maps the pack_id of each source pack to its number, and stored holds the stored records of each pack, by number.
func checkTxRecordContents(records []tracepack.TxRecord, numbers map[tracepack.UUID]int, stored []map[storedKey]storedRecord) error {
	for i := range records {
		r := &records[i]
		n, ok := numbers[r.Pack]
		if !ok || n < 0 || n >= len(stored) {
			return fmt.Errorf("corpus: kept version %d names pack %s, no source pack", i, r.Pack)
		}
		s, ok := stored[n][storedKey{block: r.Block, seq: r.Record.Seq}]
		if !ok {
			return fmt.Errorf("corpus: kept version %d: block %d of pack %d holds no record of seq %d", i, r.Block, n, r.Record.Seq)
		}
		if err := sameRecord(&r.Record, r.HeaderExtra, &s, true); err != nil {
			return fmt.Errorf("corpus: kept version %d, seq %d of pack %d, block %d: %w", i, r.Record.Seq, n, r.Block, err)
		}
	}

	return nil
}

// lookupSpecOf returns the lookup l without its expect.
func lookupSpecOf(l *LookupVector) LookupSpec {
	return LookupSpec{ID: l.ID, Cites: l.Cites, Source: l.Source, Key: l.Key, MaxScopes: l.MaxScopes}
}

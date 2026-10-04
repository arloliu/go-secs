package tracepack

import (
	"fmt"
	"unsafe"
)

// The charges of a storeSource's Observe against StoreSourceOptions.MaxSourceBytes.
// Each item is charged a fixed size, the size of what it holds rounded up to a multiple of 16 bytes,
// plus its slices' elements at their stored lengths, the strings and byte slices it holds,
// and each value it points to, at least a word;
// the cost functions below are the only place these are summed.
// The charges are nominal, as the lookup's are: slice capacity, map overhead and the text of errors are not charged.
const (
	// storeAlignMask is the mask of the rounding of every fixed charge.
	storeAlignMask = 16 - 1
	// storeWordCharge is the least charge of a value a pointer holds.
	storeWordCharge = int64(unsafe.Sizeof(uintptr(0)))
	// storeObservationCharge is the fixed charge of an observation, its evidence's fixed part included;
	// each hour adds storeScopeCharge.
	storeObservationCharge = (int64(unsafe.Sizeof(storeObservation{})) + storeAlignMask) &^ storeAlignMask
	// storeScopeCharge is the fixed charge of one hour of an observation.
	storeScopeCharge = (int64(unsafe.Sizeof(storeScope{})) + storeAlignMask) &^ storeAlignMask
	// storeScopeDescriptorCharge is the fixed charge of a scope descriptor of the catalog; its token adds its length.
	storeScopeDescriptorCharge = (int64(unsafe.Sizeof(CatalogScope{})) + storeAlignMask) &^ storeAlignMask
	// storePackDescriptorCharge is the fixed charge of a pack descriptor of the catalog, its entries in the duplicate checks included;
	// its key adds its length.
	storePackDescriptorCharge = (int64(unsafe.Sizeof(CatalogPack{})+unsafe.Sizeof(UUID{})+unsafe.Sizeof("")) +
		storeAlignMask) &^ storeAlignMask
	// storeListedCharge is the fixed charge of a key a listing returned while the source holds it, before or after it is parsed;
	// the key adds its length.
	storeListedCharge = (int64(unsafe.Sizeof(storeListed{})) + storeAlignMask) &^ storeAlignMask
	// storeListedHourCharge is the charge of the listing state of one hour the catalog does not index:
	// its storeListedHour, its slot among the listed hours and its entry in their index by hour.
	storeListedHourCharge = (int64(unsafe.Sizeof(storeListedHour{})+unsafe.Sizeof((*storeListedHour)(nil))+
		unsafe.Sizeof(int64(0))+unsafe.Sizeof((*storeListedHour)(nil))) + storeAlignMask) &^ storeAlignMask
	// storeObjectSlotCharge is the charge of an object's slot among the objects to close, held until the observation is built.
	storeObjectSlotCharge = int64(unsafe.Sizeof(Object(nil)))
	// storeOpenedCharge is the fixed charge of a listed pack opened, while the source holds its entry among its hour's packs;
	// its key adds its length.
	storeOpenedCharge = (int64(unsafe.Sizeof(storeOpened{})) + storeAlignMask) &^ storeAlignMask
	// storePackInfoCharge is the fixed charge of a PackInfo built for ActiveView;
	// its decoded pack metadata adds its own, and its bytes are the Reader's.
	storePackInfoCharge = (int64(unsafe.Sizeof(PackInfo{})) + storeAlignMask) &^ storeAlignMask
	// storeBoundaryCharge is the fixed charge of a copied Boundary; each gap bound it holds adds storeWordCharge.
	storeBoundaryCharge = (int64(unsafe.Sizeof(Boundary{})) + storeAlignMask) &^ storeAlignMask
	// storeClosureCharge is the charge of a copied EpochClosure.
	storeClosureCharge = int64(unsafe.Sizeof(EpochClosure{}))
	// storeReaderCharge is the fixed charge of a retained Reader, with its slots among a scope's readers and the objects to close.
	storeReaderCharge = (int64(unsafe.Sizeof(Reader{})+unsafe.Sizeof((*Reader)(nil))+unsafe.Sizeof(Object(nil))) +
		storeAlignMask) &^ storeAlignMask
	// storeTrailerCharge is the charge of a Reader's TrailerInfo.
	storeTrailerCharge = (int64(unsafe.Sizeof(TrailerInfo{})) + storeAlignMask) &^ storeAlignMask
	// storeBlockInfoCharge is the charge of one entry of a Reader's block index.
	storeBlockInfoCharge = int64(unsafe.Sizeof(BlockInfo{}))
	// storeDefectCharge is the fixed charge of a defect a Reader keeps; its coverage entry adds its own.
	storeDefectCharge = (int64(unsafe.Sizeof(Defect{})) + storeAlignMask) &^ storeAlignMask
	// storeFooterCharge is the fixed charge of a Reader's decoded footer.
	storeFooterCharge = (int64(unsafe.Sizeof(footerIndex{})) + storeAlignMask) &^ storeAlignMask
	// storeF3SpanCharge is the charge of the location of one block's F-3 list a footer keeps.
	storeF3SpanCharge = int64(unsafe.Sizeof(f3Span{}))
	// storeBlockSummaryCharge is the charge of one block's F-2 entry and F-3 summary in a decoded footer, beside its lists.
	storeBlockSummaryCharge = int64(unsafe.Sizeof(blockSummary{}))
	// storeEpochCharge is the charge of one epoch summary of a block or of the pack statistics.
	storeEpochCharge = int64(unsafe.Sizeof(epochSummary{}))
	// storeEpochIndexCharge is the charge of one entry of a block summary's epoch index.
	storeEpochIndexCharge = (int64(unsafe.Sizeof(uint32(0))+unsafe.Sizeof(0)) + storeAlignMask) &^ storeAlignMask
	// storeBoundarySummaryCharge is the charge of one boundary entry of a block or of the pack statistics, beside its gap bounds.
	storeBoundarySummaryCharge = int64(unsafe.Sizeof(boundarySummary{}))
	// storeSeqRangeCharge is the charge of one seq range of a block or of the pack statistics.
	storeSeqRangeCharge = int64(unsafe.Sizeof(seqRange{}))
	// storePackStatsCharge is the fixed charge of a footer's retained pack statistics.
	storePackStatsCharge = (int64(unsafe.Sizeof(packStats{})) + storeAlignMask) &^ storeAlignMask
	// storePackMetaCharge is the fixed charge of a decoded PackMeta.
	storePackMetaCharge = (int64(unsafe.Sizeof(PackMeta{})) + storeAlignMask) &^ storeAlignMask
	// storeStringCharge is the charge of a string's header, in a slice or behind a pointer; its bytes add their length.
	storeStringCharge = int64(unsafe.Sizeof(""))
	// storeUUIDCharge is the charge of one UUID of a list.
	storeUUIDCharge = int64(unsafe.Sizeof(UUID{}))
	// storeUint64Charge is the charge of one element of a list of u64.
	storeUint64Charge = int64(unsafe.Sizeof(uint64(0)))
	// storeUint32Charge is the charge of one element of a block's count array.
	storeUint32Charge = int64(unsafe.Sizeof(uint32(0)))
	// storeRawEntryCharge is the charge of one RawEntry of a list, beside its value.
	storeRawEntryCharge = int64(unsafe.Sizeof(RawEntry{}))
	// storeRedactionCharge is the charge of one RedactionEntry, beside what it holds.
	storeRedactionCharge = int64(unsafe.Sizeof(RedactionEntry{}))
	// storeMaskedRangeCharge is the charge of one MaskedRange.
	storeMaskedRangeCharge = int64(unsafe.Sizeof(MaskedRange{}))
	// storeRedactionPolicyCharge is the charge of a RedactionPolicy, beside its strings.
	storeRedactionPolicyCharge = (int64(unsafe.Sizeof(RedactionPolicy{})) + storeAlignMask) &^ storeAlignMask
	// storeHSMSTimersCharge is the charge of an HSMSTimers, beside its values.
	storeHSMSTimersCharge = (int64(unsafe.Sizeof(HSMSTimers{})) + storeAlignMask) &^ storeAlignMask
)

// storeBudget is the budget of one Observe of a storeSource:
// the bytes charged against MaxSourceBytes and the objects counted against MaxObjects.
// Neither counter ever passes its limit, so neither can wrap.
type storeBudget struct {
	maxBytes int64
	used     int64
	// peak is the largest used has been.
	peak       int64
	maxObjects int
	objects    int
}

// boundaryCost returns the charge of a copy of b.
func boundaryCost(b *Boundary) int64 {
	return storeBoundaryCharge + pointerCost(b.GapStart) + pointerCost(b.GapEnd)
}

// scopeDescriptorCost returns the charge of the catalog's scope descriptor sc, beside its packs.
func scopeDescriptorCost(sc *CatalogScope) int64 {
	return storeScopeDescriptorCharge + int64(len(sc.Token))
}

// packDescriptorCost returns the charge of the catalog's pack descriptor p.
func packDescriptorCost(p *CatalogPack) int64 {
	return storePackDescriptorCharge + int64(len(p.Key))
}

// listedKeyCost returns the charge of the listed key key while the source holds it.
func listedKeyCost(key string) int64 {
	return storeListedCharge + int64(len(key))
}

// packInfoCost returns the charge of the PackInfo p built from a retained Reader: the PackInfo and its decoded pack metadata.
func packInfoCost(p *PackInfo) int64 {
	return storePackInfoCharge + packMetaCost(p.meta)
}

// readerCost returns the charge of the Reader r as a storeSource retains it:
// what the Reader keeps once Open has returned —
// its pack metadata, raw and decoded, its block index, its trailer, its decoded footer with its statistics, and its defects —
// and its slots in the observation.
func readerCost(r *Reader) int64 {
	n := storeReaderCharge + int64(len(r.metaRaw)) + int64(len(r.blocks))*storeBlockInfoCharge
	if r.meta != nil {
		n += packMetaCost(r.meta)
	}
	if r.trailer != nil {
		n += storeTrailerCharge
	}
	if r.footer != nil {
		n += footerIndexCost(r.footer)
	}
	for i := range r.openDefects {
		n += defectCost(&r.openDefects[i])
	}
	if r.walkStop != nil {
		n += defectCost(r.walkStop)
	}

	return n
}

// defectCost returns the charge of the defect d a Reader keeps.
func defectCost(d *Defect) int64 {
	n := storeDefectCharge
	if d.Coverage != nil {
		n += coverageCost(d.Coverage)
	}

	return n
}

// footerIndexCost returns the charge of the decoded footer f a Reader keeps:
// each block's summary with its lists, the F-3 section and its spans when kept, and the pack statistics.
func footerIndexCost(f *footerIndex) int64 {
	n := storeFooterCharge + int64(len(f.f3)) + int64(len(f.f3Spans))*storeF3SpanCharge
	for i := range f.blocks {
		n += blockSummaryCost(&f.blocks[i])
	}
	if f.stats != nil {
		n += packStatsCost(f.stats)
	}

	return n
}

// blockSummaryCost returns the charge of the block summary s of a decoded footer:
// its count arrays at their stored lengths, its epochs, boundaries and seq ranges, its verbatim F-3 list and its epoch index.
func blockSummaryCost(s *blockSummary) int64 {
	n := storeBlockSummaryCharge +
		int64(len(s.kindCounts)+len(s.dirCounts)+len(s.decodeStatusCounts))*storeUint32Charge +
		int64(len(s.epochs))*storeEpochCharge +
		boundarySummariesCost(s.boundaries) +
		int64(len(s.seqRanges))*storeSeqRangeCharge +
		int64(len(s.verbatimF3))
	if s.epochIndex != nil {
		n += int64(len(s.epochIndex)) * storeEpochIndexCharge
	}

	return n
}

// packStatsCost returns the charge of the pack statistics st a decoded footer keeps:
// its count arrays at their stored lengths, its seq ranges, epochs and boundaries.
func packStatsCost(st *packStats) int64 {
	return storePackStatsCharge +
		int64(len(st.kindCounts)+len(st.dirCounts)+len(st.decodeStatusCounts))*storeUint64Charge +
		int64(len(st.seqRanges))*storeSeqRangeCharge +
		int64(len(st.epochs))*storeEpochCharge +
		boundarySummariesCost(st.boundaries)
}

// boundarySummariesCost returns the charge of the boundary entries bs, each with its gap bounds.
func boundarySummariesCost(bs []boundarySummary) int64 {
	n := int64(len(bs)) * storeBoundarySummaryCharge
	for i := range bs {
		n += pointerCost(bs[i].gapStart) + pointerCost(bs[i].gapEnd)
	}

	return n
}

// packMetaCost returns the charge of the decoded pack metadata m:
// the PackMeta, every string, list element and nested value it holds, and every value its pointers hold.
func packMetaCost(m *PackMeta) int64 {
	n := storePackMetaCharge +
		int64(len(m.ToolID)+len(m.Recorder)+len(m.Writer)) +
		stringsCost(m.Classifiers) + stringsCost(m.SourceRefs) +
		int64(len(m.MaxFrameLens))*storeUint64Charge +
		int64(len(m.Supersedes)+len(m.CompactedFrom))*storeUUIDCharge +
		rawEntriesCost(m.Unknown) +
		packMetaPointersCost(m)
	for i := range m.Coverage {
		n += coverageCost(&m.Coverage[i])
	}
	for i := range m.Redaction {
		e := &m.Redaction[i]
		n += storeRedactionCharge + stringPointerCost(e.ItemPath) + int64(len(e.MaskedRanges))*storeMaskedRangeCharge +
			int64(len(e.Domain)) + int64(len(e.Digest))
	}
	if p := m.RedactionPolicy; p != nil {
		n += storeRedactionPolicyCharge + int64(len(p.PolicyID)+len(p.KeyID))
	}
	if t := m.HSMSTimers; t != nil {
		n += storeHSMSTimersCharge
		for _, v := range []*uint64{t.T1, t.T2, t.T3, t.T4, t.T5, t.T6, t.T7, t.T8} {
			n += pointerCost(v)
		}
	}

	return n
}

// packMetaPointersCost returns the charge of the values the optional scalar and string fields of m point to.
func packMetaPointersCost(m *PackMeta) int64 {
	n := pointerCost(m.CaptureOriginUTCNs) + pointerCost(m.CaptureOriginMonoNs) +
		pointerCost(m.PreviousCaptureID) + pointerCost(m.EquipmentConnectMode) + pointerCost(m.DeviceID) +
		pointerCost(m.ClockStepToleranceNs) + pointerCost(m.ReplacementSetID) + pointerCost(m.ReplacementSetSize) +
		pointerCost(m.ReplacementSetIndex) + pointerCost(m.FlushIntervalNs) + pointerCost(m.ScopeGeneration) +
		pointerCost(m.PublisherEpoch) + pointerCost(m.PatchBase)
	for _, s := range []*string{
		m.SourceTZ, m.SourceDialect, m.Notes, m.ExtractFilter, m.SiteID, m.EquipmentModel, m.EquipmentSWRev,
		m.HostSoftware, m.HostEndpoint, m.EquipmentEndpoint,
	} {
		n += stringPointerCost(s)
	}

	return n
}

// pointerCost returns the charge of the value p points to, at least a word; 0 for a nil p.
func pointerCost[T any](p *T) int64 {
	if p == nil {
		return 0
	}

	return max(int64(unsafe.Sizeof(*p)), storeWordCharge)
}

// stringPointerCost returns the charge of the string p points to, its header and its bytes; 0 for a nil p.
func stringPointerCost(p *string) int64 {
	if p == nil {
		return 0
	}

	return storeStringCharge + int64(len(*p))
}

// stringsCost returns the charge of the elements of ss, each string's header and bytes.
func stringsCost(ss []string) int64 {
	n := int64(len(ss)) * storeStringCharge
	for _, s := range ss {
		n += int64(len(s))
	}

	return n
}

// rawEntriesCost returns the charge of the elements of es, each entry and its value.
func rawEntriesCost(es []RawEntry) int64 {
	n := int64(len(es)) * storeRawEntryCharge
	for i := range es {
		n += int64(len(es[i].Value))
	}

	return n
}

// reserve charges n bytes, not negative, before the state grows by them.
// Its caller names the state in the error.
//
// Returns:
//   - error: an error wrapping ErrReadLimit, charging nothing, when the charge would pass maxBytes.
func (b *storeBudget) reserve(n int64) error {
	if n > b.maxBytes-b.used {
		return fmt.Errorf("%d bytes beside the %d held exceed MaxSourceBytes %d: %w", n, b.used, b.maxBytes, ErrReadLimit)
	}
	b.used += n
	b.peak = max(b.peak, b.used)

	return nil
}

// release returns n bytes that reserve charged, once the state they charged is dropped.
func (b *storeBudget) release(n int64) {
	b.used -= n
}

// count counts one object against maxObjects; its caller names the object in the error.
//
// Returns:
//   - error: an error wrapping ErrReadLimit, counting nothing, when the object would pass maxObjects.
func (b *storeBudget) count() error {
	if b.objects >= b.maxObjects {
		return fmt.Errorf("more than MaxObjects %d objects: %w", b.maxObjects, ErrReadLimit)
	}
	b.objects++

	return nil
}

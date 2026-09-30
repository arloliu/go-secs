package tracepack

// coalesceGroup is the pending coalescing group of a merge (the tracepack storage specification §4 Merge):
// consecutive units of one record_header_len whose decoded size together stays within the threshold.
// It holds copies of its units' bytes and records, in buffers it reuses from one group to the next,
// so it stays valid while the buffers its units were read or encoded in are reused.
// It does not copy an input block's F-3 list, which stays in the input's retained F-3 section.
// So a group holds its records, within the threshold, its units' on-disk bytes, and one block summary per unit.
type coalesceGroup struct {
	// records holds the records of the group's units in seq order, the records of its new block;
	// its headerLen and size are the group's record_header_len and decoded size.
	records unitBuilder
	// raw holds the envelope and on-disk body of each unit, one after another, and units the rest of each unit,
	// for a group written as its units.
	raw   []byte
	units []groupUnit
	// enc holds the buffers the group's new block is encoded in.
	// They are not the resolution's, whose last block may be the unit that makes the group be written.
	enc encodeBuf
}

// groupUnit is what a coalescing group keeps of one of its units, besides its records,
// to write the unit as it is.
type groupUnit struct {
	// end is the offset in the group's raw where the unit's on-disk body ends.
	end int
	sum blockSummary
	// f3 is an input block's F-3 entry list, aliasing its input's retained F-3 section; nil for an encoded unit.
	f3      []byte
	encoded bool
}

// emit takes the next unit of the archive, in seq order, which aliases the buffers it was read or encoded in.
// Without coalescing it writes the unit as it is.
// With it, units are grouped greedily (the tracepack storage specification §4 Merge):
// the unit joins the pending group when it has the group's record_header_len
// and the group's decoded size, the unit's included, stays within the threshold;
// otherwise the pending group is written first (flushGroup), and the unit starts the next group.
// A unit whose decoded size alone reaches the threshold is a group of its own, which no later unit can join,
// so it is written at once, without a copy.
// Once a conflict was found no unit is written, and the pending group is dropped.
func (m *mergeRun) emit(u *mergeUnit) error {
	g := &m.group
	if m.rep.Conflicts > 0 {
		g.reset()
		return nil
	}
	if !m.coalesce {
		return m.writeUnit(u)
	}

	threshold := uint64(m.p.threshold)
	if len(g.units) > 0 && (u.headerLen != g.records.headerLen || g.records.size+u.size > threshold) {
		if err := m.flushGroup(); err != nil {
			return err
		}
	}
	if u.size >= threshold {
		return m.writeUnit(u)
	}
	g.add(u)

	return nil
}

// flushGroup writes the pending group, when there is one, and empties it:
// a group of one unit as that unit is, and a larger group as one new block,
// encoded with the merge's codec from the group's records and checked in memory before it is written (encodeRawBlock).
// A new block whose encoding fails, its codec or its check, is discarded,
// and the group's units are written as they are (the tracepack storage specification §4 Merge),
// each input block with its F-3 list byte for byte.
// The input blocks of an encoded group are counted as coalesced, and its blocks of the resolution are not,
// since the input blocks of their records were counted as resolved.
func (m *mergeRun) flushGroup() error {
	g := &m.group
	defer g.reset()
	switch len(g.units) {
	case 0:
		return nil
	case 1:
		return m.writeGroupUnits()
	}

	rows, payloads := g.records.records()
	raw, sum, err := encodeRawBlock(&g.enc, m.codec, g.records.headerLen, rows, payloads)
	if err != nil {
		m.rep.CoalesceFallbacks++
		return m.writeGroupUnits()
	}
	if err := m.writeBlock(raw, &sum, nil, false); err != nil {
		return err
	}
	m.rep.CoalescedEncodings++
	for i := range g.units {
		if !g.units[i].encoded {
			m.rep.Coalesced++
		}
	}

	return nil
}

// writeGroupUnits writes every unit of the pending group as it is, in order.
func (m *mergeRun) writeGroupUnits() error {
	g := &m.group
	start := 0
	for i := range g.units {
		gu := &g.units[i]
		if err := m.writeBlock(g.raw[start:gu.end], &gu.sum, gu.f3, !gu.encoded); err != nil {
			return err
		}
		start = gu.end
	}

	return nil
}

// writeUnit writes u as it is:
// an input block with its F-3 list, counted as copied, or an encoded block with the F-3 list its summary gives.
func (m *mergeRun) writeUnit(u *mergeUnit) error {
	return m.writeBlock(u.raw, &u.sum, u.f3, !u.encoded)
}

// writeBlock appends raw, a block whose records s summarizes, with the verbatim F-3 list f3 or none,
// and counts it in the report: copied reports an input block written as it is.
func (m *mergeRun) writeBlock(raw []byte, s *blockSummary, f3 []byte, copied bool) error {
	if err := m.w.appendBlock(raw, s, f3); err != nil {
		return err
	}
	m.rep.Blocks++
	m.rep.Records += uint64(s.recordCount)
	if copied {
		m.rep.Copied++
	}

	return nil
}

// add copies u, its records and its envelope and on-disk body, into the group; u's F-3 list is kept as it is.
func (g *coalesceGroup) add(u *mergeUnit) {
	if d := u.d; d != nil {
		rhl := int(d.env.RecordHeaderLen)
		for i := range d.count() {
			row, payload := d.section[i*rhl:(i+1)*rhl:(i+1)*rhl], d.payload(i)
			g.records.add(row, payload, uint64(rhl)+uint64(len(payload)))
		}
	} else {
		for i, row := range u.rows {
			g.records.add(row, u.payloads[i], uint64(len(row))+uint64(len(u.payloads[i])))
		}
	}
	g.raw = append(g.raw, u.raw...)
	g.units = append(g.units, groupUnit{end: len(g.raw), sum: u.sum, f3: u.f3, encoded: u.encoded})
}

// reset empties the group, keeping its buffers.
func (g *coalesceGroup) reset() {
	g.records.reset()
	g.raw = g.raw[:0]
	clear(g.units)
	g.units = g.units[:0]
}

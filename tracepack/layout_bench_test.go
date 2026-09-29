package tracepack

// Benchmarks of the byte-columnar header section (the tracepack format specification §6)
// against the same blocks with their record headers stored in rows, on synthetic HSMS traffic:
//
//	idle: Linktest every 30 s, S1F3/S1F4 every 5 s, S1F1/S1F2 every 60 s;
//	busy: idle plus S6F11 every 1-3 s, an S6F1 trace every 1 s and S2F41 every 5 min.
//
// They reproduce the comparison recorded with spec v2.13 in the specification changelog:
//
//	GOWORK=off go test -run '^$' -bench 'Layout' -benchtime 1x
//
// BenchmarkLayoutSize reports bytes per record of the whole pack and of the block bodies in both layouts;
// BenchmarkLayoutEncode and BenchmarkLayoutDecode time one block of about 4 MiB of busy traffic.

import (
	"bytes"
	"encoding/binary"
	"math"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/go-secs/tracepack/internal/codec"
	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// layoutMsg is one HSMS message of the synthetic traffic.
type layoutMsg struct {
	at       time.Duration
	dir      Dir
	kind     Kind
	stream   byte
	function byte
	w        bool
	stype    byte
	sysBytes uint32
	text     []byte
}

// frame returns the message's HSMS frame: length, header, message text.
func (m *layoutMsg) frame() []byte {
	f := make([]byte, 14+len(m.text))
	binary.BigEndian.PutUint32(f, uint32(10+len(m.text)))
	if m.kind == KindControl {
		f[4], f[5] = 0xFF, 0xFF
	} else {
		f[4], f[5] = 0x00, 0x01
		f[6], f[7] = m.stream, m.function
		if m.w {
			f[6] |= 0x80
		}
	}
	f[9] = m.stype
	binary.BigEndian.PutUint32(f[10:], m.sysBytes)
	copy(f[14:], m.text)

	return f
}

// SECS-II items with one length byte (SEMI E5 §9): the format byte is the format code shifted left by 2, plus 1.
func itemList(items ...[]byte) []byte {
	return slices.Concat(append([][]byte{{0x01, byte(len(items))}}, items...)...)
}
func itemASCII(s string) []byte { return append([]byte{0x41, byte(len(s))}, s...) }
func itemU4(v uint32) []byte    { return binary.BigEndian.AppendUint32([]byte{0xB1, 4}, v) }
func itemF4(v float32) []byte {
	return binary.BigEndian.AppendUint32([]byte{0x91, 4}, math.Float32bits(v))
}
func itemAck() []byte { return []byte{0x21, 1, 0} }

// layoutGen accumulates the traffic of one capture.
type layoutGen struct {
	r   *rand.Rand
	sb  uint32
	out []layoutMsg
}

func (g *layoutGen) lat() time.Duration { return time.Duration(15+g.r.IntN(70)) * time.Millisecond }

// pair adds a primary and its reply.
func (g *layoutGen) pair(at time.Duration, hostPrimary bool, s, f byte, pri, sec []byte) {
	g.sb++
	d1, d2 := DirHostToEquipment, DirEquipmentToHost
	if !hostPrimary {
		d1, d2 = d2, d1
	}
	g.out = append(g.out,
		layoutMsg{at: at, dir: d1, kind: KindData, stream: s, function: f, w: true, sysBytes: g.sb, text: pri},
		layoutMsg{at: at + g.lat(), dir: d2, kind: KindData, stream: s, function: f + 1, sysBytes: g.sb, text: sec})
}

// linktest adds a Linktest.req and its Linktest.rsp.
func (g *layoutGen) linktest(at time.Duration) {
	g.sb++
	g.out = append(g.out,
		layoutMsg{at: at, dir: DirHostToEquipment, kind: KindControl, stype: 5, sysBytes: g.sb},
		layoutMsg{at: at + g.lat(), dir: DirEquipmentToHost, kind: KindControl, stype: 6, sysBytes: g.sb})
}

// layoutTraffic returns dur of idle or busy traffic, in time order, generated from seed.
func layoutTraffic(busy bool, dur time.Duration, seed uint64) []layoutMsg {
	g := &layoutGen{r: rand.New(rand.NewPCG(seed, seed))}
	j := func() time.Duration { return time.Duration(g.r.IntN(200)) * time.Millisecond }
	for t := time.Duration(0); t < dur; t += 30 * time.Second {
		g.linktest(t + j())
	}
	for t := time.Second; t < dur; t += 5 * time.Second {
		g.pair(t+j(), true, 1, 3, itemList(itemU4(1001), itemU4(1002), itemU4(1003)),
			itemList(itemF4(23.5+g.r.Float32()), itemASCII("IDLE"), itemU4(uint32(g.r.IntN(100)))))
	}
	for t := 2 * time.Second; t < dur; t += 60 * time.Second {
		g.pair(t+j(), true, 1, 1, nil, itemList(itemASCII("ETCH-3000"), itemASCII("V2.14.3")))
	}
	if busy {
		layoutBusy(g, dur)
	}
	slices.SortStableFunc(g.out, func(a, b layoutMsg) int { return int(a.at - b.at) })

	return g.out
}

// layoutBusy adds the event reports, the trace data and the remote commands of busy traffic.
func layoutBusy(g *layoutGen, dur time.Duration) {
	dataID := uint32(0)
	for t := 500 * time.Millisecond; t < dur; t += time.Duration(1000+g.r.IntN(2000)) * time.Millisecond {
		dataID++
		vals := make([][]byte, 5+g.r.IntN(16))
		for i := range vals {
			switch i % 3 {
			case 0:
				vals[i] = itemF4(100 * g.r.Float32())
			case 1:
				vals[i] = itemU4(uint32(g.r.IntN(5000)))
			default:
				vals[i] = itemASCII([]string{"LOT12345", "W07", "RCP-ETCH-01", "PM2"}[g.r.IntN(4)])
			}
		}
		ceid := uint32(9000 + g.r.IntN(20))
		g.pair(t, false, 6, 11, itemList(itemU4(dataID), itemU4(ceid), itemList(itemList(itemU4(ceid+100), itemList(vals...)))), itemAck())
	}
	sample := uint32(0)
	for t := 700 * time.Millisecond; t < dur; t += time.Second {
		sample++
		vals := make([][]byte, 10+g.r.IntN(21))
		for i := range vals {
			vals[i] = itemF4(100 * g.r.Float32())
		}
		stime := time.Unix(0, 0).Add(t).UTC().Format("2006010215040500")
		g.pair(t, false, 6, 1, itemList(itemU4(1), itemU4(sample), itemASCII(stime), itemList(vals...)), itemAck())
	}
	for t := 3 * time.Second; t < dur; t += 5 * time.Minute {
		g.pair(t, true, 2, 41,
			itemList(itemASCII("START"), itemList(itemList(itemASCII("PPID"), itemASCII("RCP-ETCH-01")), itemList(itemASCII("LOTID"), itemASCII("LOT12345")))),
			itemList(itemAck(), itemList()))
	}
}

// layoutStart is the capture start of the synthetic traffic, a whole UTC hour.
var layoutStart = time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)

// layoutRecords returns msgs as records of a raw capture starting at layoutStart.
func layoutRecords(msgs []layoutMsg) []Record {
	recs := make([]Record, len(msgs))
	for i := range msgs {
		m := &msgs[i]
		recs[i] = Record{
			TSUTCNs: layoutStart.Add(m.at).UnixNano(), MonoNs: int64(m.at), MonoPresent: true, Epoch: 1,
			Kind: m.kind, Dir: m.dir, Fidelity: FidelityWireExact, DecodeStatus: DecodeStatusOK, Payload: m.frame(),
		}
		recs[i].SetCapturedFieldValidity()
	}

	return recs
}

// layoutPack writes recs into one pack with the zstd codec and block validation,
// closing a block at the size threshold or an hour change only, and returns the file and the Writer's block summaries.
func layoutPack(tb testing.TB, recs []Record) ([]byte, []blockSummary) {
	tb.Helper()

	m := readerTestMeta()
	m.PeriodStart, m.PeriodEnd = layoutStart.UnixNano(), layoutStart.Add(time.Hour).UnixNano()
	var buf bytes.Buffer
	w, err := NewWriter(&buf, WriterOptions{Meta: m, Facts: PackFacts{AnyClassified: true}, Codec: CodecZstd, Validate: true, AssignSeq: true})
	if err != nil {
		tb.Fatal(err)
	}
	for i := range recs {
		if err := w.Append(&recs[i]); err != nil {
			tb.Fatal(err)
		}
	}
	if _, err := w.Close(); err != nil {
		tb.Fatal(err)
	}

	return buf.Bytes(), w.blocks
}

// rowBody returns the decoded body of the block at s in file with its record headers stored in rows,
// and the columnar body as stored.
func rowBody(tb testing.TB, file []byte, s *blockSummary) (rows, cols []byte) {
	tb.Helper()

	enc := file[s.offset+format.EnvelopeLen : s.offset+uint64(s.onDiskLen)]
	cols, err := codec.Decode(codec.Zstd, nil, enc, int(s.uncompressedLen))
	if err != nil {
		tb.Fatal(err)
	}
	hsLen := int(s.recordCount) * int(s.recordHeaderLen)
	rows = format.UntransposeHeaders(nil, cols[:hsLen], int(s.recordCount), int(s.recordHeaderLen))

	return append(rows, cols[hsLen:]...), cols
}

// BenchmarkLayoutSize reports, per traffic and pack length, the bytes per record of the whole pack
// and of its zstd block bodies with the header section by columns and by rows.
func BenchmarkLayoutSize(b *testing.B) {
	for _, tt := range []struct {
		name string
		busy bool
		dur  time.Duration
	}{
		{"idle 5 min", false, 5 * time.Minute},
		{"idle 1 h", false, time.Hour},
		{"busy 1 h", true, time.Hour},
	} {
		b.Run(tt.name, func(b *testing.B) {
			recs := layoutRecords(layoutTraffic(tt.busy, tt.dur, 1))
			for b.Loop() {
				file, blocks := layoutPack(b, recs)
				var colBytes, rowBytes int
				for i := range blocks {
					rows, _ := rowBody(b, file, &blocks[i])
					rowZ, err := codec.Encode(codec.Zstd, nil, rows)
					if err != nil {
						b.Fatal(err)
					}
					colBytes += int(blocks[i].onDiskLen) - format.EnvelopeLen
					rowBytes += len(rowZ)
				}
				n := float64(len(recs))
				b.ReportMetric(float64(len(file))/n, "pack-B/rec")
				b.ReportMetric(float64(colBytes)/n, "col-B/rec")
				b.ReportMetric(float64(rowBytes)/n, "row-B/rec")
			}
		})
	}
}

var (
	layoutBlockOnce          sync.Once
	layoutRows, layoutCols   []byte
	layoutRowZ, layoutColZ   []byte
	layoutCount, layoutHSLen int
)

// layoutBlock builds the bodies of one block of about 4 MiB of busy traffic, by rows and by columns,
// and their zstd encodings, once.
func layoutBlock(b *testing.B) {
	layoutBlockOnce.Do(func() {
		recs := layoutRecords(layoutTraffic(true, 4*time.Hour, 1))
		var blk blockBuilder
		for i := range recs {
			if blk.size()+recordHeaderLen+len(recs[i].Payload) > DefaultBlockThreshold {
				break
			}
			h := canonicalHeader(&recs[i], uint64(i), nil)
			blk.add(&h, recs[i].Payload, nil)
		}
		layoutCount = int(blk.summary.recordCount)
		layoutHSLen = len(blk.headers)
		layoutRows = append(slices.Clone(blk.headers), blk.payloads...)
		layoutCols = blk.body(nil)
		var err error
		if layoutRowZ, err = codec.Encode(codec.Zstd, nil, layoutRows); err != nil {
			b.Fatal(err)
		}
		if layoutColZ, err = codec.Encode(codec.Zstd, nil, layoutCols); err != nil {
			b.Fatal(err)
		}
	})
	b.ReportMetric(float64(len(layoutRowZ)), "row-B")
	b.ReportMetric(float64(len(layoutColZ)), "col-B")
}

// BenchmarkLayoutEncode times the encoding of one block:
// zstd of the row body, and the transpose plus zstd of the columnar one.
func BenchmarkLayoutEncode(b *testing.B) {
	b.Run("rows", func(b *testing.B) {
		layoutBlock(b)
		dst := make([]byte, 0, len(layoutRows))
		for b.Loop() {
			if _, err := codec.Encode(codec.Zstd, dst[:0], layoutRows); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("columns", func(b *testing.B) {
		layoutBlock(b)
		body := make([]byte, 0, len(layoutCols))
		dst := make([]byte, 0, len(layoutCols))
		for b.Loop() {
			body = format.TransposeHeaders(body[:0], layoutRows[:layoutHSLen], layoutCount, recordHeaderLen)
			body = append(body, layoutRows[layoutHSLen:]...)
			if _, err := codec.Encode(codec.Zstd, dst[:0], body); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkLayoutDecode times a full read of one block: the zstd decode, plus the untranspose for the columnar body.
func BenchmarkLayoutDecode(b *testing.B) {
	b.Run("rows", func(b *testing.B) {
		layoutBlock(b)
		dst := make([]byte, 0, len(layoutRows))
		for b.Loop() {
			if _, err := codec.Decode(codec.Zstd, dst[:0], layoutRowZ, len(layoutRows)); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("columns", func(b *testing.B) {
		layoutBlock(b)
		dst := make([]byte, 0, len(layoutCols))
		rows := make([]byte, 0, layoutHSLen)
		for b.Loop() {
			out, err := codec.Decode(codec.Zstd, dst[:0], layoutColZ, len(layoutCols))
			if err != nil {
				b.Fatal(err)
			}
			rows = format.UntransposeHeaders(rows[:0], out[:layoutHSLen], layoutCount, recordHeaderLen)
		}
	})
}

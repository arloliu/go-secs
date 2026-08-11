package compression

import (
	"fmt"
	"math"
	"math/rand"
	"strings"

	"github.com/arloliu/go-secs/v2/secs2"
)

// Realistic SECS-II message bodies. Deliberately NOT the repo's synthetic
// benchmark shapes (byte('A'+i%26)) — those are pathologically compressible
// and would produce meaningless ratios.

type Sample struct {
	Name string
	Body []byte
}

func rngFor(seed int64) *rand.Rand { return rand.New(rand.NewSource(seed)) }

// svidNames returns realistic status-variable names as seen on real tools.
func svidNames(n int) []string {
	prefixes := []string{"PM", "LL", "TM", "EFEM", "ALIGNER", "ROBOT", "CHUCK", "RF", "GAS", "PUMP"}
	metrics := []string{
		"Temperature", "Pressure", "FlowRate", "Power", "Position", "State",
		"SetPoint", "ActualValue", "Voltage", "Current", "Speed", "Torque",
	}
	out := make([]string, n)
	for i := range n {
		out[i] = fmt.Sprintf("%s%d_%s", prefixes[i%len(prefixes)], (i%4)+1, metrics[(i/3)%len(metrics)])
	}

	return out
}

// s1f1 — Are You There. Empty body: the floor case.
func s1f1() secs2.Item { return secs2.NewEmptyItem() }

// s1f2 — On Line Data. Two short ASCII items.
// Seeded so training and measurement sets never share exact bytes.
func s1f2(seed int64) secs2.Item {
	r := rngFor(seed)

	return secs2.L(
		secs2.A(fmt.Sprintf("EQUIP-%c%d", 'A'+rune(r.Intn(26)), r.Intn(10))),
		secs2.A(fmt.Sprintf("%d.%d.%d", r.Intn(9), r.Intn(20), r.Intn(20))),
	)
}

// s2f41 — Host Command Send. Small command with a few params.
// Seeded: real traffic varies LOTID/PPID per transaction, and a fixed body
// would leak verbatim into the dictionary and inflate the measured gain.
func s2f41(seed int64) secs2.Item {
	r := rngFor(seed)
	recipes := []string{"OXIDE_ETCH_02", "NITRIDE_DEP_11", "POLY_ETCH_07", "METAL1_CMP_03"}

	return secs2.L(
		secs2.A("PP-SELECT"),
		secs2.L(
			secs2.L(secs2.A("PPID"), secs2.A("RECIPE_"+recipes[r.Intn(len(recipes))])),
			secs2.L(secs2.A("LOTID"), secs2.A(fmt.Sprintf("LOT%d%06d", 2026081100+r.Intn(900), r.Intn(1000000)))),
		),
	)
}

// s1f4 SVID reply, n variables, mixed types with realistic sensor values.
func s1f4(n int, seed int64) secs2.Item {
	r := rngFor(seed)
	names := svidNames(n)
	vals := make([]secs2.Item, n)
	for i := range n {
		switch i % 4 {
		case 0: // temperature-like float, clustered around a setpoint
			vals[i] = secs2.F4(float32(250.0 + r.NormFloat64()*1.5))
		case 1: // pressure-like float
			vals[i] = secs2.F8(0.0075 + r.NormFloat64()*0.0001)
		case 2: // counter / enum
			vals[i] = secs2.U4(uint32(r.Intn(1000)))
		default: // state string from a small set
			states := []string{"IDLE", "RUN", "PAUSE", "ALARM", "SETUP"}
			vals[i] = secs2.A(states[r.Intn(len(states))])
		}
	}
	_ = names

	return secs2.L(vals...)
}

// s6f11 event report: CEID + one report with n named variable values.
// This is the single most common high-rate message on a real link.
func s6f11(n int, seed int64) secs2.Item {
	r := rngFor(seed)
	vals := make([]secs2.Item, n)
	for i := range n {
		if i%3 == 0 {
			vals[i] = secs2.A(fmt.Sprintf("W%02d", r.Intn(25)+1))
		} else {
			vals[i] = secs2.F4(float32(100.0 + r.NormFloat64()*8))
		}
	}

	return secs2.L(
		secs2.U4(uint32(1)),    // DATAID
		secs2.U4(uint32(3005)), // CEID
		secs2.L(secs2.L(secs2.U4(uint32(101)), secs2.L(vals...))),
	)
}

// s6f11Trace: a trace/DVVAL burst — long run of float samples. Very common
// for FDC data collection, and the shape most likely to benefit.
func s6f11Trace(samples int, seed int64) secs2.Item {
	r := rngFor(seed)
	vals := make([]any, samples)
	base := 250.0
	for i := range samples {
		base += r.NormFloat64() * 0.05 // random walk, like a real sensor
		vals[i] = base
	}

	return secs2.L(
		secs2.U4(uint32(2)),
		secs2.U4(uint32(4001)),
		secs2.F8(vals...),
	)
}

// s6f11TraceQ: same trace burst but quantized to 3 decimals, as many real
// tools report. Tests whether the F8 incompressibility result is an artifact
// of full-entropy mantissas rather than a property of trace data.
func s6f11TraceQ(samples int, seed int64, decimals int) secs2.Item {
	r := rngFor(seed)
	scale := math.Pow(10, float64(decimals))
	vals := make([]any, samples)
	base := 250.0
	for i := range samples {
		base += r.NormFloat64() * 0.05
		vals[i] = math.Round(base*scale) / scale
	}

	return secs2.L(secs2.U4(uint32(2)), secs2.U4(uint32(4001)), secs2.F8(vals...))
}

// s6f11TraceF4: single-precision trace, the other common on-wire choice.
func s6f11TraceF4(samples int, seed int64) secs2.Item {
	r := rngFor(seed)
	vals := make([]any, samples)
	base := float32(250.0)
	for i := range samples {
		base += float32(r.NormFloat64() * 0.05)
		vals[i] = base
	}

	return secs2.L(secs2.U4(uint32(2)), secs2.U4(uint32(4001)), secs2.F4(vals...))
}

// s6f11TraceU2: quantized integer counts — the most compressible realistic
// trace encoding, and a lever the library's users control.
func s6f11TraceU2(samples int, seed int64) secs2.Item {
	r := rngFor(seed)
	vals := make([]any, samples)
	base := 25000.0
	for i := range samples {
		base += r.NormFloat64() * 5
		vals[i] = uint16(math.Max(0, math.Min(65535, base)))
	}

	return secs2.L(secs2.U4(uint32(2)), secs2.U4(uint32(4001)), secs2.U2(vals...))
}

// s7f3 recipe send: PPID + PPBody. Real recipes are structured step text with
// heavy line-to-line repetition — modelled here, not a repeating alphabet.
func s7f3(steps int, seed int64) secs2.Item {
	return secs2.L(secs2.A("RECIPE_OXIDE_ETCH_02"), secs2.A(recipeText(steps, seed)))
}

// recipeText is the PPBody payload alone, without SECS-II framing.
func recipeText(steps int, seed int64) string {
	r := rngFor(seed)
	var sb strings.Builder
	sb.WriteString("[RECIPE]\nNAME=OXIDE_ETCH_02\nREV=14\nAUTHOR=proc_eng\n\n")
	for i := range steps {
		fmt.Fprintf(&sb, "[STEP%03d]\n", i+1)
		fmt.Fprintf(&sb, "TIME=%.1f\n", math.Round(r.Float64()*600)/10)
		fmt.Fprintf(&sb, "PRESSURE=%.3f\n", 0.005+r.Float64()*0.02)
		fmt.Fprintf(&sb, "RF_POWER=%d\n", 200+r.Intn(800))
		fmt.Fprintf(&sb, "GAS_CF4=%.1f\n", r.Float64()*100)
		fmt.Fprintf(&sb, "GAS_O2=%.1f\n", r.Float64()*50)
		fmt.Fprintf(&sb, "GAS_AR=%.1f\n", r.Float64()*200)
		fmt.Fprintf(&sb, "TEMP_SET=%.1f\n", 20+r.Float64()*60)
		fmt.Fprintf(&sb, "ENDPOINT=%s\n\n", []string{"OES", "TIME", "NONE"}[r.Intn(3)])
	}

	return sb.String()
}

// s12f? wafer map: realistic bin map — mostly good die with clustered defects,
// not byte(i%4).
func waferMap(die int, seed int64) secs2.Item {
	return secs2.L(secs2.U4(uint32(300)), secs2.B(anySlice(waferBins(die, seed))...))
}

// waferBins is the bin-map payload alone, without SECS-II framing.
func waferBins(die int, seed int64) []byte {
	r := rngFor(seed)
	data := make([]byte, die)
	for i := range data {
		data[i] = 1 // good bin
	}
	// a handful of defect clusters
	for c := 0; c < 12; c++ {
		center := r.Intn(die)
		radius := 50 + r.Intn(400)
		bin := byte(2 + r.Intn(6))
		for j := max(0, center-radius); j < min(die, center+radius); j++ {
			if r.Float64() < 0.6 {
				data[j] = bin
			}
		}
	}
	// scattered random defects
	for i := 0; i < die/500; i++ {
		data[r.Intn(die)] = byte(2 + r.Intn(6))
	}

	return data
}

func anySlice(b []byte) []any {
	out := make([]any, len(b))
	for i, v := range b {
		out[i] = v
	}

	return out
}

// Corpus returns the measurement set, one entry per realistic message shape.
func Corpus() []Sample {
	mk := func(name string, it secs2.Item) Sample {
		return Sample{Name: name, Body: it.ToBytes()}
	}

	return []Sample{
		mk("S1F1_areyouthere", s1f1()),
		mk("S1F2_online", s1f2(700)),
		mk("S2F41_hostcmd", s2f41(701)),
		mk("S1F4_svid_20", s1f4(20, 1)),
		mk("S1F4_svid_200", s1f4(200, 2)),
		mk("S6F11_event_10", s6f11(10, 3)),
		mk("S6F11_event_100", s6f11(100, 4)),
		mk("S6F11_trace_1k", s6f11Trace(1000, 5)),
		mk("S6F11_trace_10k", s6f11Trace(10000, 6)),
		mk("S6F11_traceQ3_10k", s6f11TraceQ(10000, 6, 3)),
		mk("S6F11_traceQ1_10k", s6f11TraceQ(10000, 6, 1)),
		mk("S6F11_traceF4_10k", s6f11TraceF4(10000, 6)),
		mk("S6F11_traceU2_10k", s6f11TraceU2(10000, 6)),
		mk("S7F3_recipe_20step", s7f3(20, 7)),
		mk("S7F3_recipe_500step", s7f3(500, 8)),
		mk("WaferMap_100k", waferMap(100_000, 9)),
	}
}

// TrainingCorpus returns many same-shape-different-seed messages, standing in
// for a real captured link trace. Used to train the zstd dictionary and to
// measure cross-message redundancy capture.
func TrainingCorpus() [][]byte {
	var out [][]byte
	for s := int64(100); s < 400; s++ {
		out = append(out, s1f4(20, s).ToBytes())
		out = append(out, s6f11(10, s).ToBytes())
		out = append(out, s6f11(100, s).ToBytes())
		out = append(out, s2f41(s).ToBytes())
		out = append(out, s1f2(s).ToBytes())
	}

	return out
}

// HoldoutCorpus returns messages of the same shapes but seeds disjoint from
// TrainingCorpus — dictionary results must be measured on these, never on the
// training set itself.
func HoldoutCorpus() []Sample {
	var out []Sample
	for s := int64(900); s < 940; s++ {
		out = append(out, Sample{Name: "S1F4_svid_20", Body: s1f4(20, s).ToBytes()})
		out = append(out, Sample{Name: "S6F11_event_10", Body: s6f11(10, s).ToBytes()})
		out = append(out, Sample{Name: "S6F11_event_100", Body: s6f11(100, s).ToBytes()})
		out = append(out, Sample{Name: "S2F41_hostcmd", Body: s2f41(s).ToBytes()})
		out = append(out, Sample{Name: "S1F2_online", Body: s1f2(s).ToBytes()})
	}

	return out
}

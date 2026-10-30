package main

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/arloliu/go-secs/tracepack"
)

// reportWriter writes the lines of a report through a buffer and keeps the first write error.
type reportWriter struct {
	w   *bufio.Writer
	err error
}

// newReportWriter returns a reportWriter over w.
func newReportWriter(w io.Writer) *reportWriter {
	return &reportWriter{w: bufio.NewWriter(w)}
}

// linef writes one line, name: value, the value formatted as fmt.Sprintf does.
func (r *reportWriter) linef(name, format string, args ...any) {
	if r.err != nil {
		return
	}
	_, r.err = fmt.Fprintf(r.w, name+": "+format+"\n", args...)
}

// blank writes an empty line.
func (r *reportWriter) blank() {
	if r.err != nil {
		return
	}
	r.err = r.w.WriteByte('\n')
}

// flush writes out what is buffered and returns the first write error.
func (r *reportWriter) flush() error {
	if r.err == nil {
		r.err = r.w.Flush()
	}

	return r.err
}

// defectText formats a defect of a read of one pack: its reason, block, offset, coverage entry, boundary and error.
func defectText(d tracepack.Defect) string {
	var b strings.Builder
	b.WriteString(d.Reason.String())
	if d.Block >= 0 {
		fmt.Fprintf(&b, " block %d", d.Block)
	}
	if d.Offset >= 0 {
		fmt.Fprintf(&b, " offset %d", d.Offset)
	}
	if d.Coverage != nil {
		b.WriteString(" ")
		b.WriteString(coverageText(d.Coverage))
	}
	if d.Boundary != nil {
		b.WriteString(" ")
		b.WriteString(boundaryText(d.Boundary))
	}
	appendErr(&b, d.Err)

	return b.String()
}

// coverageText formats a coverage entry; a field it lacks is shown as "?".
func coverageText(c *tracepack.Coverage) string {
	capture := "?"
	if c.CaptureID != nil {
		capture = c.CaptureID.String()
	}

	return fmt.Sprintf("coverage capture %s seqs %s-%s time %s..%s",
		capture, optU64(c.SeqFirst), optU64(c.SeqLast), optI64(c.TimeStart), optI64(c.TimeEnd))
}

// boundaryText formats a capture-boundary entry or record; a gap bound it lacks is shown as "?".
func boundaryText(bd *tracepack.Boundary) string {
	return fmt.Sprintf("boundary capture %s seq %d kind %s ts %d epoch %d gap %s..%s",
		bd.CaptureID, bd.Seq, bd.Kind, bd.TS, bd.Epoch, optI64(bd.GapStart), optI64(bd.GapEnd))
}

// appendErr appends ": err" to b, or nothing for a nil err.
func appendErr(b *strings.Builder, err error) {
	if err != nil {
		b.WriteString(": ")
		b.WriteString(err.Error())
	}
}

// optU64 formats an optional uint64; nil is "?".
func optU64(v *uint64) string {
	if v == nil {
		return "?"
	}

	return strconv.FormatUint(*v, 10)
}

// optI64 formats an optional int64; nil is "?".
func optI64(v *int64) string {
	if v == nil {
		return "?"
	}

	return strconv.FormatInt(*v, 10)
}

// namesText joins names with commas; none is "none".
func namesText(names []string) string {
	if len(names) == 0 {
		return "none"
	}

	return strings.Join(names, ",")
}

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/arloliu/go-secs/tracepack"
)

const findUsage = "tracepack find --capture <uuid> --seq <n> (--hour <n> | --at <time>) <path>..."

// errNoPacks is the error of a find whose paths name no pack.
var errNoPacks = errors.New("the paths name no pack")

// runFind runs find: the transaction lookup of one primary over the packs the paths name.
func runFind(ctx context.Context, cfg config, args []string, stdout, stderr io.Writer) int {
	fset := newFlagSet("find")
	capture := fset.String("capture", "", "the primary's capture_id, a `uuid`")
	seq := fset.Uint64("seq", 0, "the primary's seq")
	hour := fset.Int64("hour", 0, "the primary's UTC hour since 1970-01-01T00:00Z")
	at := fset.String("at", "", "the primary's timestamp, an RFC 3339 `time`; instead of --hour")
	if code := parseFlags(fset, findUsage, args, stdout, stderr); code >= 0 {
		return code
	}
	set := map[string]bool{}
	fset.Visit(func(f *flag.Flag) { set[f.Name] = true })
	key, msg := findKey(set, *capture, *seq, *hour, *at)
	if msg != "" {
		return usageError(fset, findUsage, stderr, msg)
	}
	if fset.NArg() == 0 {
		return usageError(fset, findUsage, stderr, "find takes one or more paths")
	}

	paths, err := packPaths(fset.Args())
	if err != nil {
		return failed(stderr, "find", "", err)
	}
	if len(paths) == 0 {
		return failed(stderr, "find", "", errNoPacks)
	}
	var files []packFile
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	readers := make([]*tracepack.Reader, 0, len(paths))
	for _, path := range paths {
		f, size, err := cfg.open(path)
		if err != nil {
			return failed(stderr, "find", path, err)
		}
		files = append(files, f)
		r, err := tracepack.Open(ctx, f, size, cfg.reader)
		if err != nil {
			return failed(stderr, "find", path, err)
		}
		readers = append(readers, r)
	}
	src, err := tracepack.NewReaderSource(readers)
	if err != nil {
		return failed(stderr, "find", "", err)
	}
	res, err := tracepack.FindTransaction(ctx, src, key, cfg.tx)
	if err != nil {
		return failed(stderr, "find", "", err)
	}

	rw := newReportWriter(stdout)
	writeFindReport(rw, key, &res)
	if err := rw.flush(); err != nil {
		return failed(stderr, "find", "", err)
	}
	if len(res.Incomplete) > 0 {
		return exitNotClean
	}

	return exitClean
}

// findKey returns the lookup's key from the flags, set naming those given.
//
// Returns:
//   - tracepack.TxKey: the key.
//   - string: what is wrong with the flags; empty when nothing is.
func findKey(set map[string]bool, capture string, seq uint64, hour int64, at string) (tracepack.TxKey, string) {
	if !set["capture"] || !set["seq"] {
		return tracepack.TxKey{}, "find needs --capture and --seq"
	}
	id, err := tracepack.ParseUUID(capture)
	if err != nil {
		return tracepack.TxKey{}, fmt.Sprintf("--capture: %v", err)
	}
	switch {
	case set["hour"] == set["at"]:
		return tracepack.TxKey{}, "find needs exactly one of --hour and --at"
	case set["at"]:
		t, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return tracepack.TxKey{}, fmt.Sprintf("--at: %v", err)
		}

		return tracepack.TxKeyOf(id, seq, t), ""
	default:
		return tracepack.TxKey{Capture: id, Seq: seq, Hour: hour}, ""
	}
}

// packPaths returns the packs args name: a path that is not a directory as it is,
// and for a directory, every regular file named *.tpk below it, in lexical order, symbolic links not followed.
// A hidden directory below it, such as the .partial directory of a directory sink's unpublished segments, is skipped.
func packPaths(args []string) ([]string, error) {
	var paths []string
	for _, arg := range args {
		fi, err := os.Stat(arg)
		if err != nil {
			return nil, err
		}
		if !fi.IsDir() {
			paths = append(paths, arg)

			continue
		}
		err = filepath.WalkDir(arg, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && path != arg && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			if d.Type().IsRegular() && strings.HasSuffix(d.Name(), ".tpk") {
				paths = append(paths, path)
			}

			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	return paths, nil
}

// writeFindReport writes the report of the lookup of key.
func writeFindReport(rw *reportWriter, key tracepack.TxKey, res *tracepack.TxResult) {
	rw.linef("outcome", "%s", res.Outcome)
	rw.linef("complete", "%t", len(res.Incomplete) == 0)
	rw.linef("key", "capture %s seq %d hour %d", key.Capture, key.Seq, key.Hour)
	stream, function := "unavailable", strconv.Itoa(int(res.Function))
	if res.StreamAvailable {
		stream = strconv.Itoa(int(res.Stream))
	}
	w := "unavailable"
	if res.WAvailable {
		w = strconv.FormatBool(res.W)
	}
	rw.linef("transaction", "epoch %d dir %s session 0x%04x system-bytes %x stream %s function %s w %s",
		res.Epoch, res.Dir, res.SessionID, res.SystemBytes, stream, function, w)
	if res.WindowEnd == nil {
		rw.linef("window-end", "open")
	} else {
		rw.linef("window-end", "%d", *res.WindowEnd)
	}
	for k := range res.Records {
		rw.linef("record", "%s", txRecordText(&res.Records[k]))
	}
	for k := range res.Incomplete {
		rw.linef("incomplete", "%s", txGapText(&res.Incomplete[k]))
	}
	for _, s := range res.Searched {
		rw.linef("searched", "hour %d packs %d", s.Hour, len(s.Packs))
	}
	for _, c := range res.Conflicts {
		rw.linef("conflict", "capture %s seq %d versions %d", c.CaptureID, c.Seq, len(c.Versions))
	}
	for _, fe := range res.FooterErrs {
		rw.linef("footer-error", "hour %d pack %s: %v", fe.Hour, fe.Pack, fe.Err)
	}
}

// txRecordText formats one kept version: its seq, kind, hour, pack, block, roles and the flags set.
func txRecordText(tr *tracepack.TxRecord) string {
	var flags []string
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"in-window", tr.InWindow}, {"bound", tr.Bound}, {"conflict", tr.Conflict},
		{"decidable", tr.Decidable}, {"eligible", tr.Eligible}, {"valid", tr.Valid},
	} {
		if f.set {
			flags = append(flags, f.name)
		}
	}

	return fmt.Sprintf("seq %d kind %s hour %d pack %s block %d roles %s flags %s",
		tr.Record.Seq, tr.Record.Kind, tr.Hour, tr.Pack, tr.Block, namesText(tr.Class.Names()), namesText(flags))
}

// txGapText formats one reason a lookup is incomplete.
func txGapText(g *tracepack.TxGap) string {
	var b strings.Builder
	b.WriteString(g.Reason.String())
	if len(g.Hours) > 0 {
		hours := make([]string, len(g.Hours))
		for k, h := range g.Hours {
			hours[k] = strconv.FormatInt(h, 10)
		}
		fmt.Fprintf(&b, " hours %s", strings.Join(hours, ","))
	}
	if g.Pack != nil {
		fmt.Fprintf(&b, " pack %s", g.Pack)
	}
	if g.Block >= 0 {
		fmt.Fprintf(&b, " block %d", g.Block)
	}
	if g.Offset >= 0 {
		fmt.Fprintf(&b, " offset %d", g.Offset)
	}
	if g.Seq != nil {
		fmt.Fprintf(&b, " seq %d", *g.Seq)
	}
	if g.Defect != 0 {
		fmt.Fprintf(&b, " defect %s", g.Defect)
	}
	if g.Coverage != nil {
		b.WriteString(" ")
		b.WriteString(coverageText(g.Coverage))
	}
	if g.Boundary != nil {
		b.WriteString(" ")
		b.WriteString(boundaryText(g.Boundary))
	}
	appendErr(&b, g.Err)

	return b.String()
}

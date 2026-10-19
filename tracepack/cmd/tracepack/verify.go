package main

import (
	"context"
	"io"

	"github.com/arloliu/go-secs/tracepack"
)

const verifyUsage = "tracepack verify <pack>..."

// runVerify runs verify: each pack verified in turn, its report on stdout, the largest exit code of the packs.
func runVerify(ctx context.Context, cfg config, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("verify")
	if code := parseFlags(fs, verifyUsage, args, stdout, stderr); code >= 0 {
		return code
	}
	if fs.NArg() == 0 {
		return usageError(fs, verifyUsage, stderr, "verify takes one or more packs")
	}

	rw := newReportWriter(stdout)
	code := exitClean
	reported := false
	for _, path := range fs.Args() {
		if ctx.Err() != nil {
			code = max(code, failed(stderr, "verify", "", ctx.Err()))

			break
		}
		rep, c := verifyOne(ctx, cfg, path, stderr)
		code = max(code, c)
		if rep == nil {
			continue
		}
		if reported {
			rw.blank()
		}
		writeVerifyReport(rw, path, rep)
		reported = true
		// Flush each report before the next pack, so that it precedes the next pack's errors.
		if err := rw.flush(); err != nil {
			return failed(stderr, "verify", "", err)
		}
	}

	return code
}

// verifyOne verifies the pack at path and returns its report and exit code;
// it writes an error to stderr, and closes the pack before returning.
//
// Returns:
//   - *tracepack.VerifyReport: the report; nil on an error.
//   - int: the exit code.
func verifyOne(ctx context.Context, cfg config, path string, stderr io.Writer) (*tracepack.VerifyReport, int) {
	f, size, err := cfg.open(path)
	if err != nil {
		return nil, failed(stderr, "verify", path, err)
	}
	defer f.Close()
	rep, err := tracepack.Verify(ctx, f, size, tracepack.VerifyOptions{Reader: cfg.reader})
	if err != nil {
		return nil, failed(stderr, "verify", path, err)
	}
	if rep.Outcome != tracepack.OutcomeFinalizedConsistent {
		return &rep, exitNotClean
	}

	return &rep, exitClean
}

// writeVerifyReport writes the report of the pack at path.
func writeVerifyReport(rw *reportWriter, path string, rep *tracepack.VerifyReport) {
	rw.linef("pack", "%s", path)
	rw.linef("outcome", "%s", rep.Outcome)
	rw.linef("complete", "%t", len(rep.Failed) == 0 && len(rep.Disagreements) == 0)
	rw.linef("finalized", "%t", rep.Finalized)
	rw.linef("blocks", "%d located, %d validated", rep.Blocks, rep.Validated)
	rw.linef("records", "%d", rep.Records)
	rw.linef("prefix-end", "%d", rep.PrefixEnd)
	if rep.FooterErr != nil {
		rw.linef("footer-error", "%v", rep.FooterErr)
	}
	for _, d := range rep.Failed {
		rw.linef("failed", "%s", defectText(d))
	}
	for _, d := range rep.Disagreements {
		rw.linef("disagreement", "%s", defectText(d))
	}
	for _, wd := range rep.WriterDefects {
		rw.linef("writer-defect", "%s block %d offset %d seq %d: %v", wd.Kind, wd.Block, wd.Offset, wd.Seq, wd.Err)
	}
	for k := range rep.Lost {
		rw.linef("lost", "%s", coverageText(&rep.Lost[k]))
	}
}

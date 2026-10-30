package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/arloliu/go-secs/tracepack"
)

// Exit codes; see the package documentation.
const (
	exitClean    = 0
	exitNotClean = 1
	exitFailed   = 2
)

const usageText = `Usage:
  tracepack dump --jsonl [-o <file>] <pack>
  tracepack verify <pack>...
  tracepack find --capture <uuid> --seq <n> (--hour <n> | --at <time>) <path>...

Flags come before the paths. Run "tracepack <command> -h" for a command's flags.
find holds every pack it is given open during the lookup, a file descriptor and
the pack's index in memory each.
The flags, exit codes, stdout/stderr split and dump --jsonl bytes are contract;
the text of the verify and find reports is not, and may change in any release.

Exit codes: 0 the report is clean; 1 the report is not clean (an incomplete read
or lookup, a verify outcome other than finalized-consistent); 2 the command could
not run.
`

// errNotRegular is the error of a path that is not a regular file.
var errNotRegular = errors.New("not a regular file")

// packFile is an opened pack: its bytes and their closing.
type packFile interface {
	io.ReaderAt
	io.Closer
}

// config is what run gives every command: the library's options and the opening of a path.
// Only tests set anything but the defaults.
type config struct {
	reader tracepack.ReaderOptions
	tx     tracepack.TxOptions
	// open opens the pack at path and returns it with its size.
	open func(path string) (packFile, int64, error)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// Receiving SIGPIPE makes a write to a closed stdout or stderr fail with EPIPE instead of killing the process,
	// so a command whose reader went away still exits 2 and says what it wrote is not an export.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// defaultConfig returns the configuration of the command line: the library's defaults and the file system.
func defaultConfig() config {
	return config{open: openFile}
}

// run runs the command line args, the program name excluded, and returns the exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runWith(ctx, defaultConfig(), args, stdout, stderr)
}

// runWith is run with the configuration cfg.
func runWith(ctx context.Context, cfg config, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)

		return exitFailed
	}

	switch args[0] {
	case "dump":
		return runDump(ctx, cfg, args[1:], stdout, stderr)
	case "verify":
		return runVerify(ctx, cfg, args[1:], stdout, stderr)
	case "find":
		return runFind(ctx, cfg, args[1:], stdout, stderr)
	case "help", "-h", "-help", "--help":
		if err := writeString(stdout, usageText); err != nil {
			return failed(stderr, "help", "", err)
		}

		return exitClean
	default:
		fmt.Fprintf(stderr, "tracepack: unknown command %q\n\n%s", args[0], usageText)

		return exitFailed
	}
}

// openFile opens the regular file at path and refuses another kind of file.
// It opens with openFlags, non-blocking where the system has it, since opening a FIFO without a writer would block where no interrupt reaches;
// a regular file reads the same either way.
func openFile(path string) (packFile, int64, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|openFlags, 0) //nolint:gosec // reading the file the user names is the command's purpose
	if err != nil {
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()

		return nil, 0, err
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()

		return nil, 0, errNotRegular
	}

	return f, fi.Size(), nil
}

// newFlagSet returns the flag set of command name, which reports no error of its own:
// parseFlags does.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}

	return fs
}

// parseFlags parses args into fs.
// On -h it writes the command's usage and flags to stdout; on an error, the error, the usage and the flags to stderr.
//
// Returns:
//   - int: the exit code when the command is to stop, exitClean for -h or exitFailed for an error; -1 to go on.
func parseFlags(fs *flag.FlagSet, usage string, args []string, stdout, stderr io.Writer) int {
	err := fs.Parse(args)
	if err == nil {
		return -1
	}
	if errors.Is(err, flag.ErrHelp) {
		if err := printUsage(stdout, fs, usage); err != nil {
			return failed(stderr, fs.Name(), "", err)
		}

		return exitClean
	}
	fmt.Fprintf(stderr, "tracepack %s: %v\n", fs.Name(), err)
	_ = printUsage(stderr, fs, usage)

	return exitFailed
}

// usageError writes msg, the usage and the flags of fs to stderr and returns exitFailed.
func usageError(fs *flag.FlagSet, usage string, stderr io.Writer, msg string) int {
	fmt.Fprintf(stderr, "tracepack %s: %s\n", fs.Name(), msg)
	_ = printUsage(stderr, fs, usage)

	return exitFailed
}

// printUsage writes usage and the flags of fs to w and returns the write's error.
func printUsage(w io.Writer, fs *flag.FlagSet, usage string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Usage:\n  %s\n\nFlags:\n", usage)
	fs.SetOutput(&b)
	fs.PrintDefaults()
	fs.SetOutput(io.Discard)

	return writeString(w, b.String())
}

// writeString writes s to w, a write that takes fewer bytes without an error failing with io.ErrShortWrite.
func writeString(w io.Writer, s string) error {
	n, err := io.WriteString(w, s)
	if err == nil && n < len(s) {
		err = io.ErrShortWrite
	}

	return err
}

// failed writes the error of command name on path to stderr and returns exitFailed.
func failed(stderr io.Writer, name, path string, err error) int {
	if path == "" {
		fmt.Fprintf(stderr, "tracepack %s: %v\n", name, err)
	} else {
		fmt.Fprintf(stderr, "tracepack %s: %s: %v\n", name, path, err)
	}

	return exitFailed
}

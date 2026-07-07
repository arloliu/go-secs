// Command gemgen generates gem/ SECS-II message builders from a YAML DSL.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

func main() {
	itemsPath := flag.String("items", "", "path to items.yaml (the Data Item Dictionary)")
	msgsDir := flag.String("messages", "", "directory of per-stream messages/*.yaml files")
	outDir := flag.String("out", "", "output directory for generated gem/*.go files")
	flag.Parse()

	if err := run(*itemsPath, *msgsDir, *outDir); err != nil {
		fmt.Fprintln(os.Stderr, "gemgen:", err)
		os.Exit(1)
	}
}

// run loads the items dictionary and every stream's messages file under
// msgsDir, validates them, renders items.go/sN.go/sN_test.go, and writes the
// results under outDir. It is the whole CLI's orchestration, split out from
// main so it is testable without a subprocess.
func run(itemsPath, msgsDir, outDir string) error {
	itemsData, err := os.ReadFile(itemsPath)
	if err != nil {
		return fmt.Errorf("read items file: %w", err)
	}

	items, err := LoadItems(itemsData)
	if err != nil {
		return err
	}

	if err := ValidateItems(items); err != nil {
		return err
	}

	msgFiles, err := loadMessageFiles(msgsDir)
	if err != nil {
		return err
	}

	if err := ValidateMessages(msgFiles, items); err != nil {
		return err
	}

	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return fmt.Errorf("create out dir: %w", err)
	}

	itemsSrc, err := renderItems(items)
	if err != nil {
		return fmt.Errorf("render items.go: %w", err)
	}

	if err := os.WriteFile(filepath.Join(outDir, "items.go"), itemsSrc, 0o600); err != nil {
		return fmt.Errorf("write items.go: %w", err)
	}

	for _, mf := range msgFiles {
		if err := writeMessageFile(mf, items, outDir); err != nil {
			return err
		}
	}

	return nil
}

// loadMessageFiles globs and loads every *.yaml file under msgsDir into a
// MessageFile, sorted by path so the generated output order is deterministic.
func loadMessageFiles(msgsDir string) ([]MessageFile, error) {
	paths, err := filepath.Glob(filepath.Join(msgsDir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("glob messages dir: %w", err)
	}

	slices.Sort(paths)

	msgFiles := make([]MessageFile, 0, len(paths))
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}

		mf, err := LoadMessageFile(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}

		msgFiles = append(msgFiles, mf)
	}

	return msgFiles, nil
}

// writeMessageFile renders and writes one stream's sN.go and sN_test.go.
func writeMessageFile(mf MessageFile, items map[string]Item, outDir string) error {
	msgSrc, err := renderMessages(mf, items)
	if err != nil {
		return fmt.Errorf("render s%d.go: %w", mf.Stream, err)
	}

	msgPath := filepath.Join(outDir, fmt.Sprintf("s%d.go", mf.Stream))
	if err := os.WriteFile(msgPath, msgSrc, 0o600); err != nil {
		return fmt.Errorf("write s%d.go: %w", mf.Stream, err)
	}

	testSrc, err := renderTests(mf, items)
	if err != nil {
		return fmt.Errorf("render s%d_test.go: %w", mf.Stream, err)
	}

	testPath := filepath.Join(outDir, fmt.Sprintf("s%d_test.go", mf.Stream))
	if err := os.WriteFile(testPath, testSrc, 0o600); err != nil {
		return fmt.Errorf("write s%d_test.go: %w", mf.Stream, err)
	}

	return nil
}

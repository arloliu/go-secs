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
	groupsPath := flag.String("groups", "", "path to groups.yaml (the named list shapes messages repeat); optional")
	msgsDir := flag.String("messages", "", "directory of per-stream messages/*.yaml files")
	outDir := flag.String("out", "", "output directory for generated gem/*.go files")
	flag.Parse()

	if err := run(*itemsPath, *groupsPath, *msgsDir, *outDir); err != nil {
		fmt.Fprintln(os.Stderr, "gemgen:", err)
		os.Exit(1)
	}
}

// run loads the items dictionary, the groups file, and every stream's messages file under msgsDir,
// validates them, renders items.go, groups.go, decode.go, and each stream's files,
// and writes the results under outDir.
// An empty groupsPath means no groups, and no groups.go is written.
// It is the whole CLI's orchestration,
// split out from main so it is testable without a subprocess.
func run(itemsPath, groupsPath, msgsDir, outDir string) error {
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

	groups, err := loadGroupsFile(groupsPath)
	if err != nil {
		return err
	}

	if err := ResolveGroups(groups, msgFiles, items); err != nil {
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

	if len(groups) > 0 {
		groupsSrc, err := renderGroups(groups, items)
		if err != nil {
			return fmt.Errorf("render groups.go: %w", err)
		}

		if err := os.WriteFile(filepath.Join(outDir, "groups.go"), groupsSrc, 0o600); err != nil {
			return fmt.Errorf("write groups.go: %w", err)
		}
	}

	supportSrc, err := renderDecodeSupport()
	if err != nil {
		return fmt.Errorf("render decode.go: %w", err)
	}

	if err := os.WriteFile(filepath.Join(outDir, "decode.go"), supportSrc, 0o600); err != nil {
		return fmt.Errorf("write decode.go: %w", err)
	}

	for _, mf := range msgFiles {
		if err := writeMessageFile(mf, items, outDir); err != nil {
			return err
		}
	}

	return nil
}

// loadGroupsFile reads and parses the groups file at path,
// or returns no groups for an empty path.
func loadGroupsFile(path string) ([]Group, error) {
	if path == "" {
		return nil, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read groups file: %w", err)
	}

	return LoadGroups(data)
}

// loadMessageFiles globs and loads every *.yaml file under msgsDir into a
// MessageFile, sorted by path so the generated output order is deterministic.
// Each file must be named after the stream it declares (sN.yaml for stream: N),
// since the stream number alone picks the gem/sN*.go files it writes.
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

		if want := fmt.Sprintf("s%d.yaml", mf.Stream); filepath.Base(p) != want {
			return nil, fmt.Errorf("%s: declares stream %d, so the file must be named %s", p, mf.Stream, want)
		}

		msgFiles = append(msgFiles, mf)
	}

	return msgFiles, nil
}

// writeMessageFile renders and writes one stream's sN.go, sN_test.go, sN_decode.go, and
// sN_decode_test.go.
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

	decodeSrc, err := renderDecoders(mf, items)
	if err != nil {
		return fmt.Errorf("render s%d_decode.go: %w", mf.Stream, err)
	}

	decodePath := filepath.Join(outDir, fmt.Sprintf("s%d_decode.go", mf.Stream))
	if err := os.WriteFile(decodePath, decodeSrc, 0o600); err != nil {
		return fmt.Errorf("write s%d_decode.go: %w", mf.Stream, err)
	}

	decodeTestSrc, err := renderDecodeTests(mf, items)
	if err != nil {
		return fmt.Errorf("render s%d_decode_test.go: %w", mf.Stream, err)
	}

	decodeTestPath := filepath.Join(outDir, fmt.Sprintf("s%d_decode_test.go", mf.Stream))
	if err := os.WriteFile(decodeTestPath, decodeTestSrc, 0o600); err != nil {
		return fmt.Errorf("write s%d_decode_test.go: %w", mf.Stream, err)
	}

	return nil
}

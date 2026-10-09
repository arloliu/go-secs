package corpus

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/codec"
)

// update regenerates the corpus: go test ./internal/corpus -run '^TestCorpus$' -update.
var update = flag.Bool("update", false, "regenerate the conformance corpus under tracepack/testdata/corpus")

// corpusDir is the corpus root, from this package's directory.
const corpusDir = "../../testdata/corpus"

// maxCorpusBytes bounds the sum of the sizes of every file under the corpus root.
const maxCorpusBytes = 2_000_000

// keptRootFiles are the files at the corpus root that the generator does not write and -update keeps.
var keptRootFiles = []string{".gitattributes", "README.md"}

// offsetFiles are the files a regeneration under another zstd encoder does not compare for a vector that depends on it:
// they hold offsets, which follow the compressed sizes (the tracepack corpus specification §6.1).
var offsetFiles = []string{FileVerify, FileQueries, FileTruncation, FilePatch, FilePatchVerify}

// TestCorpus regenerates the corpus and compares it with the committed files,
// byte for byte when the linked zstd encoder is the one the manifest records,
// else by decoded equivalence for the vectors that depend on the encoder (the tracepack corpus specification §6.1);
// then reads every committed vector again, strictly, as a reader of the corpus does;
// and checks the corpus's layout and size.
// With -update it first writes the regenerated corpus.
func TestCorpus(t *testing.T) {
	t.Parallel()

	gen, err := Generate()
	require.NoError(t, err)
	if *update {
		require.NoError(t, writeCorpus(corpusDir, gen))
	}

	committed, err := readTree(corpusDir)
	require.NoError(t, err)
	var m Manifest
	require.NoError(t, Unmarshal(committed[FileManifest], &m), "the committed manifest")

	linked, linkErr := LinkedZstdEncoder(GoListEncoder)
	strict := encoderMatches(linked, linkErr)(m.ZstdEncoder)
	if !strict {
		t.Logf("the linked zstd encoder (%q, error %v) is not the manifest's %q: encoder-made vectors are compared by decoded equivalence; "+
			"the corpus is due for regeneration with -update", linked, linkErr, m.ZstdEncoder)
	}
	dependent, err := encoderMadeIDs()
	require.NoError(t, err)
	for _, p := range compareCorpus(gen, committed, strict, dependent) {
		t.Error(p)
	}

	for _, p := range checkLayout(committed, &m) {
		t.Error(p)
	}
	for i := range m.Vectors {
		v := m.Vectors[i]
		t.Run(v.ID, func(t *testing.T) {
			t.Parallel()
			require.NoError(t, readCommitted(t.Context(), &v, committed))
		})
	}

	var total int
	for _, b := range committed {
		total += len(b)
	}
	t.Logf("the corpus holds %d files, %d bytes", len(committed), total)
	require.NoError(t, checkSize(total))
}

// encoderMatches returns whether a recorded zstd_encoder is the linked encoder, the results of LinkedZstdEncoder:
// an encoder that cannot be established matches no record.
func encoderMatches(linked string, err error) func(recorded string) bool {
	return func(recorded string) bool { return err == nil && linked == recorded }
}

// encoderMadeIDs returns whether each vector depends on the zstd encoder, by its recipe.
func encoderMadeIDs() (func(id string) bool, error) {
	recipes, err := Recipes()
	if err != nil {
		return nil, err
	}

	return func(id string) bool {
		k := slices.IndexFunc(recipes, func(r Recipe) bool { return r.ID == id })

		return k >= 0 && recipes[k].EncoderMade
	}, nil
}

// compareCorpus compares the regenerated files gen with the committed ones and returns every difference.
// When strict, every file is compared byte for byte.
// Otherwise the manifest is compared except its zstd_encoder,
// and a vector that depends on the encoder has its pack compared by decoded equivalence and its offset-bearing files skipped;
// every other file is compared byte for byte.
// A file in one set and not the other is a difference, except the root files the generator does not write.
func compareCorpus(gen, committed map[string][]byte, strict bool, dependent func(id string) bool) []string {
	var keys []string
	for k := range gen {
		keys = append(keys, k)
	}
	for k := range committed {
		if _, ok := gen[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)

	var problems []string
	for _, k := range keys {
		g, inGen := gen[k]
		c, inCommitted := committed[k]
		switch {
		case !inCommitted:
			problems = append(problems, k+": generated but not committed")
		case !inGen:
			if !slices.Contains(keptRootFiles, k) {
				problems = append(problems, k+": committed but not generated")
			}
		case k == FileManifest && !strict:
			if err := sameManifestButEncoder(g, c); err != nil {
				problems = append(problems, k+": "+err.Error())
			}
		case strict || !dependent(path.Dir(k)):
			if !bytes.Equal(g, c) {
				problems = append(problems, k+": differs from the committed file")
			}
		case path.Base(k) == FilePack:
			if err := decodedEqual(g, c); err != nil {
				problems = append(problems, k+": "+err.Error())
			}
		case slices.Contains(offsetFiles, path.Base(k)):
			// Holds offsets that follow the encoder's output.
		default:
			if !bytes.Equal(g, c) {
				problems = append(problems, k+": differs from the committed file")
			}
		}
	}

	return problems
}

// sameManifestButEncoder reports a difference between the manifests gen and committed other than their zstd_encoder.
func sameManifestButEncoder(gen, committed []byte) error {
	var g, c Manifest
	if err := Unmarshal(gen, &g); err != nil {
		return err
	}
	if err := Unmarshal(committed, &c); err != nil {
		return err
	}
	g.ZstdEncoder = c.ZstdEncoder
	b, err := g.Marshal()
	if err != nil {
		return err
	}
	if !bytes.Equal(b, committed) {
		return errors.New("differs from the committed manifest beyond zstd_encoder")
	}

	return nil
}

// decodedEqual reports a difference between the packs a and b beyond the encoder's output:
// their file headers and pack metadata, and block by block their codecs and decoded bodies when the block is a decodable zstd block,
// else their stored bytes.
// Envelopes of zstd blocks, the footer and the trailer are not compared: they follow the compressed sizes.
func decodedEqual(a, b []byte) error {
	la, err := Locate(a)
	if err != nil {
		return err
	}
	lb, err := Locate(b)
	if err != nil {
		return err
	}
	if la.MetaEnd != lb.MetaEnd || !bytes.Equal(a[:la.MetaEnd], b[:lb.MetaEnd]) {
		return errors.New("the file header or pack metadata differs")
	}
	if len(la.Blocks) != len(lb.Blocks) || la.Finalized != lb.Finalized {
		return errors.New("the block count or finalization differs")
	}
	for i := range la.Blocks {
		ba := a[la.Blocks[i].Offset : la.Blocks[i].Offset+la.Blocks[i].Len]
		bb := b[lb.Blocks[i].Offset : lb.Blocks[i].Offset+lb.Blocks[i].Len]
		if ba[envelopeCodecOff] != bb[envelopeCodecOff] {
			return fmt.Errorf("block %d: the codec differs", i)
		}
		da, _, errA := decodeBlock(ba)
		db, _, errB := decodeBlock(bb)
		if ba[envelopeCodecOff] == codec.Zstd && errA == nil && errB == nil {
			if !bytes.Equal(da, db) {
				return fmt.Errorf("block %d: the decoded body differs", i)
			}

			continue
		}
		if !bytes.Equal(ba, bb) {
			return fmt.Errorf("block %d differs", i)
		}
	}

	return nil
}

// checkLayout checks the committed files against the manifest m:
// the root holds only the manifest, primitives.json and the kept files,
// and each vector's directory exactly the files its entry lists.
func checkLayout(committed map[string][]byte, m *Manifest) []string {
	var problems []string
	listed := make(map[string]bool)
	for _, v := range m.Vectors {
		for _, f := range v.Files {
			k := v.ID + "/" + f
			listed[k] = true
			if _, ok := committed[k]; !ok {
				problems = append(problems, k+": listed in the manifest, missing")
			}
		}
	}
	for k := range committed {
		root := !strings.Contains(k, "/")
		if (root && !slices.Contains(append([]string{FileManifest, FilePrimitives}, keptRootFiles...), k)) || (!root && !listed[k]) {
			problems = append(problems, k+": not listed in the manifest (an orphan)")
		}
	}
	slices.Sort(problems)

	return problems
}

// readCommitted reads the committed vector v as a reader of the corpus does and requires every file it gives to equal the committed one:
// its export or rejection, its verification report, and every optional file, with the inputs those files hold
// (the query filters, the classification, the repair options).
func readCommitted(ctx context.Context, v *Vector, committed map[string][]byte) error {
	file := func(name string) ([]byte, bool) {
		b, ok := committed[v.ID+"/"+name]
		return b, ok
	}
	pack, ok := file(FilePack)
	if !ok {
		return errors.New("no pack.tpk")
	}

	in := &readInputs{class: v.Class}
	if b, ok := file(FileQueries); ok {
		var qs Queries
		if err := Unmarshal(b, &qs); err != nil {
			return err
		}
		for _, q := range qs {
			in.queries = append(in.queries, QuerySpec{ID: q.ID, Cites: q.Cites, Filter: q.Filter})
		}
	}
	_, in.fields = file(FileFields)
	_, in.footer = file(FileFooter)
	if b, ok := file(FileClassify); ok {
		in.classify = new(Classify)
		if err := Unmarshal(b, in.classify); err != nil {
			return err
		}
	}
	if b, ok := file(FileRepair); ok {
		var r Repair
		if err := Unmarshal(b, &r); err != nil {
			return err
		}
		in.repair = &r.Options
	}
	if b, ok := file(FileTruncation); ok {
		var tr Truncation
		if err := Unmarshal(b, &tr); err != nil {
			return err
		}
		in.repair = tr.RepairOptions
	}

	out, err := readVector(ctx, pack, in)
	if err != nil {
		return err
	}
	var names []string
	for _, f := range out.files {
		names = append(names, f.name)
		want, ok := file(f.name)
		if !ok {
			return fmt.Errorf("the read gives %s, which is not committed", f.name)
		}
		if !bytes.Equal(want, f.data) {
			return fmt.Errorf("%s differs from what the read gives", f.name)
		}
	}
	if !slices.Equal(names, v.Files) {
		return fmt.Errorf("the read gives %v, the manifest lists %v", names, v.Files)
	}
	if v.Class == ClassRejection {
		return nil
	}

	return checkRecordCount(out.seqs, &out.verify)
}

// checkRecordCount checks that an export of the record seqs holds the records the verification v counts,
// when v reports no failed block and no walk stop.
func checkRecordCount(seqs []uint64, v *Verify) error {
	if len(v.FailedBlocks) == 0 && v.WalkStop == nil && U64(len(seqs)) != v.Records {
		return fmt.Errorf("the export holds %d records, the verification %d", len(seqs), v.Records)
	}

	return nil
}

// checkSize reports a corpus of total bytes over maxCorpusBytes.
func checkSize(total int) error {
	if total > maxCorpusBytes {
		return fmt.Errorf("the corpus holds %d bytes, over %d", total, maxCorpusBytes)
	}

	return nil
}

// readTree returns every file under dir, keyed by its slash-separated path from dir.
func readTree(dir string) (map[string][]byte, error) {
	out := make(map[string][]byte)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		out[filepath.ToSlash(rel)] = b

		return err
	})

	return out, err
}

// writeCorpus writes files under dir, removing every file the generator no longer writes but the kept root files,
// and the directories left empty.
// It refuses a corpus whose manifest does not establish the zstd encoder.
func writeCorpus(dir string, files map[string][]byte) error {
	var m Manifest
	if err := Unmarshal(files[FileManifest], &m); err != nil {
		return err
	}
	if m.ZstdEncoder == UnknownEncoder {
		return errors.New("the linked zstd encoder is not known, so the corpus cannot record it")
	}

	old, err := readTree(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for k := range old {
		if _, ok := files[k]; !ok && !slices.Contains(keptRootFiles, k) {
			if err := os.Remove(filepath.Join(dir, filepath.FromSlash(k))); err != nil {
				return err
			}
			// A vector directory left empty goes too; Remove fails harmlessly on one that is not empty.
			_ = os.Remove(filepath.Join(dir, filepath.FromSlash(path.Dir(k))))
		}
	}

	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		p := filepath.Join(dir, filepath.FromSlash(k))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, files[k], 0o644); err != nil {
			return err
		}
	}

	return nil
}

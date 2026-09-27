package codec_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/arloliu/go-secs/tracepack/internal/codec"
	"github.com/stretchr/testify/require"
)

// zstdFrameMagic is the four-byte signature every RFC 8878 frame starts with,
// independent of this package's own encoder.
var zstdFrameMagic = []byte{0x28, 0xB5, 0x2F, 0xFD}

// emptyZstdFrame is a complete RFC 8878 frame holding no content,
// as the zstd CLI writes it for an empty file with --no-check:
// single segment with a one-byte Frame_Content_Size of 0, then one last Raw block of size 0.
var emptyZstdFrame = []byte{0x28, 0xB5, 0x2F, 0xFD, 0x20, 0x00, 0x01, 0x00, 0x00}

// skippableZstdFrame is an RFC 8878 §3.1.2 skippable frame (magic 0x184D2A50) with no user data.
var skippableZstdFrame = []byte{0x50, 0x2A, 0x4D, 0x18, 0x00, 0x00, 0x00, 0x00}

// errCorrupted flags a concurrent decode whose result did not match the source,
// distinct from a decode error.
var errCorrupted = errors.New("codec_test: decoded result does not match source")

func TestEncodeDecodeZstdRoundTripCompressible(t *testing.T) {
	src := []byte(strings.Repeat("the tracepack format specification section 2 ", 200))

	encoded, err := codec.Encode(codec.Zstd, nil, src)
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(encoded, zstdFrameMagic))
	require.Less(t, len(encoded), len(src), "a repetitive buffer must compress smaller")

	decoded, err := codec.Decode(codec.Zstd, nil, encoded, len(src))
	require.NoError(t, err)
	require.Equal(t, src, decoded)
}

func TestEncodeDecodeZstdRoundTripRandom(t *testing.T) {
	src := make([]byte, 4096)
	_, err := rand.Read(src)
	require.NoError(t, err)

	encoded, err := codec.Encode(codec.Zstd, nil, src)
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(encoded, zstdFrameMagic))

	decoded, err := codec.Decode(codec.Zstd, nil, encoded, len(src))
	require.NoError(t, err)
	require.Equal(t, src, decoded)
}

func TestEncodeDecodeZstdEmptyBody(t *testing.T) {
	encoded, err := codec.Encode(codec.Zstd, nil, []byte{})
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(encoded, zstdFrameMagic))

	decoded, err := codec.Decode(codec.Zstd, nil, encoded, 0)
	require.NoError(t, err)
	require.Empty(t, decoded)
}

func TestDecodeZstdWrongUncompressedLenTooSmall(t *testing.T) {
	src := []byte(strings.Repeat("payload ", 64))
	encoded, err := codec.Encode(codec.Zstd, nil, src)
	require.NoError(t, err)

	_, err = codec.Decode(codec.Zstd, nil, encoded, len(src)-1)
	require.ErrorIs(t, err, codec.ErrTrailingBytes)
}

func TestDecodeZstdWrongUncompressedLenTooLarge(t *testing.T) {
	src := []byte(strings.Repeat("payload ", 64))
	encoded, err := codec.Encode(codec.Zstd, nil, src)
	require.NoError(t, err)

	_, err = codec.Decode(codec.Zstd, nil, encoded, len(src)+1)
	require.ErrorIs(t, err, codec.ErrIncompleteStream)
}

func TestDecodeZstdConcatenatedFrames(t *testing.T) {
	first := []byte(strings.Repeat("first frame ", 32))
	second := []byte(strings.Repeat("second frame ", 32))

	encodedFirst, err := codec.Encode(codec.Zstd, nil, first)
	require.NoError(t, err)
	encodedSecond, err := codec.Encode(codec.Zstd, nil, second)
	require.NoError(t, err)

	concatenated := append(append([]byte{}, encodedFirst...), encodedSecond...)

	_, err = codec.Decode(codec.Zstd, nil, concatenated, len(first))
	require.ErrorIs(t, err, codec.ErrTrailingBytes)
}

func TestDecodeZstdTruncatedFrame(t *testing.T) {
	src := []byte(strings.Repeat("truncate me ", 64))
	encoded, err := codec.Encode(codec.Zstd, nil, src)
	require.NoError(t, err)
	require.Greater(t, len(encoded), 4)

	truncated := encoded[:len(encoded)-4]
	_, err = codec.Decode(codec.Zstd, nil, truncated, len(src))
	require.ErrorIs(t, err, codec.ErrIncompleteStream)
}

func TestDecodeRejectsNegativeUncompressedLenZstd(t *testing.T) {
	src := []byte("payload")
	encoded, err := codec.Encode(codec.Zstd, nil, src)
	require.NoError(t, err)

	require.NotPanics(t, func() {
		_, err := codec.Decode(codec.Zstd, nil, encoded, -1)
		require.ErrorIs(t, err, codec.ErrLengthMismatch)
	})
}

func TestDecodeZstdRejectsEmptySource(t *testing.T) {
	_, err := codec.Decode(codec.Zstd, nil, []byte{}, 0)
	require.ErrorIs(t, err, codec.ErrIncompleteStream)
}

// TestDecodeZstdBoundedMemory proves the streaming-decoder design's reason to exist.
// Decoding a small requested length out of a frame
// whose real content is huge must not allocate anywhere near that huge size.
// A DecodeAll-based implementation would materialize the whole decoded frame before this package could reject it.
// That would defeat the bound uncompressedLen is supposed to give the caller.
func TestDecodeZstdBoundedMemory(t *testing.T) {
	const bombUncompressedLen = 64 << 20 // 64 MiB of zeros

	src := make([]byte, bombUncompressedLen)
	encoded, err := codec.Encode(codec.Zstd, nil, src)
	require.NoError(t, err)
	require.Less(t, len(encoded), 64<<10, "a run of zeros must compress to well under 64 KiB")

	const requestedLen = 4 << 10 // far less than the bomb's real decoded length

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	_, err = codec.Decode(codec.Zstd, nil, encoded, requestedLen)
	require.ErrorIs(t, err, codec.ErrTrailingBytes)

	runtime.ReadMemStats(&after)
	delta := after.TotalAlloc - before.TotalAlloc
	t.Logf("decoding a %d-byte bomb to a %d-byte request allocated %d bytes", bombUncompressedLen, requestedLen, delta)
	require.Less(t, delta, uint64(bombUncompressedLen)/2, "decode must not allocate close to the bomb's full decoded size")
}

func TestDecodeZstdConcurrent(t *testing.T) {
	src := []byte(strings.Repeat("concurrent decode from a pooled decoder ", 128))
	encoded, err := codec.Encode(codec.Zstd, nil, src)
	require.NoError(t, err)

	const goroutines = 32

	results := make(chan error, goroutines)
	for range goroutines {
		go func() {
			decoded, decErr := codec.Decode(codec.Zstd, nil, encoded, len(src))
			if decErr != nil {
				results <- decErr

				return
			}
			if !bytes.Equal(decoded, src) {
				results <- errCorrupted
			} else {
				results <- nil
			}
		}()
	}

	for range goroutines {
		require.NoError(t, <-results)
	}
}

// TestDecodeZstdCLIProducedFrame decodes a frame built by the system zstd binary.
// That binary is an implementation independent of this package's own encoder.
// It is skipped when that binary is not on PATH.
func TestDecodeZstdCLIProducedFrame(t *testing.T) {
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("zstd CLI not found on PATH; skipping independent cross-check")
	}

	src := []byte(strings.Repeat("independent cross-check with the system zstd binary ", 50))

	cmd := exec.Command("zstd", "-q", "-c")
	cmd.Stdin = bytes.NewReader(src)
	var encoded bytes.Buffer
	cmd.Stdout = &encoded
	require.NoError(t, cmd.Run())

	decoded, err := codec.Decode(codec.Zstd, nil, encoded.Bytes(), len(src))
	require.NoError(t, err)
	require.Equal(t, src, decoded)
}

// TestEncodeZstdDecodableByCLI feeds this package's Encode output to the system zstd binary.
// That checks that an implementation independent of this package's own decoder accepts it.
// It is skipped when that binary is not on PATH.
func TestEncodeZstdDecodableByCLI(t *testing.T) {
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("zstd CLI not found on PATH; skipping independent cross-check")
	}

	src := []byte(strings.Repeat("independent cross-check with the system zstd binary ", 50))
	encoded, err := codec.Encode(codec.Zstd, nil, src)
	require.NoError(t, err)

	cmd := exec.Command("zstd", "-q", "-d", "-c")
	cmd.Stdin = bytes.NewReader(encoded)
	var decoded bytes.Buffer
	cmd.Stdout = &decoded
	require.NoError(t, cmd.Run())
	require.Equal(t, src, decoded.Bytes())
}

func TestDecodeZstdRejectsAppendedEmptyFrame(t *testing.T) {
	src := []byte(strings.Repeat("one frame only ", 32))
	encoded, err := codec.Encode(codec.Zstd, nil, src)
	require.NoError(t, err)

	appended := append(append([]byte{}, encoded...), emptyZstdFrame...)
	_, err = codec.Decode(codec.Zstd, nil, appended, len(src))
	require.ErrorIs(t, err, codec.ErrTrailingBytes)
}

func TestDecodeZstdRejectsAppendedSkippableFrame(t *testing.T) {
	src := []byte(strings.Repeat("one frame only ", 32))
	encoded, err := codec.Encode(codec.Zstd, nil, src)
	require.NoError(t, err)

	appended := append(append([]byte{}, encoded...), skippableZstdFrame...)
	_, err = codec.Decode(codec.Zstd, nil, appended, len(src))
	require.ErrorIs(t, err, codec.ErrTrailingBytes)
}

func TestDecodeZstdRejectsLeadingSkippableFrame(t *testing.T) {
	src := []byte(strings.Repeat("one frame only ", 32))
	encoded, err := codec.Encode(codec.Zstd, nil, src)
	require.NoError(t, err)

	leading := append(append([]byte{}, skippableZstdFrame...), encoded...)
	_, err = codec.Decode(codec.Zstd, nil, leading, len(src))
	require.ErrorIs(t, err, codec.ErrNotZstdFrame)

	_, err = codec.Decode(codec.Zstd, nil, skippableZstdFrame, 0)
	require.ErrorIs(t, err, codec.ErrNotZstdFrame)
}

func TestDecodeZstdAcceptsEmptyFrame(t *testing.T) {
	decoded, err := codec.Decode(codec.Zstd, nil, emptyZstdFrame, 0)
	require.NoError(t, err)
	require.Empty(t, decoded)
}

func TestDecodeZstdRejectsDictionaryHeader(t *testing.T) {
	// Single segment, Dictionary_ID_flag 1 (a one-byte Dictionary_ID of 7), Frame_Content_Size 0, one last empty Raw block.
	frame := []byte{0x28, 0xB5, 0x2F, 0xFD, 0x21, 0x07, 0x00, 0x01, 0x00, 0x00}

	_, err := codec.Decode(codec.Zstd, nil, frame, 0)
	require.ErrorIs(t, err, codec.ErrDictionary)
}

// TestDecodeZstdRejectsCLIDictionaryFrame decodes a frame the system zstd binary compressed with a trained dictionary.
// The trained dictionary carries a nonzero Dictionary_ID, which the CLI writes into the frame header.
// It is skipped when that binary is not on PATH.
func TestDecodeZstdRejectsCLIDictionaryFrame(t *testing.T) {
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("zstd CLI not found on PATH; skipping independent cross-check")
	}

	dir := t.TempDir()
	words := []string{"S1F1", "S2F41", "ALARM", "EVENT", "REPORT", "CEID", "VID", "tool", "lot", "wafer"}
	samples := make([]string, 0, 40)
	for i := range 40 {
		var sb strings.Builder
		for j := range 200 {
			sb.WriteString(words[(i*7+j*j+j*i)%len(words)])
			sb.WriteByte(' ')
		}
		name := filepath.Join(dir, fmt.Sprintf("sample%02d", i))
		require.NoError(t, os.WriteFile(name, []byte(sb.String()), 0o600))
		samples = append(samples, name)
	}

	dict := filepath.Join(dir, "dict")
	train := exec.Command("zstd", append([]string{"-q", "-f", "--train", "--maxdict=2048", "-o", dict}, samples...)...)
	require.NoError(t, train.Run())

	body, err := os.ReadFile(samples[0])
	require.NoError(t, err)

	cmd := exec.Command("zstd", "-q", "-c", "-D", dict, samples[0])
	var encoded bytes.Buffer
	cmd.Stdout = &encoded
	require.NoError(t, cmd.Run())

	_, err = codec.Decode(codec.Zstd, nil, encoded.Bytes(), len(body))
	require.ErrorIs(t, err, codec.ErrDictionary)
}

func TestDecodeZstdRejectsWindowAboveLimit(t *testing.T) {
	tests := []struct {
		name  string
		frame []byte
	}{
		{
			// Window_Descriptor 0x88: exponent 17, mantissa 0, so a 128 MiB window; no blocks follow.
			name:  "window descriptor 128 MiB",
			frame: []byte{0x28, 0xB5, 0x2F, 0xFD, 0x00, 0x88},
		},
		{
			// Window_Descriptor 0x81: exponent 16, mantissa 1, so 64 MiB + 8 MiB; no blocks follow.
			name:  "window descriptor 72 MiB",
			frame: []byte{0x28, 0xB5, 0x2F, 0xFD, 0x00, 0x81},
		},
		{
			// Single segment with an eight-byte Frame_Content_Size of 128 MiB, which is the window; no blocks follow.
			name:  "single segment 128 MiB",
			frame: []byte{0x28, 0xB5, 0x2F, 0xFD, 0xE0, 0x00, 0x00, 0x00, 0x08, 0x00, 0x00, 0x00, 0x00},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := codec.Decode(codec.Zstd, nil, tt.frame, 16)
			require.ErrorIs(t, err, codec.ErrWindowTooLarge)
		})
	}
}

// TestDecodeZstdCLISingleSegmentFrame decodes a frame the system zstd binary built from a file rather than from stdin.
// Knowing the content size up front, the CLI writes a single-segment frame with a two-byte Frame_Content_Size,
// a header shape the stdin cross-check does not produce.
// It is skipped when that binary is not on PATH.
func TestDecodeZstdCLISingleSegmentFrame(t *testing.T) {
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("zstd CLI not found on PATH; skipping independent cross-check")
	}

	src := []byte(strings.Repeat("single segment cross-check ", 12))
	name := filepath.Join(t.TempDir(), "body")
	require.NoError(t, os.WriteFile(name, src, 0o600))

	for _, args := range [][]string{{"-q", "-c", name}, {"-q", "-c", "--no-check", name}} {
		cmd := exec.Command("zstd", args...)
		var encoded bytes.Buffer
		cmd.Stdout = &encoded
		require.NoError(t, cmd.Run())

		decoded, err := codec.Decode(codec.Zstd, nil, encoded.Bytes(), len(src))
		require.NoError(t, err)
		require.Equal(t, src, decoded)
	}
}

func TestDecodeZstdAcceptsWindowAtLimit(t *testing.T) {
	// Window_Descriptor 0x80: exponent 16, mantissa 0, so exactly the 64 MiB limit;
	// then one last Raw block of 3 bytes.
	frame := []byte{0x28, 0xB5, 0x2F, 0xFD, 0x00, 0x80, 0x19, 0x00, 0x00, 'a', 'b', 'c'}

	decoded, err := codec.Decode(codec.Zstd, nil, frame, 3)
	require.NoError(t, err)
	require.Equal(t, []byte("abc"), decoded)
}

package corpus

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/codec"
)

// editHeaderSection is the header section length of block 1 of editPack: two records of 44-byte headers.
const editHeaderSection = 2 * testRecordHeaderLen

// rawFrame returns a complete single-segment frame of one Raw block holding content, with a content checksum when checksum is set.
func rawFrame(content []byte, checksum bool) []byte {
	frame := zstdFrameHeader(uint32(len(content)), checksum)
	frame = appendZstdBlock(frame, zstdBlockRaw, true, content)
	if checksum {
		frame = binary.LittleEndian.AppendUint32(frame, uint32(xxh64(content)))
	}

	return frame
}

// editBody returns the decoded body of block 1 of the codec none editPack: its stored body.
func editBody(t *testing.T) []byte {
	t.Helper()

	pack := editPack(t, tracepack.CodecNone)
	s := must(Locate(pack))(t).Blocks[1]

	return bytes.Clone(pack[s.Offset+testEnvelopeLen : s.Offset+s.Len])
}

func TestXXH64(t *testing.T) {
	t.Parallel()

	// The XXH64 of the empty input with seed 0, from the xxHash specification's reference values.
	assert.Equal(t, uint64(0xEF46DB3751D8E999), xxh64(nil))

	// The encoder writes the low 32 bits of the content's XXH64 as its frame's content checksum (RFC 8878 §3.1.1).
	for _, n := range []int{1, 3, 4, 7, 8, 31, 32, 33, 63, 64, 100, 1000} {
		content := make([]byte, n)
		for i := range content {
			content[i] = byte(i*7 + n)
		}
		enc, err := codec.Encode(codec.Zstd, nil, content)
		require.NoError(t, err)
		require.NotZero(t, enc[4]&zstdChecksumFlag, "the encoder writes a content checksum")
		assert.Equal(t, binary.LittleEndian.Uint32(enc[len(enc)-4:]), uint32(xxh64(content)), "length %d", n)
	}
}

func TestZstdRawFrameChecksum(t *testing.T) {
	t.Parallel()

	content := []byte("a header section held by a Raw block")
	got, err := codec.Decode(codec.Zstd, nil, rawFrame(content, true), len(content))
	require.NoError(t, err)
	assert.Equal(t, content, got)

	// The decoder checks the content checksum, so a frame with a checksum fails only for a reason it is built for.
	bad := rawFrame(content, true)
	bad[len(bad)-1] ^= 0xFF
	_, err = codec.Decode(codec.Zstd, nil, bad, len(content))
	require.Error(t, err)
}

func TestDamageZstd(t *testing.T) {
	t.Parallel()

	for _, damage := range []ZstdDamage{ZstdMalformed, ZstdShort} {
		for _, checksum := range []bool{false, true} {
			t.Run(fmt.Sprintf("damage %d checksum %v", damage, checksum), func(t *testing.T) {
				t.Parallel()

				body := editBody(t)
				pack := must(DamageZstd(editPack(t, tracepack.CodecNone), 1, damage, checksum))(t)

				rep := verifyPack(t, pack)
				require.NoError(t, rep.FooterErr, "the F-2 entry follows the new body")
				assert.Equal(t, tracepack.OutcomeCorruptMiddle, rep.Outcome)
				require.Len(t, rep.Failed, 1)
				assert.Equal(t, 1, rep.Failed[0].Block)
				assert.Equal(t, tracepack.ReasonCorruptBlock, rep.Failed[0].Reason)
				assert.Empty(t, rep.Disagreements)

				s := must(Locate(pack))(t).Blocks[1]
				env, frame := pack[s.Offset:s.Offset+testEnvelopeLen], pack[s.Offset+testEnvelopeLen:s.Offset+s.Len]
				assert.Equal(t, codec.Zstd, env[envelopeCodecOff])
				assert.Equal(t, uint32(len(body)), binary.LittleEndian.Uint32(env[envelopeUncompressedLenOff:]))

				// The first block is a Raw block, not last, of the header section; decoded alone it yields it.
				// Block_Size 88 << 3, Block_Type 0 (Raw), Last_Block 0: 704, little-endian in 3 bytes.
				assert.Equal(t, []byte{0xC0, 0x02, 0x00}, frame[9:12])
				alone := rawFrame(frame[12:12+editHeaderSection], false)
				prefix, err := codec.Decode(codec.Zstd, nil, alone, editHeaderSection)
				require.NoError(t, err)
				assert.Equal(t, body[:editHeaderSection], prefix)
				_, err = codec.Decode(codec.Zstd, nil, frame, len(body))
				require.Error(t, err)

				// The whole frame, written by hand from RFC 8878: the magic; the descriptor of a single segment
				// with a 4-byte Frame_Content_Size (0xA0), 0xA4 with the content checksum flag;
				// the content size, 128 bytes of body or 127 for a short frame;
				// the Raw block of the 88-byte header section;
				// then for a malformed frame a last Compressed block of 1 byte (1 << 3 | 2 << 1 | 1 = 0x0D)
				// holding a Raw_Literals_Block header of Regenerated_Size 31 (31 << 3 = 0xF8) and none of its bytes,
				// for a short frame a last Raw block of the 39 payload bytes up to the body's last (39 << 3 | 1 = 0x139);
				// then the low 32 bits of the XXH64 of the content when the checksum is set.
				require.Len(t, body, 128)
				want := []byte{0x28, 0xB5, 0x2F, 0xFD, 0xA0}
				content := body
				if checksum {
					want[4] = 0xA4
				}
				if damage == ZstdShort {
					content = body[:127]
					want = append(want, 127, 0, 0, 0)
				} else {
					want = append(want, 128, 0, 0, 0)
				}
				want = append(append(want, 0xC0, 0x02, 0x00), body[:88]...)
				if damage == ZstdShort {
					want = append(append(want, 0x39, 0x01, 0x00), body[88:127]...)
				} else {
					want = append(want, 0x0D, 0x00, 0x00, 0xF8)
				}
				if checksum {
					want = binary.LittleEndian.AppendUint32(want, uint32(xxh64(content)))
				}
				assert.Equal(t, want, frame)

				if damage == ZstdShort {
					short, err := codec.Decode(codec.Zstd, nil, frame, len(body)-1)
					require.NoError(t, err, "well formed but for its length")
					assert.Equal(t, body[:len(body)-1], short)
				}
			})
		}
	}
}

func TestDamagedFrameConstruction(t *testing.T) {
	t.Parallel()

	body := editBody(t)
	first := zstdFrameHeaderLen + zstdBlockHeaderLen
	flip := func(off func(frame []byte) int, mask byte) func([]byte) {
		return func(frame []byte) { frame[off(frame)] ^= mask }
	}
	at := func(n int) func([]byte) int { return func([]byte) int { return n } }
	last := func(f []byte) int { return len(f) - 1 }
	tests := []struct {
		name     string
		damage   ZstdDamage
		checksum bool
		mutate   func([]byte)
		ok       bool
	}{
		{"malformed", ZstdMalformed, true, nil, true},
		{"short", ZstdShort, true, nil, true},
		{"malformed, checksum changed", ZstdMalformed, true, flip(last, 0x01), true},
		{"short, checksum changed", ZstdShort, true, flip(last, 0x01), false},
		{"short, last payload byte changed", ZstdShort, false, flip(last, 0x01), false},
		{"short, content size one more", ZstdShort, false, flip(at(5), 0xFF), false},
		{"first header section byte", ZstdMalformed, true, flip(at(first), 0x01), false},
		{"last header section byte", ZstdShort, true, flip(at(first+editHeaderSection-1), 0x80), false},
		{"first block type", ZstdShort, true, flip(at(zstdFrameHeaderLen), 0x02), false},
		{"first block size", ZstdMalformed, true, flip(at(zstdFrameHeaderLen), 0x08), false},
		{"frame descriptor", ZstdShort, true, flip(at(4), 0x20), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := damagedFrame(body, editHeaderSection, tt.damage, tt.checksum, tt.mutate)
			if tt.ok {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, ErrPackEdit)
			}
		})
	}

	// A frame that decodes in full is refused: it would not be damaged.
	assert.ErrorIs(t, checkDamagedFrame(rawFrame(body, true), body, len(body), ZstdMalformed), ErrPackEdit)
	_, err := damagedFrame(body, 0, ZstdMalformed, false, nil)
	assert.ErrorIs(t, err, ErrPackEdit, "an empty header section")
	_, err = damagedFrame(body, len(body)+1, ZstdMalformed, false, nil)
	assert.ErrorIs(t, err, ErrPackEdit, "a header section past the body")
	_, err = damagedFrame(body[:editHeaderSection], editHeaderSection, ZstdShort, false, nil)
	assert.ErrorIs(t, err, ErrPackEdit, "no payload byte to leave out")
}

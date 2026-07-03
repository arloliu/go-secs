package secs2

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestShortcuts verifies all shortcut aliases using a large nested literal covering every alias.
// The ToSML and ToBytes vectors are ported from git show main:secs2/shortcut_test.go.
func TestShortcuts(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	item := L(
		A("test"),
		J("こんにちは世界"),
		BOOLEAN(true, false),
		B(0x3, 0x4),
		I1(1),
		I2(1),
		I4(1),
		I8(1),
		U1(1),
		U2(1),
		U4(1),
		U8(1),
		F4(1.23),
		F8(1.23),
	)
	require.NoError(item.Error())
	require.Equal(`<L[14]
  <A[4] "test">
  <J[21] "こんにちは世界">
  <BOOLEAN[2] True False>
  <B[2] 0b11 0b100>
  <I1[1] 1>
  <I2[1] 1>
  <I4[1] 1>
  <I8[1] 1>
  <U1[1] 1>
  <U2[1] 1>
  <U4[1] 1>
  <U8[1] 1>
  <F4[1] 1.23000002>
  <F8[1] 1.23>
>`, item.ToSML())

	require.Equal([]byte{
		0x1, 0xe, 0x41, 0x4, 0x74, 0x65, 0x73, 0x74, 0x45, 0x15, 0xe3, 0x81, 0x93, 0xe3, 0x82, 0x93,
		0xe3, 0x81, 0xab, 0xe3, 0x81, 0xa1, 0xe3, 0x81, 0xaf, 0xe4, 0xb8, 0x96, 0xe7, 0x95, 0x8c,
		0x25, 0x2, 0x1, 0x0, 0x21, 0x2, 0x3, 0x4, 0x65, 0x1, 0x1, 0x69, 0x2, 0x0, 0x1, 0x71, 0x4,
		0x0, 0x0, 0x0, 0x1, 0x61, 0x8, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x1, 0xa5, 0x1, 0x1, 0xa9,
		0x2, 0x0, 0x1, 0xb1, 0x4, 0x0, 0x0, 0x0, 0x1, 0xa1, 0x8, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
		0x1, 0x91, 0x4, 0x3f, 0x9d, 0x70, 0xa4, 0x81, 0x8, 0x3f, 0xf3, 0xae, 0x14, 0x7a, 0xe1, 0x47, 0xae,
	}, item.ToBytes())
}

// TestShortcutsNestedLiteral verifies a terse nested literal built entirely from shortcut aliases
// produces the expected structure and a known independent byte vector (derived from SEMI E5 §9
// format codes, not by comparison with explicit constructors).
func TestShortcutsNestedLiteral(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	msg := L(
		A("MODEL"),
		I4(1, 2, 3),
		L(B(byte(0x01), byte(0x02)), BOOLEAN(true, false)),
	)

	require.NoError(msg.Error())
	require.Equal(3, msg.Size())
	require.Equal("list", msg.Type())
	require.True(msg.IsList())

	// Wire encoding, manually derived:
	//   L[3]          0x01 0x03
	//   A[5] "MODEL"  0x41 0x05 0x4D 0x4F 0x44 0x45 0x4C
	//   I4[3] 1 2 3   0x71 0x0C 0x00 0x00 0x00 0x01 0x00 0x00 0x00 0x02 0x00 0x00 0x00 0x03
	//   L[2]          0x01 0x02
	//     B[2]        0x21 0x02 0x01 0x02
	//     BOOLEAN[2]  0x25 0x02 0x01 0x00
	want := []byte{
		0x01, 0x03,
		0x41, 0x05, 0x4D, 0x4F, 0x44, 0x45, 0x4C,
		0x71, 0x0C, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x03,
		0x01, 0x02,
		0x21, 0x02, 0x01, 0x02,
		0x25, 0x02, 0x01, 0x00,
	}
	require.Equal(want, msg.ToBytes())
}

// TestShortcutsExplicitEquivalence verifies each shortcut produces byte-identical output to the
// explicit constructor call it aliases.
func TestShortcutsExplicitEquivalence(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	require.Equal(NewListItem(NewASCIIItem("X")).ToBytes(), L(A("X")).ToBytes())
	require.Equal(NewASCIIItem("hello").ToBytes(), A("hello").ToBytes())
	require.Equal(NewJIS8Item("jis8").ToBytes(), J("jis8").ToBytes())
	require.Equal(NewUTF8StrItem("utf8").ToBytes(), W("utf8").ToBytes())
	require.Equal(NewBinaryItem(byte(0x01)).ToBytes(), B(byte(0x01)).ToBytes())
	require.Equal(NewBooleanItem(true).ToBytes(), BOOLEAN(true).ToBytes())
	require.Equal(NewIntItem(1, 42).ToBytes(), I1(42).ToBytes())
	require.Equal(NewIntItem(2, 42).ToBytes(), I2(42).ToBytes())
	require.Equal(NewIntItem(4, 42).ToBytes(), I4(42).ToBytes())
	require.Equal(NewIntItem(8, 42).ToBytes(), I8(42).ToBytes())
	require.Equal(NewUintItem(1, 42).ToBytes(), U1(42).ToBytes())
	require.Equal(NewUintItem(2, 42).ToBytes(), U2(42).ToBytes())
	require.Equal(NewUintItem(4, 42).ToBytes(), U4(42).ToBytes())
	require.Equal(NewUintItem(8, 42).ToBytes(), U8(42).ToBytes())
	require.Equal(NewFloatItem(4, float32(1.5)).ToBytes(), F4(float32(1.5)).ToBytes())
	require.Equal(NewFloatItem(8, 1.5).ToBytes(), F8(1.5).ToBytes())
}

// TestShortcutsTypeMapping spot-checks that each alias maps to the correct SECS-II type string.
// Also verifies that W uses the UTF-8 LSH (LSHUTF8 = 2).
func TestShortcutsTypeMapping(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	require.Equal("list", L().Type())
	require.Equal("ascii", A("x").Type())
	require.Equal("jis8", J("x").Type())
	require.Equal("localized_str", W("x").Type())
	require.Equal("binary", B().Type())
	require.Equal("boolean", BOOLEAN().Type())
	require.Equal("i1", I1().Type())
	require.Equal("i2", I2().Type())
	require.Equal("i4", I4().Type())
	require.Equal("i8", I8().Type())
	require.Equal("u1", U1().Type())
	require.Equal("u2", U2().Type())
	require.Equal("u4", U4().Type())
	require.Equal("u8", U8().Type())
	require.Equal("f4", F4().Type())
	require.Equal("f8", F8().Type())

	lsh, err := W("x").ToLocalizedStrHeader()
	require.NoError(err)
	require.Equal(LSHUTF8, lsh)
}

package sml

import (
	"testing"

	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

func TestEncoder_DefaultEqualsToSML(t *testing.T) {
	items := []secs2.Item{
		// Populated across the full type matrix (T3-m1): every SECS-II leaf type plus a nested list.
		secs2.A("hello"),
		secs2.B(0x01, 0x02, 0xFF),
		secs2.BOOLEAN(true, false, true),
		secs2.I1(1, -2),
		secs2.I2(1, -2, 3),
		secs2.I4(1, -2, 3),
		secs2.I8(1, -2, 3),
		secs2.U1(1, 2),
		secs2.U2(10, 20),
		secs2.U4(1, 2, 3),
		secs2.U8(1, 2, 3),
		secs2.F4(1.5, -2.25),
		secs2.F8(1.5, -2.25),
		secs2.J("jis"),
		secs2.W("utf8"),
		secs2.L(secs2.A("a"), secs2.I1(1, 2), secs2.L(secs2.U1(9))),

		// Empty variants of every type (T3-m1): the empty-payload rendering is a distinct code path
		// and a common source of Encode/ToSML drift.
		secs2.A(""),
		secs2.B(),
		secs2.BOOLEAN(),
		secs2.I1(),
		secs2.I2(),
		secs2.I4(),
		secs2.I8(),
		secs2.U1(),
		secs2.U2(),
		secs2.U4(),
		secs2.U8(),
		secs2.F4(),
		secs2.F8(),
		secs2.J(""),
		secs2.W(""),
		secs2.L(),
		secs2.NewEmptyItem(),
	}
	enc := NewEncoder()
	for _, it := range items {
		require.Equal(t, it.ToSML(), enc.Encode(it), "type %s", it.Type())
	}
}

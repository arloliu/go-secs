package gem_test

import (
	"errors"
	"iter"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/v2/gem"
	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
)

// requireNilItemError asserts the item carries an error wrapping gem.ErrNilItem that names where,
// and that hsms.NewDataMessage refuses to send it.
func requireNilItemError(t *testing.T, item secs2.Item, where string) {
	t.Helper()

	err := item.Error()
	require.ErrorIs(t, err, gem.ErrNilItem)
	require.ErrorContains(t, err, "gem: nil item at "+where)

	_, err = hsms.NewDataMessage(1, 1, false, 0, [4]byte{}, item)
	require.ErrorIs(t, err, gem.ErrNilItem)
}

// nilMapItem is a secs2.Item of map kind, so its zero value is nil without being a pointer.
// gem must catch it as it catches a nil interface; none of its methods is ever called.
type nilMapItem map[int]int

var _ secs2.Item = nilMapItem(nil)

func (nilMapItem) Type() string                          { panic("unreachable") }
func (nilMapItem) Size() int                             { panic("unreachable") }
func (nilMapItem) Error() error                          { panic("unreachable") }
func (nilMapItem) IsEmpty() bool                         { panic("unreachable") }
func (nilMapItem) IsList() bool                          { panic("unreachable") }
func (nilMapItem) IsBinary() bool                        { panic("unreachable") }
func (nilMapItem) IsBoolean() bool                       { panic("unreachable") }
func (nilMapItem) IsASCII() bool                         { panic("unreachable") }
func (nilMapItem) IsJIS8() bool                          { panic("unreachable") }
func (nilMapItem) IsLocalizedStr() bool                  { panic("unreachable") }
func (nilMapItem) IsInt8() bool                          { panic("unreachable") }
func (nilMapItem) IsInt16() bool                         { panic("unreachable") }
func (nilMapItem) IsInt32() bool                         { panic("unreachable") }
func (nilMapItem) IsInt64() bool                         { panic("unreachable") }
func (nilMapItem) IsUint8() bool                         { panic("unreachable") }
func (nilMapItem) IsUint16() bool                        { panic("unreachable") }
func (nilMapItem) IsUint32() bool                        { panic("unreachable") }
func (nilMapItem) IsUint64() bool                        { panic("unreachable") }
func (nilMapItem) IsFloat32() bool                       { panic("unreachable") }
func (nilMapItem) IsFloat64() bool                       { panic("unreachable") }
func (nilMapItem) Get(...int) (secs2.Item, error)        { panic("unreachable") }
func (nilMapItem) ToList() ([]secs2.Item, error)         { panic("unreachable") }
func (nilMapItem) ToBinary() ([]byte, error)             { panic("unreachable") }
func (nilMapItem) ToBoolean() ([]bool, error)            { panic("unreachable") }
func (nilMapItem) ToASCII() (string, error)              { panic("unreachable") }
func (nilMapItem) ToJIS8() (string, error)               { panic("unreachable") }
func (nilMapItem) ToLocalizedStr() (string, error)       { panic("unreachable") }
func (nilMapItem) ToLocalizedStrHeader() (uint16, error) { panic("unreachable") }
func (nilMapItem) ToInt() ([]int64, error)               { panic("unreachable") }
func (nilMapItem) ToUint() ([]uint64, error)             { panic("unreachable") }
func (nilMapItem) ToFloat() ([]float64, error)           { panic("unreachable") }
func (nilMapItem) ItemAt(int) (secs2.Item, error)        { panic("unreachable") }
func (nilMapItem) ByteAt(int) (byte, error)              { panic("unreachable") }
func (nilMapItem) BoolAt(int) (bool, error)              { panic("unreachable") }
func (nilMapItem) IntAt(int) (int64, error)              { panic("unreachable") }
func (nilMapItem) UintAt(int) (uint64, error)            { panic("unreachable") }
func (nilMapItem) FloatAt(int) (float64, error)          { panic("unreachable") }
func (nilMapItem) Items() iter.Seq[secs2.Item]           { panic("unreachable") }
func (nilMapItem) Bools() iter.Seq[bool]                 { panic("unreachable") }
func (nilMapItem) Ints() iter.Seq[int64]                 { panic("unreachable") }
func (nilMapItem) Uints() iter.Seq[uint64]               { panic("unreachable") }
func (nilMapItem) Floats() iter.Seq[float64]             { panic("unreachable") }
func (nilMapItem) AppendBinaryTo([]byte) []byte          { panic("unreachable") }
func (nilMapItem) EncodedLen() int                       { panic("unreachable") }
func (nilMapItem) AppendTo([]byte) []byte                { panic("unreachable") }
func (nilMapItem) ToBytes() []byte                       { panic("unreachable") }
func (nilMapItem) ToSML() string                         { panic("unreachable") }

func TestNilItemFailsTheSend(t *testing.T) {
	tests := []struct {
		name  string
		item  secs2.Item
		where string
	}{
		{
			"report value",
			gem.Report{RPTID: secs2.U4(10), VALUES: []secs2.Item{secs2.U4(1), nil, secs2.U4(3)}}.Item(),
			"Report.VALUES[1]",
		},
		{
			"report value in S6F11",
			gem.S6F11(secs2.U4(1), secs2.U4(2), gem.Report{RPTID: secs2.U4(10), VALUES: []secs2.Item{secs2.U4(1), nil}}).Item(),
			"Report.VALUES[1]",
		},
		{"S6F11 CEID", gem.S6F11(secs2.U4(1), nil).Item(), "S6F11 CEID"},
		{"top-level opaque body", gem.S1F6(nil).Item(), "S1F6 body"},
		{"typed-nil pointer", gem.S6F11(secs2.U4(1), (*secs2.ListItem)(nil)).Item(), "S6F11 CEID"},
		{"nil map-kind item", gem.S6F11(nilMapItem(nil), secs2.U4(2)).Item(), "S6F11 DATAID"},
		{"repeat element", gem.S1F3(secs2.U4(1), nil).Item(), "S1F3 SVIDS[1]"},
		{
			"repeat of repeats",
			gem.S1F20([][]secs2.Item{{secs2.A("a")}, {secs2.A("b"), nil}}).Item(),
			"S1F20 OBJECTS[1][1]",
		},
		{"group field in a repeat", gem.S1F20(nil, gem.ErrorInfo{ERRTEXT: "x"}).Item(), "ErrorInfo.ERRCODE"},
		{"group of nil fields", gem.Limit{LIMITID: 1, DEADBAND: &gem.Deadband{}}.Item(), "Deadband.UPPERDB"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requireNilItemError(t, tt.item, tt.where)
		})
	}
}

func TestNilItemErrorText(t *testing.T) {
	err := gem.S6F11(secs2.U4(1), nil).Item().Error()
	require.EqualError(t, err, "gem: nil item at S6F11 CEID")
	require.ErrorIs(t, err, gem.ErrNilItem)

	err = gem.Report{RPTID: secs2.U4(10), VALUES: []secs2.Item{secs2.U4(1), nil}}.Item().Error()
	require.EqualError(t, err, "gem: nil item at Report.VALUES[1]")
}

func TestNilItemNamesEveryNil(t *testing.T) {
	err := gem.S1F3(nil, secs2.U4(1), nil).Item().Error()
	require.ErrorContains(t, err, "gem: nil item at S1F3 SVIDS[0]")
	require.ErrorContains(t, err, "gem: nil item at S1F3 SVIDS[2]")
}

func TestNilDeadbandSendsEmptyList(t *testing.T) {
	item := gem.Limit{LIMITID: 1}.Item()
	require.NoError(t, item.Error())
	require.Equal(t, secs2.L(secs2.B(1), secs2.L()).ToBytes(), item.ToBytes())
}

func TestNilItemLeavesCallerSliceUnchanged(t *testing.T) {
	values := []secs2.Item{secs2.U4(1), nil, secs2.U4(3)}

	item := gem.Report{RPTID: secs2.U4(10), VALUES: values}.Item()
	require.ErrorIs(t, item.Error(), gem.ErrNilItem)

	require.Len(t, values, 3)
	require.Equal(t, secs2.U4(1), values[0])
	require.Nil(t, values[1])
	require.Equal(t, secs2.U4(3), values[2])
}

func TestBuildersWithoutNilSendTheSameBody(t *testing.T) {
	report := gem.Report{RPTID: secs2.U4(10), VALUES: []secs2.Item{secs2.A("IDLE"), secs2.U4(42)}}

	tests := []struct {
		name string
		msg  secs2.SECS2Message
		want secs2.Item
	}{
		{
			"S6F11",
			gem.S6F11(secs2.U4(1), secs2.U4(2), report),
			secs2.L(secs2.U4(1), secs2.U4(2), secs2.L(secs2.L(secs2.U4(10), secs2.L(secs2.A("IDLE"), secs2.U4(42))))),
		},
		{"S1F3", gem.S1F3(secs2.U4(1), secs2.U2(2)), secs2.L(secs2.U4(1), secs2.U2(2))},
		{"S1F6", gem.S1F6(secs2.L(secs2.A("x"))), secs2.L(secs2.A("x"))},
		{
			"S2F23",
			gem.S2F23(secs2.A("t"), "000010", secs2.U4(5), secs2.U4(1), secs2.U4(7)),
			secs2.L(secs2.A("t"), secs2.A("000010"), secs2.U4(5), secs2.U4(1), secs2.L(secs2.U4(7))),
		},
		{
			"S1F20",
			gem.S1F20([][]secs2.Item{{secs2.A("a")}, {secs2.A("b"), secs2.U1(1)}}, gem.ErrorInfo{ERRCODE: secs2.U2(3), ERRTEXT: "e"}),
			secs2.L(secs2.L(secs2.L(secs2.A("a")), secs2.L(secs2.A("b"), secs2.U1(1))), secs2.L(secs2.L(secs2.U2(3), secs2.A("e")))),
		},
		{
			"S2F45",
			gem.S2F45(secs2.U4(1), gem.VariableLimits{VID: secs2.U4(2), LIMITS: []gem.Limit{
				{LIMITID: 1, DEADBAND: &gem.Deadband{UPPERDB: secs2.U4(9), LOWERDB: secs2.U4(3)}},
			}}),
			secs2.L(secs2.U4(1), secs2.L(secs2.L(secs2.U4(2), secs2.L(secs2.L(secs2.B(1), secs2.L(secs2.U4(9), secs2.U4(3))))))),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, tt.msg.Item().Error())
			require.Equal(t, tt.want.ToBytes(), tt.msg.Item().ToBytes())
		})
	}
}

func TestPresentEachDoesNotAllocateWithoutNil(t *testing.T) {
	items := []secs2.Item{secs2.U4(1), secs2.A("x"), secs2.L(secs2.U1(1))}

	got := gem.PresentEach(items, "S1F3 SVIDS")
	require.Same(t, &items[0], &got[0], "presentEach must return the caller's slice itself")

	allocs := testing.AllocsPerRun(100, func() {
		got = gem.PresentEach(items, "S1F3 SVIDS")
	})
	require.Zero(t, allocs)

	allocs = testing.AllocsPerRun(100, func() {
		got = gem.PresentEach(items, "S1F20 OBJECTS", 1)
	})
	require.Zero(t, allocs)
}

func TestPresentDoesNotAllocateWithoutNil(t *testing.T) {
	item := secs2.U4(1)

	var got secs2.Item

	allocs := testing.AllocsPerRun(100, func() {
		got = gem.Present(item, "S6F11 CEID")
	})
	require.Zero(t, allocs)
	require.Equal(t, item, got)
}

func TestPresentEachCopiesOnNil(t *testing.T) {
	items := []secs2.Item{secs2.U4(1), nil, nilMapItem(nil)}

	got := gem.PresentEach(items, "S1F20 OBJECTS", 4)
	require.NotSame(t, &items[0], &got[0])
	require.Nil(t, items[1])
	require.Equal(t, secs2.U4(1), got[0])
	require.EqualError(t, got[1].Error(), "gem: nil item at S1F20 OBJECTS[4][1]")
	require.EqualError(t, got[2].Error(), "gem: nil item at S1F20 OBJECTS[4][2]")
	require.True(t, errors.Is(got[2].Error(), gem.ErrNilItem))
}

// TestZeroLengthShortForms proves each short form whose body is only the zero-length list
// sends exactly what its main builder sent for all-nil items before a nil item became an error,
// when secs2.L dropped the nils and left L[0];
// the main builder now refuses those nils, and the main decoder still decodes the short form.
func TestZeroLengthShortForms(t *testing.T) {
	tests := []struct {
		name    string
		short   secs2.SECS2Message
		allNil  secs2.SECS2Message
		oldBody secs2.Item
		decode  func(secs2.Item) error
	}{
		{
			name:    "S7F6Denied",
			short:   gem.S7F6Denied(),
			allNil:  gem.S7F6(nil, nil),
			oldBody: secs2.L(nil, nil),
			decode:  func(item secs2.Item) error { _, err := gem.DecodeS7F6(item); return err },
		},
		{
			name:    "S7F8NoSuchMID",
			short:   gem.S7F8NoSuchMID(),
			allNil:  gem.S7F8(nil, nil),
			oldBody: secs2.L(nil, nil),
			decode:  func(item secs2.Item) error { _, err := gem.DecodeS7F8(item); return err },
		},
		{
			name:    "S7F36NoSuchMID",
			short:   gem.S7F36NoSuchMID(),
			allNil:  gem.S7F36(nil, nil, nil),
			oldBody: secs2.L(nil, nil, nil),
			decode:  func(item secs2.Item) error { _, err := gem.DecodeS7F36(item); return err },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, []byte{0x01, 0x00}, tt.oldBody.ToBytes(), "the old all-nil body is L[0]")

			require.NoError(t, tt.short.Item().Error())
			require.Equal(t, tt.allNil.StreamCode(), tt.short.StreamCode())
			require.Equal(t, tt.allNil.FunctionCode(), tt.short.FunctionCode())
			require.False(t, tt.short.WaitBit())
			require.Equal(t, tt.oldBody.ToBytes(), tt.short.Item().ToBytes())

			require.ErrorIs(t, tt.allNil.Item().Error(), gem.ErrNilItem)

			decoded, err := secs2.Decode(tt.short.Item().ToBytes())
			require.NoError(t, err)
			require.NoError(t, tt.decode(decoded))
		})
	}
}

// TestNilOpaqueGroupFieldNamedByLabel proves a nil opaque field of a group is named as its decode errors name it,
// by its parameter name rather than its Go field name.
func TestNilOpaqueGroupFieldNamedByLabel(t *testing.T) {
	job := gem.ProcessJob{PRJOBID: "job", MF: secs2.A("mf"), RCPSPEC: "recipe", PRPAUSEEVENT: secs2.L()}

	requireNilItemError(t, job.Item(), "ProcessJob.material")
}

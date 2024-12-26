package sml

import (
	"testing"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/require"
)

// Tests SECS Message Language (SML) parser
type testSlowCase struct {
	description       string   // Test case description
	input             string   // Input to the parser
	expectedNumOfMsgs int      // expected number of parsed messages
	expectedNumOfErrs int      // expected number of parsing errors
	expectedStr       []string // expected string representation of messages
	expectedErrStr    []string // expected error strings in form of "line:col:subset of error text"
}

func checkSlowTestCase(t *testing.T, tests []testSlowCase) {
	hsms.UseStreamFunctionSingleQuote()
	secs2.UseASCIISingleQuote()

	require := require.New(t)
	for i, test := range tests {
		t.Logf("Test #%d: %s", i, test.description)
		msgs, errs := ParseHSMSSlow(test.input)

		for j, msg := range msgs {
			str := msg.ToSML()
			require.Equal(test.expectedStr[j], str)

			reparsedMsgs, reparsedErrs := ParseHSMSSlow(str)
			require.Len(reparsedMsgs, 1)
			require.Len(reparsedErrs, 0)
			require.Equal(msg, reparsedMsgs[0])
		}

		require.Lenf(msgs, test.expectedNumOfMsgs, "errs:%v", errs)
		require.Len(errs, test.expectedNumOfErrs)
		for j, err := range errs {
			errStr := err.Error()
			require.Contains(errStr, test.expectedErrStr[j])
		}
	}
}

func TestParser_NoErrorCases(t *testing.T) {
	tests := []testSlowCase{
		{
			description:       "empty input",
			input:             "",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 0,
			expectedStr:       []string{},
		},
		{
			description:       "0 message",
			input:             "// comment 中文\n",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 0,
			expectedStr:       []string{},
		},
		{
			description:       "1 message, no data item",
			input:             "S0F0 .",
			expectedNumOfMsgs: 1,
			expectedNumOfErrs: 0,
			expectedStr:       []string{"'S0F0'\n."},
		},
		{
			description:       "1 message, no data item, with message name at frond",
			input:             "TestMessage:S0F1 W\n.",
			expectedNumOfMsgs: 1,
			expectedNumOfErrs: 0,
			expectedStr:       []string{"TestMessage:'S0F1' W\n."},
		},
		{
			description:       "1 message, no data item, with single quoted stream-function",
			input:             "TestMessage : 'S0F1' W\n.",
			expectedNumOfMsgs: 1,
			expectedNumOfErrs: 0,
			expectedStr:       []string{"TestMessage:'S0F1' W\n."},
		},
		{
			description:       "1 message, double-quote ASCII node",
			input:             `TestMessage:'S1F1' W <A "text">.`,
			expectedNumOfMsgs: 1,
			expectedNumOfErrs: 0,
			expectedStr:       []string{"TestMessage:'S1F1' W\n<A[4] 'text'>\n."},
		},
		{
			description:       "1 message, single-quote ASCII node",
			input:             `TestMessage:'S1F1' W <A 'text'>.`,
			expectedNumOfMsgs: 1,
			expectedNumOfErrs: 0,
			expectedStr:       []string{"TestMessage:'S1F1' W\n<A[4] 'text'>\n."},
		},
		{
			description:       "1 message, Binary node",
			input:             `TestMessage   : S63F127 W <B[3] 0b0 0xFE 255>.`,
			expectedNumOfMsgs: 1,
			expectedNumOfErrs: 0,
			expectedStr:       []string{"TestMessage:'S63F127' W\n<B[3] 0b0 0b11111110 0b11111111>\n."},
		},
		{
			description:       "1 message, Boolean node",
			input:             `TestMessage:'S126F254' <BOOLEAN T F>.`,
			expectedNumOfMsgs: 1,
			expectedNumOfErrs: 0,
			expectedStr:       []string{"TestMessage:'S126F254'\n<BOOLEAN[2] T F>\n."},
		},
		{
			description: "2 messages, F4, F8 node",
			input: `S126F254 <F4 +0.1 -0.1>.
			        S127F255 <F8 1e3 1E-3 .5e-1>.`,
			expectedNumOfMsgs: 2,
			expectedNumOfErrs: 0,
			expectedStr: []string{
				"'S126F254'\n<F4[2] 0.1 -0.1>\n.",
				"'S127F255'\n<F8[3] 1000 0.001 0.05>\n.",
			},
		},
		{
			description: "4 messages, I1, I2, I4, I8 node",
			input: `'S0F0' <I1 -128 -64 -1 0 1 64 127>.
			        Line2: "S0F0" <I2 -32768 32767>.
			        S0F0 <I4 -2147483648 2147483647>.
			        S0F0 <I8 -9223372036854775808 9223372036854775807>.`,
			expectedNumOfMsgs: 4,
			expectedNumOfErrs: 0,
			expectedStr: []string{
				"'S0F0'\n<I1[7] -128 -64 -1 0 1 64 127>\n.",
				"Line2:'S0F0'\n<I2[2] -32768 32767>\n.",
				"'S0F0'\n<I4[2] -2147483648 2147483647>\n.",
				"'S0F0'\n<I8[2] -9223372036854775808 9223372036854775807>\n.",
			},
		},
		{
			description: "5 messages, U1, U2, U4, U8 node",
			input: `S0F0 <U1[0..4] 0 1 128 255>.
			        S0F0 <U2[0..4] 1 65535>.
			        S0F0 <U2[3] 0 1 65535>.
			        S0F0 <U4[..3] 0 1 4294967295>.
			        S0F0 <U8[0..] 0 1 18446744073709551615>.`,
			expectedNumOfMsgs: 5,
			expectedNumOfErrs: 0,
			expectedStr: []string{
				"'S0F0'\n<U1[4] 0 1 128 255>\n.",
				"'S0F0'\n<U2[2] 1 65535>\n.",
				"'S0F0'\n<U2[3] 0 1 65535>\n.",
				"'S0F0'\n<U4[3] 0 1 4294967295>\n.",
				"'S0F0'\n<U8[3] 0 1 18446744073709551615>\n.",
			},
		},
		{
			description: "1 message, Nested list node with comment",
			input: `S0F0 // message header comment
<L          // comment1
  <L[0]>    // comment
  <L[2]     // comment
    <A[0]>  // comment
    <B[0]>  // comment
  >         // comment
>           // comment
.           // comment
`,
			expectedNumOfMsgs: 1,
			expectedNumOfErrs: 0,
			expectedStr: []string{
				`'S0F0'
<L[2]
  <L[0]>
  <L[2]
    <A[0] ''>
    <B[0]>
  >
>
.`,
			},
		},
	}

	checkSlowTestCase(t, tests)
}

func TestParser_CommonErrorCases(t *testing.T) {
	tests := []testSlowCase{
		{
			description:       "stream function: unexpected token",
			input:             "SxFy",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"expected stream function"},
		},
		{
			description:       "stream function: stream overflow",
			input:             "S128F255 .",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"overflow"},
		},
		{
			description:       "stream function: function overflow",
			input:             "S127F256 .",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"overflow"},
		},
		{
			description:       "wait bit: true on reply message, direction: not specified",
			input:             "S1F2 W .",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 2,
			expectedErrStr:    []string{"wait bit", "message is not a valid response/secondary message"},
		},
		{
			description:       "message text: unexpected token",
			input:             "S0F0\n//comment\n*",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"expected"},
		},
		{
			description:       "data item: invalid data item type",
			input:             "S0F0\n<BOOL[1] T>",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"data item type"},
		},
		{
			description:       "data item size: syntax error",
			input:             "S0F0\n<B[-3] 0> .",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"syntax error"},
		},
		{
			description:       "data item size: underflow",
			input:             "S0F0\n<B[3] 0> .",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"overflow"},
		},
		{
			description:       "data item size: underflow",
			input:             "S0F0\n<B[3..] 0> .",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"overflow"},
		},
		{
			description:       "data item size: overflow",
			input:             "S0F0\n<B[..2] 0 1 2> .",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"overflow"},
		},
		{
			description:       "missing token: message end",
			input:             "S0F0\n<B[0]>\n",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"expected message end"},
		},
	}

	checkSlowTestCase(t, tests)
}

func TestParser_List_ErrorCases(t *testing.T) {
	tests := []testSlowCase{
		{
			description:       "unexpected token",
			input:             "S0F0\n<L[1] T>\n.",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"expected child data item"},
		},
		{
			description:       "unexpected token",
			input:             "S0F0\n<L[1] !@#>\n.",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"syntax error"},
		},
	}

	checkSlowTestCase(t, tests)
}

func TestParser_ASCII_ErrorCases(t *testing.T) {
	tests := []testSlowCase{
		{
			description:       "non-ascii characters",
			input:             "S0F0\n<A \"စာသား\"> .",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"expected latin-1 characters"},
		},
		{
			description:       "invalid character number code",
			input:             "S0F0\n<A 0.01> .",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"number code"},
		},
		{
			description:       "non-ascii number code",
			input:             "S0F0\n<A 128> .",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"overflow"},
		},
		{
			description:       "unexpected token",
			input:             "S0F0\n<A BOOLEAN> .",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"expected quoted string"},
		},
		{
			description:       "unexpected token (error token)",
			input:             "S0F0\n<A[..10] !@#> .",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"syntax error"},
		},
	}

	checkSlowTestCase(t, tests)
}

func TestParser_Binary_ErrorCases(t *testing.T) {
	tests := []testSlowCase{
		{
			description:       "underflow",
			input:             "S0F0\n<B -1> .",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"overflow"},
		},
		{
			description:       "overflow",
			input:             "S0F0\n<B 256> .",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"overflow"},
		},
		{
			description:       "unexpected token",
			input:             "S0F0\n<B[1] T> .",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"expected binary byte"},
		},
		{
			description:       "unexpected token (error token)",
			input:             "S0F0\n<B[2] !@#> .",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"expected binary byte"},
		},
	}

	checkSlowTestCase(t, tests)
}

func TestParser_Boolean_ErrorCases(t *testing.T) {
	tests := []testSlowCase{
		{
			description:       "unexpected token",
			input:             "S0F0\n<BOOLEAN[1] 10> .",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"expect boolean"},
		},
		{
			description:       "unexpected token (error token)",
			input:             "S0F0\n<BOOLEAN[1] !@#> .",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"expect boolean"},
		},
	}

	checkSlowTestCase(t, tests)
}

func TestParser_Float_ErrorCases(t *testing.T) {
	tests := []testSlowCase{
		{
			description:       "F4 overflow",
			input:             "S0F0\n<F4 1e99999> .",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"overflow"},
		},
		{
			description:       "F8 overflow",
			input:             "S0F0\n<F8 1e99999> .",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"overflow"},
		},
		{
			description:       "unexpected token",
			input:             "S0F0\n<F4[1] T> .",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"expect float"},
		},
		{
			description:       "unexpected token (error token)",
			input:             "S0F0\n<F4[1] !@#> .",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"expect float"},
		},
	}

	checkSlowTestCase(t, tests)
}

func TestParser_Int_ErrorCases(t *testing.T) {
	tests := []testSlowCase{
		{
			description: "underflow",
			input: `S0F0
<L[4]
<I1 -129>
<I2 -32769>
<I4 -2147483649>
<I8 -9223372036854775809>
>.`,
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"overflow"},
		},
		{
			description: "overflow",
			input: `S0F0
<L[4]
<I1 128>
<I2 32768>
<I4 2147483648>
<I8 9223372036854775808>
>.`,
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"overflow"},
		},
		{
			description:       "unexpected token",
			input:             "S0F0\n<I1[2] 0.12 T> .",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"expected integer"},
		},
		{
			description:       "unexpected token (error token)",
			input:             "S0F0\n<I1[1] !@#> .",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"expected integer"},
		},
	}

	checkSlowTestCase(t, tests)
}

func TestParser_Uint_ErrorCases(t *testing.T) {
	tests := []testSlowCase{
		{
			description: "overflow",
			input: `S0F0
<L[4]
<U1 256>
<U2 65536>
<U4 4294967296>
<U8 18446744073709551616>
>.`,
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"overflow"},
		},
		{
			description:       "unexpected token",
			input:             "S0F0\n<U1[1] -1 T> .",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"expected unsigned integer"},
		},
		{
			description:       "unexpected token (error token)",
			input:             "S0F0\n<U1[1] !@#> .",
			expectedNumOfMsgs: 0,
			expectedNumOfErrs: 1,
			expectedErrStr:    []string{"expected unsigned integer"},
		},
	}

	checkSlowTestCase(t, tests)
}

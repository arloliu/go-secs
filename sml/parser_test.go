package sml

import (
	"testing"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/require"
)

type testCase struct {
	description       string   // Test case description
	input             string   // Input to the parser
	expectedNumOfMsgs int      // expected number of parsed messages
	expectedStr       []string // expected string representation of messages
	expectedErrStr    string   // expected error strings
}

func checkTestCase(t *testing.T, tests []testCase) {
	require := require.New(t)
	for i, test := range tests {
		t.Logf("Test #%d: %s", i, test.description)
		msgs, err := ParseHSMS(test.input)

		require.Lenf(msgs, test.expectedNumOfMsgs, "should have %d message, got %d, error:%s", test.expectedNumOfMsgs, len(msgs), err)

		for j, msg := range msgs {
			require.NotNil(msg)
			str := msg.ToSML()
			require.Equal(test.expectedStr[j], str)

			reparsedMsgs, reparseErr := ParseHSMS(str)
			require.NoError(reparseErr)
			require.Lenf(reparsedMsgs, 1, "should have 1 message, error:%s", err)
			require.Equal(msg, reparsedMsgs[0])
		}

		if len(test.expectedErrStr) > 0 {
			errStr := err.Error()
			require.Contains(errStr, test.expectedErrStr)
		}
	}
}

func TestParseHSMS_NoErrorCases_StrictMode(t *testing.T) {
	tests := commonTestCases()
	tests = append(tests,
		testCase{
			description:       "1 message, contains non-printable ASCII node, case 1",
			input:             `TestMessage:'S1F1' W <A 'te"s\'t 1' 0x0A 0x0D ' test \'2\''>.`,
			expectedNumOfMsgs: 1,
			expectedStr:       []string{"TestMessage:'S1F1' W\n<A[19] 'te\"s\\'t 1' 0x0A 0x0D ' test \\'2\\''>\n."},
		},
		testCase{
			description: "1 message, contains non-printable ASCII node, case 2",
			input: `TestMessage:'S1F1' W
<L[2]
	<A '\'quote\'
string 1'>
	<A '\'quote\'
string 2'>
>
.`,
			expectedNumOfMsgs: 1,
			expectedStr:       []string{"TestMessage:'S1F1' W\n<L[2]\n  <A[16] '\\'quote\\'' 0x0A 'string 1'>\n  <A[16] '\\'quote\\'' 0x0A 'string 2'>\n>\n."},
		},
	)

	secs2.UseASCIISingleQuote()
	WithStrictMode(true)
	checkTestCase(t, tests)

	msg, _ := hsms.NewDataMessage(1, 1, true, 0, nil, secs2.A("first 'line'\n\rsecond line"))
	tests = []testCase{
		{
			description:       "1 message, single-quote ASCII node",
			input:             `TestMessage:'S1F1' W <A 'text'>.`,
			expectedNumOfMsgs: 1,
			expectedStr:       []string{"TestMessage:'S1F1' W\n<A[4] 'text'>\n."},
		},
		{
			description:       "1 message, single-quote ASCII node with newlines",
			input:             "TestMessage:'S1F1' W <A 'text1\ntest2\ntest3'>.",
			expectedNumOfMsgs: 1,
			expectedStr:       []string{"TestMessage:'S1F1' W\n<A[17] 'text1' 0x0A 'test2' 0x0A 'test3'>\n."},
		},
		{
			description:       "1 message, from ToSML",
			input:             msg.ToSML(),
			expectedNumOfMsgs: 1,
			expectedStr:       []string{"'S1F1' W\n<A[25] 'first \\'line\\'' 0x0A 0x0D 'second line'>\n."},
		},
	}
	secs2.UseASCIISingleQuote()
	checkTestCase(t, tests)

	tests = []testCase{
		{
			description:       "1 message, double-quote ASCII node",
			input:             `TestMessage:'S1F1' W <A "text">.`,
			expectedNumOfMsgs: 1,
			expectedStr:       []string{"TestMessage:'S1F1' W\n<A[4] \"text\">\n."},
		},
	}

	secs2.UseASCIIDoubleQuote()
	checkTestCase(t, tests)
}

func TestParseHSMS_NoErrorCases_NonStrictMode(t *testing.T) {
	tests := commonTestCases()

	secs2.UseASCIISingleQuote()
	WithStrictMode(false)
	checkTestCase(t, tests)

	tests = []testCase{
		{
			description:       "1 message, single-quote ASCII node",
			input:             `TestMessage:'S1F1' W <A 'text'>.`,
			expectedNumOfMsgs: 1,
			expectedStr:       []string{"TestMessage:'S1F1' W\n<A[4] 'text'>\n."},
		},
		{
			description:       "1 message, single-quote ASCII node with newlines",
			input:             "TestMessage:'S1F1' W <A 'text1\ntest2\ntest3'>.",
			expectedNumOfMsgs: 1,
			expectedStr:       []string{"TestMessage:'S1F1' W\n<A[17] 'text1\ntest2\ntest3'>\n."},
		},
	}
	secs2.UseASCIISingleQuote()
	checkTestCase(t, tests)

	tests = []testCase{
		{
			description:       "1 message, double-quote ASCII node",
			input:             `TestMessage:'S1F1' W <A "text">.`,
			expectedNumOfMsgs: 1,
			expectedStr:       []string{"TestMessage:'S1F1' W\n<A[4] \"text\">\n."},
		},
		{
			description:       "1 message, double-quote ASCII node with newlines",
			input:             "TestMessage:'S1F1' W <A \"text1\ntest2\ntest3\">.",
			expectedNumOfMsgs: 1,
			expectedStr:       []string{"TestMessage:'S1F1' W\n<A[17] \"text1\ntest2\ntest3\">\n."},
		},
	}

	secs2.UseASCIIDoubleQuote()
	checkTestCase(t, tests)
}

func commonTestCases() []testCase {
	return []testCase{
		{
			description:       "empty input",
			input:             "",
			expectedNumOfMsgs: 0,
		},
		{
			description:       "0 message",
			input:             "// comment 中文\n",
			expectedNumOfMsgs: 0,
		},
		{
			description:       "1 message, no data item",
			input:             "S0F0 .",
			expectedNumOfMsgs: 1,
			expectedStr:       []string{"'S0F0'\n."},
		},
		{
			description:       "1 message, no data item, with message name at frond",
			input:             "TestMessage:S0F1 W\n.",
			expectedNumOfMsgs: 1,
			expectedStr:       []string{"TestMessage:'S0F1' W\n."},
		},
		{
			description:       "1 message, no data item, with single quoted stream-function",
			input:             "TestMessage : 'S0F1' W\n.",
			expectedNumOfMsgs: 1,
			expectedStr:       []string{"TestMessage:'S0F1' W\n."},
		},
		{
			description:       "1 message, single-quote ASCII node",
			input:             `TestMessage:'S1F1' W <A 'text'>.`,
			expectedNumOfMsgs: 1,
			expectedStr:       []string{"TestMessage:'S1F1' W\n<A[4] 'text'>\n."},
		},
		{
			description:       "1 message, Binary node",
			input:             `TestMessage   : S63F127 W <B[3] 0b0 0xFE 255>.`,
			expectedNumOfMsgs: 1,
			expectedStr:       []string{"TestMessage:'S63F127' W\n<B[3] 0b0 0b11111110 0b11111111>\n."},
		},
		{
			description:       "1 message, Boolean node",
			input:             `TestMessage:'S126F254' <BOOLEAN T F>.`,
			expectedNumOfMsgs: 1,
			expectedStr:       []string{"TestMessage:'S126F254'\n<BOOLEAN[2] T F>\n."},
		},
		{
			description: "2 messages, F4, F8 node",
			input: `S126F254 <F4 +0.1 -0.1>.
			        S127F255 <F8 1e3 1E-3 .5e-1>.`,
			expectedNumOfMsgs: 2,
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
			expectedStr: []string{
				"'S0F0'\n<U1[4] 0 1 128 255>\n.",
				"'S0F0'\n<U2[2] 1 65535>\n.",
				"'S0F0'\n<U2[3] 0 1 65535>\n.",
				"'S0F0'\n<U4[3] 0 1 4294967295>\n.",
				"'S0F0'\n<U8[3] 0 1 18446744073709551615>\n.",
			},
		},
		{
			description: "1 message, Nested list node with line comment",
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
			expectedStr: []string{
				`'S0F0'
<L[2]
  <L[0]>
  <L[2]
    <A[0]>
    <B[0]>
  >
>
.`,
			},
		},
		{
			description: "1 message, Nested list node with block comment",
			input: `S0F0 /* message header comment */
<L          /* comment1 */
  <L[0]>    /* comment */
  <L[2]     /* comment */
    <A[0]>  /* comment */
    <B[0]>  /* comment */
  >         /* comment */
>           /* comment */
.           /* comment */
`,
			expectedNumOfMsgs: 1,
			expectedStr: []string{
				`'S0F0'
<L[2]
  <L[0]>
  <L[2]
    <A[0]>
    <B[0]>
  >
>
.`,
			},
		},
	}
}

func TestParseHSMS_List_ErrorCases(t *testing.T) {
	tests := []testCase{
		{
			description:       "unexpected token",
			input:             "S0F0\n<L[1] T>\n.",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "expected child data item",
		},
		{
			description:       "unexpected token",
			input:             "S0F0\n<L[1] !@#>\n.",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "expected child data item",
		},
	}

	checkTestCase(t, tests)
}

func TestParseHSMS_ASCII_ErrorCases_NonStrictMode(t *testing.T) {
	tests := []testCase{
		{
			description:       "invalid character number code",
			input:             "S0F0\n<A 0.01> .",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "invalid quote for ASCII string",
		},
		{
			description:       "non-ascii number code",
			input:             "S0F0\n<A 128> .",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "invalid quote for ASCII string",
		},
		{
			description:       "unexpected token",
			input:             "S0F0\n<A ABCD> .",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "invalid quote for ASCII string",
		},
		{
			description:       "unexpected token (invalid token)",
			input:             "S0F0\n<A[..10] !@#> .",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "invalid quote for ASCII string",
		},
		{
			description:       "unexpected token (> in quote string)",
			input:             "S0F0\n<A 'ab>'> .",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "unclosed quote string",
		},
	}

	secs2.UseASCIISingleQuote()
	WithStrictMode(false)
	checkTestCase(t, tests)
}

func TestParseHSMS_ASCII_ErrorCases_StrictMode(t *testing.T) {
	tests := []testCase{
		{
			description:       "invalid ASCII characters",
			input:             "S0F0\n<A ABCD> .",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "invalid syntax",
		},
		{
			description:       "invalid ASCII number string",
			input:             "S0F0\n<A 0.01> .",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "invalid syntax",
		},
		{
			description:       "ASCII number character out of range",
			input:             "S0F0\n<A 128> .",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "out of ASCII range",
		},
		{
			description:       "unexpected token (invalid token)",
			input:             "S0F0\n<A[..10] !@#> .",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "invalid syntax",
		},
		{
			description:       "unexpected token (quote)",
			input:             "S0F0\n<A 'ab''> .",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "unclosed quote string",
		},
		{
			description:       "unexpected token (> in quote string)",
			input:             "S0F0\n<A 'ab>'> .",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "unclosed quote string",
		},
	}

	secs2.UseASCIISingleQuote()
	WithStrictMode(true)
	checkTestCase(t, tests)
}

func TestParseHSMS_Binary_ErrorCases(t *testing.T) {
	tests := []testCase{
		{
			description:       "underflow",
			input:             "S0F0\n<B -1> .",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "overflow",
		},
		{
			description:       "overflow",
			input:             "S0F0\n<B 256> .",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "overflow",
		},
		{
			description:       "unexpected token",
			input:             "S0F0\n<B[1] T> .",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "expect binary",
		},
		{
			description:       "unexpected token (error token)",
			input:             "S0F0\n<B[2] !@#> .",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "expect binary",
		},
	}

	checkTestCase(t, tests)
}

func TestParseHSMS_Boolean_ErrorCases(t *testing.T) {
	tests := []testCase{
		{
			description:       "unexpected token",
			input:             "S0F0\n<BOOLEAN[1] 10> .",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "expect boolean",
		},
		{
			description:       "unexpected token (error token)",
			input:             "S0F0\n<BOOLEAN[1] !@#> .",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "expect boolean",
		},
	}

	checkTestCase(t, tests)
}

func TestParseHSMS_Float_ErrorCases(t *testing.T) {
	tests := []testCase{
		{
			description:       "F4 overflow",
			input:             "S0F0\n<F4 1e99999> .",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "overflow",
		},
		{
			description:       "F8 overflow",
			input:             "S0F0\n<F8 1e99999> .",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "overflow",
		},
		{
			description:       "unexpected token",
			input:             "S0F0\n<F4[1] T> .",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "expect float",
		},
		{
			description:       "unexpected token (error token)",
			input:             "S0F0\n<F4[1] !@#> .",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "expect float",
		},
	}

	checkTestCase(t, tests)
}

func TestParseHSMS_Int_ErrorCases(t *testing.T) {
	tests := []testCase{
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
			expectedErrStr:    "overflow",
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
			expectedErrStr:    "overflow",
		},
		{
			description:       "unexpected token",
			input:             "S0F0\n<I1[2] 0.12 T> .",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "expect signed integer",
		},
		{
			description:       "unexpected token (error token)",
			input:             "S0F0\n<I1[1] !@#> .",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "expect signed integer",
		},
	}

	checkTestCase(t, tests)
}

func TestParseHSMS_Uint_ErrorCases(t *testing.T) {
	tests := []testCase{
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
			expectedErrStr:    "overflow",
		},
		{
			description:       "unexpected token",
			input:             "S0F0\n<U1[1] -1> .",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "expect unsigned integer",
		},
		{
			description:       "unexpected token (error token)",
			input:             "S0F0\n<U1[1] !@#> .",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "expect unsigned integer",
		},
	}

	checkTestCase(t, tests)
}

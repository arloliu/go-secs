package sml

import (
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// msgExpect holds per-message parse expectations.
type msgExpect struct {
	stream   uint8
	function uint8
	wbit     bool
}

// testCase describes a parser test scenario.
type testCase struct {
	description       string
	input             string
	expectedNumOfMsgs int
	expectedMsgs      []msgExpect // nil: skip per-msg checks; len must match expectedNumOfMsgs when set
	expectedErrStr    string
}

// checkTestCase runs each test case through Parse (non-strict) and verifies count, per-msg
// stream/function/wbit, and error strings. It does not assert item SML (encoder concern).
func checkTestCase(t *testing.T, tests []testCase, strict bool) {
	t.Helper()
	req := require.New(t)

	var parser *Parser
	if strict {
		parser = NewParser(WithParserStrictMode(true))
	} else {
		parser = NewParser()
	}

	for i, test := range tests {
		t.Logf("Test #%d: %s", i, test.description)

		msgs, err := parser.Parse(test.input)

		if test.expectedErrStr != "" {
			req.ErrorContainsf(err, test.expectedErrStr, "test %d", i)
			continue
		}

		req.NoErrorf(err, "test %d", i)
		req.Lenf(msgs, test.expectedNumOfMsgs, "test %d: message count", i)

		if test.expectedMsgs != nil {
			for j, exp := range test.expectedMsgs {
				if j >= len(msgs) {
					break
				}
				req.Equalf(exp.stream, msgs[j].Stream(), "test %d, msg %d: stream", i, j)
				req.Equalf(exp.function, msgs[j].Function(), "test %d, msg %d: function", i, j)
				req.Equalf(exp.wbit, msgs[j].WaitBit(), "test %d, msg %d: wbit", i, j)
			}
		}

		// verify single-message parse consistency via ParseMessage and ParseHeader
		if test.expectedNumOfMsgs == 1 && len(msgs) == 1 {
			msg := msgs[0]

			smsg, perr := parser.ParseMessage(test.input)
			req.NoErrorf(perr, "ParseMessage test %d", i)
			req.NotNilf(smsg, "ParseMessage test %d", i)
			req.Equalf(msg.Stream(), smsg.Stream(), "ParseMessage stream test %d", i)
			req.Equalf(msg.Function(), smsg.Function(), "ParseMessage function test %d", i)
			req.Equalf(msg.WaitBit(), smsg.WaitBit(), "ParseMessage wbit test %d", i)

			hdr, herr := parser.ParseHeader(test.input)
			req.NoErrorf(herr, "ParseHeader test %d", i)
			req.NotNilf(hdr, "ParseHeader test %d", i)
			req.Equalf(msg.Stream(), hdr.Stream(), "ParseHeader stream test %d", i)
			req.Equalf(msg.Function(), hdr.Function(), "ParseHeader function test %d", i)
			req.Equalf(msg.WaitBit(), hdr.WaitBit(), "ParseHeader wbit test %d", i)
		}
	}
}

func TestParse_TestData_Common(t *testing.T) {
	require := require.New(t)
	data, err := os.ReadFile("./testdata/common.sml")
	require.NoError(err)
	require.NotNil(data)

	msgs, err := ParseStrict(string(data))
	require.NoError(err)
	require.NotNil(msgs)

	msgs, err = Parse(string(data))
	require.NoError(err)
	require.NotNil(msgs)
}

func TestParse_NoErrorCases_StrictMode(t *testing.T) {
	tests := commonTestCases()
	tests = append(tests,
		testCase{
			description:       "1 message, contains non-printable ASCII node, case 1",
			input:             `TestMessage:'S1F1' W <A 'te"s\'t 1' 0x0A 0x0D ' test \'2\''>.`,
			expectedNumOfMsgs: 1,
			expectedMsgs:      []msgExpect{{stream: 1, function: 1, wbit: true}},
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
			expectedMsgs:      []msgExpect{{stream: 1, function: 1, wbit: true}},
		},
	)

	checkTestCase(t, tests, true)

	// Verify strict ASCII item content for the non-printable case.
	require := require.New(t)
	p := NewParser(WithParserStrictMode(true))
	msgs, err := p.Parse(`TestMessage:'S1F1' W <A 'te"s\'t 1' 0x0A 0x0D ' test \'2\''>.`)
	require.NoError(err)
	require.Len(msgs, 1)
	item, ierr := msgs[0].Item()
	require.NoError(ierr)
	ascii, aerr := item.ToASCII()
	require.NoError(aerr)
	require.Equal("te\"s't 1\n\r test '2'", ascii)
	require.Equal(19, item.Size())

	// Verify strict ASCII with single-quote input and double-quote input.
	tests2 := []testCase{
		{
			description:       "1 message, single-quote ASCII node",
			input:             `TestMessage:'S1F1' W <A 'text'>.`,
			expectedNumOfMsgs: 1,
			expectedMsgs:      []msgExpect{{stream: 1, function: 1, wbit: true}},
		},
		{
			description:       "1 message, single-quote ASCII node with newlines",
			input:             "TestMessage:'S1F1' W <A 'text1\ntest2\ntest3'>.",
			expectedNumOfMsgs: 1,
			expectedMsgs:      []msgExpect{{stream: 1, function: 1, wbit: true}},
		},
	}
	checkTestCase(t, tests2, true)

	tests3 := []testCase{
		{
			description:       "1 message, double-quote ASCII node",
			input:             `TestMessage:'S1F1' W <A "text">.`,
			expectedNumOfMsgs: 1,
			expectedMsgs:      []msgExpect{{stream: 1, function: 1, wbit: true}},
		},
	}
	checkTestCase(t, tests3, true)
}

func TestParse_NoErrorCases_NonStrictMode(t *testing.T) {
	tests := commonTestCases()
	checkTestCase(t, tests, false)

	tests = []testCase{
		{
			description:       "1 message, single-quote ASCII node",
			input:             `TestMessage:'S1F1' W <A 'text'>.`,
			expectedNumOfMsgs: 1,
			expectedMsgs:      []msgExpect{{stream: 1, function: 1, wbit: true}},
		},
		{
			description:       "1 message, single-quote ASCII node with newlines",
			input:             "TestMessage:'S1F1' W <A 'text1\ntest2\ntest3'>.",
			expectedNumOfMsgs: 1,
			expectedMsgs:      []msgExpect{{stream: 1, function: 1, wbit: true}},
		},
		{
			description:       "1 message, single-quote ASCII node with space before closing quote",
			input:             `S99F99 <A 'test1  'test2'  >   .`,
			expectedNumOfMsgs: 1,
			expectedMsgs:      []msgExpect{{stream: 99, function: 99, wbit: false}},
		},
	}
	checkTestCase(t, tests, false)

	tests = []testCase{
		{
			description:       "1 message, double-quote ASCII node",
			input:             `TestMessage:'S1F1' W <A "text">.`,
			expectedNumOfMsgs: 1,
			expectedMsgs:      []msgExpect{{stream: 1, function: 1, wbit: true}},
		},
		{
			description:       "1 message, double-quote ASCII node with newlines",
			input:             "TestMessage:'S1F1' W <A \"text1\ntest2\ntest3\">.",
			expectedNumOfMsgs: 1,
			expectedMsgs:      []msgExpect{{stream: 1, function: 1, wbit: true}},
		},
		{
			description:       "1 message, double-quote ASCII node with space before closing quote",
			input:             `S99F99 <A "test1  "test2"  >   .`,
			expectedNumOfMsgs: 1,
			expectedMsgs:      []msgExpect{{stream: 99, function: 99, wbit: false}},
		},
	}
	checkTestCase(t, tests, false)
}

func TestParseItem_ASCII(t *testing.T) {
	testcases := []struct {
		description    string
		strictMode     bool
		sml            string
		expectedStr    string
		expectedErrStr string
	}{
		{
			description: "ASCII empty string with quote, with size hint",
			strictMode:  false,
			sml:         "<A[0] ''>",
			expectedStr: "",
		},
		{
			description: "ASCII empty string with quote, without size hint",
			strictMode:  false,
			sml:         "<A ''>",
			expectedStr: "",
		},
		{
			description: "ASCII empty string without quote, with size hint",
			strictMode:  false,
			sml:         "<A[0]>",
			expectedStr: "",
		},
		{
			description: "ASCII empty string without quote, without size hint",
			strictMode:  false,
			sml:         "<A>",
			expectedStr: "",
		},
		{
			description: "ASCII normal string",
			strictMode:  false,
			sml:         "<A 'text'>",
			expectedStr: "text",
		},
		{
			description: "ASCII unescaped single quote with characters",
			strictMode:  false,
			sml:         "<A[5] 'a'b'c'>",
			expectedStr: "a'b'c",
		},
		{
			description: "ASCII unescaped single quote only",
			strictMode:  false,
			sml:         "<A[2] ''''>",
			expectedStr: "''",
		},
		{
			description: "ASCII unescaped single quote with new line",
			strictMode:  false,
			sml:         "<A[3] '''\n'>",
			expectedStr: "''\n",
		},
		{
			description: "ASCII extended character",
			strictMode:  false,
			sml:         "<A[1] '\xa9'>",
			expectedStr: "\xa9",
		},
		{
			description: "ASCII string with size hint, with extended character",
			strictMode:  false,
			sml:         "<A[4] '\xa9abc'>",
			expectedStr: "\xa9abc",
		},
		{
			description: "ASCII string without size hint, with extended character",
			strictMode:  false,
			sml:         "<A '\xa9abc'>",
			expectedStr: "\xa9abc",
		},
		{
			description: "ASCII unescaped single quote, with characters, with new line",
			strictMode:  false,
			sml:         "<A[4] 'a''\n'>",
			expectedStr: "a''\n",
		},
		{
			description: "ASCII '> in quote string, with size hint",
			strictMode:  false,
			sml:         "<A[5] 'ab'>c'>",
			expectedStr: "ab'>c",
		},
		{
			description: "ASCII '> in quote string, without size hint",
			strictMode:  false,
			sml:         "<A 'ab'>c'>",
			expectedStr: "ab",
		},
		{
			description: "ASCII special characters, with size hint",
			strictMode:  false,
			sml:         "<A[31] '~`!@#$%^&*()_+-=[]\\{}|:;,./<>?\"'>",
			expectedStr: "~`!@#$%^&*()_+-=[]\\{}|:;,./<>?\"",
		},
		{
			description: "ASCII special characters, without size hint",
			strictMode:  false,
			sml:         "<A '~`!@#$%^&*()_+-=[]\\{}|:;,./<>?\"'>",
			expectedStr: "~`!@#$%^&*()_+-=[]\\{}|:;,./<>?\"",
		},
		{
			description:    "invalid size hint, size larger than actual string",
			strictMode:     false,
			sml:            "<A[5] 'abcd'>",
			expectedErrStr: "size overflow",
		},
		{
			description:    "invalid ASCII quote",
			strictMode:     false,
			sml:            "<A[1] abcd'>",
			expectedErrStr: "invalid quote for ASCII string",
		},
		{
			description:    "invalid ASCII quote duplicate",
			strictMode:     false,
			sml:            "<A[1] abcd'>",
			expectedErrStr: "invalid quote for ASCII string",
		},
	}

	require := require.New(t)

	for i, tt := range testcases {
		t.Logf("Test #%d: %s", i, tt.description)
		parser := NewParser()
		parser.strict = tt.strictMode
		parser.input = tt.sml
		parser.data = tt.sml
		parser.len = len(tt.sml)
		parser.pos = 0

		item, err := parser.parseItem()
		if len(tt.expectedErrStr) > 0 {
			require.Nil(item)
			require.ErrorContains(err, tt.expectedErrStr)
		} else {
			require.NoError(err)
			require.NotNil(item)
			str, err := item.ToASCII()
			require.NoError(err)
			require.Equal(tt.expectedStr, str)
		}
	}
}

func TestParseItem_LocalizedStr(t *testing.T) {
	testcases := []struct {
		description    string
		strictMode     bool
		sml            string
		expectedStr    string
		expectedErrStr string
	}{
		{
			description: "LocalizedStr empty string with quote",
			strictMode:  false,
			sml:         "<W ''>",
			expectedStr: "",
		},
		{
			description: "LocalizedStr normal string",
			strictMode:  false,
			sml:         "<W 'text'>",
			expectedStr: "text",
		},
		{
			description: "LocalizedStr unicode string",
			strictMode:  false,
			sml:         "<W '你好'>",
			expectedStr: "你好",
		},
		{
			description: "LocalizedStr '> in quote string",
			strictMode:  false,
			sml:         "<W 'ab'>c'>",
			expectedStr: "ab",
		},
		{
			description:    "invalid LocalizedStr quote",
			strictMode:     false,
			sml:            "<W[1] abcd'>",
			expectedErrStr: "invalid quote for Localized string",
		},
	}

	require := require.New(t)

	for i, tt := range testcases {
		t.Logf("Test #%d: %s", i, tt.description)
		parser := NewParser()
		parser.strict = tt.strictMode
		parser.input = tt.sml
		parser.data = tt.sml
		parser.len = len(tt.sml)
		parser.pos = 0

		item, err := parser.parseItem()
		if len(tt.expectedErrStr) > 0 {
			require.Nil(item)
			require.ErrorContains(err, tt.expectedErrStr)
		} else {
			require.NoError(err)
			require.NotNil(item)
			str, err := item.ToLocalizedStr()
			require.NoError(err)
			require.Equal(tt.expectedStr, str)
		}
	}
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
			expectedMsgs:      []msgExpect{{stream: 0, function: 0, wbit: false}},
		},
		{
			description:       "1 message, no data item, with message name at front",
			input:             "TestMessage:S0F1 W\n.",
			expectedNumOfMsgs: 1,
			expectedMsgs:      []msgExpect{{stream: 0, function: 1, wbit: true}},
		},
		{
			description:       "1 message, no data item, with single quoted stream-function",
			input:             "TestMessage : 'S0F1' W\n.",
			expectedNumOfMsgs: 1,
			expectedMsgs:      []msgExpect{{stream: 0, function: 1, wbit: true}},
		},
		{
			description:       "1 message, single-quote ASCII node",
			input:             `TestMessage:'S1F1' W <A 'text'>.`,
			expectedNumOfMsgs: 1,
			expectedMsgs:      []msgExpect{{stream: 1, function: 1, wbit: true}},
		},
		{
			description:       "1 message, single-quote ASCII node, message name contains dot",
			input:             `Test.Messaage : 'S1F1' W <A 'text'>.`,
			expectedNumOfMsgs: 1,
			expectedMsgs:      []msgExpect{{stream: 1, function: 1, wbit: true}},
		},
		{
			description:       "1 message, single-quote ASCII node, empty message name",
			input:             `  :  'S1F1' W <A 'text'>.`,
			expectedNumOfMsgs: 1,
			expectedMsgs:      []msgExpect{{stream: 1, function: 1, wbit: true}},
		},
		{
			description:       "1 message, single-quote ASCII node, without message name",
			input:             `'S1F1' W <A 'text'>.`,
			expectedNumOfMsgs: 1,
			expectedMsgs:      []msgExpect{{stream: 1, function: 1, wbit: true}},
		},
		{
			description:       "1 message, single-quote ASCII node, without message name and contains colon in text",
			input:             `'S1F1' W <A 'this.is:text'>.`,
			expectedNumOfMsgs: 1,
			expectedMsgs:      []msgExpect{{stream: 1, function: 1, wbit: true}},
		},
		{
			description:       "1 message, Binary node",
			input:             `TestMessage   : S63F127 W <B[3] 0b0 0xFE 255>.`,
			expectedNumOfMsgs: 1,
			expectedMsgs:      []msgExpect{{stream: 63, function: 127, wbit: true}},
		},
		{
			description:       "1 message, Boolean node",
			input:             `TestMessage:'S126F254' <BOOLEAN True False>.`,
			expectedNumOfMsgs: 1,
			expectedMsgs:      []msgExpect{{stream: 126, function: 254, wbit: false}},
		},
		{
			description: "2 messages, F4, F8 node, empty message names",
			input: `  :  S126F254 <F4 +0.1 -0.1>.
			         : S127F255 <F8 1e3 1E-3 .5e-1>.`,
			expectedNumOfMsgs: 2,
			expectedMsgs: []msgExpect{
				{stream: 126, function: 254, wbit: false},
				{stream: 127, function: 255, wbit: false},
			},
		},
		{
			description: "2 messages, F4, F8 node",
			input: `S126F254 <F4 +0.1 -0.1>.
			        S127F255 <F8 1e3 1E-3 .5e-1>.`,
			expectedNumOfMsgs: 2,
			expectedMsgs: []msgExpect{
				{stream: 126, function: 254, wbit: false},
				{stream: 127, function: 255, wbit: false},
			},
		},
		{
			description: "4 messages, I1, I2, I4, I8 node",
			input: `'S0F0' <I1 -128 -64 -1 0 1 64 127>.
			        Line2: "S0F0" <I2 -32768 32767>.
			        S0F0 <I4 -2147483648 2147483647>.
			        S0F0 <I8 -9223372036854775808 9223372036854775807>.`,
			expectedNumOfMsgs: 4,
			expectedMsgs: []msgExpect{
				{stream: 0, function: 0, wbit: false},
				{stream: 0, function: 0, wbit: false},
				{stream: 0, function: 0, wbit: false},
				{stream: 0, function: 0, wbit: false},
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
			expectedMsgs: []msgExpect{
				{stream: 0, function: 0, wbit: false},
				{stream: 0, function: 0, wbit: false},
				{stream: 0, function: 0, wbit: false},
				{stream: 0, function: 0, wbit: false},
				{stream: 0, function: 0, wbit: false},
			},
		},
		{
			description: "1 message, Nested list node with line comment",
			input: `S0F0 // message header comment
<L          // comment1
  <L[0]>    // comment
  <L[2]     // comment
    <A[0] ''>  // comment
    <B[0]>  // comment
  >         // comment
>           // comment
.           // comment
`,
			expectedNumOfMsgs: 1,
			expectedMsgs:      []msgExpect{{stream: 0, function: 0, wbit: false}},
		},
		{
			description: "1 message, Nested list node with block comment",
			input: `S0F0 /* message header comment */
<L          /* comment1 */
  <L[0]>    /* comment */
  <L[2]     /* comment */
    <A[0] ''>  /* comment */
    <B[0]>  /* comment */
  >         /* comment */
>           /* comment */
.           /* comment */
`,
			expectedNumOfMsgs: 1,
			expectedMsgs:      []msgExpect{{stream: 0, function: 0, wbit: false}},
		},
	}
}

func TestParse_List_ErrorCases(t *testing.T) {
	tests := []testCase{
		{
			description:       "unexpected token (bare identifier as child)",
			input:             "S0F0\n<L[1] T>\n.",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "expected child data item",
		},
		{
			description:       "unexpected token (garbage punctuation as child)",
			input:             "S0F0\n<L[1] !@#>\n.",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "expected child data item",
		},
	}

	checkTestCase(t, tests, true)
}

func TestParse_ASCII_ErrorCases_NonStrictMode(t *testing.T) {
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
			description:       "unexpected token (has extra >)",
			input:             "S0F0\n<A 'ab>'>> .",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "expect dot in the end",
		},
		{
			description:       "unexpected token (has '> in quote string)",
			input:             "S0F0\n<L <A 'ab'>'> <A 'second'> >.",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "expected child data item",
		},
	}

	checkTestCase(t, tests, false)
}

func TestParse_ASCII_ErrorCases_StrictMode(t *testing.T) {
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
			description:       "ASCII number character out of latin-1 range",
			input:             "S0F0\n<A 256> .",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "out of latin-1 range",
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
			description:       "unexpected token (extra > in quote string)",
			input:             "S0F0\n<A 'ab>'> .",
			expectedNumOfMsgs: 0,
			expectedErrStr:    "unclosed quote string",
		},
	}

	checkTestCase(t, tests, true)
}

func TestParse_Binary_ErrorCases(t *testing.T) {
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

	checkTestCase(t, tests, false)
}

func TestParse_Boolean_ErrorCases(t *testing.T) {
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

	checkTestCase(t, tests, false)
}

func TestParse_Float_ErrorCases(t *testing.T) {
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

	checkTestCase(t, tests, false)
}

func TestParse_Int_ErrorCases(t *testing.T) {
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

	checkTestCase(t, tests, false)
}

func TestParse_Uint_ErrorCases(t *testing.T) {
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

	checkTestCase(t, tests, false)
}

// TestParse_TruncatedSizeRange verifies that truncated size-range input returns
// a *ParseError rather than panicking with an index out of range.
func TestParse_TruncatedSizeRange(t *testing.T) {
	inputs := []struct {
		desc  string
		input string
	}{
		{"truncated after open bracket", "S1F1\n<A["},
		{"truncated after min size digit", "S1F1\n<A[1"},
		{"truncated after range operator", "S1F1\n<A[1.."},
		{"truncated no-min range operator", "S1F1\n<A[.."},
	}

	p := NewParser()

	for _, tc := range inputs {
		t.Run(tc.desc, func(t *testing.T) {
			var err error

			require.NotPanics(t, func() {
				_, err = p.Parse(tc.input)
			}, "Parse must not panic on truncated input: %q", tc.input)

			require.Error(t, err, "expected error for truncated input: %q", tc.input)

			var parseErr *ParseError
			require.True(t, errors.As(err, &parseErr),
				"expected *ParseError for truncated input %q, got %T: %v", tc.input, err, err)
		})
	}
}

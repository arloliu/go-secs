package hsmstest_test

import (
	"os"
	"os/exec"
	"testing"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/hsms/hsmstest"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

func TestRequireDataMessageEqual_Exact(t *testing.T) {
	t.Parallel()

	a, err := hsms.NewDataMessage(1, 1, true, 5, [4]byte{0, 0, 0, 1}, secs2.A("hi"))
	require.NoError(t, err)
	b, err := hsms.NewDataMessage(1, 1, true, 5, [4]byte{0, 0, 0, 1}, secs2.A("hi"))
	require.NoError(t, err)

	hsmstest.RequireDataMessageEqual(t, a, b)
}

func TestRequireDataMessageEqual_IgnoreSystemBytes(t *testing.T) {
	t.Parallel()

	a, err := hsms.NewDataMessage(1, 1, true, 5, [4]byte{0, 0, 0, 1}, secs2.A("hi"))
	require.NoError(t, err)
	b, err := hsms.NewDataMessage(1, 1, true, 5, [4]byte{0, 0, 0, 2}, secs2.A("hi"))
	require.NoError(t, err)

	hsmstest.RequireDataMessageEqual(t, a, b, hsmstest.IgnoreSystemBytes())
}

func TestRequireDataMessageEqual_BodyOnly(t *testing.T) {
	t.Parallel()

	a, err := hsms.NewDataMessage(1, 1, true, 5, [4]byte{0, 0, 0, 1}, secs2.A("hi"))
	require.NoError(t, err)
	b, err := hsms.NewDataMessage(2, 3, false, 9, [4]byte{9, 9, 9, 9}, secs2.A("hi"))
	require.NoError(t, err)

	hsmstest.RequireDataMessageEqual(t, a, b, hsmstest.BodyOnly())
}

// TestRequireDataMessageEqual_FailingAssertionMessage verifies a mismatch produces a failing,
// readable assertion. RequireDataMessageEqual takes a concrete *testing.T (per its documented
// signature), so the only way to observe its failure without failing this test binary is to
// re-exec the induced-failure case in a subprocess — the standard Go idiom for testing a
// function that calls t.FailNow.
func TestRequireDataMessageEqual_FailingAssertionMessage(t *testing.T) {
	t.Parallel()

	if os.Getenv("HSMSTEST_INDUCE_FAILURE") == "1" {
		a, err := hsms.NewDataMessage(1, 1, true, 5, [4]byte{0, 0, 0, 1}, secs2.A("hi"))
		require.NoError(t, err)
		b, err := hsms.NewDataMessage(1, 1, true, 5, [4]byte{0, 0, 0, 1}, secs2.A("bye"))
		require.NoError(t, err)

		hsmstest.RequireDataMessageEqual(t, a, b) // expected to fail: proves the message is readable

		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRequireDataMessageEqual_FailingAssertionMessage$", "-test.v")
	cmd.Env = append(os.Environ(), "HSMSTEST_INDUCE_FAILURE=1")
	out, err := cmd.CombinedOutput()

	require.Error(t, err, "the induced mismatch must make the subprocess test fail")
	require.Contains(t, string(out), "mismatch", "failure output must be a readable, item-specific message")
}

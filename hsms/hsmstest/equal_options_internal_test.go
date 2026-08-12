package hsmstest

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEqualOptions_SetOnlyTheirOwnField pins what each option narrows.
//
// RequireDataMessageEqual skips whole groups of comparisons based on these flags,
// so an option that set the wrong one would keep passing every equality test
// while silently disabling the checks those tests believe they are making.
// IgnoreSessionID setting bodyOnly, for instance, would drop stream, function,
// wait bit, and System Bytes from every comparison that uses it.
//
// This runs in-package because the flags are the thing under test.
func TestEqualOptions_SetOnlyTheirOwnField(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opt  EqualOption
		want equalOpts
	}{
		{name: "IgnoreSessionID", opt: IgnoreSessionID(), want: equalOpts{ignoreSessionID: true}},
		{name: "IgnoreSystemBytes", opt: IgnoreSystemBytes(), want: equalOpts{ignoreSystemByte: true}},
		{name: "BodyOnly", opt: BodyOnly(), want: equalOpts{bodyOnly: true}},
		{name: "HeaderOnly", opt: HeaderOnly(), want: equalOpts{headerOnly: true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got equalOpts
			tt.opt(&got)

			require.Equal(t, tt.want, got, "%s must set its own flag and no other", tt.name)
		})
	}
}

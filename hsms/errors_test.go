package hsms

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRejectError_Error pins the exact rendered string RejectError.Error() produces.
//
// Reason is a plain byte (no String() mapping — SEMI E37 Table 9 leaves 5-255 to
// subsidiary standards/local entities), so every reason code, defined or not,
// renders through the same "%d" verb in errors.go.
func TestRejectError_Error(t *testing.T) {
	tests := []struct {
		name string
		err  *RejectError
		want string
	}{
		{
			name: "defined reason: RejectNotSelected",
			err:  &RejectError{Reason: RejectNotSelected},
			want: "hsms: peer rejected message (reason 4)",
		},
		{
			name: "defined reason: RejectSTypeNotSupported",
			err:  &RejectError{Reason: RejectSTypeNotSupported},
			want: "hsms: peer rejected message (reason 1)",
		},
		{
			name: "reserved or local reason code (outside E37 Table 9's defined 1-4)",
			err:  &RejectError{Reason: 200},
			want: "hsms: peer rejected message (reason 200)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.err.Error())
		})
	}
}

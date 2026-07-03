package hsms

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConnState_String(t *testing.T) {
	require.Equal(t, "NotConnected", NotConnectedState.String())
	require.Equal(t, "NotSelected", NotSelectedState.String())
	require.Equal(t, "Selected", SelectedState.String())
	require.Contains(t, ConnState(99).String(), "ConnState")
}

func TestConnState_Ordering(t *testing.T) {
	require.Less(t, uint32(NotConnectedState), uint32(NotSelectedState))
	require.Less(t, uint32(NotSelectedState), uint32(SelectedState))
}

func TestConnSentinels_Distinct(t *testing.T) {
	all := []error{ErrAlreadyOpen, ErrNotOpen, ErrNotSelectedState, ErrConnClosed, ErrT3Timeout, ErrT6Timeout, ErrCloseTimeout}
	for i := range all {
		for j := range all {
			if i != j {
				require.False(t, errors.Is(all[i], all[j]))
			}
		}
	}
}

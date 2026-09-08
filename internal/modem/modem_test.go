package modem

import (
	"testing"

	"modem-service/internal/mm"
)

func TestIsModemReadyState(t *testing.T) {
	tests := []struct {
		state int32
		want  bool
	}{
		{mm.MMModemStateFailed, false},
		{mm.MMModemStateUnknown, false},
		{mm.MMModemStateInitializing, false},
		{mm.MMModemStateLocked, true},
		{mm.MMModemStateDisabled, false},
		{mm.MMModemStateDisabling, false},
		{mm.MMModemStateEnabling, false},
		{mm.MMModemStateEnabled, true},
		{mm.MMModemStateSearching, true},
		{mm.MMModemStateRegistered, true},
		{mm.MMModemStateConnecting, true},
		{mm.MMModemStateConnected, true},
	}

	for _, tt := range tests {
		if got := isModemReadyState(tt.state); got != tt.want {
			t.Errorf("isModemReadyState(%s) = %t, want %t", mm.ModemStateToString(tt.state), got, tt.want)
		}
	}
}

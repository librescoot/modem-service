package mm

import "testing"

func TestLockReasonToString(t *testing.T) {
	for _, tc := range []struct {
		lock uint32
		want string
	}{
		{MMLockUnknown, "unknown"},
		{MMLockNone, "none"},
		{MMLockSimPin, "sim-pin"},
		{MMLockSimPin2, "sim-pin2"},
		{MMLockSimPuk, "sim-puk"},
		{MMLockSimPuk2, "sim-puk2"},
		{MMLockPhSimPin, "ph-sim-pin"},
		{MMLockPhFsimPin, "ph-fsim-pin"},
		{MMLockPhFsimPuk, "ph-fsim-puk"},
		{MMLockPhNetPin, "ph-net-pin"},
		{MMLockPhNetPuk, "ph-net-puk"},
		{MMLockPhSpPin, "ph-sp-pin"},
		{MMLockPhSpPuk, "ph-sp-puk"},
		{MMLockPhCorpPin, "ph-corp-pin"},
		{MMLockPhCorpPuk, "ph-corp-puk"},
		{999, "unknown"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			if got := LockReasonToString(tc.lock); got != tc.want {
				t.Errorf("LockReasonToString(%d) = %q, want %q", tc.lock, got, tc.want)
			}
		})
	}
}

package connectivity

import (
	"testing"
	"time"
)

const simPresent = "present"

func newTestClassifier(start time.Time) (*Classifier, *time.Time) {
	t := start
	c := newWithClock(func() time.Time { return t })
	return c, &t
}

func TestRawState(t *testing.T) {
	tests := []struct {
		name   string
		status string
		sim    string
		want   State
	}{
		{"no-sim-even-when-connected", StatusConnected, SIMMissing, NoSIM},
		{"no-sim-with-off-modem", StatusOff, SIMMissing, NoSIM},
		{"connected-present", StatusConnected, simPresent, Online},
		{"connected-locked-sim", StatusConnected, "locked", Online},
		{"disconnected", StatusDisconnected, simPresent, Offline},
		{"off", StatusOff, simPresent, Offline},
		{"no-modem", StatusNoModem, simPresent, Offline},
		{"empty-status", "", simPresent, Offline},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := rawState(tc.status, tc.sim); got != tc.want {
				t.Errorf("rawState(status=%q, sim=%q) = %q, want %q",
					tc.status, tc.sim, got, tc.want)
			}
		})
	}
}

func TestFirstObservationCommitsImmediately(t *testing.T) {
	c, _ := newTestClassifier(time.Unix(0, 0))
	if got := c.Classify(StatusConnected, simPresent); got != Online {
		t.Errorf("first observation: got %q, want %q", got, Online)
	}
}

func TestNoSIMCommitsImmediately(t *testing.T) {
	c, now := newTestClassifier(time.Unix(0, 0))
	c.Classify(StatusConnected, simPresent)
	*now = now.Add(1 * time.Second)
	if got := c.Classify(StatusNoModem, SIMMissing); got != NoSIM {
		t.Errorf("sim-missing: got %q, want %q", got, NoSIM)
	}
}

func TestOnlineToOfflineDebounces(t *testing.T) {
	c, now := newTestClassifier(time.Unix(0, 0))
	c.Classify(StatusConnected, simPresent) // committed=online

	// 30s disconnected — pendingSince set here
	*now = now.Add(30 * time.Second)
	if got := c.Classify(StatusDisconnected, simPresent); got != Online {
		t.Errorf("after 30s disconnected: got %q, want online", got)
	}

	// +90s => 2min from pendingSince, still not enough
	*now = now.Add(90 * time.Second)
	if got := c.Classify(StatusDisconnected, simPresent); got != Online {
		t.Errorf("after 2min disconnected: got %q, want online", got)
	}

	// +90s => 3min from pendingSince, commit offline
	*now = now.Add(90 * time.Second)
	if got := c.Classify(StatusDisconnected, simPresent); got != Offline {
		t.Errorf("after 3min disconnected: got %q, want offline", got)
	}
}

func TestOfflineToOnlineDebounces(t *testing.T) {
	c, now := newTestClassifier(time.Unix(0, 0))
	c.Classify(StatusDisconnected, simPresent) // committed=offline

	*now = now.Add(30 * time.Second)
	if got := c.Classify(StatusConnected, simPresent); got != Offline {
		t.Errorf("after 30s connected: got %q, want offline", got)
	}

	// 60s from pendingSince
	*now = now.Add(60 * time.Second)
	if got := c.Classify(StatusConnected, simPresent); got != Online {
		t.Errorf("after 60s connected: got %q, want online", got)
	}
}

func TestCoverageFlickerDoesNotFlipCommitted(t *testing.T) {
	c, now := newTestClassifier(time.Unix(0, 0))
	c.Classify(StatusConnected, simPresent)

	*now = now.Add(10 * time.Second)
	c.Classify(StatusDisconnected, simPresent)

	*now = now.Add(10 * time.Second)
	c.Classify(StatusConnected, simPresent)

	*now = now.Add(10 * time.Second)
	if got := c.Classify(StatusConnected, simPresent); got != Online {
		t.Errorf("after coverage flicker: got %q, want online (should never have left)", got)
	}
}

func TestLockedSIMPin2StillOnline(t *testing.T) {
	// Deep Blue real-world state: sim-state=locked (pin2), modem status=connected.
	// Data works fine, classifier should report online.
	c, _ := newTestClassifier(time.Unix(0, 0))
	if got := c.Classify(StatusConnected, "locked"); got != Online {
		t.Errorf("pin2-locked but connected: got %q, want online", got)
	}
}

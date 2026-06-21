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

// in builds an Inputs for an enabled, healthy modem in the given status/SIM
// state with home registration. Tests override fields as needed.
func in(status, sim string) Inputs {
	return Inputs{ModemStatus: status, SIMState: sim, Registration: "home", Enabled: true}
}

func TestRawState(t *testing.T) {
	tests := []struct {
		name string
		in   Inputs
		want State
	}{
		{"disabled-wins-over-everything", Inputs{ModemStatus: StatusConnected, SIMState: simPresent, Enabled: false}, Disabled},
		{"hard-failed", Inputs{ModemStatus: StatusNoModem, SIMState: simPresent, Enabled: true, HardFailed: true}, Failed},
		{"no-sim-even-when-connected", in(StatusConnected, SIMMissing), NoSIM},
		{"no-sim-with-off-modem", in(StatusOff, SIMMissing), NoSIM},
		{"no-sim-inactive", in(StatusDisconnected, SIMInactive), NoSIM},
		{"denied-registration", Inputs{ModemStatus: StatusDisconnected, SIMState: simPresent, Registration: RegistrationDenied, Enabled: true}, Denied},
		{"failed-registration", Inputs{ModemStatus: StatusDisconnected, SIMState: simPresent, Registration: RegistrationFailed, Enabled: true}, Denied},
		{"connected-present", in(StatusConnected, simPresent), Connected},
		{"connected-locked-sim", in(StatusConnected, "locked"), Connected},
		{"disconnected", in(StatusDisconnected, simPresent), Disconnected},
		{"off", in(StatusOff, simPresent), Disconnected},
		{"no-modem", in(StatusNoModem, simPresent), Disconnected},
		{"empty-status", in("", simPresent), Disconnected},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := rawState(tc.in); got != tc.want {
				t.Errorf("rawState(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestFirstObservationCommitsImmediately(t *testing.T) {
	c, _ := newTestClassifier(time.Unix(0, 0))
	if got := c.Classify(in(StatusConnected, simPresent)); got != Connected {
		t.Errorf("first observation: got %q, want %q", got, Connected)
	}
}

func TestNoSIMCommitsImmediately(t *testing.T) {
	c, now := newTestClassifier(time.Unix(0, 0))
	c.Classify(in(StatusConnected, simPresent))
	*now = now.Add(1 * time.Second)
	if got := c.Classify(in(StatusNoModem, SIMMissing)); got != NoSIM {
		t.Errorf("sim-missing: got %q, want %q", got, NoSIM)
	}
}

func TestDisabledCommitsImmediately(t *testing.T) {
	c, now := newTestClassifier(time.Unix(0, 0))
	c.Classify(in(StatusConnected, simPresent)) // committed=connected
	*now = now.Add(1 * time.Second)
	got := c.Classify(Inputs{ModemStatus: StatusOff, SIMState: simPresent, Enabled: false})
	if got != Disabled {
		t.Errorf("disable: got %q, want %q", got, Disabled)
	}
}

func TestDeniedDebouncesBeforeHiding(t *testing.T) {
	c, now := newTestClassifier(time.Unix(0, 0))
	c.Classify(in(StatusConnected, simPresent)) // committed=connected

	denied := Inputs{ModemStatus: StatusDisconnected, SIMState: simPresent, Registration: RegistrationDenied, Enabled: true}

	// A transient registration denial must not hide the icon immediately.
	// pendingSince is set on this first denied observation.
	*now = now.Add(30 * time.Second)
	if got := c.Classify(denied); got != Connected {
		t.Errorf("after 30s denied: got %q, want connected", got)
	}
	// 60s from pendingSince commits denied.
	*now = now.Add(60 * time.Second)
	if got := c.Classify(denied); got != Denied {
		t.Errorf("after 60s denied: got %q, want denied", got)
	}
}

func TestConnectedToDisconnectedDebounces(t *testing.T) {
	c, now := newTestClassifier(time.Unix(0, 0))
	c.Classify(in(StatusConnected, simPresent)) // committed=connected

	// 30s disconnected — pendingSince set here
	*now = now.Add(30 * time.Second)
	if got := c.Classify(in(StatusDisconnected, simPresent)); got != Connected {
		t.Errorf("after 30s disconnected: got %q, want connected", got)
	}

	// +90s => 2min from pendingSince, still not enough
	*now = now.Add(90 * time.Second)
	if got := c.Classify(in(StatusDisconnected, simPresent)); got != Connected {
		t.Errorf("after 2min disconnected: got %q, want connected", got)
	}

	// +90s => 3min from pendingSince, commit disconnected
	*now = now.Add(90 * time.Second)
	if got := c.Classify(in(StatusDisconnected, simPresent)); got != Disconnected {
		t.Errorf("after 3min disconnected: got %q, want disconnected", got)
	}
}

func TestDisconnectedToConnectedDebounces(t *testing.T) {
	c, now := newTestClassifier(time.Unix(0, 0))
	c.Classify(in(StatusDisconnected, simPresent)) // committed=disconnected

	*now = now.Add(30 * time.Second)
	if got := c.Classify(in(StatusConnected, simPresent)); got != Disconnected {
		t.Errorf("after 30s connected: got %q, want disconnected", got)
	}

	// 60s from pendingSince
	*now = now.Add(60 * time.Second)
	if got := c.Classify(in(StatusConnected, simPresent)); got != Connected {
		t.Errorf("after 60s connected: got %q, want connected", got)
	}
}

// Leaving a hidden state toward a provisioned-but-down state shows the icon
// promptly (no offline debounce, unlike connected -> disconnected).
func TestLeavingDisabledShowsPromptly(t *testing.T) {
	c, now := newTestClassifier(time.Unix(0, 0))
	c.Force(Disabled)

	*now = now.Add(1 * time.Second)
	if got := c.Classify(in(StatusDisconnected, simPresent)); got != Disconnected {
		t.Errorf("re-enabled: got %q, want disconnected immediately", got)
	}
}

func TestCoverageFlickerDoesNotFlipCommitted(t *testing.T) {
	c, now := newTestClassifier(time.Unix(0, 0))
	c.Classify(in(StatusConnected, simPresent))

	*now = now.Add(10 * time.Second)
	c.Classify(in(StatusDisconnected, simPresent))

	*now = now.Add(10 * time.Second)
	c.Classify(in(StatusConnected, simPresent))

	*now = now.Add(10 * time.Second)
	if got := c.Classify(in(StatusConnected, simPresent)); got != Connected {
		t.Errorf("after coverage flicker: got %q, want connected (should never have left)", got)
	}
}

func TestLockedSIMPin2StillConnected(t *testing.T) {
	// Deep Blue real-world state: sim-state=locked (pin2), modem status=connected.
	// Data works fine, classifier should report connected.
	c, _ := newTestClassifier(time.Unix(0, 0))
	if got := c.Classify(in(StatusConnected, "locked")); got != Connected {
		t.Errorf("pin2-locked but connected: got %q, want connected", got)
	}
}

func TestIsConnected(t *testing.T) {
	c, _ := newTestClassifier(time.Unix(0, 0))
	c.Classify(in(StatusConnected, simPresent))
	if !c.IsConnected() {
		t.Error("IsConnected: got false, want true after connected")
	}
	c.Force(Disconnected)
	if c.IsConnected() {
		t.Error("IsConnected: got true, want false after disconnected")
	}
}

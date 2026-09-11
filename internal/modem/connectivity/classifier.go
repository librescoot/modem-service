// Package connectivity classifies modem state with transition hysteresis.
package connectivity

import "time"

// State is the committed connectivity state consumed by the dashboard.
type State string

const (
	Unknown      State = ""
	Connected    State = "connected"    // modem connected, data path up
	Disconnected State = "disconnected" // enabled + SIM present, searching/registering/no signal
	Disabled     State = "disabled"     // modem intentionally powered off by command
	NoSIM        State = "no-sim"       // SIM missing or inactive
	Denied       State = "denied"       // registration denied/failed (e.g. deactivated SIM)
	Failed       State = "failed"       // health terminal: modem broken / needs replacement
)

// Modem status values — must match the "connected"/"disconnected"/"off"/
// "no-modem" strings that modem.Manager.GetModemInfo assigns to State.Status.
const (
	StatusConnected    = "connected"
	StatusDisconnected = "disconnected"
	StatusOff          = "off"
	StatusNoModem      = "no-modem"
)

// SIM state values — must match modem.SIMState* string constants.
const (
	SIMMissing  = "missing"
	SIMInactive = "inactive"
)

// Registration values — must match modem.Registration* string constants.
const (
	RegistrationDenied = "denied"
	RegistrationFailed = "failed"
)

// Transition delays prevent coverage flicker from thrashing UI and GPS mode.
const (
	OnlineDebounce  = 60 * time.Second
	OfflineDebounce = 3 * time.Minute
	// DeniedDebounce rides out brief registration dips during handover/search
	// (those last seconds) without letting a genuinely deactivated SIM linger.
	DeniedDebounce = 60 * time.Second
)

// Inputs is the raw snapshot the classifier folds into a committed State.
type Inputs struct {
	ModemStatus  string // modem.State.Status: connected/disconnected/off/no-modem
	SIMState     string // modem.SIMState*: present/missing/inactive/locked/unknown
	Registration string // modem.Registration*: home/roaming/denied/failed/unknown
	Enabled      bool   // modem intentionally enabled (false => powered off by command)
	HardFailed   bool   // health terminal: modem broken / needs replacement
}

// Classifier debounces transitions so brief coverage flickers don't cause
// downstream consumers to thrash.
type Classifier struct {
	committed    State
	pending      State
	pendingSince time.Time
	now          func() time.Time
}

// New returns a Classifier that reads wall-clock time from time.Now.
func New() *Classifier {
	return &Classifier{now: time.Now}
}

// newWithClock is a test helper.
func newWithClock(clock func() time.Time) *Classifier {
	return &Classifier{now: clock}
}

// Classify updates the classifier with the latest raw inputs and returns the
// currently committed connectivity after applying hysteresis.
func (c *Classifier) Classify(in Inputs) State {
	raw := rawState(in)
	now := c.now()

	// Startup must not leave consumers at unknown for a full debounce window.
	if c.committed == Unknown {
		c.committed = raw
		c.pending = raw
		c.pendingSince = now
		return c.committed
	}

	if raw != c.pending {
		c.pending = raw
		c.pendingSince = now
	}

	if raw == c.committed {
		return c.committed
	}

	// commitDelay of 0 (deterministic states) commits on first sight, since
	// pendingSince == now makes elapsed == 0 >= 0.
	if now.Sub(c.pendingSince) >= commitDelay(c.committed, raw) {
		c.committed = raw
	}
	return c.committed
}

// Committed returns the last committed state without updating it.
func (c *Classifier) Committed() State {
	return c.committed
}

// IsConnected reports whether the committed state is Connected. Used by the GPS
// subsystem, which only cares about having a data path.
func (c *Classifier) IsConnected() bool {
	return c.committed == Connected
}

// Force overrides the committed state. The service calls this when it powers the
// modem off out-of-band (disable command): the monitor loop won't run while the
// modem is disabled, so without this the classifier would keep its pre-disable
// value and mis-report on resume.
func (c *Classifier) Force(s State) {
	c.committed = s
	c.pending = s
	c.pendingSince = c.now()
}

// rawState maps inputs before hysteresis; decisive states take precedence.
func rawState(in Inputs) State {
	if !in.Enabled {
		return Disabled
	}
	if in.HardFailed {
		return Failed
	}
	if in.SIMState == SIMMissing || in.SIMState == SIMInactive {
		return NoSIM
	}
	if in.Registration == RegistrationDenied || in.Registration == RegistrationFailed {
		return Denied
	}
	if in.ModemStatus == StatusConnected {
		return Connected
	}
	return Disconnected
}

// commitDelay returns how long the candidate state `to` must persist before it
// replaces `from`. Deterministic states commit immediately; we debounce only
// the transitions that would otherwise flap the icon away or thrash GPS mode.
func commitDelay(from, to State) time.Duration {
	switch to {
	case Disabled, NoSIM, Failed:
		// Stable, deterministic inputs (command / SIM presence / health machine).
		return 0
	case Denied:
		// Registration can dip during handovers; ride it out before hiding.
		return DeniedDebounce
	case Connected:
		return OnlineDebounce
	case Disconnected:
		if from == Connected {
			// Absorb tunnel/garage coverage gaps; don't tear down GPS mode.
			return OfflineDebounce
		}
		// Coming from a hidden/unknown state: show the icon promptly.
		return 0
	}
	return 0
}

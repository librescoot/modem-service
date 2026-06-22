// Package connectivity classifies the modem's high-level state into a
// connectivity value with hysteresis. Kept as a separate package so it has no
// hardware dependencies and can be unit-tested on any platform.
//
// The committed value answers one question for downstream consumers: is this
// scooter provisioned for connectivity, and if not, why? The dashboard uses it
// to decide whether the internet icon is worth showing at all; the GPS
// subsystem uses it (via IsConnected) to pick a positioning mode.
package connectivity

import "time"

// State is the committed connectivity state.
//
// Values split into three groups by what the dashboard does with them:
//   - Connected / Disconnected: provisioned and in use (or trying) -> show.
//   - Failed: modem broken/absent -> show (it's an actionable fault).
//   - Disabled / NoSIM / Denied: offline by design or unprovisioned -> hide.
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

// Debounce thresholds. We stay quick to SHOW the icon and slow to flip it into
// a teardown/hidden state: connecting is low-cost, but flapping the icon away
// (or tearing down GPS mode) on a brief coverage gap is not.
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

	// First observation after startup: commit immediately so downstream
	// consumers aren't stuck at unknown.
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

// rawState maps a raw snapshot to a connectivity state, before hysteresis.
// Order matters: the most decisive / least-flickery reasons win first.
func rawState(in Inputs) State {
	// Intentional disable wins: the rider (via pm-service) turned the modem off.
	if !in.Enabled {
		return Disabled
	}
	// Broken hardware: the health machine exhausted recovery.
	if in.HardFailed {
		return Failed
	}
	// SIM physically absent or unusable. Keyed on the modem actually reporting
	// missing/inactive — not on the modem being off or unreachable.
	if in.SIMState == SIMMissing || in.SIMState == SIMInactive {
		return NoSIM
	}
	// SIM present but the network refuses it (e.g. carrier-deactivated SIM).
	if in.Registration == RegistrationDenied || in.Registration == RegistrationFailed {
		return Denied
	}
	if in.ModemStatus == StatusConnected {
		return Connected
	}
	// Everything else (disconnected / off / no-modem / unknown) is a provisioned
	// modem that simply isn't connected right now.
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

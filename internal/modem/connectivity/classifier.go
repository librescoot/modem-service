// Package connectivity classifies the modem's high-level state into a
// coarse connectivity value with hysteresis. Kept as a separate package so
// it has no hardware dependencies and can be unit-tested on any platform.
package connectivity

import "time"

// State is the committed connectivity state.
type State string

const (
	Unknown State = ""
	Online  State = "online"
	Offline State = "offline"
	NoSIM   State = "no-sim"
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
	SIMMissing = "missing"
)

// Debounce thresholds. Online commits fast because going online is low-cost;
// offline commits slower to ride out tunnel/garage coverage gaps without
// tearing down GPS mode repeatedly.
const (
	OnlineDebounce  = 60 * time.Second
	OfflineDebounce = 3 * time.Minute
)

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

// Classify updates the classifier with the latest raw inputs and returns
// the currently committed connectivity after applying hysteresis.
//
// modemStatus is the already-derived high-level modem state string from
// modem.State.Status ("connected", "disconnected", "off", "no-modem").
// simState is the modem.SIMState value, used only to distinguish "no-sim"
// from plain offline.
func (c *Classifier) Classify(modemStatus, simState string) State {
	raw := rawState(modemStatus, simState)
	now := c.now()

	// no-sim commits immediately — SIM presence doesn't flicker
	if raw == NoSIM {
		c.committed = raw
		c.pending = raw
		c.pendingSince = now
		return c.committed
	}

	if raw != c.pending {
		c.pending = raw
		c.pendingSince = now
	}

	// First observation after startup: commit immediately so downstream
	// consumers aren't stuck at unknown.
	if c.committed == Unknown {
		c.committed = raw
		return c.committed
	}

	if raw == c.committed {
		return c.committed
	}

	elapsed := now.Sub(c.pendingSince)

	switch raw {
	case Online:
		if elapsed >= OnlineDebounce {
			c.committed = Online
		}
	case Offline:
		if elapsed >= OfflineDebounce {
			c.committed = Offline
		}
	}

	return c.committed
}

// Committed returns the last committed state without updating it.
func (c *Classifier) Committed() State {
	return c.committed
}

func rawState(modemStatus, simState string) State {
	// no-sim wins only when the modem tells us SIM is missing — not just
	// because the modem is off or unreachable.
	if simState == SIMMissing {
		return NoSIM
	}
	if modemStatus == StatusConnected {
		return Online
	}
	// Everything else (disconnected / off / no-modem / unknown) is offline.
	// Hysteresis absorbs short "disconnected" blips during tunnel crossings
	// and handovers.
	return Offline
}

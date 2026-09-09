// Package link assesses the modem's local connectivity layers and picks a
// proportionate remedy. It has no hardware or D-Bus dependencies so it can be
// unit-tested on any platform, following internal/modem/connectivity.
//
// The invariant this package exists to enforce: only local signals may trigger
// a modem action. Whether some destination is reachable is a separate question
// with its own answer, and it is never grounds for touching the hardware. A
// fleet on a restricted APN is unreachable by design and permanently healthy.
package link

import "fmt"

// Layer identifies one rung of the connectivity stack, ordered from the
// hardware upward. Assess reports the lowest failing rung.
type Layer int

const (
	LayerHardware Layer = iota
	LayerSIM
	LayerRegistration
	LayerPacketService
	LayerAPN
	LayerBearer
	LayerNetdev
	LayerLiveness
)

func (l Layer) String() string {
	switch l {
	case LayerHardware:
		return "hardware"
	case LayerSIM:
		return "sim"
	case LayerRegistration:
		return "registration"
	case LayerPacketService:
		return "packet-service"
	case LayerAPN:
		return "apn"
	case LayerBearer:
		return "bearer"
	case LayerNetdev:
		return "netdev"
	case LayerLiveness:
		return "liveness"
	}
	return "unknown"
}

// Remedy is the cheapest action that could plausibly fix the failing layer.
type Remedy int

const (
	RemedyNone         Remedy = iota
	RemedyReattach            // LTE reattach, COPS=2/0
	RemedyBearerBounce        // NetworkManager connection down/up
	RemedyModemReset          // the full recovery ladder
)

func (r Remedy) String() string {
	switch r {
	case RemedyNone:
		return "none"
	case RemedyReattach:
		return "reattach"
	case RemedyBearerBounce:
		return "bearer-bounce"
	case RemedyModemReset:
		return "modem-reset"
	}
	return "unknown"
}

// A session that comes up and dies again inside stableSessionSeconds was not
// really up. Counting those, rather than counting bytes, is what makes layer 7
// destination-independent: no amount of silence from the far end can fake a
// bearer that keeps tearing itself down.
const stableSessionSeconds = 120

// flapsBeforeFailing is how many short-lived sessions in a row constitute a
// flapping data session rather than one unlucky reconnect.
const flapsBeforeFailing = 3

// livenessLadder is walked in cost order as liveness keeps failing.
//
// RemedyModemReset is deliberately absent. The design called for a soak on
// affected hardware before layer 7 is allowed to power-cycle a modem, and that
// soak has not happened: the only on-device run was six minutes on an
// unrestricted SIM with no remedy fired. A genuinely dead modem still reaches
// the reset ladder through layer 0 via the health check, which is where it
// shows up anyway. Add the rung here once there is evidence it is needed.
var livenessLadder = []Remedy{RemedyBearerBounce, RemedyReattach}

// Snapshot is one observation of layers 0 through 7. Empty string fields mean
// "not read": Assess treats unknown as passing, so a flaky D-Bus read can never
// manufacture a remedy.
type Snapshot struct {
	ModemPresent     bool
	PrimaryPortKnown bool
	PrimaryPortOK    bool
	PowerState       string

	SIMLock string // "" when unlocked; sim-pin2 does not count as locked

	Registration string

	PacketService string // attached / detached / ""

	BearerKnown          bool
	BearerConnected      bool
	BearerConnectedKnown bool
	BearerSuspended      bool
	BearerSuspendedKnown bool
	BearerInterface      string
	BearerIPKnown        bool
	BearerIP             string
	BearerStatsKnown     bool
	BearerAttempts       uint32
	BearerDuration       uint64

	// AT cross-check, populated only when the previous Assess asked for it.
	ATChecked   bool
	CGACTActive bool
	CGPADDR     string

	CarrierKnown      bool
	Carrier           bool
	NetdevIP          string
	DefaultRouteKnown bool
	HasDefaultRoute   bool

	// Byte counters are deliberately absent. Layer 7 used to compare tx
	// against rx, which cannot distinguish a wedged link from a network that
	// drops traffic by design, and which the connectivity probe's own traffic
	// feeds directly. See sessionFlapped.
}

// Assessment is the verdict for one snapshot.
type Assessment struct {
	Healthy     bool
	FailedLayer Layer
	Reason      string
	Remedy      Remedy
	WantATCheck bool
}

// Assessor holds the state the layer rules need across ticks.
type Assessor struct {
	prev       Snapshot
	havePrev   bool
	flaps      int // consecutive short-lived data sessions
	escalation int // index into the liveness remedy ladder
}

// New returns a ready Assessor.
func New() *Assessor { return &Assessor{} }

// Assess folds the next snapshot into a verdict. On the first call there is no
// previous snapshot, so liveness is not evaluated.
func (a *Assessor) Assess(cur Snapshot) Assessment {
	prev, havePrev := a.prev, a.havePrev
	a.prev, a.havePrev = cur, true

	if fail, ok := checkLocal(cur); ok {
		fail.WantATCheck = suspicious(cur)
		return fail
	}

	if havePrev {
		if sessionFlapped(prev, cur) {
			a.flaps++
		} else if cur.BearerStatsKnown && cur.BearerDuration >= stableSessionSeconds {
			// The session has been up long enough to count as stable, so past
			// flaps are history and the ladder starts over. This must be
			// evaluated before the threshold below, or a recovered session
			// would keep reporting the failure it already grew out of.
			a.flaps = 0
			a.escalation = 0
		}
	}

	if a.flaps >= flapsBeforeFailing {
		// The count is deliberately not cleared here. The caller debounces by
		// requiring the same failing layer on consecutive assessments, so
		// reporting once and then falling back to healthy would reset that
		// counter every time and no liveness remedy could ever be confirmed.
		return Assessment{
			FailedLayer: LayerLiveness,
			Reason: fmt.Sprintf("data session flapping: %d short-lived sessions, latest lasted %ds",
				flapsBeforeFailing, cur.BearerDuration),
			Remedy:      livenessLadder[min(a.escalation, len(livenessLadder)-1)],
			WantATCheck: true,
		}
	}

	return Assessment{Healthy: true, WantATCheck: suspicious(cur)}
}

// NoteRemedyApplied advances the liveness ladder. The caller invokes it only
// when a remedy was actually carried out.
//
// Promotion deliberately does not happen inside Assess. The caller can decline
// to act, most often because that remedy is still within its cooldown, and if
// the ladder advanced on merely reporting a remedy then a suppressed bounce
// would buy promotion to a costlier action that no evidence justifies. The
// point of a cheapest-first ladder is that each rung was tried and did not
// work, which is only true if the rung actually ran.
func (a *Assessor) NoteRemedyApplied(layer Layer, r Remedy) {
	// Only liveness advances the liveness ladder. A bearer bounce applied for
	// a layer 5 fault says nothing about whether a flapping session has
	// already resisted the cheapest remedy, and counting it would start the
	// next liveness episode part-way up the ladder.
	if r == RemedyNone || layer != LayerLiveness {
		return
	}
	a.escalation++
}

// checkLocal walks layers 0 through 6 and returns the first failure.
func checkLocal(s Snapshot) (Assessment, bool) {
	fail := func(l Layer, r Remedy, format string, args ...any) (Assessment, bool) {
		return Assessment{FailedLayer: l, Remedy: r, Reason: fmt.Sprintf(format, args...)}, true
	}

	if !s.ModemPresent {
		return fail(LayerHardware, RemedyModemReset, "modem not present on D-Bus")
	}
	if s.PrimaryPortKnown && !s.PrimaryPortOK {
		return fail(LayerHardware, RemedyModemReset, "primary port unavailable")
	}
	if s.PowerState != "" && s.PowerState != "on" {
		return fail(LayerHardware, RemedyModemReset, "power state %q", s.PowerState)
	}

	// PIN2 gates fixed dialling, not attach or data, so it is not a lock.
	if s.SIMLock != "" && s.SIMLock != "sim-pin2" && s.SIMLock != "sim-puk2" {
		return fail(LayerSIM, RemedyNone, "SIM locked (%s)", s.SIMLock)
	}

	// Roaming is as healthy as home: the restricted fleet registers roaming.
	if s.Registration != "" && s.Registration != "home" && s.Registration != "roaming" {
		return fail(LayerRegistration, RemedyNone, "registration %q", s.Registration)
	}

	if s.PacketService == "detached" {
		return fail(LayerPacketService, RemedyReattach, "packet service detached")
	}

	if s.BearerKnown && s.BearerConnectedKnown && !s.BearerConnected {
		return fail(LayerBearer, RemedyBearerBounce, "bearer not connected")
	}
	if s.BearerKnown && s.BearerSuspendedKnown && s.BearerSuspended {
		return fail(LayerBearer, RemedyBearerBounce, "bearer suspended")
	}
	if s.BearerKnown && s.BearerConnected && s.BearerIPKnown && s.BearerIP == "" {
		return fail(LayerBearer, RemedyBearerBounce, "bearer has no address")
	}
	if s.ATChecked {
		if !s.CGACTActive {
			return fail(LayerBearer, RemedyBearerBounce,
				"modem reports PDP context inactive while the bearer claims connected")
		}
		if s.CGPADDR != "" && s.CGPADDR != s.BearerIP {
			return fail(LayerBearer, RemedyBearerBounce,
				"modem address %s disagrees with bearer address %s", s.CGPADDR, s.BearerIP)
		}
	}

	if s.CarrierKnown && !s.Carrier {
		return fail(LayerNetdev, RemedyBearerBounce, "no carrier on %s", s.BearerInterface)
	}
	if s.DefaultRouteKnown && !s.HasDefaultRoute {
		return fail(LayerNetdev, RemedyBearerBounce, "no default route via %s", s.BearerInterface)
	}
	if s.BearerIPKnown && s.NetdevIP != "" && s.NetdevIP != s.BearerIP {
		return fail(LayerNetdev, RemedyBearerBounce,
			"interface address %s disagrees with bearer address %s", s.NetdevIP, s.BearerIP)
	}

	return Assessment{}, false
}

// sessionFlapped reports whether the data session was torn down and brought
// back inside this window, having lived only briefly.
//
// This is what layer 7 measures instead of byte counters. "Did my traffic get
// answered" is inherently destination-dependent: on a network that drops
// everything by design, the honest answer is indistinguishable from a broken
// modem, and acting on it is the exact bug this package exists to remove.
// A bearer that keeps collapsing and re-establishing, by contrast, is a local
// fact that no amount of silence from the far end can fabricate.
//
// ModemManager increments Bearer.Stats attempts on each reconnect and resets
// duration with it, so a rising attempts count paired with a short duration
// means the session did not survive.
func sessionFlapped(prev, cur Snapshot) bool {
	if !prev.BearerStatsKnown || !cur.BearerStatsKnown {
		return false
	}
	if cur.BearerAttempts <= prev.BearerAttempts {
		return false // no reconnect happened in this window
	}
	// A reconnect that produced a session already past the stability mark was
	// a clean handover, not a flap.
	return cur.BearerDuration < stableSessionSeconds
}

// suspicious reports whether a cheap signal warrants spending an AT round trip
// next tick. The AT ports are shared with GPS and ModemManager serialises
// access, so the cross-check is not run in steady state.
func suspicious(s Snapshot) bool {
	if s.BearerKnown && ((s.BearerConnectedKnown && !s.BearerConnected) || (s.BearerSuspendedKnown && s.BearerSuspended)) {
		return true
	}
	if s.PacketService == "detached" {
		return true
	}
	if s.NetdevIP != "" && s.BearerIP != "" && s.NetdevIP != s.BearerIP {
		return true
	}
	return false
}

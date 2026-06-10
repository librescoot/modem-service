// Package sim reconciles SIM PIN state with the configured cellular.sim-pin
// setting. It owns the decision of whether to send the PIN (when the SIM is
// PIN-locked) or enable PIN-lock (when it's currently disabled). Wrong-PIN
// attempts are gated by the modem's UnlockRetries counter so the service
// can never push a SIM into PUK state on its own — see the design doc at
// docs/superpowers/specs/2026-05-05-sim-pin-support-design.md.
package sim

import (
	"log"
	"sync"

	"github.com/godbus/dbus/v5"

	"modem-service/internal/mm"
)

// Outcome describes the result of one Reconcile call. It's published verbatim
// to the modem.pin-action Redis field.
type Outcome string

const (
	OutcomeUnconfigured   Outcome = "unconfigured"
	OutcomeOK             Outcome = "ok"
	OutcomeUnlocked       Outcome = "unlocked"
	OutcomeLockEnabled    Outcome = "lock-enabled"
	OutcomeWrongPin       Outcome = "wrong-pin"
	OutcomeLowRetriesBail Outcome = "low-retries-bail"
	OutcomePukRequired    Outcome = "puk-required"
	OutcomeError          Outcome = "error"
)

// minRetriesBeforeAttempt is the SIM-PIN retry count required for the manager
// to attempt a SendPin or EnablePin. Three is the standard maximum on a fresh
// SIM, so requiring "== max" means we only ever consume one retry: if our
// guess is wrong, we stop and let a human recover by sending the correct PIN
// manually (which restores the counter to 3).
const minRetriesBeforeAttempt = 3

// SimDBus is the narrow D-Bus surface the manager needs. The mm.Client
// satisfies this interface; tests substitute a recorder.
type SimDBus interface {
	SendPin(simPath dbus.ObjectPath, pin string) error
	EnablePin(simPath dbus.ObjectPath, pin string, enabled bool) error
}

// Input is the per-cycle snapshot passed to Reconcile. All fields come from
// modem.State and the cached settings hash; Reconcile does no I/O of its own
// beyond the SimDBus calls.
type Input struct {
	SIMPath           dbus.ObjectPath
	LockStatus        string // "" if unlocked, otherwise mm.LockReasonToString
	SIMPinLockEnabled bool
	UnlockRetriesPin  uint32
	ConfiguredPIN     string
}

// Manager keeps a single boolean of "did we already try and fail this run?"
// across cycles. State is process-local; a restart resets it but the
// retry-count gate still protects the SIM.
type Manager struct {
	dbus   SimDBus
	logger *log.Logger

	mu           sync.Mutex
	triedThisRun bool
}

// New returns a Manager bound to the given D-Bus surface and logger.
func New(d SimDBus, logger *log.Logger) *Manager {
	if logger == nil {
		logger = log.Default()
	}
	return &Manager{dbus: d, logger: logger}
}

// Reconcile runs the decision matrix described in the design doc and returns
// the outcome for publication. It may make at most one D-Bus call per
// invocation.
func (m *Manager) Reconcile(in Input) Outcome {
	// Diagnostic — log what the service sees each tick so we can confirm
	// settings-service delivered a PIN. The value is sensitive and never
	// logged; only whether one is configured.
	m.logger.Printf("sim-reconcile: lock=%q pin-configured=%v enabled=%v retries=%d",
		in.LockStatus, in.ConfiguredPIN != "", in.SIMPinLockEnabled, in.UnlockRetriesPin)

	if in.ConfiguredPIN == "" {
		return OutcomeUnconfigured
	}

	switch in.LockStatus {
	case "sim-puk", "sim-puk2":
		return OutcomePukRequired
	case "sim-pin2":
		// PIN2 doesn't block data. Whatever sim-pin's enabled state is, we
		// don't touch it via PIN2 path.
		return OutcomeOK
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	switch in.LockStatus {
	case "sim-pin":
		return m.actUnlock(in)
	case "":
		// Unlocked. Enable lock if it isn't already.
		if in.SIMPinLockEnabled {
			return OutcomeOK
		}
		return m.actEnableLock(in)
	default:
		// Other lock reasons (ph-sim-pin etc.) — out of scope.
		m.logger.Printf("sim: unhandled lock status %q, no action", in.LockStatus)
		return OutcomeOK
	}
}

func (m *Manager) actUnlock(in Input) Outcome {
	if in.SIMPath == "" {
		m.logger.Printf("sim: cannot unlock — SIM D-Bus path is empty")
		return OutcomeError
	}
	if m.triedThisRun {
		return OutcomeWrongPin
	}
	if in.UnlockRetriesPin < minRetriesBeforeAttempt {
		m.logger.Printf("sim: refusing to send PIN (retries=%d < %d) to avoid PUK lock",
			in.UnlockRetriesPin, minRetriesBeforeAttempt)
		return OutcomeLowRetriesBail
	}

	err := m.dbus.SendPin(in.SIMPath, in.ConfiguredPIN)
	if err == nil {
		m.logger.Printf("sim: SendPin succeeded, SIM unlocked")
		return OutcomeUnlocked
	}
	if mm.IsPukRequiredError(err) {
		m.logger.Printf("sim: SendPin returned PUK required: %v", err)
		return OutcomePukRequired
	}
	if mm.IsWrongPinError(err) {
		m.triedThisRun = true
		m.logger.Printf("sim: SendPin rejected (wrong PIN); will not retry until restart and retries are restored to %d",
			minRetriesBeforeAttempt)
		return OutcomeWrongPin
	}
	m.logger.Printf("sim: SendPin failed: %v", err)
	return OutcomeError
}

func (m *Manager) actEnableLock(in Input) Outcome {
	if in.SIMPath == "" {
		m.logger.Printf("sim: cannot enable PIN lock — SIM D-Bus path is empty")
		return OutcomeError
	}
	if m.triedThisRun {
		return OutcomeWrongPin
	}
	if in.UnlockRetriesPin < minRetriesBeforeAttempt {
		m.logger.Printf("sim: refusing to enable PIN lock (retries=%d < %d) to avoid PUK lock",
			in.UnlockRetriesPin, minRetriesBeforeAttempt)
		return OutcomeLowRetriesBail
	}

	err := m.dbus.EnablePin(in.SIMPath, in.ConfiguredPIN, true)
	if err == nil {
		m.logger.Printf("sim: EnablePin(true) succeeded, PIN lock now enabled")
		return OutcomeLockEnabled
	}
	if mm.IsPukRequiredError(err) {
		m.logger.Printf("sim: EnablePin returned PUK required: %v", err)
		return OutcomePukRequired
	}
	if mm.IsWrongPinError(err) {
		m.triedThisRun = true
		m.logger.Printf("sim: EnablePin rejected (wrong PIN); will not retry until restart and retries are restored to %d",
			minRetriesBeforeAttempt)
		return OutcomeWrongPin
	}
	m.logger.Printf("sim: EnablePin failed: %v", err)
	return OutcomeError
}

// Package sim reconciles PIN state without risking an automatic transition
// to PUK lock.
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

// Require at least the standard fresh retry count, so one failure blocks retries.
const minRetriesBeforeAttempt = 3

// SimDBus is the narrow D-Bus surface the manager needs. The mm.Client
// satisfies this interface; tests substitute a recorder.
type SimDBus interface {
	SendPin(simPath dbus.ObjectPath, pin string) error
	EnablePin(simPath dbus.ObjectPath, pin string, enabled bool) error
}

// Input is the per-cycle SIM and settings snapshot.
type Input struct {
	SIMPath           dbus.ObjectPath
	LockStatus        string // "" if unlocked, otherwise mm.LockReasonToString
	LockStatusKnown   bool
	SIMPinLockEnabled bool
	SIMPinLockKnown   bool
	UnlockRetriesPin  uint32
	ConfiguredPIN     string
}

// Manager remembers failed attempts for the process lifetime; the hardware
// retry count remains authoritative across restarts.
type Manager struct {
	dbus   SimDBus
	logger *log.Logger

	mu           sync.Mutex
	triedThisRun bool

	lastObservation    reconcileObservation
	hasLastObservation bool
}

type reconcileObservation struct {
	lockStatus       string
	lockStatusKnown  bool
	pinConfigured    bool
	pinLockEnabled   bool
	pinLockKnown     bool
	unlockRetriesPin uint32
}

// New returns a Manager bound to the given D-Bus surface and logger.
func New(d SimDBus, logger *log.Logger) *Manager {
	if logger == nil {
		logger = log.Default()
	}
	return &Manager{dbus: d, logger: logger}
}

// Reconcile performs at most one D-Bus action and returns its outcome.
func (m *Manager) Reconcile(in Input) Outcome {
	observationChanged := m.logObservation(in)

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
		if !in.LockStatusKnown || !in.SIMPinLockKnown {
			if observationChanged {
				m.logger.Printf("sim: lock state unavailable, no action")
			}
			return OutcomeError
		}
		if in.SIMPinLockEnabled {
			return OutcomeOK
		}
		return m.actEnableLock(in)
	default:
		// Other lock reasons (ph-sim-pin etc.) — out of scope. Log this only
		// when the observed SIM state changes; the diagnostic contains the
		// same status and should not fill the journal every monitor tick.
		if observationChanged {
			m.logger.Printf("sim: unhandled lock status %q, no action", in.LockStatus)
		}
		return OutcomeOK
	}
}

// logObservation emits the per-cycle diagnostic only when its observable
// state changes. The configured PIN is deliberately represented only by its
// presence, never by its value.
func (m *Manager) logObservation(in Input) bool {
	observation := reconcileObservation{
		lockStatus:       in.LockStatus,
		lockStatusKnown:  in.LockStatusKnown,
		pinConfigured:    in.ConfiguredPIN != "",
		pinLockEnabled:   in.SIMPinLockEnabled,
		pinLockKnown:     in.SIMPinLockKnown,
		unlockRetriesPin: in.UnlockRetriesPin,
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hasLastObservation && m.lastObservation == observation {
		return false
	}
	m.lastObservation = observation
	m.hasLastObservation = true
	m.logger.Printf("sim-reconcile: lock=%q pin-configured=%v enabled=%v retries=%d",
		in.LockStatus, observation.pinConfigured, in.SIMPinLockEnabled, in.UnlockRetriesPin)
	return true
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

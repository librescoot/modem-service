package sim

import (
	"errors"
	"io"
	"log"
	"testing"

	"github.com/godbus/dbus/v5"

	"modem-service/internal/mm"
)

const testSimPath dbus.ObjectPath = "/org/freedesktop/ModemManager1/SIM/0"

// fakeDBus records calls and returns canned errors.
type fakeDBus struct {
	sendPinErr   error
	enablePinErr error

	sendPinCalls   int
	enablePinCalls int
	lastEnabled    bool
}

func (f *fakeDBus) SendPin(_ dbus.ObjectPath, _ string) error {
	f.sendPinCalls++
	return f.sendPinErr
}

func (f *fakeDBus) EnablePin(_ dbus.ObjectPath, _ string, enabled bool) error {
	f.enablePinCalls++
	f.lastEnabled = enabled
	return f.enablePinErr
}

func newManager(d *fakeDBus) *Manager {
	return &Manager{
		dbus:   d,
		logger: log.New(io.Discard, "", 0),
	}
}

func wrongPinErr() error {
	return dbus.Error{Name: mm.ErrIncorrectPassword}
}

func pukErr() error {
	return dbus.Error{Name: mm.ErrSimPuk}
}

// --- Configured PIN empty ----------------------------------------------------

func TestReconcile_NoPinConfigured(t *testing.T) {
	d := &fakeDBus{}
	m := newManager(d)

	out := m.Reconcile(Input{
		SIMPath:          testSimPath,
		LockStatus:       "sim-pin",
		UnlockRetriesPin: 3,
		ConfiguredPIN:    "",
	})

	if out != OutcomeUnconfigured {
		t.Fatalf("got %q, want %q", out, OutcomeUnconfigured)
	}
	if d.sendPinCalls != 0 || d.enablePinCalls != 0 {
		t.Fatalf("must not call D-Bus when PIN is unconfigured: send=%d enable=%d",
			d.sendPinCalls, d.enablePinCalls)
	}
}

// --- Locked → SendPin --------------------------------------------------------

func TestReconcile_LockedSendsPinWhenRetriesFull(t *testing.T) {
	d := &fakeDBus{}
	m := newManager(d)

	out := m.Reconcile(Input{
		SIMPath:          testSimPath,
		LockStatus:       "sim-pin",
		UnlockRetriesPin: 3,
		ConfiguredPIN:    "0000",
	})

	if out != OutcomeUnlocked {
		t.Fatalf("got %q, want %q", out, OutcomeUnlocked)
	}
	if d.sendPinCalls != 1 {
		t.Fatalf("expected 1 SendPin call, got %d", d.sendPinCalls)
	}
}

func TestReconcile_LockedWrongPinTriedOnce(t *testing.T) {
	d := &fakeDBus{sendPinErr: wrongPinErr()}
	m := newManager(d)

	in := Input{
		SIMPath:          testSimPath,
		LockStatus:       "sim-pin",
		UnlockRetriesPin: 3,
		ConfiguredPIN:    "0000",
	}

	out := m.Reconcile(in)
	if out != OutcomeWrongPin {
		t.Fatalf("first call: got %q, want %q", out, OutcomeWrongPin)
	}
	if d.sendPinCalls != 1 {
		t.Fatalf("first call: expected 1 SendPin, got %d", d.sendPinCalls)
	}

	// Second cycle: triedThisRun must short-circuit, no further D-Bus call.
	in.UnlockRetriesPin = 2
	out = m.Reconcile(in)
	if out != OutcomeWrongPin {
		t.Fatalf("second call: got %q, want %q", out, OutcomeWrongPin)
	}
	if d.sendPinCalls != 1 {
		t.Fatalf("second call: expected still 1 SendPin, got %d", d.sendPinCalls)
	}
}

func TestReconcile_LockedRefusesWhenRetriesLow(t *testing.T) {
	d := &fakeDBus{}
	m := newManager(d)

	out := m.Reconcile(Input{
		SIMPath:          testSimPath,
		LockStatus:       "sim-pin",
		UnlockRetriesPin: 2,
		ConfiguredPIN:    "0000",
	})

	if out != OutcomeLowRetriesBail {
		t.Fatalf("got %q, want %q", out, OutcomeLowRetriesBail)
	}
	if d.sendPinCalls != 0 {
		t.Fatalf("must not call SendPin when retries < 3: %d", d.sendPinCalls)
	}
}

func TestReconcile_LockedRefusesWhenRetriesUnknown(t *testing.T) {
	d := &fakeDBus{}
	m := newManager(d)

	out := m.Reconcile(Input{
		SIMPath:          testSimPath,
		LockStatus:       "sim-pin",
		UnlockRetriesPin: 0,
		ConfiguredPIN:    "0000",
	})

	if out != OutcomeLowRetriesBail {
		t.Fatalf("got %q, want %q", out, OutcomeLowRetriesBail)
	}
	if d.sendPinCalls != 0 {
		t.Fatalf("must not call SendPin when retries unknown: %d", d.sendPinCalls)
	}
}

func TestReconcile_LockedDBusErrorReturnsError(t *testing.T) {
	d := &fakeDBus{sendPinErr: errors.New("bus blew up")}
	m := newManager(d)

	out := m.Reconcile(Input{
		SIMPath:          testSimPath,
		LockStatus:       "sim-pin",
		UnlockRetriesPin: 3,
		ConfiguredPIN:    "0000",
	})

	if out != OutcomeError {
		t.Fatalf("got %q, want %q", out, OutcomeError)
	}
	// triedThisRun must NOT be set on a non-bad-pin error: that error
	// didn't necessarily consume a retry, and we want a chance on the
	// next cycle if the bus recovers.
	out = m.Reconcile(Input{
		SIMPath:          testSimPath,
		LockStatus:       "sim-pin",
		UnlockRetriesPin: 3,
		ConfiguredPIN:    "0000",
	})
	if d.sendPinCalls != 2 {
		t.Fatalf("non-IncorrectPassword error must not flip triedThisRun, got %d calls", d.sendPinCalls)
	}
	_ = out
}

// --- PIN2 / PUK / unknown locks ---------------------------------------------

func TestReconcile_LockedPin2IsHarmless(t *testing.T) {
	d := &fakeDBus{}
	m := newManager(d)

	out := m.Reconcile(Input{
		SIMPath:           testSimPath,
		LockStatus:        "sim-pin2",
		SIMPinLockEnabled: true,
		UnlockRetriesPin:  3,
		ConfiguredPIN:     "0000",
	})

	if out != OutcomeOK {
		t.Fatalf("pin2 with sim-pin enabled: got %q, want %q", out, OutcomeOK)
	}
	if d.sendPinCalls != 0 || d.enablePinCalls != 0 {
		t.Fatalf("pin2 must not trigger any D-Bus call")
	}
}

func TestReconcile_LockedPukRequired(t *testing.T) {
	d := &fakeDBus{}
	m := newManager(d)

	for _, lock := range []string{"sim-puk", "sim-puk2"} {
		out := m.Reconcile(Input{
			SIMPath:          testSimPath,
			LockStatus:       lock,
			UnlockRetriesPin: 3,
			ConfiguredPIN:    "0000",
		})
		if out != OutcomePukRequired {
			t.Fatalf("%s: got %q, want %q", lock, out, OutcomePukRequired)
		}
	}
	if d.sendPinCalls != 0 || d.enablePinCalls != 0 {
		t.Fatalf("PUK lock must not trigger any D-Bus call")
	}
}

// --- Unlocked: enable lock ---------------------------------------------------

func TestReconcile_UnlockedEnablesLockWhenDisabled(t *testing.T) {
	d := &fakeDBus{}
	m := newManager(d)

	out := m.Reconcile(Input{
		SIMPath:           testSimPath,
		LockStatus:        "",
		SIMPinLockEnabled: false,
		UnlockRetriesPin:  3,
		ConfiguredPIN:     "0000",
	})

	if out != OutcomeLockEnabled {
		t.Fatalf("got %q, want %q", out, OutcomeLockEnabled)
	}
	if d.enablePinCalls != 1 || d.lastEnabled != true {
		t.Fatalf("expected 1 EnablePin(true), got calls=%d enabled=%v",
			d.enablePinCalls, d.lastEnabled)
	}
}

func TestReconcile_UnlockedNoOpWhenLockAlreadyEnabled(t *testing.T) {
	d := &fakeDBus{}
	m := newManager(d)

	out := m.Reconcile(Input{
		SIMPath:           testSimPath,
		LockStatus:        "",
		SIMPinLockEnabled: true,
		UnlockRetriesPin:  3,
		ConfiguredPIN:     "0000",
	})

	if out != OutcomeOK {
		t.Fatalf("got %q, want %q", out, OutcomeOK)
	}
	if d.sendPinCalls != 0 || d.enablePinCalls != 0 {
		t.Fatalf("must not touch D-Bus when already in desired state")
	}
}

func TestReconcile_UnlockedWrongPinOnEnableTriedOnce(t *testing.T) {
	d := &fakeDBus{enablePinErr: wrongPinErr()}
	m := newManager(d)

	in := Input{
		SIMPath:           testSimPath,
		LockStatus:        "",
		SIMPinLockEnabled: false,
		UnlockRetriesPin:  3,
		ConfiguredPIN:     "0000",
	}

	out := m.Reconcile(in)
	if out != OutcomeWrongPin {
		t.Fatalf("first: got %q, want %q", out, OutcomeWrongPin)
	}

	// Second cycle: same input, must not retry.
	in.UnlockRetriesPin = 2
	out = m.Reconcile(in)
	if out != OutcomeWrongPin {
		t.Fatalf("second: got %q, want %q", out, OutcomeWrongPin)
	}
	if d.enablePinCalls != 1 {
		t.Fatalf("expected 1 EnablePin call across two cycles, got %d", d.enablePinCalls)
	}
}

func TestReconcile_UnlockedRefusesEnableWhenRetriesLow(t *testing.T) {
	d := &fakeDBus{}
	m := newManager(d)

	out := m.Reconcile(Input{
		SIMPath:           testSimPath,
		LockStatus:        "",
		SIMPinLockEnabled: false,
		UnlockRetriesPin:  2,
		ConfiguredPIN:     "0000",
	})

	if out != OutcomeLowRetriesBail {
		t.Fatalf("got %q, want %q", out, OutcomeLowRetriesBail)
	}
	if d.enablePinCalls != 0 {
		t.Fatalf("must not call EnablePin when retries < 3")
	}
}

// --- Cross-state safety -----------------------------------------------------

func TestReconcile_NoSimPath(t *testing.T) {
	d := &fakeDBus{}
	m := newManager(d)

	out := m.Reconcile(Input{
		SIMPath:          "",
		LockStatus:       "sim-pin",
		UnlockRetriesPin: 3,
		ConfiguredPIN:    "0000",
	})

	// Without a SIM path we cannot call SendPin/EnablePin. Treat as error
	// so the operator sees something is off; do not flip triedThisRun.
	if out != OutcomeError {
		t.Fatalf("got %q, want %q", out, OutcomeError)
	}
	if d.sendPinCalls != 0 || d.enablePinCalls != 0 {
		t.Fatalf("must not call D-Bus without a SIM path")
	}
}

func TestReconcile_PukErrorFromBadPinAttempt(t *testing.T) {
	// EnablePin/SendPin can return SimPuk if the SIM was already PUK-locked
	// when the call landed (race against external mmcli). Treat as PUK,
	// not as a counted retry.
	d := &fakeDBus{sendPinErr: pukErr()}
	m := newManager(d)

	out := m.Reconcile(Input{
		SIMPath:          testSimPath,
		LockStatus:       "sim-pin",
		UnlockRetriesPin: 3,
		ConfiguredPIN:    "0000",
	})

	if out != OutcomePukRequired {
		t.Fatalf("got %q, want %q", out, OutcomePukRequired)
	}
}

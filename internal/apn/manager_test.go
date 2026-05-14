package apn

import (
	"io"
	"log"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"modem-service/internal/mm"
)

type fakeMM struct {
	current  map[string]dbus.Variant
	setErr   error
	getErr   error
	lastSet  map[string]dbus.Variant
	atCmds   []string
	atErr    error
	atOnSend func(cmd string) error
}

func (f *fakeMM) GetInitialEpsBearerSettings(dbus.ObjectPath) (map[string]dbus.Variant, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.current, nil
}

func (f *fakeMM) SetInitialEpsBearerSettings(_ dbus.ObjectPath, s map[string]dbus.Variant) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.current = s
	f.lastSet = s
	return nil
}

func (f *fakeMM) SendCommand(_ dbus.ObjectPath, cmd string, _ time.Duration) (string, error) {
	f.atCmds = append(f.atCmds, cmd)
	if f.atOnSend != nil {
		if err := f.atOnSend(cmd); err != nil {
			return "", err
		}
	}
	if f.atErr != nil {
		return "", f.atErr
	}
	return "", nil
}

type fakeNM struct {
	current Config
	setErr  error
	getErr  error
	lastSet *Config
	reapply int
}

func (f *fakeNM) GetGSM(string) (Config, error) {
	if f.getErr != nil {
		return Config{}, f.getErr
	}
	return f.current, nil
}

func (f *fakeNM) SetGSM(_ string, cfg Config) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.current = cfg
	c := cfg
	f.lastSet = &c
	return nil
}

func (f *fakeNM) Reapply(string) error {
	f.reapply++
	return nil
}

const testPath dbus.ObjectPath = "/test/modem"

func newTestManager() (*Manager, *fakeMM, *fakeNM) {
	mmFake := &fakeMM{}
	nmFake := &fakeNM{}
	return New(mmFake, nmFake, "wwan", log.New(io.Discard, "", 0)), mmFake, nmFake
}

func TestReconcileNoSIM(t *testing.T) {
	m, _, _ := newTestManager()
	got := m.Reconcile(Input{ICCID: "", ModemPath: testPath})
	if got != OutcomeNoSIM {
		t.Fatalf("got %s want %s", got, OutcomeNoSIM)
	}
}

func TestReconcileUnconfiguredClean(t *testing.T) {
	m, _, _ := newTestManager()
	got := m.Reconcile(Input{ICCID: "ICCID-A", ModemPath: testPath})
	if got != OutcomeUnconfigured {
		t.Fatalf("got %s want %s", got, OutcomeUnconfigured)
	}
}

func TestReconcileUnconfiguredButDirty(t *testing.T) {
	m, mmFake, nmFake := newTestManager()
	nmFake.current = Config{APN: "stale.apn"}
	mmFake.current = map[string]dbus.Variant{
		"apn":          dbus.MakeVariant("stale.apn"),
		"allowed-auth": dbus.MakeVariant(mm.BearerAuthNone),
	}
	got := m.Reconcile(Input{ICCID: "ICCID-A", ModemPath: testPath})
	if got != OutcomeApplied {
		t.Fatalf("got %s want %s", got, OutcomeApplied)
	}
	if nmFake.lastSet == nil || !nmFake.lastSet.IsEmpty() {
		t.Errorf("NM not cleared: %+v", nmFake.lastSet)
	}
	if mmFake.lastSet == nil {
		t.Errorf("MM not set")
	}
}

func TestReconcileApplyFromEmpty(t *testing.T) {
	m, mmFake, nmFake := newTestManager()
	desired := Config{APN: "internet.telekom", Username: "congstar", Password: "cs", Auth: "pap"}
	got := m.Reconcile(Input{ICCID: "ICCID-A", ModemPath: testPath, Desired: desired})
	if got != OutcomeApplied {
		t.Fatalf("got %s want %s", got, OutcomeApplied)
	}
	if nmFake.lastSet == nil || !nmFake.lastSet.Equal(desired) {
		t.Errorf("NM: %+v want %+v", nmFake.lastSet, desired)
	}
	if mmFake.lastSet == nil {
		t.Fatalf("MM not set")
	}
	if v, _ := mmFake.lastSet["apn"].Value().(string); v != desired.APN {
		t.Errorf("MM apn=%q want %q", v, desired.APN)
	}
	if v, _ := mmFake.lastSet["allowed-auth"].Value().(uint32); v != mm.BearerAuthPAP {
		t.Errorf("MM allowed-auth=%d want %d", v, mm.BearerAuthPAP)
	}
}

func TestReconcileIdempotent(t *testing.T) {
	m, _, nmFake := newTestManager()
	desired := Config{APN: "internet.telekom", Username: "congstar", Password: "cs", Auth: "pap"}
	if got := m.Reconcile(Input{ICCID: "ICCID-A", ModemPath: testPath, Desired: desired}); got != OutcomeApplied {
		t.Fatalf("first apply: got %s", got)
	}
	nmFake.lastSet = nil
	if got := m.Reconcile(Input{ICCID: "ICCID-A", ModemPath: testPath, Desired: desired}); got != OutcomeOK {
		t.Fatalf("second apply: got %s want %s", got, OutcomeOK)
	}
	if nmFake.lastSet != nil {
		t.Errorf("NM re-set unexpectedly: %+v", nmFake.lastSet)
	}
}

func TestReconcileICCIDChangeClears(t *testing.T) {
	m, mmFake, nmFake := newTestManager()
	desired := Config{APN: "internet.telekom", Username: "congstar", Password: "cs", Auth: "pap"}
	if got := m.Reconcile(Input{ICCID: "ICCID-A", ModemPath: testPath, Desired: desired}); got != OutcomeApplied {
		t.Fatalf("apply: got %s", got)
	}
	nmFake.lastSet = nil
	mmFake.lastSet = nil
	got := m.Reconcile(Input{ICCID: "ICCID-B", ModemPath: testPath, Desired: desired})
	if got != OutcomeICCIDChangedClear {
		t.Fatalf("got %s want %s", got, OutcomeICCIDChangedClear)
	}
	if nmFake.lastSet == nil || !nmFake.lastSet.IsEmpty() {
		t.Errorf("NM not cleared on ICCID change: %+v", nmFake.lastSet)
	}
	if mmFake.lastSet == nil {
		t.Errorf("MM not cleared on ICCID change")
	}
}

func TestReconcileAfterICCIDClearAwaitsUserAction(t *testing.T) {
	m, _, nmFake := newTestManager()
	// Apply for SIM A.
	if got := m.Reconcile(Input{ICCID: "ICCID-A", ModemPath: testPath, Desired: Config{APN: "a"}}); got != OutcomeApplied {
		t.Fatalf("apply A: got %s", got)
	}
	// SIM swap; clears.
	if got := m.Reconcile(Input{ICCID: "ICCID-B", ModemPath: testPath, Desired: Config{APN: "a"}}); got != OutcomeICCIDChangedClear {
		t.Fatalf("swap: got %s", got)
	}
	// Subsequent tick with the same (stale) settings should re-apply for
	// the new SIM, because clear path resets lastAppliedICCID. That's the
	// pragmatic choice: settings reflect the user's intent and we honor
	// it; if they don't want this APN on the new SIM, they have to
	// change settings.
	nmFake.lastSet = nil
	got := m.Reconcile(Input{ICCID: "ICCID-B", ModemPath: testPath, Desired: Config{APN: "a"}})
	if got != OutcomeApplied {
		t.Fatalf("after-clear apply: got %s want %s", got, OutcomeApplied)
	}
}

func TestApplySendsATCommands(t *testing.T) {
	m, mmFake, _ := newTestManager()
	desired := Config{APN: "internet.telekom", Username: "congstar", Password: "cs", Auth: "pap"}
	if got := m.Reconcile(Input{ICCID: "ICCID-A", ModemPath: testPath, Desired: desired}); got != OutcomeApplied {
		t.Fatalf("got %s", got)
	}
	if len(mmFake.atCmds) != 2 {
		t.Fatalf("want 2 AT commands, got %d: %v", len(mmFake.atCmds), mmFake.atCmds)
	}
	want := `AT+CGDCONT=1,"IP","internet.telekom"`
	if mmFake.atCmds[0] != want {
		t.Errorf("CGDCONT: got %q want %q", mmFake.atCmds[0], want)
	}
	wantAuth := `AT+CGAUTH=1,1,"congstar","cs"`
	if mmFake.atCmds[1] != wantAuth {
		t.Errorf("CGAUTH: got %q want %q", mmFake.atCmds[1], wantAuth)
	}
}

func TestClearSendsEmptyCGDCONT(t *testing.T) {
	m, mmFake, nmFake := newTestManager()
	nmFake.current = Config{APN: "stale"}
	mmFake.current = map[string]dbus.Variant{"apn": dbus.MakeVariant("stale")}
	if got := m.Reconcile(Input{ICCID: "ICCID-A", ModemPath: testPath}); got != OutcomeApplied {
		t.Fatalf("got %s", got)
	}
	if len(mmFake.atCmds) < 2 {
		t.Fatalf("want CGDCONT+CGAUTH, got %v", mmFake.atCmds)
	}
	want := `AT+CGDCONT=1,"IPV4V6",""`
	if mmFake.atCmds[0] != want {
		t.Errorf("clear CGDCONT: got %q want %q", mmFake.atCmds[0], want)
	}
	if mmFake.atCmds[1] != "AT+CGAUTH=1,0" {
		t.Errorf("clear CGAUTH: got %q", mmFake.atCmds[1])
	}
}

func TestApplyChapAuth(t *testing.T) {
	m, mmFake, _ := newTestManager()
	desired := Config{APN: "x", Username: "u", Password: "p", Auth: "chap"}
	if got := m.Reconcile(Input{ICCID: "ICCID-A", ModemPath: testPath, Desired: desired}); got != OutcomeApplied {
		t.Fatalf("got %s", got)
	}
	want := `AT+CGAUTH=1,2,"u","p"`
	if mmFake.atCmds[1] != want {
		t.Errorf("CHAP CGAUTH: got %q want %q", mmFake.atCmds[1], want)
	}
}

func TestApplyRejectsQuotedValues(t *testing.T) {
	m, _, _ := newTestManager()
	desired := Config{APN: `bad"apn`}
	got := m.Reconcile(Input{ICCID: "ICCID-A", ModemPath: testPath, Desired: desired})
	if got != OutcomeError {
		t.Fatalf("got %s want %s", got, OutcomeError)
	}
}

func TestReattachSendsCOPS(t *testing.T) {
	m, mmFake, _ := newTestManager()
	if err := m.Reattach(testPath); err != nil {
		t.Fatalf("reattach: %v", err)
	}
	if len(mmFake.atCmds) != 2 || mmFake.atCmds[0] != "AT+COPS=2" || mmFake.atCmds[1] != "AT+COPS=0" {
		t.Errorf("got %v want [AT+COPS=2 AT+COPS=0]", mmFake.atCmds)
	}
}

func TestEncodeDecodeAuth(t *testing.T) {
	cases := []struct {
		in  string
		val uint32
	}{
		{"", mm.BearerAuthNone},
		{"none", mm.BearerAuthNone},
		{"pap", mm.BearerAuthPAP},
		{"chap", mm.BearerAuthCHAP},
		{"bogus", mm.BearerAuthNone},
	}
	for _, c := range cases {
		if got := encodeAuth(c.in); got != c.val {
			t.Errorf("encodeAuth(%q) = %d want %d", c.in, got, c.val)
		}
	}
	if got := decodeAuth(mm.BearerAuthPAP); got != "pap" {
		t.Errorf("decodeAuth(PAP) = %q", got)
	}
	if got := decodeAuth(mm.BearerAuthCHAP); got != "chap" {
		t.Errorf("decodeAuth(CHAP) = %q", got)
	}
	if got := decodeAuth(mm.BearerAuthNone); got != "none" {
		t.Errorf("decodeAuth(NONE) = %q", got)
	}
}

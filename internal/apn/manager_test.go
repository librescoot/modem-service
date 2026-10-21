package apn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

const testPath = dbus.ObjectPath("/Modem/0")

var oldConfig = Config{APN: "old.example", Username: "old-user", Password: "old-password", Auth: "pap"}
var newConfig = Config{APN: "new.example", Username: "new-user", Password: "new-password", Auth: "chap"}

type fakeBackends struct {
	mu         sync.Mutex
	cfg        Config
	settings   map[string]dbus.Variant
	cid        attachContext
	state      ConnectionState
	registered bool
	calls      []string
	counts     map[string]int
	hook       func(context.Context, string, int) error
}

func fixture() (*Manager, *fakeBackends, *bytes.Buffer) {
	f := &fakeBackends{cfg: oldConfig, settings: encodeMMSettings(oldConfig), cid: desiredContext(oldConfig),
		state: ConnectionState{Autoconnect: true, Active: true}, registered: true, counts: make(map[string]int)}
	var logs bytes.Buffer
	return New(f, f, "wwan", log.New(&logs, "", 0)), f, &logs
}

func input(cfg Config) Input { return Input{ICCID: "sim-a", ModemPath: testPath, Desired: cfg} }

func (f *fakeBackends) step(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	f.calls = append(f.calls, name)
	f.counts[name]++
	n, hook := f.counts[name], f.hook
	f.mu.Unlock()
	if hook != nil {
		return hook(ctx, name, n)
	}
	return nil
}

func cloneSettings(settings map[string]dbus.Variant) map[string]dbus.Variant {
	out := make(map[string]dbus.Variant)
	for k, v := range settings {
		out[k] = v
	}
	return out
}

func (f *fakeBackends) GetInitialEpsBearerSettingsContext(ctx context.Context, _ dbus.ObjectPath) (map[string]dbus.Variant, error) {
	if err := f.step(ctx, "mm get"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneSettings(f.settings), nil
}
func (f *fakeBackends) SetInitialEpsBearerSettingsContext(ctx context.Context, _ dbus.ObjectPath, settings map[string]dbus.Variant) error {
	if err := f.step(ctx, "mm set"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settings = cloneSettings(settings)
	return nil
}
func (f *fakeBackends) SendCommandContext(ctx context.Context, _ dbus.ObjectPath, cmd string, _ time.Duration) (string, error) {
	if err := f.step(ctx, cmd); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case cmd == "AT+CGDCONT?":
		if !f.cid.Present {
			return "OK", nil
		}
		return fmt.Sprintf("+CGDCONT: 1,\"%s\",\"%s\",\"0.0.0.0\",0,0\r\nOK", f.cid.PDPType, f.cid.APN), nil
	case cmd == "AT+COPS=2":
		f.registered = false
	case cmd == "AT+COPS=0":
		f.registered = true
	case strings.HasPrefix(cmd, "AT+CGDCONT="):
		if f.registered || f.state.Active || f.state.Autoconnect {
			return "", errors.New("CID write while attached or autoconnect enabled")
		}
		if cmd == "AT+CGDCONT=1" {
			f.cid = attachContext{}
			break
		}
		cid, err := parseCID1("+CGDCONT: " + strings.TrimPrefix(cmd, "AT+CGDCONT="))
		if err != nil {
			return "", err
		}
		f.cid = cid
	case strings.HasPrefix(cmd, "AT+CGAUTH="):
		if f.registered || f.state.Active || f.state.Autoconnect {
			return "", errors.New("auth write while attached or autoconnect enabled")
		}
	default:
		return "", errors.New("unexpected AT command")
	}
	return "OK", nil
}
func (f *fakeBackends) GetGSM(ctx context.Context, _ string) (Config, error) {
	if err := f.step(ctx, "nm get"); err != nil {
		return Config{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg, nil
}
func (f *fakeBackends) SetGSM(ctx context.Context, _ string, cfg Config) error {
	if err := f.step(ctx, "nm set"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfg = cfg
	return nil
}
func (f *fakeBackends) State(ctx context.Context, _ string) (ConnectionState, error) {
	if err := f.step(ctx, "nm state"); err != nil {
		return ConnectionState{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, nil
}
func (f *fakeBackends) SetAutoconnect(ctx context.Context, _ string, enabled bool) error {
	if err := f.step(ctx, fmt.Sprintf("nm auto %t", enabled)); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state.Autoconnect = enabled
	return nil
}
func (f *fakeBackends) Commit(ctx context.Context, _ string, enabled bool) error {
	if err := f.step(ctx, fmt.Sprintf("nm commit %t", enabled)); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state.Autoconnect = enabled
	return nil
}

func (f *fakeBackends) Disconnect(ctx context.Context, _ string) error {
	if err := f.step(ctx, "nm down"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state.Active = false
	return nil
}
func (f *fakeBackends) Connect(ctx context.Context, _ string) error {
	if err := f.step(ctx, "nm up"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state.Active, f.registered = true, true
	return nil
}

func TestReconcileTransactionOrder(t *testing.T) {
	m, f, logs := fixture()
	if got := m.Reconcile(context.Background(), input(newConfig)); got != OutcomeApplied {
		t.Fatalf("outcome=%s, logs=%s", got, logs.String())
	}
	want := []string{"nm get", "mm get", "AT+CGDCONT?", "nm state", "nm auto false", "nm down", "AT+COPS=2", "nm set", "mm set",
		`AT+CGDCONT=1,"IP","new.example"`, `AT+CGAUTH=1,2,"new-user","new-password"`, "AT+CGDCONT?", "nm get", "AT+COPS=0", "nm up", "AT+CGDCONT?", "nm commit true"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls=%q, want=%q", f.calls, want)
	}
	if !f.cfg.Equal(newConfig) || f.cid != desiredContext(newConfig) || !f.state.Active || !f.state.Autoconnect {
		t.Fatal("transaction did not commit desired state")
	}
	if strings.Contains(logs.String(), newConfig.Password) || strings.Contains(logs.String(), newConfig.Username) {
		t.Fatal("credentials logged")
	}
	f.calls = nil
	if got := m.Reconcile(context.Background(), input(newConfig)); got != OutcomeOK {
		t.Fatalf("second outcome=%s", got)
	}
	if want := []string{"nm get", "mm get", "AT+CGDCONT?"}; !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("idempotent calls=%q", f.calls)
	}
}

func TestActualCIDOverridesMatchingCaches(t *testing.T) {
	m, f, logs := fixture()
	if got := m.Reconcile(context.Background(), input(oldConfig)); got != OutcomeOK {
		t.Fatal(got)
	}
	f.cid.APN = "drift.example"
	if got := m.Reconcile(context.Background(), input(oldConfig)); got != OutcomeApplied {
		t.Fatalf("outcome=%s, logs=%s", got, logs.String())
	}
	if f.cid != desiredContext(oldConfig) || f.counts["AT+COPS=2"] != 1 {
		t.Fatal("cached ICCID masked CID1 drift")
	}
}

func TestAlreadyCorrectAfterRestartDoesNotReattach(t *testing.T) {
	m, f, _ := fixture()
	if got := m.Reconcile(context.Background(), input(oldConfig)); got != OutcomeOK {
		t.Fatal(got)
	}
	if f.counts["nm down"] != 0 {
		t.Fatal("unnecessary restart reattach")
	}
}

func TestMMFallbackDoesNotLoopOnStaleCache(t *testing.T) {
	m, f, logs := fixture()
	f.hook = func(_ context.Context, step string, _ int) error {
		if step == "mm set" {
			return errors.New("unsupported")
		}
		return nil
	}
	if got := m.Reconcile(context.Background(), input(newConfig)); got != OutcomeApplied {
		t.Fatalf("outcome=%s logs=%s", got, logs.String())
	}
	if got := m.Reconcile(context.Background(), input(newConfig)); got != OutcomeOK {
		t.Fatal(got)
	}
	if f.counts["nm down"] != 1 {
		t.Fatal("stale MM cache caused another detach")
	}
}

func TestFailuresRollbackAndBackOff(t *testing.T) {
	for _, fail := range []string{"nm auto false", "nm down", "AT+COPS=2", "nm set", `AT+CGDCONT=1,"IP","new.example"`,
		`AT+CGAUTH=1,2,"new-user","new-password"`, "AT+COPS=0", "nm up", "nm commit true"} {
		t.Run(fail, func(t *testing.T) {
			m, f, logs := fixture()
			now := time.Unix(1000, 0)
			m.now = func() time.Time { return now }
			f.hook = func(_ context.Context, step string, n int) error {
				if step == fail && n == 1 {
					return errors.New("rejected")
				}
				return nil
			}
			if got := m.Reconcile(context.Background(), input(newConfig)); got != OutcomeError {
				t.Fatalf("outcome=%s", got)
			}
			if !f.cfg.Equal(oldConfig) || f.cid != desiredContext(oldConfig) || !parseMMSettings(f.settings).Equal(oldConfig) || !f.state.Autoconnect || !f.state.Active {
				t.Fatalf("rollback did not restore state: calls=%q logs=%s", f.calls, logs.String())
			}
			if m.lastAppliedICCID != "" {
				t.Fatal("failed transaction cached success")
			}
			attempts := f.counts["nm state"]
			if got := m.Reconcile(context.Background(), input(newConfig)); got != OutcomeError {
				t.Fatal(got)
			}
			if f.counts["nm state"] != attempts {
				t.Fatal("retry during backoff")
			}
			now = now.Add(retryDelay)
			if got := m.Reconcile(context.Background(), input(newConfig)); got != OutcomeApplied {
				t.Fatalf("retry=%s logs=%s", got, logs.String())
			}
		})
	}
}

func TestReadbackMismatchRollsBack(t *testing.T) {
	for _, atRead := range []int{2, 3} {
		t.Run(fmt.Sprint(atRead), func(t *testing.T) {
			m, f, logs := fixture()
			f.hook = func(_ context.Context, step string, n int) error {
				if step == "AT+CGDCONT?" && n == atRead {
					f.mu.Lock()
					f.cid.APN = "mismatch.example"
					f.mu.Unlock()
				}
				return nil
			}
			if got := m.Reconcile(context.Background(), input(newConfig)); got != OutcomeError {
				t.Fatal(got)
			}
			if f.cid != desiredContext(oldConfig) || !f.cfg.Equal(oldConfig) {
				t.Fatalf("rollback failed: %s", logs.String())
			}
			if !strings.Contains(logs.String(), "mismatch") && !strings.Contains(logs.String(), "during NM activation") {
				t.Fatalf("missing readback error: %s", logs.String())
			}
		})
	}
}

func TestReadsFailWithoutMutating(t *testing.T) {
	for _, fail := range []string{"nm get", "mm get", "AT+CGDCONT?", "nm state"} {
		t.Run(fail, func(t *testing.T) {
			m, f, _ := fixture()
			f.hook = func(_ context.Context, step string, _ int) error {
				if step == fail {
					return errors.New("unavailable")
				}
				return nil
			}
			if got := m.Reconcile(context.Background(), input(newConfig)); got != OutcomeError {
				t.Fatal(got)
			}
			if f.counts["nm down"] != 0 || f.counts["nm auto false"] != 0 || f.counts["nm set"] != 0 {
				t.Fatalf("mutated after failed snapshot: %q", f.calls)
			}
		})
	}
}

func TestRetryLimitAndRecoveryReset(t *testing.T) {
	m, f, _ := fixture()
	now := time.Unix(1000, 0)
	m.now = func() time.Time { return now }
	f.hook = func(_ context.Context, step string, _ int) error {
		if step == `AT+CGDCONT=1,"IP","new.example"` {
			return errors.New("rejected")
		}
		return nil
	}
	for range maxApplyAttempts + 2 {
		if got := m.Reconcile(context.Background(), input(newConfig)); got != OutcomeError {
			t.Fatal(got)
		}
		now = now.Add(time.Hour)
	}
	if f.counts["nm state"] != maxApplyAttempts {
		t.Fatalf("attempts=%d", f.counts["nm state"])
	}
	f.hook = nil
	m.ResetRetry()
	if got := m.Reconcile(context.Background(), input(newConfig)); got != OutcomeApplied {
		t.Fatal(got)
	}
}

func TestPartialRollbackCannotBecomeOK(t *testing.T) {
	m, f, logs := fixture()
	f.hook = func(_ context.Context, step string, n int) error {
		if step == "nm up" || step == "nm set" && n > 1 || step == `AT+CGDCONT=1,"IP","old.example"` || step == "mm set" && n > 1 {
			return errors.New("unavailable")
		}
		return nil
	}
	if got := m.Reconcile(context.Background(), input(newConfig)); got != OutcomeError {
		t.Fatal(got)
	}
	if !strings.Contains(logs.String(), "rollback") {
		t.Fatalf("rollback failure not reported: %s", logs.String())
	}
	if got := m.Reconcile(context.Background(), input(newConfig)); got != OutcomeError {
		t.Fatalf("partial write incorrectly accepted as %s", got)
	}
	if f.state.Autoconnect {
		t.Fatal("enabled autoconnect with unverified rollback")
	}
	f.hook = nil
	m.ResetRetry()
	if got := m.Reconcile(context.Background(), input(newConfig)); got != OutcomeApplied {
		t.Fatal(got)
	}
	if !f.state.Autoconnect || m.pending != nil {
		t.Fatal("lost original policy after rollback failure")
	}
}

func TestCancelRollsBackWithoutReattaching(t *testing.T) {
	m, f, logs := fixture()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.hook = func(ctx context.Context, step string, n int) error {
		if step == `AT+CGAUTH=1,2,"new-user","new-password"` && n == 1 {
			cancel()
			return ctx.Err()
		}
		return nil
	}
	if got := m.Reconcile(ctx, input(newConfig)); got != OutcomeError {
		t.Fatal(got)
	}
	if !f.cfg.Equal(oldConfig) || f.cid != desiredContext(oldConfig) || !f.state.Autoconnect {
		t.Fatalf("cancel rollback failed: %s", logs.String())
	}
	if f.counts["AT+COPS=0"] != 0 || f.counts["nm up"] != 0 {
		t.Fatalf("reattached after cancellation: %q", f.calls)
	}
}

func TestConcurrentReconcileAndReattachAreExcluded(t *testing.T) {
	m, f, _ := fixture()
	entered, release := make(chan struct{}), make(chan struct{})
	f.hook = func(_ context.Context, step string, _ int) error {
		if step == "nm down" {
			close(entered)
			<-release
		}
		return nil
	}
	done := make(chan Outcome, 1)
	go func() { done <- m.Reconcile(context.Background(), input(newConfig)) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("transaction did not enter")
	}
	if got := m.Reconcile(context.Background(), input(newConfig)); got != OutcomeError {
		t.Fatal(got)
	}
	if err := m.Reattach(context.Background(), testPath); err == nil {
		t.Fatal("reattach overlapped transaction")
	}
	close(release)
	if got := <-done; got != OutcomeApplied {
		t.Fatal(got)
	}
}

func TestNoSIMAndInvalidConfigDoNotMutate(t *testing.T) {
	for _, in := range []Input{{}, {ICCID: "sim-a"}, input(Config{APN: "bad\"value"}), input(Config{Password: "bad\nvalue"}), input(Config{Username: "bad\x00value"}), input(Config{Auth: "unknown"})} {
		m, f, _ := fixture()
		got := m.Reconcile(context.Background(), in)
		if got != OutcomeNoSIM && got != OutcomeError {
			t.Fatal(got)
		}
		if len(f.calls) != 0 {
			t.Fatal("invalid input reached backend")
		}
	}
}

func TestSIMSwapStaysClearedUntilSettingsChange(t *testing.T) {
	m, f, logs := fixture()
	if got := m.Reconcile(context.Background(), input(oldConfig)); got != OutcomeOK {
		t.Fatal(got)
	}
	in := input(oldConfig)
	in.ICCID = "sim-b"
	if got := m.Reconcile(context.Background(), in); got != OutcomeICCIDChangedClear {
		t.Fatalf("outcome=%s logs=%s", got, logs.String())
	}
	if !f.cfg.IsEmpty() || f.cid.APN != "" {
		t.Fatal("old SIM credentials retained")
	}
	count := len(f.calls)
	if got := m.Reconcile(context.Background(), in); got != OutcomeUnconfigured {
		t.Fatal(got)
	}
	if len(f.calls) != count {
		t.Fatal("blocked SIM reapplied credentials")
	}
	m.ResetRetry()
	if got := m.Reconcile(context.Background(), in); got != OutcomeUnconfigured {
		t.Fatal(got)
	}
	in.Desired = newConfig
	if got := m.Reconcile(context.Background(), in); got != OutcomeApplied {
		t.Fatal(got)
	}
}

func TestUnconfiguredAndMissingCID(t *testing.T) {
	for _, present := range []bool{false, true} {
		m, f, _ := fixture()
		f.cfg, f.settings, f.cid = Config{}, encodeMMSettings(Config{}), attachContext{Present: present}
		if present {
			f.cid.PDPType = "IP"
		}
		if got := m.Reconcile(context.Background(), input(Config{})); got != OutcomeUnconfigured {
			t.Fatal(got)
		}
		if f.counts["nm down"] != 0 {
			t.Fatal("empty context was unnecessarily changed")
		}
		if got := m.Reconcile(context.Background(), input(newConfig)); got != OutcomeApplied {
			t.Fatal(got)
		}
	}
}

func TestPreservesDisabledAutoconnect(t *testing.T) {
	m, f, _ := fixture()
	f.state.Autoconnect = false
	if got := m.Reconcile(context.Background(), input(newConfig)); got != OutcomeApplied {
		t.Fatal(got)
	}
	if f.state.Autoconnect {
		t.Fatal("enabled autoconnect")
	}
}

func TestParseCID1(t *testing.T) {
	for _, tc := range []struct {
		response string
		want     attachContext
		bad      bool
	}{
		{"AT+CGDCONT?\r\n+CGDCONT: 0,\"IP\",\"ignored\"\r\n+CGDCONT: 1,\"IPV4V6\",\"carrier.example\",\"0.0.0.0\",0,0\r\nOK", attachContext{true, "IPV4V6", "carrier.example"}, false},
		{"+CGDCONT: 1, \"IP\", \"\"\r\nOK", attachContext{true, "IP", ""}, false},
		{"OK", attachContext{}, false},
		{"+CGDCONT: 2,\"IP\",\"other\"", attachContext{}, false},
		{"+CGDCONT: 1,\"IP\"", attachContext{}, true},
		{"+CGDCONT: 1,\"IP\",\"unterminated", attachContext{}, true},
		{"+CGDCONT: 1,\"invalid\",\"apn\"", attachContext{}, true},
		{"+CGDCONT: 1,\"IP\",\"a\"\n+CGDCONT: 1,\"IP\",\"b\"", attachContext{}, true},
	} {
		got, err := parseCID1(tc.response)
		if (err != nil) != tc.bad || !tc.bad && got != tc.want {
			t.Errorf("parse %q = %v, %v", tc.response, got, err)
		}
	}
}

func TestPartialCommitIsSuspendedBeforeRollback(t *testing.T) {
	m, f, logs := fixture()
	f.hook = func(_ context.Context, step string, n int) error {
		if step == "nm commit true" && n == 1 {
			f.mu.Lock()
			f.state.Autoconnect = true
			f.mu.Unlock()
			return errors.New("commit response lost")
		}
		return nil
	}
	if got := m.Reconcile(context.Background(), input(newConfig)); got != OutcomeError {
		t.Fatal(got)
	}
	if m.pending != nil || f.cid != desiredContext(oldConfig) || !f.state.Autoconnect {
		t.Fatalf("rollback failed: %s", logs.String())
	}
}

func TestRollbackUsesActualCIDRatherThanMMCache(t *testing.T) {
	m, f, _ := fixture()
	f.cid = attachContext{true, "IPV6", "actual.example"}
	before := f.cid
	f.hook = func(_ context.Context, step string, n int) error {
		if step == "nm up" && n == 1 {
			return errors.New("failed")
		}
		return nil
	}
	if got := m.Reconcile(context.Background(), input(newConfig)); got != OutcomeError {
		t.Fatal(got)
	}
	if f.cid != before {
		t.Fatal("rollback restored cached rather than actual CID1")
	}
}

func TestMissingCIDIsRemovedOnRollback(t *testing.T) {
	m, f, _ := fixture()
	f.cid = attachContext{}
	f.hook = func(_ context.Context, step string, n int) error {
		if step == "nm up" && n == 1 {
			return errors.New("failed")
		}
		return nil
	}
	if got := m.Reconcile(context.Background(), input(newConfig)); got != OutcomeError {
		t.Fatal(got)
	}
	if f.cid.Present {
		t.Fatal("rollback retained newly created CID1")
	}
}

func TestCancellationDuringRollbackPreventsReattach(t *testing.T) {
	m, f, _ := fixture()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.hook = func(_ context.Context, step string, n int) error {
		if step == `AT+CGAUTH=1,2,"new-user","new-password"` {
			return errors.New("rejected")
		}
		if step == `AT+CGAUTH=1,1,"old-user","old-password"` {
			cancel()
		}
		return nil
	}
	if got := m.Reconcile(ctx, input(newConfig)); got != OutcomeError {
		t.Fatal(got)
	}
	if f.counts["AT+COPS=0"] != 0 || f.counts["nm up"] != 0 {
		t.Fatal("reattached after cancellation during rollback")
	}
}

func TestUnconfiguredOperatorContextAndSubsequentDrift(t *testing.T) {
	m, f, logs := fixture()
	f.hook = func(_ context.Context, step string, _ int) error {
		if step == "nm up" {
			f.mu.Lock()
			f.cid = attachContext{true, "IP", "operator.example"}
			f.mu.Unlock()
		}
		return nil
	}
	if got := m.Reconcile(context.Background(), input(Config{})); got != OutcomeApplied {
		t.Fatalf("outcome=%s logs=%s", got, logs.String())
	}
	if got := m.Reconcile(context.Background(), input(Config{})); got != OutcomeUnconfigured {
		t.Fatal(got)
	}
	f.cid.APN = "drift.example"
	if got := m.Reconcile(context.Background(), input(Config{})); got != OutcomeApplied {
		t.Fatal(got)
	}
}

func TestNMReadbackMismatchRollsBack(t *testing.T) {
	m, f, _ := fixture()
	f.hook = func(_ context.Context, step string, n int) error {
		if step == "nm get" && n == 2 {
			f.mu.Lock()
			f.cfg.APN = "mismatch.example"
			f.mu.Unlock()
		}
		return nil
	}
	if got := m.Reconcile(context.Background(), input(newConfig)); got != OutcomeError {
		t.Fatal(got)
	}
	if !f.cfg.Equal(oldConfig) || f.cid != desiredContext(oldConfig) {
		t.Fatal("NM mismatch not rolled back")
	}
}

func TestFailedSIMSwapNeverRestoresPreviousCredentials(t *testing.T) {
	m, f, logs := fixture()
	if got := m.Reconcile(context.Background(), input(oldConfig)); got != OutcomeOK {
		t.Fatal(got)
	}
	f.calls = nil
	f.hook = func(_ context.Context, step string, n int) error {
		if step == "nm up" && n == 1 {
			return errors.New("failed")
		}
		return nil
	}
	in := input(oldConfig)
	in.ICCID = "sim-b"
	if got := m.Reconcile(context.Background(), in); got != OutcomeError {
		t.Fatal(got)
	}
	if !f.cfg.IsEmpty() || !parseMMSettings(f.settings).IsEmpty() || f.cid.APN != "" {
		t.Fatalf("SIM swap rollback retained credentials: %s", logs.String())
	}
	for _, call := range f.calls {
		if strings.Contains(call, oldConfig.Password) {
			t.Fatal("old SIM credentials sent after swap")
		}
	}
}

func TestAuthRoundTrip(t *testing.T) {
	for _, auth := range []string{"none", "pap", "chap"} {
		cfg := Config{APN: "example", Username: "user", Password: "password", Auth: auth}
		if got := parseMMSettings(encodeMMSettings(cfg)); !got.Equal(cfg) {
			t.Fatal("MM settings did not round trip")
		}
	}
	if !(Config{}).Equal(Config{Auth: "none"}) || !(Config{}).IsEmpty() || (Config{Auth: "pap"}).IsEmpty() {
		t.Fatal("empty auth normalization")
	}
}

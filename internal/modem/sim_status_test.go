package modem

import (
	"bufio"
	"io"
	"log"
	"os/exec"
	"sync"
	"testing"

	"github.com/godbus/dbus/v5"

	"modem-service/internal/mm"
)

type simStatusProperties struct {
	mu     sync.RWMutex
	values map[string]map[string]dbus.Variant
}

func (p *simStatusProperties) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if value, ok := p.values[iface][name]; ok {
		return value, nil
	}
	return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.UnknownProperty", []interface{}{name})
}

type simStatusObjects struct{}

func (simStatusObjects) GetManagedObjects() (map[dbus.ObjectPath]map[string]map[string]dbus.Variant, *dbus.Error) {
	return map[dbus.ObjectPath]map[string]map[string]dbus.Variant{
		"/org/freedesktop/ModemManager1/Modem/0": {mm.ModemInterface: {}},
	}, nil
}

func TestGetModemInfoDistinguishesSecondarySIMLocks(t *testing.T) {
	daemon, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Skip("dbus-daemon is required for the isolated ModemManager fixture")
	}
	cmd := exec.Command(daemon, "--session", "--nofork", "--print-address=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		t.Fatalf("private D-Bus address missing: %v", scanner.Err())
	}
	address := scanner.Text()
	t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", address)
	server, err := dbus.Connect(address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	props := &simStatusProperties{values: map[string]map[string]dbus.Variant{
		mm.ModemInterface: {
			"PowerState": dbus.MakeVariant(uint32(mm.MMModemPowerStateOn)),
			"State":      dbus.MakeVariant(mm.MMModemStateRegistered),
		},
		mm.Modem3gppInterface: {
			"RegistrationState": dbus.MakeVariant(mm.MMModem3gppRegistrationStateHome),
		},
	}}
	if err := server.Export(props, "/org/freedesktop/ModemManager1/Modem/0", mm.DBusPropertiesInterface); err != nil {
		t.Fatal(err)
	}
	if err := server.Export(simStatusObjects{}, dbus.ObjectPath(mm.ModemManagerPath), mm.DBusObjectManager); err != nil {
		t.Fatal(err)
	}
	if reply, err := server.RequestName(mm.ModemManagerService, dbus.NameFlagDoNotQueue); err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("claim private ModemManager name: %v, %v", reply, err)
	}
	client, err := mm.NewClient(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	manager := &Manager{client: client, logger: log.New(io.Discard, "", 0)}
	const sim = "/org/freedesktop/ModemManager1/SIM/0"
	tests := []struct {
		name      string
		lock      uint32
		simPath   string
		wantState string
		wantLock  string
		wantError string
		wantKnown bool
	}{
		{"none", mm.MMLockNone, sim, SIMStatePresent, "", "ok", true},
		{"pin", mm.MMLockSimPin, sim, SIMStateLocked, "sim-pin", "sim-locked", true},
		{"puk", mm.MMLockSimPuk, sim, SIMStateLocked, "sim-puk", "sim-locked", true},
		{"pin2", mm.MMLockSimPin2, sim, SIMStatePresent, "sim-pin2", "ok", true},
		{"puk2", mm.MMLockSimPuk2, sim, SIMStatePresent, "sim-puk2", "ok", true},
		{"network-pin", mm.MMLockPhNetPin, sim, SIMStateLocked, "ph-net-pin", "sim-locked", true},
		{"network-puk", mm.MMLockPhNetPuk, sim, SIMStateLocked, "ph-net-puk", "sim-locked", true},
		{"unknown-lock", mm.MMLockUnknown, sim, SIMStatePresent, "", "ok", false},
		{"future-lock", 999, sim, SIMStatePresent, "", "ok", false},
		{"missing-sim-pin2", mm.MMLockSimPin2, "/", SIMStateMissing, "sim-pin2", "sim-missing", true},
		{"unreadable-sim-pin2", mm.MMLockSimPin2, "", SIMStateUnknown, "sim-pin2", "ok", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			props.mu.Lock()
			props.values[mm.ModemInterface]["UnlockRequired"] = dbus.MakeVariant(tc.lock)
			if tc.simPath == "" {
				delete(props.values[mm.ModemInterface], "Sim")
			} else {
				props.values[mm.ModemInterface]["Sim"] = dbus.MakeVariant(dbus.ObjectPath(tc.simPath))
			}
			props.mu.Unlock()
			state, err := manager.GetModemInfo("unused-test-interface")
			if err != nil {
				t.Fatal(err)
			}
			if state.SIMState != tc.wantState || state.SIMLockStatus != tc.wantLock || state.ErrorState != tc.wantError || state.SIMLockStatusKnown != tc.wantKnown {
				t.Errorf("SIM=(%q, %q, known=%v), error=%q; want (%q, %q, known=%v), error=%q",
					state.SIMState, state.SIMLockStatus, state.SIMLockStatusKnown, state.ErrorState,
					tc.wantState, tc.wantLock, tc.wantKnown, tc.wantError)
			}
		})
	}
}

package modem

import (
	"testing"

	"github.com/godbus/dbus/v5"

	"modem-service/internal/mm"
)

func TestGetModemInfoOperatorSources(t *testing.T) {
	manager, server, modemProps := privateModemManager(t)
	const simPath = dbus.ObjectPath("/org/freedesktop/ModemManager1/SIM/0")
	simProps := &simStatusProperties{values: map[string]map[string]dbus.Variant{
		mm.SimInterface: {
			"OperatorName":       dbus.MakeVariant("Lebara"),
			"OperatorIdentifier": dbus.MakeVariant("26214"),
		},
	}}
	if err := server.Export(simProps, simPath, mm.DBusPropertiesInterface); err != nil {
		t.Fatal(err)
	}
	modemProps.mu.Lock()
	modemProps.values[mm.ModemInterface]["Sim"] = dbus.MakeVariant(simPath)
	modemProps.values[mm.Modem3gppInterface]["RegistrationState"] = dbus.MakeVariant(mm.MMModem3gppRegistrationStateRoaming)
	modemProps.values[mm.Modem3gppInterface]["OperatorName"] = dbus.MakeVariant("E-Plus")
	modemProps.values[mm.Modem3gppInterface]["OperatorCode"] = dbus.MakeVariant("26203")
	modemProps.mu.Unlock()

	check := func(t *testing.T, wantSIMState, wantRegistration, simName, simCode, networkName, networkCode, legacyName, legacyCode string) {
		t.Helper()
		state, err := manager.GetModemInfo("unused-test-interface")
		if err != nil {
			t.Fatal(err)
		}
		if state.SIMState != wantSIMState || state.Registration != wantRegistration ||
			state.SIMOperatorName != simName || state.SIMOperatorCode != simCode ||
			state.NetworkOperatorName != networkName || state.NetworkOperatorCode != networkCode ||
			state.OperatorName != legacyName || state.OperatorCode != legacyCode {
			t.Errorf("state: SIM=%q registration=%q SIM operator=%q/%q network operator=%q/%q legacy operator=%q/%q; want %q %q %q/%q %q/%q %q/%q",
				state.SIMState, state.Registration, state.SIMOperatorName, state.SIMOperatorCode,
				state.NetworkOperatorName, state.NetworkOperatorCode, state.OperatorName, state.OperatorCode,
				wantSIMState, wantRegistration, simName, simCode, networkName, networkCode, legacyName, legacyCode)
		}
	}
	t.Run("roaming distinguishes SIM and network", func(t *testing.T) {
		check(t, SIMStatePresent, RegistrationRoaming, "Lebara", "26214", "E-Plus", "26203", "Lebara", "26214")
	})
	t.Run("registration loss clears network even when modem retains properties", func(t *testing.T) {
		modemProps.mu.Lock()
		modemProps.values[mm.Modem3gppInterface]["RegistrationState"] = dbus.MakeVariant(mm.MMModem3gppRegistrationStateSearching)
		modemProps.mu.Unlock()
		check(t, SIMStatePresent, "searching", "Lebara", "26214", "", "", "Lebara", "26214")
	})
	t.Run("unavailable SIM properties never inherit network values", func(t *testing.T) {
		modemProps.mu.Lock()
		modemProps.values[mm.Modem3gppInterface]["RegistrationState"] = dbus.MakeVariant(mm.MMModem3gppRegistrationStateHome)
		modemProps.mu.Unlock()
		simProps.mu.Lock()
		delete(simProps.values[mm.SimInterface], "OperatorName")
		delete(simProps.values[mm.SimInterface], "OperatorIdentifier")
		simProps.mu.Unlock()
		check(t, SIMStatePresent, RegistrationHome, "", "", "E-Plus", "26203", "E-Plus", "26203")
	})
	t.Run("legacy fallback remains independent for partial SIM data", func(t *testing.T) {
		simProps.mu.Lock()
		simProps.values[mm.SimInterface]["OperatorName"] = dbus.MakeVariant("Lebara")
		simProps.mu.Unlock()
		check(t, SIMStatePresent, RegistrationHome, "Lebara", "", "E-Plus", "26203", "Lebara", "26203")
	})
	t.Run("unregistered legacy fallback remains compatible", func(t *testing.T) {
		modemProps.mu.Lock()
		modemProps.values[mm.Modem3gppInterface]["RegistrationState"] = dbus.MakeVariant(mm.MMModem3gppRegistrationStateSearching)
		modemProps.mu.Unlock()
		check(t, SIMStatePresent, "searching", "Lebara", "", "", "", "Lebara", "26203")
	})

	t.Run("missing SIM never inherits network values", func(t *testing.T) {
		modemProps.mu.Lock()
		modemProps.values[mm.ModemInterface]["Sim"] = dbus.MakeVariant(dbus.ObjectPath("/"))
		modemProps.values[mm.Modem3gppInterface]["RegistrationState"] = dbus.MakeVariant(mm.MMModem3gppRegistrationStateHome)
		modemProps.mu.Unlock()
		check(t, SIMStateMissing, RegistrationHome, "", "", "E-Plus", "26203", "E-Plus", "26203")
	})
}

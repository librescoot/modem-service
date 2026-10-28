package service

import (
	"fmt"
	"io"
	"log"
	"reflect"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

type fakeSMSStorage struct {
	initial   string
	probe     string
	supported []uint32
	commands  []string
	defaults  []uint32
	failFinal bool
}

func (f *fakeSMSStorage) SendCommand(_ dbus.ObjectPath, command string, _ time.Duration) (string, error) {
	f.commands = append(f.commands, command)
	switch command {
	case "AT+CPMS?":
		return f.initial, nil
	case `AT+CPMS="SM","ME","ME"`:
		return f.probe, nil
	case `AT+CPMS="SM","SM","SM"`:
		if f.failFinal {
			return "", fmt.Errorf("failed")
		}
		return "+CPMS: 0,40,0,40,0,40", nil
	default:
		return "OK", nil
	}
}
func (f *fakeSMSStorage) GetProperty(_ dbus.ObjectPath, _, property string) (dbus.Variant, error) {
	if property == "SupportedStorages" {
		return dbus.MakeVariant(f.supported), nil
	}
	return dbus.MakeVariant(uint32(2)), nil
}
func (f *fakeSMSStorage) SetSMSDefaultStorage(_ dbus.ObjectPath, value uint32) error {
	f.defaults = append(f.defaults, value)
	return nil
}
func TestRecoverFullSMSStorage(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	const initial = `+CPMS: "SR",0,0,"ME",23,23,"ME",23,23`
	const probe = `+CPMS: 0,40,23,23,23,23`
	f := &fakeSMSStorage{initial: initial, probe: probe, supported: []uint32{1, 2}}
	recoverFullSMSStorage(f, "/modem", logger)
	want := []string{"AT+CPMS?", `AT+CPMS="SM","ME","ME"`, `AT+CPMS="SM","SM","SM"`}
	if !reflect.DeepEqual(f.commands, want) || !reflect.DeepEqual(f.defaults, []uint32{1}) {
		t.Fatalf("commands=%v defaults=%v", f.commands, f.defaults)
	}
}
func TestRecoverFullSMSStorageRestoresOnFullOrFailure(t *testing.T) {
	for _, tc := range []struct {
		probe        string
		failFinal    bool
		wantDefaults []uint32
	}{
		{`+CPMS: 40,40,23,23,23,23`, false, nil},
		{`+CPMS: 0,40,23,23,23,23`, true, []uint32{1, 2}},
	} {
		f := &fakeSMSStorage{initial: `+CPMS: "SR",0,0,"ME",23,23,"ME",23,23`, probe: tc.probe, supported: []uint32{1, 2}, failFinal: tc.failFinal}
		recoverFullSMSStorage(f, "/modem", log.New(io.Discard, "", 0))
		if f.commands[len(f.commands)-1] != `AT+CPMS="SR","ME","ME"` || !reflect.DeepEqual(f.defaults, tc.wantDefaults) {
			t.Fatalf("commands=%v defaults=%v", f.commands, f.defaults)
		}
	}
}
func TestRecoverFullSMSStorageDoesNotChangeAvailableStore(t *testing.T) {
	f := &fakeSMSStorage{initial: `+CPMS: "SM",0,40,"SM",0,40,"SM",0,40`}
	recoverFullSMSStorage(f, "/modem", log.New(io.Discard, "", 0))
	if !reflect.DeepEqual(f.commands, []string{"AT+CPMS?"}) || len(f.defaults) != 0 {
		t.Fatalf("commands=%v defaults=%v", f.commands, f.defaults)
	}
}

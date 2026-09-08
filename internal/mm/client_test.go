package mm

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"log"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

func privateBus(t *testing.T) (*Client, *dbus.Conn, *bytes.Buffer) {
	t.Helper()
	if _, err := exec.LookPath("dbus-daemon"); err != nil {
		t.Skip("dbus-daemon unavailable")
	}
	cmd := exec.Command("dbus-daemon", "--session", "--nofork", "--print-address=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		t.Fatal("private bus did not print its address")
	}
	address := scanner.Text()
	t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", address)
	var logs bytes.Buffer
	c, err := NewClient(true, log.New(&logs, "", 0).Printf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	server, err := dbus.Connect(address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	if _, err := server.RequestName(ModemManagerService, dbus.NameFlagDoNotQueue); err != nil {
		t.Fatal(err)
	}
	return c, server, &logs
}

type testModem struct {
	entered chan struct{}
	release chan struct{}
}

func (m *testModem) Command(command string, timeout uint32) (string, *dbus.Error) {
	if command == "AT+WAIT" {
		close(m.entered)
		<-m.release
		return "OK", nil
	}
	return "", dbus.NewError("org.freedesktop.ModemManager1.Error.Core.Failed", []interface{}{command})
}

func (m *testModem) Get(iface, property string) (dbus.Variant, *dbus.Error) {
	if property == "Enabled" {
		close(m.entered)
		<-m.release
		return dbus.MakeVariant(uint32(16)), nil
	}
	return dbus.MakeVariant(map[string]dbus.Variant{"password": dbus.MakeVariant("secret-password")}), nil
}

func TestContextCallsCancelInFlight(t *testing.T) {
	for _, kind := range []string{"command", "location-property"} {
		t.Run(kind, func(t *testing.T) {
			c, server, _ := privateBus(t)
			m := &testModem{entered: make(chan struct{}), release: make(chan struct{})}
			defer close(m.release)
			const path = dbus.ObjectPath("/Modem/0")
			if err := server.Export(m, path, ModemInterface); err != nil {
				t.Fatal(err)
			}
			if err := server.Export(m, path, DBusPropertiesInterface); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var err error
				if kind == "command" {
					_, err = c.SendCommandContext(ctx, path, "AT+WAIT", time.Minute)
				} else {
					_, err = c.GetEnabledLocationSourcesContext(ctx, path)
				}
				done <- err
			}()
			select {
			case <-m.entered:
			case <-time.After(2 * time.Second):
				t.Fatal("method never entered")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error = %v, want context.Canceled", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("call did not stop on cancellation")
			}
		})
	}
}

func TestAPNCredentialsAreRedacted(t *testing.T) {
	c, server, logs := privateBus(t)
	const path = dbus.ObjectPath("/Modem/0")
	m := &testModem{}
	if err := server.Export(m, path, ModemInterface); err != nil {
		t.Fatal(err)
	}
	if err := server.Export(m, path, DBusPropertiesInterface); err != nil {
		t.Fatal(err)
	}
	_, err := c.SendCommand(path, `AT+CGAUTH=1,1,"user","secret-password"`, time.Second)
	if err == nil || !strings.Contains(err.Error(), "CGAUTH") {
		t.Fatalf("expected identifiable CGAUTH failure, got %v", err)
	}
	if _, err := c.GetInitialEpsBearerSettings(path); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String()+err.Error(), "secret-password") {
		t.Fatalf("credential leaked: logs=%q error=%v", logs.String(), err)
	}
}

func TestClientOwnsItsConnection(t *testing.T) {
	c, _, _ := privateBus(t)
	other, err := NewClient(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := other.conn.BusObject().Call("org.freedesktop.DBus.ListNames", 0).Err; err != nil {
		t.Fatalf("closing one client broke another: %v", err)
	}
}

package mm

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

type fakeTransport struct {
	io.Reader
	io.Writer
}

func (fakeTransport) Close() error { return nil }

func TestWatchSignalLoopSurvivesConnectionClose(t *testing.T) {
	conn, err := dbus.NewConn(fakeTransport{Reader: bytes.NewReader(nil), Writer: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	signals := make(chan *dbus.Signal, 100)
	conn.Signal(signals)
	c := &Client{conn: conn}

	// Force godbus to close the registered channel before watch teardown.
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.watchSignalLoop(context.Background(), signals, []string{"type='signal'"}, func(*dbus.Signal) {
			t.Error("unexpected signal")
		})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not stop after connection close")
	}
}

func TestWatchSignalsRejectsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (&Client{}).watchSignals(ctx, nil, nil); err != context.Canceled {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestWatchersDeliverModemPropertyAndSMSChanges(t *testing.T) {
	c, server, _ := privateBus(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const path = dbus.ObjectPath("/Modem/0")
	seen := make(chan string, 4)
	if err := c.WatchModems(ctx, func(p dbus.ObjectPath) {
		if p == path {
			seen <- "added"
		}
	}, func(p dbus.ObjectPath) {
		if p == path {
			seen <- "removed"
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.WatchPropertyChanges(ctx, path, func(iface, property string, value dbus.Variant) {
		if iface == ModemInterface && property == "State" && value.Value() == MMModemStateEnabled {
			seen <- "state"
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.WatchSMSAdded(ctx, path, func(p dbus.ObjectPath, received bool) {
		if p == "/SMS/0" && received {
			seen <- "sms"
		}
	}); err != nil {
		t.Fatal(err)
	}

	emit := func(name string, args ...interface{}) {
		t.Helper()
		if err := server.Emit(path, name, args...); err != nil {
			t.Fatal(err)
		}
	}
	emit(DBusObjectManager+".InterfacesAdded", path, map[string]map[string]dbus.Variant{ModemInterface: {}})
	emit(DBusPropertiesInterface+".PropertiesChanged", ModemInterface, map[string]dbus.Variant{"State": dbus.MakeVariant(MMModemStateEnabled)}, []string{})
	emit(ModemMessagingInterface+".Added", dbus.ObjectPath("/SMS/0"), true)
	emit(DBusObjectManager+".InterfacesRemoved", path, []string{ModemInterface})
	got := make(map[string]bool)
	for len(got) < 4 {
		select {
		case event := <-seen:
			got[event] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("missing event: got %v", got)
		}
	}
	cancel()
	// All three watches share the same connection-close-safe teardown.
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWatchSignalLoopCancellationUnregistersChannel(t *testing.T) {
	conn, err := dbus.NewConn(fakeTransport{Reader: bytes.NewReader(nil), Writer: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := &Client{conn: conn}
	signals := make(chan *dbus.Signal, 1)
	conn.Signal(signals)
	signals <- &dbus.Signal{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.watchSignalLoop(ctx, signals, nil, func(*dbus.Signal) {
		t.Error("callback invoked after cancellation")
	})
	select {
	case <-signals:
	default:
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	// Only registered channels are closed by godbus.
	select {
	case _, ok := <-signals:
		if !ok {
			t.Fatal("cancelled watch left its channel registered")
		}
	default:
	}
}

package mm

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// fakeTransport is a no-op io.ReadWriteCloser suitable for constructing a
// *dbus.Conn without a live D-Bus daemon: reads report EOF, writes are
// discarded, and Close never errors. It's exactly the machinery WatchSMSAdded
// touches on shutdown (Conn.Close, BusObject().Call after close) without
// needing a real bus on the other end.
type fakeTransport struct {
	io.Reader
	io.Writer
}

func (fakeTransport) Close() error { return nil }

// TestWatchSMSAddedLoopSurvivesConnectionClose reproduces bean
// librescoot-2guc: modem-service panicked with "close of closed channel" on
// every systemctl stop/restart.
//
// service.Service.Run cancels the inbound-SMS watch (smsWatchCancel) and then,
// a few lines later with no synchronization between the two, closes the
// shared mm.Client (MMClient.Close -> conn.Close). Conn.Close's default
// signal handler closes every channel still registered via Conn.Signal,
// including the watch's own signals channel, whenever the watch goroutine
// hasn't yet unregistered it. The watch goroutine used to close that same
// channel itself in a defer, so whichever of the two ran second panicked on
// an already-closed channel.
//
// This test forces the "godbus closes it first" ordering deterministically
// by closing the connection before the watch loop has a chance to run its
// own teardown, and asserts the loop returns cleanly instead of panicking.
func TestWatchSMSAddedLoopSurvivesConnectionClose(t *testing.T) {
	conn, err := dbus.NewConn(fakeTransport{Reader: bytes.NewReader(nil), Writer: io.Discard})
	if err != nil {
		t.Fatalf("dbus.NewConn: %v", err)
	}

	signals := make(chan *dbus.Signal, 100)
	conn.Signal(signals)

	c := &Client{conn: conn, logger: func(string, ...interface{}) {}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const modemPath = dbus.ObjectPath("/org/freedesktop/ModemManager1/Modem/0")
	const rule = "type='signal'"

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.watchSMSAddedLoop(ctx, signals, rule, modemPath, nil)
	}()

	// Simulate MMClient.Close() running (or winning a race) before the watch
	// goroutine notices its context is done, exactly as happens in
	// service.Service.Run's shutdown sequence: smsWatchCancel() is called,
	// then MMClient.Close() follows without waiting for the watch goroutine
	// to actually exit. Conn.Close's Terminate() closes every channel still
	// registered via Signal -- including ours, since we haven't unregistered
	// yet.
	if err := conn.Close(); err != nil {
		t.Fatalf("conn.Close: %v", err)
	}
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchSMSAddedLoop did not return after the connection closed")
	}
}

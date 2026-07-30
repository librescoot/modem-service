package health

import (
	"net"
	"testing"
	"time"
)

func TestBindToDeviceEmptyInterfaceIsNil(t *testing.T) {
	// An empty interface name means "do not bind", which net.Dialer spells
	// as a nil Control func. This is what makes the prober testable off
	// target.
	if bindToDevice("") != nil {
		t.Error("bindToDevice(\"\") returned a control func, want nil")
	}
}

func TestDialerWithEmptyBindStillConnects(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Close()
		}
	}()

	dialer := &net.Dialer{Timeout: time.Second, Control: bindToDevice("")}
	conn, err := dialer.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial with unbound control func: %v", err)
	}
	conn.Close()
}

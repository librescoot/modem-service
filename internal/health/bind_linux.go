//go:build linux

package health

import "syscall"

// bindToDevice returns a net.Dialer Control function that binds the socket to
// iface with SO_BINDTODEVICE, so probe traffic cannot escape via the wifi or
// wired path the MDB might also have. An empty iface returns nil, which
// net.Dialer treats as no control function.
func bindToDevice(iface string) func(network, address string, c syscall.RawConn) error {
	if iface == "" {
		return nil
	}
	return func(_, _ string, c syscall.RawConn) error {
		var sockErr error
		err := c.Control(func(fd uintptr) {
			sockErr = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, iface)
		})
		if err != nil {
			return err
		}
		return sockErr
	}
}

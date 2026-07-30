//go:build !linux

package health

import "syscall"

// bindToDevice is a no-op away from Linux, where SO_BINDTODEVICE has no
// portable equivalent. The target is linux/arm; this exists so the package
// builds and its tests run on a development host.
func bindToDevice(string) func(network, address string, c syscall.RawConn) error {
	return nil
}

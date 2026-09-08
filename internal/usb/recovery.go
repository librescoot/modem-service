package usb

import (
	"io"
	"os"
	"syscall"
	"time"

	"github.com/pkg/errors"
)

const (
	USBDevice = "1-1" // Physical USB topology path of the modem.

	USBUnbindPath = "/sys/bus/usb/drivers/usb/unbind"
	USBBindPath   = "/sys/bus/usb/drivers/usb/bind"

	// The modem requires these delays to disappear and enumerate after reset.
	UnbindWaitMS = 2000
	BindWaitMS   = 2000
)

type controlFile interface {
	WriteString(string) (int, error)
	Sync() error
	Close() error
}

var openControlFile = func(path string) (controlFile, error) {
	return os.OpenFile(path, os.O_WRONLY, 0)
}

type Recovery struct {
	device string
	logger func(string, ...interface{})
}

func NewRecovery(logger func(string, ...interface{})) *Recovery {
	if logger == nil {
		logger = func(string, ...interface{}) {}
	}

	return &Recovery{
		device: USBDevice,
		logger: logger,
	}
}

// ErrDeviceNotPresent is returned when USB recovery is attempted while the
// device is not currently bound to the USB bus. This happens during a
// ModemManager reset window where the modem is transiently off the bus and
// will reappear on its own; unbind/bind would just fail with "no such
// device". Callers should treat this as a hint to wait or escalate past
// USB recovery rather than as a failure.
var ErrDeviceNotPresent = errors.New("USB device not present on bus")

func (r *Recovery) Present() bool {
	_, err := os.Stat("/sys/bus/usb/devices/" + r.device)
	return err == nil
}

// Unbind treats transient modem absence during a ModemManager reset as non-fatal.
func (r *Recovery) Unbind() error {
	if !r.Present() {
		r.log("USB device %s not present on bus, skipping unbind", r.device)
		return ErrDeviceNotPresent
	}
	r.log("Unbinding USB device %s...", r.device)

	if err := writeControl(USBUnbindPath, r.device); err != nil {
		return errors.Wrap(err, "failed to write unbind control")
	}

	r.log("USB device unbound, waiting %dms...", UnbindWaitMS)
	time.Sleep(time.Duration(UnbindWaitMS) * time.Millisecond)

	return nil
}

func (r *Recovery) Bind() error {
	r.log("Binding USB device %s...", r.device)

	if err := writeControl(USBBindPath, r.device); err != nil {
		return errors.Wrap(err, "failed to write bind control")
	}

	r.log("USB device bound, waiting up to %dms for enumeration...", BindWaitMS)

	// Binding is asynchronous; wait for the modem's sysfs node to reappear.
	deadline := time.Now().Add(time.Duration(BindWaitMS) * time.Millisecond)
	devicePath := "/sys/bus/usb/devices/" + r.device
	for time.Now().Before(deadline) {
		if _, err := os.Stat(devicePath); err == nil {
			r.log("USB device %s re-enumerated", r.device)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.Errorf("USB device %s did not re-enumerate within %dms", r.device, BindWaitMS)
}

func writeControl(path, value string) error {
	f, err := openControlFile(path)
	if err != nil {
		return errors.Wrap(err, "open")
	}
	if n, err := f.WriteString(value); err != nil {
		_ = f.Close()
		return errors.Wrap(err, "write")
	} else if n != len(value) {
		_ = f.Close()
		return io.ErrShortWrite
	}
	if err := f.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) &&
		!errors.Is(err, syscall.ENOTSUP) && !errors.Is(err, syscall.EOPNOTSUPP) {
		_ = f.Close()
		return errors.Wrap(err, "sync")
	}
	if err := f.Close(); err != nil {
		return errors.Wrap(err, "close")
	}
	return nil
}

func (r *Recovery) Recover() error {
	r.log("Starting USB recovery...")

	if err := r.Unbind(); err != nil {
		return errors.Wrap(err, "USB recovery failed during unbind")
	}

	if err := r.Bind(); err != nil {
		return errors.Wrap(err, "USB recovery failed during bind")
	}

	r.log("USB recovery complete")
	return nil
}

func (r *Recovery) log(format string, args ...interface{}) {
	r.logger("[USB] "+format, args...)
}

package usb

import (
	"os"
	"time"

	"github.com/pkg/errors"
)

const (
	// USB device path for modem
	USBDevice = "1-1"

	// USB sysfs paths
	USBUnbindPath = "/sys/bus/usb/drivers/usb/unbind"
	USBBindPath   = "/sys/bus/usb/drivers/usb/bind"

	// Timing
	UnbindWaitMS = 2000 // Wait 2 seconds after unbind
	BindWaitMS   = 2000 // Wait 2 seconds after bind
)

// Recovery manages USB recovery operations
type Recovery struct {
	device string
	logger func(string, ...interface{})
}

// NewRecovery creates a new USB recovery manager
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

// Present reports whether the USB device is currently bound to the bus.
func (r *Recovery) Present() bool {
	_, err := os.Stat("/sys/bus/usb/devices/" + r.device)
	return err == nil
}

// Unbind unbinds the USB device from the driver. Returns ErrDeviceNotPresent
// if the device isn't currently bound (transient absence during MM reset).
func (r *Recovery) Unbind() error {
	if !r.Present() {
		r.log("USB device %s not present on bus, skipping unbind", r.device)
		return ErrDeviceNotPresent
	}
	r.log("Unbinding USB device %s...", r.device)

	f, err := os.OpenFile(USBUnbindPath, os.O_WRONLY, 0)
	if err != nil {
		return errors.Wrap(err, "failed to open unbind path")
	}
	if _, err := f.WriteString(r.device); err != nil {
		f.Close()
		return errors.Wrap(err, "failed to write to unbind")
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return errors.Wrap(err, "failed to sync unbind")
	}
	if err := f.Close(); err != nil {
		return errors.Wrap(err, "failed to close unbind")
	}

	r.log("USB device unbound, waiting %dms...", UnbindWaitMS)
	time.Sleep(time.Duration(UnbindWaitMS) * time.Millisecond)

	return nil
}

// Bind binds the USB device to the driver
func (r *Recovery) Bind() error {
	r.log("Binding USB device %s...", r.device)

	f, err := os.OpenFile(USBBindPath, os.O_WRONLY, 0)
	if err != nil {
		return errors.Wrap(err, "failed to open bind path")
	}
	if _, err := f.WriteString(r.device); err != nil {
		f.Close()
		return errors.Wrap(err, "failed to write to bind")
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return errors.Wrap(err, "failed to sync bind")
	}
	if err := f.Close(); err != nil {
		return errors.Wrap(err, "failed to close bind")
	}

	r.log("USB device bound, waiting up to %dms for enumeration...", BindWaitMS)

	// Poll for device to re-enumerate (up to BindWaitMS)
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

// Recover performs a full USB recovery (unbind + bind)
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

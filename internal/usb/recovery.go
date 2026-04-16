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

// Unbind unbinds the USB device from the driver
func (r *Recovery) Unbind() error {
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

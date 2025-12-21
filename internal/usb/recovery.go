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
	defer f.Close()

	if _, err := f.WriteString(r.device); err != nil {
		return errors.Wrap(err, "failed to write to unbind")
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
	defer f.Close()

	if _, err := f.WriteString(r.device); err != nil {
		return errors.Wrap(err, "failed to write to bind")
	}

	r.log("USB device bound, waiting %dms for enumeration...", BindWaitMS)
	time.Sleep(time.Duration(BindWaitMS) * time.Millisecond)

	return nil
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

package gpio

import (
	"context"
	"time"

	"github.com/pkg/errors"
	"github.com/warthog618/go-gpiocdev"
)

// waitCtx sleeps for d unless ctx is cancelled first. Returns ctx.Err() on
// cancellation so callers can abort cleanly during shutdown.
func waitCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

const (
	// GPIO pin configuration (GPIO4.14 = pin 110)
	GPIOChip = "gpiochip3" // GPIO chip 3
	GPIOLine = 14          // GPIO line 14

	// Pulse timing from SIM7100E hardware spec
	ModemOnPulseMS  = 500  // 500ms to turn ON (per SIM7100_Hardware_Design v1.11)
	ModemOffPulseMS = 3500 // 3500ms to turn OFF (increased from 2.5s minimum for safety)

	// Wait time after power off
	ModemOffWaitMS = 12000 // 12 seconds wait after power off
)

// PowerController manages modem power via GPIO
type PowerController struct {
	line   *gpiocdev.Line
	logger func(string, ...interface{})
}

// NewPowerController creates a new GPIO power controller
func NewPowerController(logger func(string, ...interface{})) (*PowerController, error) {
	if logger == nil {
		logger = func(string, ...interface{}) {}
	}

	pc := &PowerController{
		logger: logger,
	}

	return pc, nil
}

// Init initializes the GPIO line
func (pc *PowerController) Init() error {
	// Request the GPIO line as output, initially low
	line, err := gpiocdev.RequestLine(GPIOChip, GPIOLine,
		gpiocdev.AsOutput(0),
		gpiocdev.WithConsumer("modem-power"),
	)
	if err != nil {
		return errors.Wrap(err, "failed to request GPIO line")
	}

	pc.line = line
	pc.log("GPIO power controller initialized (chip=%s, line=%d)", GPIOChip, GPIOLine)
	return nil
}

// Close releases the GPIO line
func (pc *PowerController) Close() error {
	if pc.line == nil {
		return nil
	}

	err := pc.line.Close()
	pc.line = nil
	pc.log("GPIO power controller closed")
	return err
}

// PowerOn sends a power-on pulse to the modem
func (pc *PowerController) PowerOn() error {
	if pc.line == nil {
		return errors.New("GPIO not initialized")
	}

	pc.log("Sending power ON pulse (%dms)...", ModemOnPulseMS)

	// Set high
	if err := pc.line.SetValue(1); err != nil {
		return errors.Wrap(err, "failed to set GPIO high")
	}

	// Hold pulse
	time.Sleep(time.Duration(ModemOnPulseMS) * time.Millisecond)

	// Set low
	if err := pc.line.SetValue(0); err != nil {
		return errors.Wrap(err, "failed to set GPIO low")
	}

	pc.log("Power ON pulse complete")
	return nil
}

// PowerOff sends a power-off pulse to the modem. ctx is used to interrupt
// the 12-second post-pulse wait during shutdown.
func (pc *PowerController) PowerOff(ctx context.Context) error {
	if pc.line == nil {
		return errors.New("GPIO not initialized")
	}

	pc.log("Sending power OFF pulse (%dms)...", ModemOffPulseMS)

	// Set high
	if err := pc.line.SetValue(1); err != nil {
		return errors.Wrap(err, "failed to set GPIO high")
	}

	// Hold pulse (longer for power off). The pulse itself must complete
	// regardless of ctx — interrupting it mid-pulse could leave the modem
	// in an indeterminate state.
	time.Sleep(time.Duration(ModemOffPulseMS) * time.Millisecond)

	// Set low
	if err := pc.line.SetValue(0); err != nil {
		return errors.Wrap(err, "failed to set GPIO low")
	}

	pc.log("Power OFF pulse complete, waiting %dms...", ModemOffWaitMS)

	// Wait for modem to fully power down — interruptible.
	if err := waitCtx(ctx, time.Duration(ModemOffWaitMS)*time.Millisecond); err != nil {
		pc.log("Power OFF wait interrupted: %v", err)
		return err
	}

	pc.log("Power OFF complete")
	return nil
}

// Cycle performs a full power cycle (off then on).
func (pc *PowerController) Cycle(ctx context.Context) error {
	pc.log("Power cycling modem...")

	if err := pc.PowerOff(ctx); err != nil {
		return errors.Wrap(err, "power cycle failed during power off")
	}

	if err := pc.PowerOn(); err != nil {
		return errors.Wrap(err, "power cycle failed during power on")
	}

	pc.log("Power cycle complete")
	return nil
}

func (pc *PowerController) log(format string, args ...interface{}) {
	pc.logger("[GPIO] "+format, args...)
}

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
	GPIOChip = "gpiochip3" // GPIO4.14, physical pin 110
	GPIOLine = 14

	// Pulses satisfy the SIM7100E hardware specification; shutdown uses margin.
	ModemOnPulseMS  = 500
	ModemOffPulseMS = 3500
	ModemOffWaitMS  = 12000
)

// PowerController manages modem power via GPIO.
type PowerController struct {
	line   *gpiocdev.Line
	logger func(string, ...interface{})
}

// NewPowerController creates a GPIO power controller.
func NewPowerController(logger func(string, ...interface{})) (*PowerController, error) {
	if logger == nil {
		logger = func(string, ...interface{}) {}
	}

	pc := &PowerController{
		logger: logger,
	}

	return pc, nil
}

// Init requests the GPIO line as output, initially low.
func (pc *PowerController) Init() error {
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

// Close releases the GPIO line.
func (pc *PowerController) Close() error {
	if pc.line == nil {
		return nil
	}

	err := pc.line.Close()
	pc.line = nil
	pc.log("GPIO power controller closed")
	return err
}

// PowerOn sends the modem's power-on pulse.
func (pc *PowerController) PowerOn() error {
	if pc.line == nil {
		return errors.New("GPIO not initialized")
	}

	pc.log("Sending power ON pulse (%dms)...", ModemOnPulseMS)

	if err := pc.line.SetValue(1); err != nil {
		return errors.Wrap(err, "failed to set GPIO high")
	}

	time.Sleep(time.Duration(ModemOnPulseMS) * time.Millisecond)

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

	if err := pc.line.SetValue(1); err != nil {
		return errors.Wrap(err, "failed to set GPIO high")
	}

	// Never interrupt the pulse itself; partial timing leaves hardware state unknown.
	time.Sleep(time.Duration(ModemOffPulseMS) * time.Millisecond)

	if err := pc.line.SetValue(0); err != nil {
		return errors.Wrap(err, "failed to set GPIO low")
	}

	pc.log("Power OFF pulse complete, waiting %dms...", ModemOffWaitMS)

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

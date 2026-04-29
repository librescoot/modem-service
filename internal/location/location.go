package location

import (
	"context"
	"fmt"
	"log"
	"modem-service/internal/mm"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/stratoberry/go-gpsd"
)

const (
	GPSUpdateInterval          = 1 * time.Second
	CellLocationUpdateInterval = 5 * time.Second
	GPSTimeout                 = 10 * time.Minute
	GPSTimestampStaleness      = 180 * time.Second
	MaxGPSRetries              = 10
	GPSRetryInterval           = 5 * time.Second
	GPSConfigTimeout           = 30 * time.Second
	MaxConfigRetries           = 3

	// gpsWeekRollover is the GPS week-number rollover period (1024 weeks ≈ 19.6 years).
	// SIMCom GPS firmwares with a stale rollover epoch report timestamps this much
	// behind the real time, e.g. April 2026 → April 2006.
	gpsWeekRollover = 1024 * 7 * 24 * time.Hour
)

// GPSMode selects how the receiver acquires satellites.
// Standalone uses satellite signals only. UEBased additionally pulls assistance
// data from a SUPL server (faster TTFF when cellular is available); it falls
// back to standalone automatically when the server is unreachable, so it's
// safe even during brief network loss — but on persistently offline scooters
// we switch explicitly to standalone to avoid futile SUPL attempts.
type GPSMode int

const (
	ModeStandalone GPSMode = iota
	ModeUEBased
)

func (m GPSMode) String() string {
	switch m {
	case ModeStandalone:
		return "standalone"
	case ModeUEBased:
		return "ue-based"
	}
	return fmt.Sprintf("unknown(%d)", int(m))
}

// cgpsArg returns the second argument for AT+CGPS=1,<arg>.
func (m GPSMode) cgpsArg() string {
	if m == ModeUEBased {
		return "2"
	}
	return "1"
}

// MinValidGPSDate is the start of the current GPS week-number rollover epoch
// (2019-04-07). Any GPS timestamp earlier than this is definitively wrong and
// should be corrected by adding multiples of gpsWeekRollover.
var MinValidGPSDate = time.Date(2019, 4, 7, 0, 0, 0, 0, time.UTC)

// age formats a time.Time as a human-readable age relative to now. Returns
// "never" for zero values so log output doesn't show absurd durations.
func age(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return time.Since(t).Round(time.Millisecond).String()
}

// correctGPSWeekRollover compensates for receivers stuck in an older GPS week
// rollover epoch by advancing the timestamp by 1024 weeks until it falls inside
// the current epoch. Returns the (possibly unchanged) timestamp and whether a
// correction was applied.
func correctGPSWeekRollover(t time.Time) (time.Time, bool) {
	if t.IsZero() {
		return t, false
	}
	corrected := t
	for corrected.Before(MinValidGPSDate) {
		corrected = corrected.Add(gpsWeekRollover)
	}
	return corrected, !corrected.Equal(t)
}

type Config struct {
	SuplServer     string
	RefreshRate    time.Duration
	AccuracyThresh float64
	AntennaVoltage float64
}

type Location struct {
	Latitude  float64   `json:"latitude"`
	Longitude float64   `json:"longitude"`
	Altitude  float64   `json:"altitude"`
	Speed     float64   `json:"speed"`
	Course    float64   `json:"course"`
	Timestamp time.Time `json:"timestamp"`
}

type Service struct {
	ModemPath  dbus.ObjectPath
	MMClient   *mm.Client
	Config     Config
	Enabled    bool
	Logger     *log.Logger
	GpsdConn   *gpsd.Session
	GpsdServer string
	Done       chan bool

	// GPS state fields — written by gpsd callbacks, read by monitorStatus.
	// Simple scalars use atomics; compound types (Location, time.Time) use stateMutex.
	hasValidFix   atomic.Bool
	fixMode       atomic.Value // string: "none", "2d", "3d"
	snr           atomic.Value // float64: average SNR of used satellites (dBHz)
	hdop          atomic.Value // float64
	vdop          atomic.Value // float64
	pdop          atomic.Value // float64
	eph           atomic.Value // float64: horizontal position error (m)
	eps           atomic.Value // float64: speed error (m/s)
	ept           atomic.Value // float64: time precision (s)
	satsUsed      atomic.Int32
	satsVisible   atomic.Int32
	gpsdConnected atomic.Bool
	state         atomic.Value // string: "off", "searching", "fix-established", "error"

	// Protected by stateMutex — compound types that can't use atomics
	stateMutex             sync.RWMutex
	currentLoc             Location
	lastFix                time.Time
	lastDataReceived       time.Time // Last time any GPS data was received (even without fix)
	lastGPSTimestamp       time.Time // Last GPS timestamp received from GPSD
	lastGPSTimestampUpdate time.Time // When we last saw the GPS timestamp change

	// Accessed only from the main goroutine or under explicit coordination
	GPSLostTime  time.Time   // Time when GPS fix was lost
	gpsFreshInit atomic.Bool // True if GPS has just been initialized

	configMutex      sync.Mutex    // Protects GPS configuration to prevent concurrent attempts
	currentMode      GPSMode       // Current GPS mode; protected by configMutex
	monitoringActive atomic.Bool   // True if monitoring goroutine is already running
	stopChan         chan struct{} // Signals monitoring goroutine to stop

	closeMu sync.Mutex // Serializes stopChan create/close so Close() can be called concurrently without double-closing

	rolloverLogged sync.Once // Logs GPS week-rollover correction at most once per session
}

func NewService(logger *log.Logger, gpsdServer string, mmClient *mm.Client, suplServer string) *Service {
	if suplServer == "" {
		suplServer = "supl.google.com:7276"
	}

	s := &Service{
		Config: Config{
			SuplServer:     suplServer,
			RefreshRate:    GPSUpdateInterval,
			AccuracyThresh: 50.0,
			AntennaVoltage: 3.05,
		},
		MMClient:   mmClient,
		Logger:     logger,
		GpsdServer: gpsdServer,
		Done:       make(chan bool),
	}
	s.gpsFreshInit.Store(true)
	s.state.Store("off")
	s.fixMode.Store("none")
	s.snr.Store(float64(0))
	s.hdop.Store(float64(0))
	s.vdop.Store(float64(0))
	s.pdop.Store(float64(0))
	s.eph.Store(float64(0))
	s.eps.Store(float64(0))
	s.ept.Store(float64(0))
	return s
}

// Accessor methods for atomic fields

func (s *Service) HasValidFix() bool   { return s.hasValidFix.Load() }
func (s *Service) FixMode() string     { return s.fixMode.Load().(string) }
func (s *Service) SNR() float64        { return s.snr.Load().(float64) }
func (s *Service) HDOP() float64       { return s.hdop.Load().(float64) }
func (s *Service) VDOP() float64       { return s.vdop.Load().(float64) }
func (s *Service) PDOP() float64       { return s.pdop.Load().(float64) }
func (s *Service) EPH() float64        { return s.eph.Load().(float64) }
func (s *Service) EPS() float64        { return s.eps.Load().(float64) }
func (s *Service) EPT() float64        { return s.ept.Load().(float64) }
func (s *Service) SatsUsed() int32     { return s.satsUsed.Load() }
func (s *Service) SatsVisible() int32  { return s.satsVisible.Load() }
func (s *Service) GpsdConnected() bool { return s.gpsdConnected.Load() }
func (s *Service) State() string       { return s.state.Load().(string) }
func (s *Service) GPSFreshInit() bool  { return s.gpsFreshInit.Load() }

func (s *Service) SetGPSFreshInit(v bool) {
	s.gpsFreshInit.Store(v)
}

func (s *Service) CurrentLoc() Location {
	s.stateMutex.RLock()
	defer s.stateMutex.RUnlock()
	return s.currentLoc
}

func (s *Service) LastGPSTimestampUpdate() time.Time {
	s.stateMutex.RLock()
	defer s.stateMutex.RUnlock()
	return s.lastGPSTimestampUpdate
}

func (s *Service) ResetTimestampTracking() {
	s.stateMutex.Lock()
	defer s.stateMutex.Unlock()
	s.lastGPSTimestamp = time.Time{}
	s.lastGPSTimestampUpdate = time.Time{}
	s.lastDataReceived = time.Time{}
}

func (s *Service) SetLastGPSTimestampUpdate(t time.Time) {
	s.stateMutex.Lock()
	defer s.stateMutex.Unlock()
	s.lastGPSTimestampUpdate = t
}

func (s *Service) SetHasValidFix(v bool) {
	s.hasValidFix.Store(v)
}

func (s *Service) EnableGPS(modemPath dbus.ObjectPath) error {
	s.ModemPath = modemPath
	s.Enabled = true

	// Prevent multiple monitoring goroutines from running
	if s.monitoringActive.Load() {
		s.Logger.Printf("GPS monitoring already active, skipping duplicate EnableGPS call")
		return nil
	}

	s.monitoringActive.Store(true)
	s.closeMu.Lock()
	s.stopChan = make(chan struct{})
	stopChan := s.stopChan
	s.closeMu.Unlock()

	go func() {
		defer func() {
			s.monitoringActive.Store(false)
		}()

		attempt := 0
		for {
			// Check both Enabled flag and stop channel for shutdown
			select {
			case <-stopChan:
				return
			default:
			}
			if !s.Enabled {
				return
			}

			if s.GpsdConn == nil {
				s.configMutex.Lock()
				// Double-check after acquiring lock (another goroutine might have configured it)
				if s.GpsdConn == nil {
					// On first attempt, try connecting to gpsd without reconfiguring GPS
					// (GPS might already be running from previous service instance).
					// We require an actual valid fix — not just any TPV — because
					// a no-fix TPV (mode=1) can arrive from a half-configured chip
					// and would wrongly convince us to skip full reconfiguration.
					if attempt == 0 && s.gpsFreshInit.Load() {
						s.Logger.Printf("Trying to connect to existing gpsd...")
						if err := s.connectToGPSD(); err == nil {
							s.Logger.Printf("Connected to gpsd, probing chip state and waiting for valid fix (up to 5s)...")
							s.configMutex.Unlock()

							// Log current chip state so post-hibernation half-configured
							// cases are visible in the journal.
							probeCtx, probeCancel := context.WithTimeout(context.Background(), 3*time.Second)
							s.probeChipState(probeCtx)
							probeCancel()

							// Poll every 500ms for up to 5s — exit early once
							// we see a valid fix instead of always waiting 5s.
							probeDeadline := time.Now().Add(5 * time.Second)
							haveFix := false
							for time.Now().Before(probeDeadline) {
								if s.hasValidFix.Load() {
									haveFix = true
									break
								}
								select {
								case <-stopChan:
									return
								case <-time.After(500 * time.Millisecond):
								}
							}

							s.stateMutex.RLock()
							lastData := s.lastDataReceived
							lastFix := s.lastFix
							s.stateMutex.RUnlock()
							s.Logger.Printf("reuse probe: haveFix=%v lastData=%s lastFix=%s",
								haveFix, age(lastData), age(lastFix))

							if haveFix {
								s.Logger.Printf("GPS already running with valid fix, reusing existing connection")
								// Probe so currentMode reflects the actual
								// modem state instead of a zero-value default.
								s.ProbeGPSMode(context.Background())
								s.gpsFreshInit.Store(false)
								attempt = 0
								continue
							}

							// No valid fix, need to reconfigure
							s.Logger.Printf("No valid fix from existing gpsd, will reconfigure")
							s.configMutex.Lock()
							if s.GpsdConn != nil {
								s.GpsdConn.Close()
								s.GpsdConn = nil
							}
						}
						s.gpsFreshInit.Store(false)
					}

					s.Logger.Printf("Configuring GPS (attempt %d)", attempt+1)

					if err := s.configureGPS(); err != nil {
						s.Logger.Printf("GPS configuration attempt %d failed: %v", attempt+1, err)
						s.configMutex.Unlock()
						select {
						case <-stopChan:
							return
						case <-time.After(GPSRetryInterval):
						}
						attempt++
						continue
					}

					if err := s.connectToGPSD(); err != nil {
						s.Logger.Printf("Failed to connect to gpsd: %v", err)
						s.configMutex.Unlock()
						select {
						case <-stopChan:
							return
						case <-time.After(GPSRetryInterval):
						}
						attempt++
						continue
					}

					s.Logger.Printf("Successfully connected to gpsd")
					attempt = 0
				}
				s.configMutex.Unlock()
			}

			if s.hasValidFix.Load() && func() bool {
				s.stateMutex.RLock()
				defer s.stateMutex.RUnlock()
				return time.Since(s.lastFix) > GPSTimeout
			}() {
				s.Logger.Printf("No GPS updates received for %v, reconnecting", GPSTimeout)
				s.configMutex.Lock()
				if s.GpsdConn != nil {
					s.GpsdConn.Close()
					s.GpsdConn = nil
				}
				s.configMutex.Unlock()
				continue
			}

			select {
			case <-stopChan:
				return
			case <-time.After(GPSRetryInterval):
			}
		}
	}()

	return nil
}

func (s *Service) configureGPS() error {
	if s.MMClient == nil {
		return fmt.Errorf("MMClient not configured")
	}

	// Use timeout context for the entire configuration process
	ctx, cancel := context.WithTimeout(context.Background(), GPSConfigTimeout)
	defer cancel()

	return s.configureGPSWithRetries(ctx)
}

func (s *Service) configureGPSWithRetries(ctx context.Context) error {
	var lastErr error
	for attempt := 0; attempt < MaxConfigRetries; attempt++ {
		if err := s.doGPSConfiguration(ctx); err != nil {
			lastErr = err
			s.Logger.Printf("GPS configuration attempt %d/%d failed: %v", attempt+1, MaxConfigRetries, err)
			if attempt < MaxConfigRetries-1 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(2 * time.Second):
					// Brief pause before retry
				}
			}
			continue
		}
		s.Logger.Printf("GPS configuration successful on attempt %d", attempt+1)
		return nil
	}
	return fmt.Errorf("GPS configuration failed after %d attempts, last error: %v", MaxConfigRetries, lastErr)
}

func (s *Service) doGPSConfiguration(ctx context.Context) error {
	// Configure GPS via AT commands first
	if err := s.configureGPSViaATCommands(ctx); err != nil {
		s.Logger.Printf("Warning: GPS AT config failed: %v", err)
	}

	// Get current ModemManager location status
	status, err := s.getLocationStatusWithTimeout(ctx)
	if err != nil {
		s.Logger.Printf("Warning: Could not get location status: %v", err)
	}

	var enabledSources uint32
	if status != nil {
		enabledSources = status.EnabledSources
		s.Logger.Printf("Location sources enabled: 0x%x", enabledSources)
	}

	// Disable conflicting sources first (gps-nmea and gps-raw)
	if err := s.disableConflictingSources(ctx, enabledSources); err != nil {
		s.Logger.Printf("Warning: Failed to disable conflicting sources: %v", err)
	}

	// Enable required location sources
	if err := s.enableLocationSources(ctx, enabledSources); err != nil {
		return fmt.Errorf("failed to enable location sources: %v", err)
	}

	// Set GPS refresh rate
	if err := s.setGPSRefreshRate(ctx); err != nil {
		s.Logger.Printf("Warning: Failed to set GPS refresh rate: %v", err)
	}

	// Configure antenna power (critical - can reset on reboot)
	if err := s.configureAntennaPower(ctx); err != nil {
		s.Logger.Printf("Warning: Failed to configure antenna power: %v", err)
	}

	return nil
}

// sendATCommand is a helper to send AT commands with context and logging support.
//
// When ctx is cancelled while a command is in flight we return immediately,
// but the inner goroutine keeps running until the underlying D-Bus
// SendCommand call completes (bounded by its 10-second timeout) — there is
// no interruptible D-Bus call on godbus, so we accept that bounded drift.
// The done channel is buffered so the goroutine never blocks on send even
// after the caller has given up, guaranteeing it exits within the timeout.
func (s *Service) sendATCommand(ctx context.Context, command string, logResponse bool) (string, error) {
	done := make(chan struct {
		response string
		err      error
	}, 1)

	go func() {
		response, err := s.MMClient.SendCommand(s.ModemPath, command, 10*time.Second)
		done <- struct {
			response string
			err      error
		}{response, err}
	}()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case result := <-done:
		if result.err != nil {
			return "", result.err
		}
		if logResponse && result.response != "" {
			s.Logger.Printf("%s -> %s", command, result.response)
		}
		return result.response, nil
	}
}

// configureGPSViaATCommands configures GPS using AT commands for optimal performance
// Uses direct modem AT commands for comprehensive GPS setup including antenna power
// and accuracy thresholds for faster and more reliable GPS fixes
func (s *Service) configureGPSViaATCommands(ctx context.Context) error {
	s.Logger.Printf("Configuring GPS via AT commands...")

	// Stop GPS before configuration (ignore errors if not running)
	s.sendATCommand(ctx, "AT+CGPS=0", false)

	// Wait a moment for GPS to stop
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(1 * time.Second):
	}

	// Disable GPS auto-start on boot; mode is set explicitly on each start.
	s.sendATCommand(ctx, "AT+CGPSAUTO=0", false)

	// Set accuracy threshold (50 meters - higher = faster fix)
	accuracyMeters := int(s.Config.AccuracyThresh)
	cmd := fmt.Sprintf("AT+CGPSHOR=%d", accuracyMeters)
	s.sendATCommand(ctx, cmd, false)
	s.Logger.Printf("GPS accuracy threshold: %dm", accuracyMeters)

	// Set GPS antenna GPIO (GPIO 41 as output, high)
	s.sendATCommand(ctx, "AT+CGDRT=41,1", false)
	s.sendATCommand(ctx, "AT+CGSETV=41,1", false)

	// Set GPS clock from system time
	s.syncGPSClock(ctx)

	// Configure NMEA sentence set.
	s.sendATCommand(ctx, "AT+CGPSNMEA=511", false)
	return nil
}

// configureAntennaPower configures the GPS antenna power supply.
// CRITICAL: Can reset to 2950mV after reboot, preventing GPS from working.
// Must be called on every GPS enable, not just initial configuration.
// Caller must hold s.configMutex — this function reads and writes s.currentMode
// without locking, relying on that invariant.
func (s *Service) configureAntennaPower(ctx context.Context) error {
	voltageMillivolts := int(s.Config.AntennaVoltage * 1000)

	// Check current antenna voltage (extract just the +CVAUXV line from response)
	if response, err := s.sendATCommand(ctx, "AT+CVAUXV?", false); err == nil {
		for _, line := range strings.Split(response, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "+CVAUXV:") {
				s.Logger.Printf("Current antenna voltage: %s", strings.TrimSpace(line))
				break
			}
		}
	}

	// Set antenna voltage (3050 = 3.05V for 3V antenna)
	cmd := fmt.Sprintf("AT+CVAUXV=%d", voltageMillivolts)
	if _, err := s.sendATCommand(ctx, cmd, false); err != nil {
		return fmt.Errorf("failed to set antenna voltage: %v", err)
	}

	// Enable antenna power supply
	if _, err := s.sendATCommand(ctx, "AT+CVAUXS=1", false); err != nil {
		return fmt.Errorf("failed to enable antenna power: %v", err)
	}
	s.Logger.Printf("GPS antenna powered: %dmV", voltageMillivolts)

	// Update GPS clock after antenna power up
	s.syncGPSClock(ctx)

	// Check if GPS is enabled, start it in standalone mode if needed.
	// Online scooters will be transitioned to UE-Based by SetGPSMode once the
	// connectivity classifier stabilizes; starting standalone is the safe
	// default that works even if we never reach SUPL.
	response, err := s.sendATCommand(ctx, "AT+CGPS?", false)
	s.Logger.Printf("configureAntennaPower: CGPS?=%q prevMode=%s",
		strings.TrimSpace(response), s.currentMode)
	if err == nil && !gpsRunning(response) {
		s.sendATCommand(ctx, "AT+CGPS=1,1", false)
		s.currentMode = ModeStandalone
		s.Logger.Printf("GPS started in standalone mode")
	} else if err == nil {
		// GPS already running — record the mode we observe so a no-op
		// SetGPSMode(same) doesn't tear it down.
		s.currentMode = parseCGPSMode(response)
	}

	// Set GPS notification mode
	s.sendATCommand(ctx, "AT+CGPSNOTIFY=0", false)

	return nil
}

// gpsRunning returns true when the AT+CGPS? response indicates GPS is on
// in any mode (1,1 standalone / 1,2 UE-based / 1,3 UE-assisted).
func gpsRunning(resp string) bool {
	return strings.Contains(resp, "+CGPS: 1,") || strings.Contains(resp, "+CGPS:1,")
}

// parseCGPSMode extracts the current mode from AT+CGPS? output. Defaults to
// standalone on any parse failure.
func parseCGPSMode(resp string) GPSMode {
	if strings.Contains(resp, "+CGPS: 1,2") || strings.Contains(resp, "+CGPS:1,2") {
		return ModeUEBased
	}
	return ModeStandalone
}

// SetGPSMode reconfigures the GPS for the requested mode. If GPS is already
// running in that mode, it's a no-op. Otherwise GPS is stopped, mode-specific
// AT commands are issued, and GPS is restarted.
//
// Safe to call from a state-change handler; serialized on the same mutex as
// initial configuration via configMutex.
// CurrentGPSMode returns the last-known GPS mode without issuing AT commands.
// This reflects what SetGPSMode set or what configureAntennaPower observed at
// startup; it may lag reality briefly during a mode transition.
func (s *Service) CurrentGPSMode() GPSMode {
	s.configMutex.Lock()
	defer s.configMutex.Unlock()
	return s.currentMode
}

// probeChipState logs the current GPS chip configuration: AT+CGPS? (enabled
// and mode), AT+CVAUXV? (antenna voltage), AT+CVAUXS? (antenna power enable).
// Used to make post-hibernation half-configured states visible in the journal
// when we'd otherwise reuse an existing gpsd connection without reconfiguring.
// Safe to call even if ModemPath isn't set (returns silently).
func (s *Service) probeChipState(ctx context.Context) {
	if s.ModemPath == "" {
		return
	}
	probe := func(label, cmd string) {
		resp, err := s.sendATCommand(ctx, cmd, false)
		if err != nil {
			s.Logger.Printf("chip probe: %s=%s err=%v", label, cmd, err)
			return
		}
		s.Logger.Printf("chip probe: %s -> %s", label, strings.TrimSpace(resp))
	}
	probe("cgps", "AT+CGPS?")
	probe("cvauxv", "AT+CVAUXV?")
	probe("cvauxs", "AT+CVAUXS?")
}

// ProbeGPSMode queries the modem with AT+CGPS? and records whatever mode
// it's currently running. Used at service startup when GPS is already running
// from a previous service instance, so we don't default to a wrong mode.
// Safe to call even if ModemPath isn't set (returns silently).
func (s *Service) ProbeGPSMode(ctx context.Context) {
	if s.ModemPath == "" {
		return
	}
	s.configMutex.Lock()
	defer s.configMutex.Unlock()
	resp, err := s.sendATCommand(ctx, "AT+CGPS?", false)
	if err != nil || !gpsRunning(resp) {
		return
	}
	mode := parseCGPSMode(resp)
	if mode != s.currentMode {
		s.Logger.Printf("GPS mode probe: %s (from modem state)", mode)
		s.currentMode = mode
	}
}

func (s *Service) SetGPSMode(ctx context.Context, mode GPSMode) error {
	s.configMutex.Lock()
	defer s.configMutex.Unlock()

	resp, err := s.sendATCommand(ctx, "AT+CGPS?", false)
	if err == nil && gpsRunning(resp) && parseCGPSMode(resp) == mode {
		s.currentMode = mode
		return nil
	}

	s.Logger.Printf("Switching GPS to %s mode", mode)

	s.sendATCommand(ctx, "AT+CGPS=0", false)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(3 * time.Second):
		// Per AT manual: must wait 2-30s between CGPS=0 and CGPS=1.
	}

	if mode == ModeUEBased {
		s.sendATCommand(ctx, fmt.Sprintf(`AT+CGPSURL="%s"`, s.Config.SuplServer), false)
		s.sendATCommand(ctx, "AT+CGPSSSL=0", false)
		// CGPSMSB=1: fall back to standalone automatically if SUPL becomes
		// unreachable mid-session. Essential for a scooter that drops signal
		// in tunnels and garages.
		s.sendATCommand(ctx, "AT+CGPSMSB=1", false)
	} else {
		// Switching to standalone: the modem rejects AT+CGPS=1,1 (bare
		// ERROR, mode marker stays at 2) if UE-Based session state is
		// still resident. Clear CGPSURL and CGPSMSB first to release it.
		s.sendATCommand(ctx, `AT+CGPSURL=""`, false)
		s.sendATCommand(ctx, "AT+CGPSMSB=0", false)
	}

	// On SIM7100E, AT+CGPS=1,X sometimes reports "Unknown error" even when
	// the mode was actually applied. Re-query to confirm rather than
	// trusting the start command's return value.
	startCmd := fmt.Sprintf("AT+CGPS=1,%s", mode.cgpsArg())
	startErr := (error)(nil)
	if _, err := s.sendATCommand(ctx, startCmd, false); err != nil {
		startErr = err
	}

	verifyResp, verifyErr := s.sendATCommand(ctx, "AT+CGPS?", false)
	if verifyErr != nil {
		if startErr != nil {
			return fmt.Errorf("start %s mode: %v; verify failed: %v", mode, startErr, verifyErr)
		}
		return fmt.Errorf("verify %s mode: %v", mode, verifyErr)
	}
	if !gpsRunning(verifyResp) || parseCGPSMode(verifyResp) != mode {
		return fmt.Errorf("start %s mode failed (CGPS? = %q); start error: %v",
			mode, strings.TrimSpace(verifyResp), startErr)
	}
	if startErr != nil {
		s.Logger.Printf("GPS start command reported %v but CGPS? confirms %s mode", startErr, mode)
	}
	s.currentMode = mode
	// Reset the GPS-timestamp staleness clock. Our CGPS=0 / sleep 3s /
	// CGPS=1,X dance briefly silences gpsd; without this reset, a pre-
	// existing aging timestamp could trip checkGPSHealth's 180s stuck
	// window mid-session and cause a self-inflicted recovery.
	s.SetLastGPSTimestampUpdate(time.Now())
	s.Logger.Printf("GPS running in %s mode", mode)
	return nil
}

// syncGPSClock sets the modem's GPS clock from system time
func (s *Service) syncGPSClock(ctx context.Context) {
	now := time.Now().UTC()
	clockCmd := fmt.Sprintf(`AT+CCLK="%s"`, now.Format("06/01/02,15:04:05+00"))
	s.sendATCommand(ctx, clockCmd, false)
	s.Logger.Printf("GPS clock synced: %s", now.Format("2006-01-02 15:04:05 MST"))
}

// setGPSRefreshRate sets the GPS refresh rate via ModemManager D-Bus
func (s *Service) setGPSRefreshRate(ctx context.Context) error {
	// Set GPS refresh rate to 1 second (matches GPSUpdateInterval)
	refreshSeconds := uint32(s.Config.RefreshRate.Seconds())

	if err := s.MMClient.SetGPSRefreshRate(s.ModemPath, refreshSeconds); err != nil {
		return fmt.Errorf("failed to set GPS refresh rate to %ds: %v", refreshSeconds, err)
	}
	s.Logger.Printf("Set GPS refresh rate to %d second(s)", refreshSeconds)
	return nil
}

// LocationStatus holds the location configuration status
type LocationStatus struct {
	EnabledSources uint32
}

func (s *Service) getLocationStatusWithTimeout(ctx context.Context) (*LocationStatus, error) {
	type result struct {
		status *LocationStatus
		err    error
	}

	done := make(chan result, 1)

	go func() {
		enabled, err := s.MMClient.GetEnabledLocationSources(s.ModemPath)
		if err != nil {
			done <- result{nil, err}
			return
		}

		done <- result{&LocationStatus{
			EnabledSources: enabled,
		}, nil}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-done:
		return r.status, r.err
	}
}

func (s *Service) disableConflictingSources(ctx context.Context, enabledSources uint32) error {
	// Check if conflicting sources (gps-nmea or gps-raw) are enabled
	conflictingMask := mm.MMModemLocationSourceGpsNmea | mm.MMModemLocationSourceGpsRaw

	if enabledSources&conflictingMask != 0 {
		// Calculate new sources mask without conflicting sources
		newSources := enabledSources &^ conflictingMask
		s.Logger.Printf("Disabling conflicting GPS sources (nmea/raw), new mask: 0x%x", newSources)

		if err := s.MMClient.SetupLocation(s.ModemPath, newSources, false); err != nil {
			return fmt.Errorf("failed to disable conflicting sources: %v", err)
		}
	}
	return nil
}

func (s *Service) enableLocationSources(ctx context.Context, currentSources uint32) error {
	// Required sources: 3gpp-lac-ci (cell-tower fallback), gps-unmanaged (we drive GPS via AT).
	// agps-msb is not included — the SimTech MM plugin does not act on it, and
	// we configure SUPL ourselves via AT commands when online.
	requiredSources := mm.MMModemLocationSource3gppLacCi |
		mm.MMModemLocationSourceGpsUnmanaged

	// Check which sources need to be enabled
	missingSourcesDisplay := []string{}
	if currentSources&mm.MMModemLocationSource3gppLacCi == 0 {
		missingSourcesDisplay = append(missingSourcesDisplay, "3gpp-lac-ci")
	}
	if currentSources&mm.MMModemLocationSourceGpsUnmanaged == 0 {
		missingSourcesDisplay = append(missingSourcesDisplay, "gps-unmanaged")
	}

	// Calculate final sources (current + required, minus conflicting)
	conflictingMask := mm.MMModemLocationSourceGpsNmea | mm.MMModemLocationSourceGpsRaw
	finalSources := (currentSources | requiredSources) &^ conflictingMask

	if len(missingSourcesDisplay) > 0 {
		s.Logger.Printf("Enabling location sources: %s", strings.Join(missingSourcesDisplay, ", "))

		// Try enabling with retries
		var enableErr error
		for attempt := 0; attempt < 3; attempt++ {
			enableErr = s.MMClient.SetupLocation(s.ModemPath, finalSources, false)
			if enableErr == nil {
				s.Logger.Printf("Location sources enabled successfully")
				break
			}
			s.Logger.Printf("Warning: Failed to enable sources (attempt %d/3): %v", attempt+1, enableErr)

			// Brief pause before retry
			if attempt < 2 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(1 * time.Second):
				}
			}
		}

		if enableErr != nil {
			return fmt.Errorf("failed to enable location sources after 3 attempts: %v", enableErr)
		}
	} else {
		s.Logger.Printf("Required location sources already configured")
	}

	// Wait 3 seconds after enabling GPS before restarting gpsd
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(3 * time.Second):
	}

	s.Logger.Printf("Restarting gpsd service after GPS configuration")
	restartCmd := exec.CommandContext(ctx, "systemctl", "restart", "gpsd")
	if err := restartCmd.Run(); err != nil {
		s.Logger.Printf("Warning: Failed to restart gpsd: %v", err)
	} else {
		s.Logger.Printf("Successfully restarted gpsd service")
	}

	return nil
}

func (s *Service) connectToGPSD() error {
	if s.GpsdConn != nil {
		s.Logger.Printf("Closing existing gpsd connection")
		s.GpsdConn.Close()
		s.GpsdConn = nil
		// Wait for the prior Watch goroutine to exit before overwriting
		// s.Done on reconnect — otherwise it leaks one goroutine per cycle.
		if s.Done != nil {
			select {
			case <-s.Done:
			case <-time.After(2 * time.Second):
				s.Logger.Printf("Warning: prior gpsd Watch goroutine did not exit within 2s")
			}
		}
	}

	s.Logger.Printf("Connecting to gpsd on %s", s.GpsdServer)
	conn, err := gpsd.Dial(s.GpsdServer)
	if err != nil {
		return fmt.Errorf("failed to connect to gpsd: %v", err)
	}
	if conn == nil {
		return fmt.Errorf("failed to connect to gpsd")
	}

	s.GpsdConn = conn

	// Subscribe to SKY reports for DOP values
	s.GpsdConn.AddFilter("SKY", func(r interface{}) {
		report, ok := r.(*gpsd.SKYReport)
		if !ok {
			s.Logger.Printf("Error: Could not cast SKY report")
			return
		}

		s.hdop.Store(report.Hdop)
		s.vdop.Store(report.Vdop)
		s.pdop.Store(report.Pdop)
		if len(report.Satellites) > 0 {
			var used int32
			var usedSnrSum, visSnrSum float64
			var visCount int
			for _, sat := range report.Satellites {
				if sat.Used {
					used++
					usedSnrSum += sat.Ss
				}
				if sat.Ss > 0 {
					visSnrSum += sat.Ss
					visCount++
				}
			}
			s.satsUsed.Store(used)
			s.satsVisible.Store(int32(len(report.Satellites)))
			// Prefer the average SNR over satellites used in the fix; if
			// none are used (typical during search), fall back to the
			// average over visible birds with measurable signal so the
			// reading stays live instead of sticking at the last fix's
			// value indefinitely.
			switch {
			case used > 0:
				s.snr.Store(usedSnrSum / float64(used))
			case visCount > 0:
				s.snr.Store(visSnrSum / float64(visCount))
			default:
				s.snr.Store(float64(0))
			}
		}
	})

	s.GpsdConn.AddFilter("TPV", func(r interface{}) {
		report, ok := r.(*gpsd.TPVReport)
		if !ok {
			s.Logger.Printf("Error: Could not cast TPV report")
			s.state.Store("error")
			return
		}

		// Track when we receive any GPS data (even without fix)
		s.stateMutex.Lock()
		s.lastDataReceived = time.Now()
		s.stateMutex.Unlock()

		// gpsd Mode==0 (NoValueSeen) means the report carries no mode/fix
		// info — typically a cycle-continuation TPV or a partial JSON update
		// where the mode field wasn't repeated. Treating it as "no fix" (as
		// we used to) caused spurious fix-loss flicker because every such
		// TPV would flip hasValidFix=false until the next mode==3 arrived,
		// taking the no-fix publish branch with it. Preserve state instead:
		// no field updates, no fix-state change.
		if report.Mode == 0 {
			return
		}

		// Update fix status; log mode transitions so silent "stuck at mode=1"
		// windows are visible even before a valid fix is ever established.
		prevMode := s.fixMode.Load().(string)
		var newMode string
		switch report.Mode {
		case 1:
			newMode = "none"
			s.state.Store("searching")
		case 2:
			newMode = "2d"
			s.state.Store("fix-established")
		case 3:
			newMode = "3d"
			s.state.Store("fix-established")
		}
		if newMode != "" {
			s.fixMode.Store(newMode)
			if newMode != prevMode {
				s.Logger.Printf("tpv mode transition: %s -> %s (raw=%d)", prevMode, newMode, report.Mode)
			}
		}

		// Update error estimates from TPV report
		s.eph.Store(report.Eph)
		s.eps.Store(report.Eps)
		s.ept.Store(report.Ept)

		if report.Mode == 1 {
			if s.hasValidFix.Swap(false) {
				s.Logger.Printf("GPS fix lost: tpv mode=%d", report.Mode)
			}
			return
		}

		s.stateMutex.Lock()
		prevLoc := s.currentLoc
		s.stateMutex.Unlock()

		rawLocation := Location{
			Latitude:  report.Lat,
			Longitude: report.Lon,
			Altitude:  prevLoc.Altitude,
			Speed:     prevLoc.Speed,
			Course:    prevLoc.Course,
		}

		if report.Alt != 0 {
			rawLocation.Altitude = report.Alt
		}
		if report.Speed != 0 {
			rawLocation.Speed = report.Speed
		}
		if report.Track != 0 {
			rawLocation.Course = report.Track
		}

		if !report.Time.IsZero() {
			gpsTime, corrected := correctGPSWeekRollover(report.Time)
			if corrected {
				s.rolloverLogged.Do(func() {
					s.Logger.Printf("GPS week-rollover correction active: receiver reports %s, using %s",
						report.Time.Format(time.RFC3339),
						gpsTime.Format(time.RFC3339))
				})
			}
			rawLocation.Timestamp = gpsTime

			// Track GPS timestamp changes (use corrected time so staleness
			// detection still works after rollover compensation).
			s.stateMutex.Lock()
			if s.lastGPSTimestamp.IsZero() || !gpsTime.Equal(s.lastGPSTimestamp) {
				s.lastGPSTimestamp = gpsTime
				s.lastGPSTimestampUpdate = time.Now()
			}
			s.stateMutex.Unlock()
		} else {
			rawLocation.Timestamp = time.Now()
		}

		// Store raw location directly (no filtering)
		s.stateMutex.Lock()
		s.currentLoc = rawLocation
		s.lastFix = time.Now()
		s.stateMutex.Unlock()
		s.hasValidFix.Store(true)
	})

	// Track gpsd connection state
	s.gpsdConnected.Store(true)

	s.Done = s.GpsdConn.Watch()

	return nil
}

func (s *Service) StopGPSD() error {
	cmd := exec.Command("systemctl", "stop", "gpsd")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to stop gpsd: %v", err)
	}
	s.Logger.Printf("Successfully stopped gpsd service")
	return nil
}

func (s *Service) Close() {
	s.closeMu.Lock()
	if s.stopChan != nil {
		close(s.stopChan)
		s.stopChan = nil
	}
	s.closeMu.Unlock()

	s.Enabled = false
	// Mark disconnected first so concurrent GetGPSStatus() never observes
	// state="off" while connected=true.
	s.gpsdConnected.Store(false)
	s.state.Store("off")
	s.configMutex.Lock()
	if s.GpsdConn != nil {
		s.GpsdConn.Close()
		s.GpsdConn = nil
	}
	s.configMutex.Unlock()
}

func (s *Service) GetGPSStatus() map[string]interface{} {
	return map[string]interface{}{
		"fix":                s.FixMode(),
		"snr":                s.SNR(),
		"hdop":               s.HDOP(),
		"vdop":               s.VDOP(),
		"pdop":               s.PDOP(),
		"eph":                s.EPH(),
		"eps":                s.EPS(),
		"ept":                s.EPT(),
		"satellites-used":    s.SatsUsed(),
		"satellites-visible": s.SatsVisible(),
		"active":             s.HasValidFix(),
		"connected":          s.GpsdConnected(),
		"state":              s.State(),
	}
}

// ShouldPublishRecovery determines if GPS recovery notification should be published.
// This happens when GPS becomes available after being unavailable, and only if the
// outage was significant (>5 minutes) or it's the first fix after initialization.
func (s *Service) ShouldPublishRecovery(hasInternetConnection bool) bool {
	if !hasInternetConnection {
		return false
	}

	if s.gpsFreshInit.Load() {
		return true
	}

	if !s.GPSLostTime.IsZero() {
		duration := time.Since(s.GPSLostTime)
		return duration > 5*time.Minute
	}

	return false
}

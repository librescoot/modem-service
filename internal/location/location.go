package location

import (
	"context"
	"fmt"
	"log"
	"modem-service/internal/mm"
	"os/exec"
	"strconv"
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
	MaxGPSRetries              = 10
	GPSRetryInterval           = 5 * time.Second
	MaxGPSRetryInterval        = 60 * time.Second
	GPSConfigTimeout           = 30 * time.Second
	MaxConfigRetries           = 3

	requiredAntennaVoltageMillivolts = 3050
	requiredGPSPowerMode             = 7
	gpsRestartMinimumInterval        = 2 * time.Second
	gpsStopConfirmationTimeout       = 10 * time.Second
	gpsStopPollInterval              = 100 * time.Millisecond
	startupReuseProbeTimeout         = 5 * time.Second
	locationSourceRestartDelay       = 3 * time.Second

	// gpsWeekRollover is the GPS week-number rollover period (1024 weeks ≈ 19.6 years).
	// SIMCom GPS firmwares with a stale rollover epoch report timestamps this much
	// behind the real time, e.g. April 2026 → April 2006.
	gpsWeekRollover = 1024 * 7 * 24 * time.Hour
)

// GPSMode selects standalone or SUPL-assisted satellite acquisition.
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

func configRetryDelay(attempt int) time.Duration {
	delay := GPSRetryInterval
	for range attempt {
		delay *= 2
		if delay >= MaxGPSRetryInterval {
			return MaxGPSRetryInterval
		}
	}
	return delay
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

// ModemPathResolver finds the current modem after ModemManager rebinds it.
type ModemPathResolver func() (dbus.ObjectPath, error)

type Service struct {
	ModemPath        dbus.ObjectPath
	ResolveModemPath ModemPathResolver
	MMClient         *mm.Client
	Config           Config
	Logger           *log.Logger
	GpsdConn         *gpsd.Session
	GpsdServer       string
	Done             chan bool

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

	stateMutex       sync.RWMutex
	currentLoc       Location
	lastFix          time.Time
	lastDataReceived time.Time // includes reports without a fix

	GPSLostTime  time.Time // owned by the service monitor goroutine
	gpsFreshInit atomic.Bool

	configMutex sync.Mutex
	currentMode GPSMode

	lifecycleMu      sync.Mutex
	monitorCtx       context.Context
	monitorCancel    context.CancelFunc
	monitorDone      chan struct{}
	monitoringActive atomic.Bool
	modeSwitchReady  atomic.Bool
	beforeConfigure  func()
	sendATCommandFn  func(context.Context, string) (string, error)
	nowFn            func() time.Time
	waitFn           func(context.Context, time.Duration) error

	rolloverLogged sync.Once
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

func (s *Service) HasValidFix() bool        { return s.hasValidFix.Load() }
func (s *Service) FixMode() string          { return s.fixMode.Load().(string) }
func (s *Service) SNR() float64             { return s.snr.Load().(float64) }
func (s *Service) HDOP() float64            { return s.hdop.Load().(float64) }
func (s *Service) VDOP() float64            { return s.vdop.Load().(float64) }
func (s *Service) PDOP() float64            { return s.pdop.Load().(float64) }
func (s *Service) EPH() float64             { return s.eph.Load().(float64) }
func (s *Service) EPS() float64             { return s.eps.Load().(float64) }
func (s *Service) EPT() float64             { return s.ept.Load().(float64) }
func (s *Service) SatsUsed() int32          { return s.satsUsed.Load() }
func (s *Service) SatsVisible() int32       { return s.satsVisible.Load() }
func (s *Service) GpsdConnected() bool      { return s.gpsdConnected.Load() }
func (s *Service) State() string            { return s.state.Load().(string) }
func (s *Service) GPSFreshInit() bool       { return s.gpsFreshInit.Load() }
func (s *Service) IsEnabled() bool          { return s.monitoringActive.Load() }
func (s *Service) ReadyForModeSwitch() bool { return s.modeSwitchReady.Load() }

func (s *Service) IsConfiguring() bool {
	if s.configMutex.TryLock() {
		s.configMutex.Unlock()
		return false
	}
	return true
}

func (s *Service) SetGPSFreshInit(v bool) {
	s.gpsFreshInit.Store(v)
}

func (s *Service) CurrentLoc() Location {
	s.stateMutex.RLock()
	defer s.stateMutex.RUnlock()
	return s.currentLoc
}

func (s *Service) LastDataReceived() time.Time {
	s.stateMutex.RLock()
	defer s.stateMutex.RUnlock()
	return s.lastDataReceived
}

func (s *Service) SetLastDataReceived(t time.Time) {
	s.stateMutex.Lock()
	defer s.stateMutex.Unlock()
	s.lastDataReceived = t
}

func (s *Service) SetHasValidFix(v bool) {
	s.hasValidFix.Store(v)
}

func (s *Service) EnableGPS(modemPath dbus.ObjectPath) error {
	s.lifecycleMu.Lock()
	if s.monitorCancel != nil {
		s.lifecycleMu.Unlock()
		s.Logger.Printf("GPS monitoring already active, skipping duplicate EnableGPS call")
		return nil
	}
	s.configMutex.Lock()
	s.ModemPath = modemPath
	s.configMutex.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.monitorCtx = ctx
	s.monitorCancel = cancel
	s.monitorDone = done
	s.modeSwitchReady.Store(false)
	s.monitoringActive.Store(true)
	s.lifecycleMu.Unlock()

	go func() {
		defer func() {
			s.modeSwitchReady.Store(false)
			s.monitoringActive.Store(false)
			close(done)
		}()

		attempt := 0
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			if s.GpsdConn == nil {
				if s.beforeConfigure != nil {
					s.beforeConfigure()
				}
				s.configMutex.Lock()
				if s.GpsdConn == nil {
					if attempt == 0 && s.gpsFreshInit.Load() {
						probeStarted := time.Now()
						s.Logger.Printf("GPS startup reuse probe started")
						reuse, reason := s.probeStartupReuse(ctx, probeStarted)
						elapsed := time.Since(probeStarted).Round(time.Millisecond)
						if reuse {
							s.currentMode = ModeStandalone
							s.gpsFreshInit.Store(false)
							s.modeSwitchReady.Store(true)
							s.Logger.Printf("GPS startup reuse accepted after %s: %s", elapsed, reason)
							attempt = 0
							s.configMutex.Unlock()
							continue
						}
						if s.GpsdConn != nil {
							s.GpsdConn.Close()
							s.GpsdConn = nil
							s.gpsdConnected.Store(false)
						}
						s.gpsFreshInit.Store(false)
						s.Logger.Printf("GPS startup reconfiguration required after %s: %s", elapsed, reason)
					}

					s.Logger.Printf("Configuring GPS (attempt %d)", attempt+1)

					err := s.configureGPS(ctx)
					if err != nil {
						s.Logger.Printf("GPS configuration attempt %d failed: %v", attempt+1, err)
						s.configMutex.Unlock()
						select {
						case <-ctx.Done():
							return
						case <-time.After(configRetryDelay(attempt)):
						}
						attempt++
						continue
					}

					if err := s.connectToGPSD(); err != nil {
						s.Logger.Printf("Failed to connect to gpsd: %v", err)
						s.configMutex.Unlock()
						select {
						case <-ctx.Done():
							return
						case <-time.After(configRetryDelay(attempt)):
						}
						attempt++
						continue
					}

					s.SetLastDataReceived(time.Now())
					s.modeSwitchReady.Store(true)
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
				s.modeSwitchReady.Store(false)
				s.configMutex.Lock()
				if s.GpsdConn != nil {
					s.GpsdConn.Close()
					s.GpsdConn = nil
				}
				s.configMutex.Unlock()
				continue
			}

			select {
			case <-ctx.Done():
				return
			case <-time.After(GPSRetryInterval):
			}
		}
	}()

	return nil
}

func (s *Service) configureGPS(parent context.Context) error {
	if s.MMClient == nil {
		return fmt.Errorf("MMClient not configured")
	}

	ctx, cancel := context.WithTimeout(parent, GPSConfigTimeout)
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
	started := time.Now()
	if err := s.configureReceiver(ctx); err != nil {
		return err
	}
	s.Logger.Printf("GPS initialization phase=receiver-running elapsed=%s",
		time.Since(started).Round(time.Millisecond))

	status, err := s.getLocationStatusWithTimeout(ctx)
	if err != nil {
		s.Logger.Printf("Warning: Could not get location status: %v", err)
	}

	var enabledSources uint32
	if status != nil {
		enabledSources = status.EnabledSources
		s.Logger.Printf("Location sources enabled: 0x%x", enabledSources)
	}

	if err := s.disableConflictingSources(ctx, enabledSources); err != nil {
		s.Logger.Printf("Warning: Failed to disable conflicting sources: %v", err)
	}

	if err := s.enableLocationSources(ctx, enabledSources, started); err != nil {
		return fmt.Errorf("failed to enable location sources: %v", err)
	}

	if err := s.setGPSRefreshRate(ctx); err != nil {
		s.Logger.Printf("Warning: Failed to set GPS refresh rate: %v", err)
	}

	return nil
}

// isStalePathError reports whether err is the ModemManager-rebind signature:
// "Object does not exist at path". Comes through godbus as the message
// portion of an org.freedesktop.DBus.Error.UnknownObject error.
func isStalePathError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Object does not exist at path") ||
		strings.Contains(msg, "UnknownObject")
}

// refreshModemPathIfStale updates a rebound modem path and requests one retry.
func (s *Service) refreshModemPathIfStale(err error) bool {
	if !isStalePathError(err) || s.ResolveModemPath == nil {
		return false
	}
	newPath, rerr := s.ResolveModemPath()
	if rerr != nil || newPath == s.ModemPath {
		return false
	}
	s.Logger.Printf("ModemManager rebind detected: modem path %s -> %s", s.ModemPath, newPath)
	s.ModemPath = newPath
	return true
}

func (s *Service) ensureModemPath() error {
	if s.ModemPath != "" {
		return nil
	}
	if s.ResolveModemPath == nil {
		return fmt.Errorf("modem path unavailable")
	}
	path, err := s.ResolveModemPath()
	if err != nil {
		return fmt.Errorf("resolve modem path: %w", err)
	}
	if path == "" {
		return fmt.Errorf("modem path unavailable")
	}
	s.ModemPath = path
	return nil
}

// sendATCommand retries once if ModemManager rebound the modem's object path.
func (s *Service) sendATCommand(ctx context.Context, command string, logResponse bool) (string, error) {
	if err := s.ensureModemPath(); err != nil {
		return "", err
	}
	send := func() (string, error) {
		if s.sendATCommandFn != nil {
			return s.sendATCommandFn(ctx, command)
		}
		return s.MMClient.SendCommandContext(ctx, s.ModemPath, command, 10*time.Second)
	}

	response, err := send()
	if err != nil && s.refreshModemPathIfStale(err) {
		response, err = send()
	}
	if err != nil {
		return "", err
	}
	if logResponse && response != "" {
		s.Logger.Printf("%s -> %s", command, response)
	}
	return response, nil
}

func (s *Service) now() time.Time {
	if s.nowFn != nil {
		return s.nowFn()
	}
	return time.Now()
}

func (s *Service) waitUntil(ctx context.Context, notBefore time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	remaining := notBefore.Sub(s.now())
	if remaining <= 0 {
		return nil
	}
	if s.waitFn != nil {
		return s.waitFn(ctx, remaining)
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *Service) configureReceiver(ctx context.Context) error {
	restartNotBefore, err := s.configureGPSViaATCommands(ctx)
	if err != nil {
		return fmt.Errorf("GPS AT config: %w", err)
	}
	if err := s.configureAntennaPower(ctx, restartNotBefore); err != nil {
		return fmt.Errorf("configure antenna and receiver: %w", err)
	}
	return nil
}

// configureGPSViaATCommands applies settings that require a stopped receiver.
func (s *Service) configureGPSViaATCommands(ctx context.Context) (time.Time, error) {
	s.Logger.Printf("Configuring GPS via AT commands...")

	stopIssuedAt := s.now()
	restartNotBefore := stopIssuedAt.Add(gpsRestartMinimumInterval)
	stopDeadline := stopIssuedAt.Add(gpsStopConfirmationTimeout)
	_, stopErr := s.sendATCommand(ctx, "AT+CGPS=0", false)

	if err := s.waitForReceiverStopped(ctx, stopDeadline); err != nil {
		if stopErr != nil {
			return restartNotBefore, fmt.Errorf("stop receiver: %v; verify: %w", stopErr, err)
		}
		return restartNotBefore, err
	}
	if stopErr != nil {
		s.Logger.Printf("GPS stop command reported %v but CGPS? confirms receiver stopped", stopErr)
	}
	if err := s.configureGPSPowerMode(ctx); err != nil {
		return restartNotBefore, err
	}

	// Disable GPS auto-start on boot; mode is set explicitly on each start.
	s.sendATCommand(ctx, "AT+CGPSAUTO=0", false)

	accuracyMeters := int(s.Config.AccuracyThresh)
	cmd := fmt.Sprintf("AT+CGPSHOR=%d", accuracyMeters)
	s.sendATCommand(ctx, cmd, false)
	s.Logger.Printf("GPS accuracy threshold: %dm", accuracyMeters)

	s.sendATCommand(ctx, "AT+CGDRT=41,1", false)
	s.sendATCommand(ctx, "AT+CGSETV=41,1", false)

	s.syncGPSClock(ctx)

	// Configure NMEA sentence set.
	s.sendATCommand(ctx, "AT+CGPSNMEA=511", false)
	return restartNotBefore, nil
}

func (s *Service) waitForReceiverStopped(ctx context.Context, deadline time.Time) error {
	for {
		response, err := s.sendATCommand(ctx, "AT+CGPS?", false)
		if err != nil {
			return fmt.Errorf("verify stopped receiver: %w", err)
		}
		running, _, ok := parseCGPSResponse(response)
		if !ok {
			return fmt.Errorf("verify stopped receiver: malformed CGPS? response %q", strings.TrimSpace(response))
		}
		if !running {
			return nil
		}
		now := s.now()
		if !now.Before(deadline) {
			return fmt.Errorf("receiver did not stop before restart deadline (CGPS? = %q)", strings.TrimSpace(response))
		}
		nextPoll := now.Add(gpsStopPollInterval)
		if nextPoll.After(deadline) {
			nextPoll = deadline
		}
		if err := s.waitUntil(ctx, nextPoll); err != nil {
			return fmt.Errorf("wait for receiver to stop: %w", err)
		}
	}
}

func (s *Service) configureGPSPowerMode(ctx context.Context) error {
	if _, err := s.sendATCommand(ctx, "AT+CGPSPMD=7", false); err != nil {
		return fmt.Errorf("set CGPSPMD=7: %w", err)
	}
	response, err := s.sendATCommand(ctx, "AT+CGPSPMD?", false)
	if err != nil {
		return fmt.Errorf("verify CGPSPMD=7: %w", err)
	}
	mode, ok := parseSingleValueResponse(response, "+CGPSPMD:")
	if !ok || mode != requiredGPSPowerMode {
		return fmt.Errorf("verify CGPSPMD=7: response %q", strings.TrimSpace(response))
	}
	return nil
}

// configureAntennaPower restores the required 3.05 V after every reboot.
// The caller must hold configMutex because this also updates currentMode.
func (s *Service) configureAntennaPower(ctx context.Context, restartNotBefore time.Time) error {
	voltageMillivolts := int(s.Config.AntennaVoltage * 1000)

	if response, err := s.sendATCommand(ctx, "AT+CVAUXV?", false); err == nil {
		for _, line := range strings.Split(response, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "+CVAUXV:") {
				s.Logger.Printf("Current antenna voltage: %s", strings.TrimSpace(line))
				break
			}
		}
	}

	cmd := fmt.Sprintf("AT+CVAUXV=%d", voltageMillivolts)
	if _, err := s.sendATCommand(ctx, cmd, false); err != nil {
		return fmt.Errorf("failed to set antenna voltage: %v", err)
	}

	if _, err := s.sendATCommand(ctx, "AT+CVAUXS=1", false); err != nil {
		return fmt.Errorf("failed to enable antenna power: %v", err)
	}
	s.Logger.Printf("GPS antenna powered: %dmV", voltageMillivolts)

	s.syncGPSClock(ctx)

	if err := s.startStandaloneReceiver(ctx, restartNotBefore); err != nil {
		return err
	}

	s.sendATCommand(ctx, "AT+CGPSNOTIFY=0", false)
	return nil
}

func (s *Service) startStandaloneReceiver(ctx context.Context, restartNotBefore time.Time) error {
	if err := s.waitUntil(ctx, restartNotBefore); err != nil {
		return fmt.Errorf("wait for receiver restart interval: %w", err)
	}
	_, startErr := s.sendATCommand(ctx, "AT+CGPS=1,1", false)
	response, queryErr := s.sendATCommand(ctx, "AT+CGPS?", false)
	if queryErr != nil {
		if startErr != nil {
			return fmt.Errorf("start standalone receiver: %v; verify: %w", startErr, queryErr)
		}
		return fmt.Errorf("verify standalone receiver: %w", queryErr)
	}
	running, mode, ok := parseCGPSResponse(response)
	if !ok || !running || mode != ModeStandalone {
		return fmt.Errorf("start standalone receiver failed (CGPS? = %q; start error: %v)",
			strings.TrimSpace(response), startErr)
	}
	if startErr != nil {
		s.Logger.Printf("GPS start command reported %v but CGPS? confirms standalone mode", startErr)
	}
	s.currentMode = ModeStandalone
	s.Logger.Printf("GPS started in standalone mode")
	return nil
}

func parseCGPSResponse(response string) (running bool, mode GPSMode, ok bool) {
	values, ok := parseIntegerResponse(response, "+CGPS:")
	if !ok || len(values) == 0 {
		return false, ModeStandalone, false
	}
	if values[0] == 0 {
		return false, ModeStandalone, true
	}
	if values[0] != 1 || len(values) < 2 {
		return false, ModeStandalone, false
	}
	switch values[1] {
	case 1:
		return true, ModeStandalone, true
	case 2:
		return true, ModeUEBased, true
	default:
		return true, ModeStandalone, false
	}
}

func parseSingleValueResponse(response, prefix string) (int, bool) {
	values, ok := parseIntegerResponse(response, prefix)
	if !ok || len(values) != 1 {
		return 0, false
	}
	return values[0], true
}

func parseIntegerResponse(response, prefix string) ([]int, bool) {
	for _, line := range strings.Split(strings.ReplaceAll(response, "\r", ""), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Split(strings.TrimSpace(strings.TrimPrefix(line, prefix)), ",")
		values := make([]int, 0, len(fields))
		for _, field := range fields {
			value, err := strconv.Atoi(strings.TrimSpace(field))
			if err != nil {
				return nil, false
			}
			values = append(values, value)
		}
		return values, len(values) > 0
	}
	return nil, false
}

func gpsRunning(resp string) bool {
	running, _, ok := parseCGPSResponse(resp)
	return ok && running
}

func parseCGPSMode(resp string) GPSMode {
	_, mode, _ := parseCGPSResponse(resp)
	return mode
}

// CurrentGPSMode returns the last mode set or observed during configuration.
func (s *Service) CurrentGPSMode() GPSMode {
	s.configMutex.Lock()
	defer s.configMutex.Unlock()
	return s.currentMode
}

type gpsChipState struct {
	running           bool
	mode              GPSMode
	antennaMillivolts int
	powerMode         int
}

func parseGPSChipState(cgps, cvauxv, cgpspmd string) (gpsChipState, error) {
	running, mode, ok := parseCGPSResponse(cgps)
	if !ok {
		return gpsChipState{}, fmt.Errorf("malformed CGPS response %q", strings.TrimSpace(cgps))
	}
	voltage, ok := parseSingleValueResponse(cvauxv, "+CVAUXV:")
	if !ok {
		return gpsChipState{}, fmt.Errorf("malformed CVAUXV response %q", strings.TrimSpace(cvauxv))
	}
	powerMode, ok := parseSingleValueResponse(cgpspmd, "+CGPSPMD:")
	if !ok {
		return gpsChipState{}, fmt.Errorf("malformed CGPSPMD response %q", strings.TrimSpace(cgpspmd))
	}
	return gpsChipState{
		running:           running,
		mode:              mode,
		antennaMillivolts: voltage,
		powerMode:         powerMode,
	}, nil
}

func (state gpsChipState) reuseEligibility() (bool, string) {
	if !state.running {
		return false, "receiver is stopped"
	}
	if state.mode != ModeStandalone {
		return false, fmt.Sprintf("receiver mode is %s", state.mode)
	}
	if state.antennaMillivolts != requiredAntennaVoltageMillivolts {
		return false, fmt.Sprintf("antenna voltage is %dmV, want %dmV",
			state.antennaMillivolts, requiredAntennaVoltageMillivolts)
	}
	if state.powerMode != requiredGPSPowerMode {
		return false, fmt.Sprintf("CGPSPMD is %d, want %d", state.powerMode, requiredGPSPowerMode)
	}
	return true, fmt.Sprintf("chip state is standalone, antenna=%dmV, CGPSPMD=%d",
		state.antennaMillivolts, state.powerMode)
}

func (s *Service) queryGPSChipState(ctx context.Context) (gpsChipState, error) {
	responses := make([]string, 3)
	for i, command := range []string{"AT+CGPS?", "AT+CVAUXV?", "AT+CGPSPMD?"} {
		response, err := s.sendATCommand(ctx, command, false)
		if err != nil {
			return gpsChipState{}, fmt.Errorf("%s failed: %w", command, err)
		}
		responses[i] = response
	}
	return parseGPSChipState(responses[0], responses[1], responses[2])
}

// probeStartupReuse accepts live receiver state without requiring a fix.
func (s *Service) probeStartupReuse(ctx context.Context, started time.Time) (bool, string) {
	if err := s.connectToGPSD(); err != nil {
		return false, fmt.Sprintf("gpsd connection failed: %v", err)
	}

	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	state, err := s.queryGPSChipState(probeCtx)
	cancel()
	if err != nil {
		return false, fmt.Sprintf("chip state unavailable: %v", err)
	}
	eligible, chipReason := state.reuseEligibility()
	if !eligible {
		return false, chipReason
	}
	if _, err := s.sendATCommand(ctx, "AT+CVAUXS=1", false); err != nil {
		return false, fmt.Sprintf("enable antenna supply: %v", err)
	}

	deadline := started.Add(startupReuseProbeTimeout)
	for {
		lastData := s.LastDataReceived()
		if lastData.After(started) {
			return true, "gpsd produced fresh data and " + chipReason
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, fmt.Sprintf("gpsd produced no fresh data within %s; %s",
				startupReuseProbeTimeout, chipReason)
		}
		wait := min(100*time.Millisecond, remaining)
		select {
		case <-ctx.Done():
			return false, fmt.Sprintf("reuse probe cancelled: %v", ctx.Err())
		case <-time.After(wait):
		}
	}
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

func (s *Service) modeContext(parent context.Context) (context.Context, context.CancelFunc) {
	s.lifecycleMu.Lock()
	monitorCtx := s.monitorCtx
	s.lifecycleMu.Unlock()
	ctx, cancel := context.WithCancel(parent)
	if monitorCtx == nil {
		return ctx, cancel
	}
	stop := context.AfterFunc(monitorCtx, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// SetGPSMode restarts GPS in mode unless the modem already reports that mode.
func (s *Service) SetGPSMode(parent context.Context, mode GPSMode) error {
	ctx, cancel := s.modeContext(parent)
	defer cancel()
	s.configMutex.Lock()
	defer s.configMutex.Unlock()
	if !s.modeSwitchReady.Load() {
		return nil
	}

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
	// Bump the no-data watchdog. Our CGPS=0 / sleep 3s / CGPS=1,X dance
	// briefly silences gpsd; without this reset the next checkGPSHealth
	// could trip on the silence and cause a self-inflicted recovery.
	s.SetLastDataReceived(time.Now())
	s.Logger.Printf("GPS running in %s mode", mode)
	return nil
}

// syncGPSClock sets the modem GPS clock from system time.
func (s *Service) syncGPSClock(ctx context.Context) {
	now := time.Now().UTC()
	clockCmd := fmt.Sprintf(`AT+CCLK="%s"`, now.Format("06/01/02,15:04:05+00"))
	s.sendATCommand(ctx, clockCmd, false)
	s.Logger.Printf("GPS clock synced: %s", now.Format("2006-01-02 15:04:05 MST"))
}

// setGPSRefreshRate sets the GPS refresh rate via ModemManager.
func (s *Service) setGPSRefreshRate(ctx context.Context) error {
	refreshSeconds := uint32(s.Config.RefreshRate.Seconds())

	err := s.MMClient.SetGPSRefreshRate(s.ModemPath, refreshSeconds)
	if err != nil && s.refreshModemPathIfStale(err) {
		err = s.MMClient.SetGPSRefreshRate(s.ModemPath, refreshSeconds)
	}
	if err != nil {
		return fmt.Errorf("failed to set GPS refresh rate to %ds: %v", refreshSeconds, err)
	}
	s.Logger.Printf("Set GPS refresh rate to %d second(s)", refreshSeconds)
	return nil
}

// LocationStatus is the enabled ModemManager location-source mask.
type LocationStatus struct {
	EnabledSources uint32
}

func (s *Service) getLocationStatusWithTimeout(ctx context.Context) (*LocationStatus, error) {
	enabled, err := s.MMClient.GetEnabledLocationSourcesContext(ctx, s.ModemPath)
	if err != nil && s.refreshModemPathIfStale(err) {
		enabled, err = s.MMClient.GetEnabledLocationSourcesContext(ctx, s.ModemPath)
	}
	if err != nil {
		return nil, err
	}
	return &LocationStatus{EnabledSources: enabled}, nil
}

func (s *Service) disableConflictingSources(ctx context.Context, enabledSources uint32) error {
	// The service starts the receiver with AT commands and lets gpsd own the
	// GPS TTY. Do not ask ModemManager to manage any GPS source as well.
	conflictingMask := mm.MMModemLocationSourceGpsNmea |
		mm.MMModemLocationSourceGpsRaw |
		mm.MMModemLocationSourceGpsUnmanaged

	if enabledSources&conflictingMask != 0 {
		newSources := enabledSources &^ conflictingMask
		s.Logger.Printf("Disabling ModemManager GPS sources (nmea/raw/unmanaged), new mask: 0x%x", newSources)

		err := s.MMClient.SetupLocation(s.ModemPath, newSources, false)
		if err != nil && s.refreshModemPathIfStale(err) {
			err = s.MMClient.SetupLocation(s.ModemPath, newSources, false)
		}
		if err != nil {
			return fmt.Errorf("failed to disable conflicting sources: %v", err)
		}
	}
	return nil
}

func locationSourceMask(currentSources uint32) uint32 {
	return (currentSources &^ (mm.MMModemLocationSourceGpsNmea |
		mm.MMModemLocationSourceGpsRaw |
		mm.MMModemLocationSourceGpsUnmanaged)) |
		mm.MMModemLocationSource3gppLacCi
}

func (s *Service) setupLocationSources(ctx context.Context, sources uint32) error {
	var setupErr error
	for attempt := 0; attempt < 3; attempt++ {
		setupErr = s.MMClient.SetupLocation(s.ModemPath, sources, false)
		if setupErr != nil && s.refreshModemPathIfStale(setupErr) {
			setupErr = s.MMClient.SetupLocation(s.ModemPath, sources, false)
		}
		if setupErr == nil {
			return nil
		}
		s.Logger.Printf("Warning: Failed to enable sources (attempt %d/3): %v", attempt+1, setupErr)
		if attempt < 2 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(1 * time.Second):
			}
		}
	}
	return setupErr
}

func (s *Service) enableLocationSources(ctx context.Context, currentSources uint32, initializationStarted time.Time) error {
	// gpsd reads the modem's dedicated GPS TTY, while configureReceiver owns
	// AT+CGPS. Enabling gps-unmanaged would make ModemManager issue a second
	// AT+CGPS=1,1, which SIM7100E rejects while already running.
	if currentSources&mm.MMModemLocationSource3gppLacCi == 0 {
		s.Logger.Printf("Enabling optional location source: 3gpp-lac-ci")
		if err := s.setupLocationSources(ctx, locationSourceMask(currentSources)); err != nil {
			s.Logger.Printf("Warning: Cell location source unavailable; continuing with GPS only: %v", err)
		} else {
			s.Logger.Printf("Cell location source enabled successfully")
		}
	}

	s.Logger.Printf("GPS initialization phase=gpsd-restart-delay elapsed=%s delay=%s",
		time.Since(initializationStarted).Round(time.Millisecond), locationSourceRestartDelay)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(locationSourceRestartDelay):
	}

	s.Logger.Printf("Restarting gpsd service after GPS configuration elapsed=%s",
		time.Since(initializationStarted).Round(time.Millisecond))
	restartCmd := exec.CommandContext(ctx, "systemctl", "restart", "gpsd")
	if err := restartCmd.Run(); err != nil {
		s.Logger.Printf("Warning: Failed to restart gpsd: %v", err)
	} else {
		s.Logger.Printf("Successfully restarted gpsd service elapsed=%s",
			time.Since(initializationStarted).Round(time.Millisecond))
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

	s.GpsdConn.AddFilter("SKY", func(r interface{}) {
		report, ok := r.(*gpsd.SKYReport)
		if !ok {
			s.Logger.Printf("Error: Could not cast SKY report")
			return
		}

		s.stateMutex.Lock()
		s.lastDataReceived = time.Now()
		s.stateMutex.Unlock()

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

		s.stateMutex.Lock()
		s.lastDataReceived = time.Now()
		s.stateMutex.Unlock()

		// Mode 0 is a partial report with no fix update; preserve the prior state.
		if report.Mode == 0 {
			return
		}

		// Log transitions so pre-fix mode=1 stalls remain diagnosable.
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
		} else {
			rawLocation.Timestamp = time.Now()
		}

		s.stateMutex.Lock()
		s.currentLoc = rawLocation
		s.lastFix = time.Now()
		s.stateMutex.Unlock()
		s.hasValidFix.Store(true)
	})

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
	s.modeSwitchReady.Store(false)
	s.lifecycleMu.Lock()
	if s.monitorCancel != nil {
		s.monitorCancel()
		<-s.monitorDone
		s.monitorCtx = nil
		s.monitorCancel = nil
		s.monitorDone = nil
	}

	// Mark disconnected first so concurrent GetGPSStatus() never observes
	// state="off" while connected=true.
	s.gpsdConnected.Store(false)
	s.state.Store("off")

	// Clear fix timestamps so resume cannot feed stale GPS time to chrony.
	// Monotonic staleness cannot detect suspend because that clock also stops;
	// last coordinates remain available only as an explicitly inactive fix.
	s.hasValidFix.Store(false)
	s.stateMutex.Lock()
	s.currentLoc.Timestamp = time.Time{}
	s.lastFix = time.Time{}
	s.stateMutex.Unlock()

	s.configMutex.Lock()
	if s.GpsdConn != nil {
		s.GpsdConn.Close()
		s.GpsdConn = nil
	}
	s.configMutex.Unlock()
	s.lifecycleMu.Unlock()
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

// ShouldPublishRecovery reports a first fix or recovery after a five-minute outage.
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

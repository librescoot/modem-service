package service

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"modem-service/internal/cell"
	"modem-service/internal/config"
	"modem-service/internal/health"
	"modem-service/internal/location"
	"modem-service/internal/mm"
	"modem-service/internal/modem"
	"modem-service/internal/modem/connectivity"
	redisClient "modem-service/internal/redis"
)

// Vehicle states that should trigger modem enable
var modemOnlineStates = map[string]bool{
	"parked":         true,
	"ready-to-drive": true,
}

// clockSyncInterval is how often we re-feed the system clock from rollover-corrected
// GPS time while the scooter is offline. When connectivity is available, chrony's
// NTP pool takes over and we suppress manual settime samples to avoid competing
// with the higher-precision source.
const clockSyncInterval = 60 * time.Second

// dataSessionStallTimeout is how long the modem is allowed to stay in a
// non-"connected" status while still registered to the carrier before we
// force a recovery cycle. Tuned generously so tunnels, underground parking,
// and normal handoffs don't trigger spurious resets; only truly wedged data
// sessions should cross this threshold.
const dataSessionStallTimeout = 15 * time.Minute

type Service struct {
	Config                *config.Config
	Redis                 *redisClient.Client
	Logger                *log.Logger
	Health                *health.Health
	Location              *location.Service
	Modem                 *modem.Manager
	MMClient              *mm.Client
	LastState             *modem.State
	WaitingForGPSLogged   bool       // Tracks if we've already logged the waiting for GPS message
	LastGPSDataTime       time.Time  // Last time we received any GPS data
	GPSEnabledTime        time.Time  // When GPS was first enabled
	GPSRecoveryCount      int        // Number of GPS recovery attempts
	LastGPSQualityLog     time.Time  // Last time GPS quality was logged
	gpsRecoveryMutex      sync.Mutex // Prevents concurrent GPS recovery/configuration attempts
	gpsRecoveryInProgress bool       // Tracks if GPS recovery is currently running
	gpsRecoveryUntil      time.Time  // Protected by gpsRecoveryMutex; monitor skips EnableGPS until this passes
	connectivityFailures  int        // Consecutive internet connectivity check failures

	// disconnectedSince is set on the first tick where the modem reports
	// status != "connected" (zero otherwise). When we're still registered to
	// the carrier but the data session hasn't come back after
	// dataSessionStallTimeout, we escalate to handleModemFailure. Without this
	// path nothing triggers recovery if the modem says "disconnected" — the
	// TCP-probe counter only runs while status == "connected".
	disconnectedSince time.Time

	lastClockSync time.Time // Last time syncClockFromGPS successfully fed chrony

	// Settings (from Redis) — atomic so the Redis watcher goroutine can
	// update them without racing the monitor goroutine that reads them.
	gpsEnabled          atomic.Bool
	cellLocationEnabled atomic.Bool
	lastCellTower       *cell.CellTower
	lastCellLoc         *cell.CellLocation

	// Modem enable/disable target state. Atomic because it's read from the
	// monitor goroutine and written from the Redis command/vehicle-state
	// watcher goroutines.
	modemEnabled atomic.Bool

	// Service-level context, captured in Run() so handlers spawned from
	// Redis watchers (e.g. disableModem goroutine) can respect shutdown.
	ctx context.Context

	// Connectivity classifier derives online/searching/offline/no-sim from
	// raw modem state with hysteresis to avoid thrashing on coverage flickers.
	connClassifier *connectivity.Classifier
	lastPubConn    connectivity.State // last value published to Redis

	// TTFF measurement. ttffStart is the moment we went from "no fix" to
	// "actively searching". Mode is read at fix time rather than wait-start,
	// because startup paths sometimes don't know the real modem mode until
	// ProbeGPSMode has run — capturing at wait-start gave misleading labels.
	ttffStart time.Time

	// lastPubGPSMode is the last value of gps.mode published to Redis.
	// Guarded by modePubMu because requestGPSModeForConnectivity can spawn
	// overlapping goroutines on rapid connectivity changes.
	modePubMu      sync.Mutex
	lastPubGPSMode location.GPSMode

	// monitorDone is closed by monitorStatus when it returns, so Run() can
	// wait for it before closing MMClient/Modem/Redis. Prevents the "D-Bus
	// call fails mid-shutdown" noise where the monitor goroutine is in the
	// middle of an AT command when its transport gets closed out from under
	// it.
	monitorDone chan struct{}
}

func New(cfg *config.Config, logger *log.Logger, version string) (*Service, error) {
	redis, err := redisClient.New(cfg.RedisURL, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create Redis client: %v", err)
	}

	// Create ModemManager D-Bus client
	mmClient, err := mm.NewClient(cfg.Debug, logger.Printf)
	if err != nil {
		return nil, fmt.Errorf("failed to create ModemManager client: %v", err)
	}

	// Create modem manager, sharing the same D-Bus client.
	modemMgr, err := modem.NewManager(mmClient, logger)
	if err != nil {
		mmClient.Close()
		return nil, fmt.Errorf("failed to create modem manager: %v", err)
	}

	service := &Service{
		Config:              cfg,
		Redis:               redis,
		Logger:              logger,
		Health:              health.New(),
		Modem:               modemMgr,
		MMClient:            mmClient,
		Location:            location.NewService(logger, cfg.GpsdServer, mmClient, cfg.SuplServer),
		LastState:           modem.NewState(),
		WaitingForGPSLogged: false,
		connClassifier:      connectivity.New(),
		monitorDone:         make(chan struct{}),
	}
	service.gpsEnabled.Store(true)           // default: GPS on
	service.cellLocationEnabled.Store(false) // default: cell location off
	service.modemEnabled.Store(true)         // default: modem on until pm-service says otherwise

	cell.SetVersion(version)
	service.Logger.Printf("modem-service %s", version)

	return service, nil
}

func (s *Service) Run(ctx context.Context) error {
	// Capture the service context so handlers spawned from Redis watchers
	// (e.g. the disableModem goroutine) can respect shutdown.
	s.ctx = ctx

	if err := s.Redis.Ping(); err != nil {
		return fmt.Errorf("redis connection failed: %v", err)
	}

	// Start listening for modem enable/disable commands from pm-service
	if err := s.Redis.StartModemCommandHandler(s.handleModemCommand); err != nil {
		s.Logger.Printf("Failed to start modem command handler: %v", err)
	}

	// Start watching vehicle state to auto-enable modem
	if err := s.Redis.StartVehicleStateWatcher(s.handleVehicleState); err != nil {
		s.Logger.Printf("Failed to start vehicle state watcher: %v", err)
	}

	// Watch location settings
	s.Redis.StartSettingsWatcher("modem.gps", func(value string) error {
		enabled := value != "false"
		s.gpsEnabled.Store(enabled)
		s.Logger.Printf("GPS %s", map[bool]string{true: "enabled", false: "disabled"}[enabled])
		return nil
	})
	s.Redis.StartSettingsWatcher("modem.cell-location", func(value string) error {
		enabled := value == "true"
		s.cellLocationEnabled.Store(enabled)
		s.Logger.Printf("Cell location %s", map[bool]string{true: "enabled", false: "disabled"}[enabled])
		return nil
	})
	s.Redis.StartSettingsWatching()

	// Try to enable the modem if it's not present
	if err := s.ensureModemEnabled(ctx); err != nil {
		s.Logger.Printf("SEVERE ERROR: Failed to ensure modem is enabled: %v", err)

		// If the modem interface is still not present, we cannot continue
		if !modem.IsInterfacePresent(s.Config.Interface) && !s.Modem.IsModemPresent() {
			s.Logger.Printf("Cannot continue without modem interface or D-Bus presence")
			return fmt.Errorf("modem not available: %v", err)
		}
	}

	s.Logger.Printf("Starting modem service on interface %s", s.Config.Interface)
	go s.monitorStatus(ctx)

	<-ctx.Done()

	// Give the monitor goroutine a chance to exit its current tick before
	// we tear down MMClient/Modem/Redis. Without this, an AT command or
	// D-Bus call in-flight will error out when its transport is closed,
	// producing noise in the journal. Bounded so shutdown stays snappy.
	select {
	case <-s.monitorDone:
	case <-time.After(10 * time.Second):
		s.Logger.Printf("Monitor goroutine did not exit within 10s; proceeding with shutdown")
	}

	// Graceful shutdown: keep last lat/lng in Redis as a useful fallback
	// for consumers, but clear the fix indicators so nobody treats the
	// stale coords as a current position.
	s.Location.Close()
	s.Redis.PublishLocationState(map[string]interface{}{
		"state":              "off",
		"fix":                "none",
		"active":             false,
		"connected":          false,
		"snr":                float64(0),
		"hdop":               float64(0),
		"vdop":               float64(0),
		"pdop":               float64(0),
		"eph":                float64(0),
		"satellites-used":    int32(0),
		"satellites-visible": int32(0),
	}, false)

	// Release service-owned resources. Modem.Close does not close the
	// shared mm.Client — service owns it and closes it below.
	if err := s.Modem.Close(); err != nil {
		s.Logger.Printf("Error closing modem manager: %v", err)
	}
	if err := s.MMClient.Close(); err != nil {
		s.Logger.Printf("Error closing ModemManager D-Bus client: %v", err)
	}
	if err := s.Redis.Close(); err != nil {
		s.Logger.Printf("Error closing Redis client: %v", err)
	}

	return nil
}

// handleModemCommand handles enable/disable commands from pm-service
func (s *Service) handleModemCommand(command string) error {
	switch command {
	case "enable":
		s.Logger.Printf("Received modem enable command")
		s.modemEnabled.Store(true)
		// Modem will be enabled by ensureModemEnabled or monitor loop
	case "disable":
		s.Logger.Printf("Received modem disable command")
		s.modemEnabled.Store(false)
		// Disable the modem
		go s.disableModem(s.ctx)
	default:
		s.Logger.Printf("Unknown modem command: %s", command)
	}
	return nil
}

// handleVehicleState handles vehicle state changes to auto-enable modem
func (s *Service) handleVehicleState(state string) error {
	if modemOnlineStates[state] {
		if s.modemEnabled.CompareAndSwap(false, true) {
			s.Logger.Printf("Vehicle state '%s' - enabling modem", state)
		}
	}
	return nil
}

// disableModem turns off the modem and publishes the off state
func (s *Service) disableModem(ctx context.Context) {
	s.Logger.Printf("Disabling modem...")

	// Close GPS first. Preserve last lat/lng but clear the fix indicators
	// so consumers don't treat stale coords as a current position.
	s.Location.Close()
	s.Redis.PublishLocationState(map[string]interface{}{
		"state":              "off",
		"fix":                "none",
		"active":             false,
		"connected":          false,
		"snr":                float64(0),
		"hdop":               float64(0),
		"vdop":               float64(0),
		"pdop":               float64(0),
		"eph":                float64(0),
		"satellites-used":    int32(0),
		"satellites-visible": int32(0),
	}, false)

	// Publish off states
	s.Redis.PublishInternetState("status", "disconnected")
	s.Redis.PublishInternetState("modem-state", "off")
	s.Redis.PublishModemState("power-state", "off")

	if err := s.Modem.PowerOffModem(ctx); err != nil {
		s.Logger.Printf("Failed to disable modem via GPIO: %v", err)
	}

	s.Logger.Printf("Modem disabled")
}

func (s *Service) ensureModemEnabled(ctx context.Context) error {
	if s.Modem.IsModemPresent() {
		s.Logger.Printf("Modem is already present via D-Bus")
		return nil
	}

	if modem.IsInterfacePresent(s.Config.Interface) {
		s.Logger.Printf("Modem interface %s is present, waiting for ModemManager...", s.Config.Interface)
		waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := s.Modem.WaitForModem(waitCtx, s.Config.Interface); err == nil {
			return nil
		}
		s.Logger.Printf("ModemManager did not register modem, proceeding to GPIO recovery")
	}

	s.Logger.Printf("Modem not detected, will attempt to enable via GPIO")

	// Try multiple times with increasing wait times
	for attempt := range health.MaxRecoveryAttempts {
		waitTime := min(time.Duration(60*(attempt+1))*time.Second, 300*time.Second)

		s.Logger.Printf("Modem start attempt %d/%d with %v wait time",
			attempt+1, health.MaxRecoveryAttempts, waitTime)

		if err := s.Modem.StartModem(); err != nil {
			continue
		}

		attemptCtx, cancel := context.WithTimeout(ctx, waitTime)

		err := s.Modem.WaitForModem(attemptCtx, s.Config.Interface)
		cancel()

		if err == nil {
			s.Logger.Printf("Modem successfully enabled on attempt %d", attempt+1)
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Continue to next attempt
		}
	}

	// If we get here, all attempts failed
	s.Logger.Printf("SEVERE ERROR: Modem failed to come up after %d attempts with up to 5 minute wait times",
		health.MaxRecoveryAttempts)

	// Mark modem as potentially defective in Redis
	s.Health.State = health.StatePermanentFailure
	s.publishHealthState(ctx)

	// Log fault to events stream and add to fault set
	s.Redis.LogFault("internet", redisClient.FaultCodeModemRecoveryFailed, "Modem recovery failed")
	s.Redis.AddFault(redisClient.FaultCodeModemRecoveryFailed)

	return fmt.Errorf("modem failed to come up after multiple attempts, marked as potentially defective")
}

// probeHealth checks whether the modem is currently healthy on D-Bus without
// triggering any recovery machinery. Used inside attemptRecovery to verify a
// strategy succeeded; going through checkHealth there would re-enter
// handleModemFailure and collapse the escalation sequence.
func (s *Service) probeHealth() bool {
	if _, err := s.Modem.FindModem(); err != nil {
		return false
	}
	if err := s.Modem.CheckPrimaryPort(); err != nil {
		return false
	}
	if err := s.Modem.CheckPowerState(); err != nil {
		return false
	}
	return true
}

// recoverySucceeded is the shared post-strategy bookkeeping: mark healthy,
// reset GPS, clear the fault. Called whenever probeHealth returns true after
// a recovery strategy.
func (s *Service) recoverySucceeded(ctx context.Context, strategy string) {
	s.Logger.Printf("Modem recovery successful via %s", strategy)
	s.Health.MarkNormal()
	s.GPSRecoveryCount = 0
	s.resetGPSAfterModemRecovery()
	s.publishHealthState(ctx)
	s.Redis.RemoveFault(redisClient.FaultCodeModemRecoveryFailed)
}

func (s *Service) checkHealth(ctx context.Context) error {
	// Skip health check if we're in a terminal state
	if s.Health.IsTerminal() {
		return fmt.Errorf("modem in terminal state: %s", s.Health.State)
	}

	if !s.probeHealth() {
		return s.handleModemFailure(ctx, "probe_failed")
	}

	s.Health.MarkNormal()
	// Clear any active faults when modem is healthy
	s.Redis.RemoveFault(redisClient.FaultCodeModemRecoveryFailed)
	return nil
}

func (s *Service) handleModemFailure(ctx context.Context, reason string) error {
	s.Logger.Printf("Modem failure detected: %s", reason)

	if s.Health.IsRecovering() {
		return fmt.Errorf("recovery in progress")
	}

	// Exhausted retries: publish terminal Wait state, back off 2 minutes,
	// then reset and allow recovery to try again on the next failure.
	// StatePermanentFailure is reached only from ensureModemEnabled when
	// the modem never responded at all.
	if !s.Health.CanRecover() {
		s.Health.MarkRecoveryFailed()
		s.publishHealthState(ctx)
		s.Redis.LogFault("internet", redisClient.FaultCodeModemRecoveryFailed,
			fmt.Sprintf("Max recovery attempts (%d) exhausted, entering recovery-failed-wait", health.MaxRecoveryAttempts))
		s.Redis.AddFault(redisClient.FaultCodeModemRecoveryFailed)
		s.Logger.Printf("Max recovery attempts reached, entering %s state", s.Health.State)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Minute):
		}
		s.Health.MarkNormal() // clears both state and RecoveryAttempts
		s.publishHealthState(ctx)
		s.Logger.Printf("Recovery-failed-wait expired, will retry on next failure")
		return nil
	}

	return s.attemptRecovery(ctx)
}

func (s *Service) attemptRecovery(ctx context.Context) error {
	s.Health.StartRecovery()

	s.Logger.Printf("Attempting modem recovery (attempt %d/%d)",
		s.Health.RecoveryAttempts, health.MaxRecoveryAttempts)

	s.publishHealthState(ctx)

	// Strategy 1: Try software reset first if modem is present
	_, err := s.Modem.FindModem()
	if err == nil {
		s.Logger.Printf("Attempting to reset the modem via D-Bus")
		if err := s.Modem.ResetModem(); err != nil {
			s.Logger.Printf("Failed to reset modem via D-Bus: %v", err)
		} else {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(health.RecoveryWaitTime):
			}

			if s.probeHealth() {
				s.recoverySucceeded(ctx, "D-Bus reset")
				return nil
			}
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// Strategy 2: Try USB unbind/bind recovery
	s.Logger.Printf("Attempting USB recovery (unbind/bind)...")
	if err := s.Modem.RecoverUSB(); err != nil {
		s.Logger.Printf("USB recovery failed: %v", err)
	} else {
		usbCtx, usbCancel := context.WithTimeout(ctx, health.RecoveryWaitTime)
		err := s.Modem.WaitForModem(usbCtx, s.Config.Interface)
		usbCancel()

		if err == nil && s.probeHealth() {
			s.recoverySucceeded(ctx, "USB recovery")
			return nil
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// Strategy 3: Try hardware reset via GPIO
	s.Logger.Printf("Attempting modem restart (GPIO with D-Bus fallback)...")
	if err := s.Modem.RestartModem(ctx); err != nil {
		s.Logger.Printf("GPIO restart failed: %v", err)
	} else {
		gpioCtx, gpioCancel := context.WithTimeout(ctx, health.RecoveryWaitTime)
		err := s.Modem.WaitForModem(gpioCtx, s.Config.Interface)
		gpioCancel()

		if err == nil && s.probeHealth() {
			s.recoverySucceeded(ctx, "GPIO restart")
			return nil
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// Strategy 4: Just wait longer and hope the modem recovers
	s.Logger.Printf("Hardware recovery uncertain, waiting additional time for modem to stabilize...")
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(30 * time.Second):
	}

	if s.probeHealth() {
		s.recoverySucceeded(ctx, "extended wait")
		return nil
	}

	s.Logger.Printf("Recovery attempt %d failed, will retry", s.Health.RecoveryAttempts)
	return fmt.Errorf("recovery attempt failed, will retry")
}

func (s *Service) publishHealthState(ctx context.Context) error {
	return s.Redis.PublishInternetState("modem-health", s.Health.State)
}

// handleGPSFailure attempts GPS-specific recovery before escalating to modem recovery
func (s *Service) handleGPSFailure(ctx context.Context, gpsErr error) error {
	s.Logger.Printf("Attempting GPS-specific recovery for: %v", gpsErr)

	// Try to restart GPS configuration without restarting the entire modem
	if err := s.attemptGPSRecovery(gpsErr); err != nil {
		s.Logger.Printf("GPS-specific recovery failed: %v", err)
		// Only escalate to modem recovery for severe GPS issues after GPS recovery fails
		if recoveryErr := s.handleModemFailure(ctx, fmt.Sprintf("gps_stuck_after_gps_recovery: %v", gpsErr)); recoveryErr != nil {
			return fmt.Errorf("both GPS and modem recovery failed: %v", recoveryErr)
		}
	}

	return nil
}

// attemptGPSRecovery tries to recover GPS without restarting the modem.
// trigger is the underlying failure that caused recovery to be requested
// (e.g. gps_data_stale, gps_timestamp_stuck, gps_fix_timeout). Logged so
// we can correlate unexpected GPS restarts with the triggering check.
//
// Pacing is handled via s.gpsRecoveryUntil (see the monitor loop) — this
// function does not sleep while holding any mutex.
func (s *Service) attemptGPSRecovery(trigger error) error {
	// Acquire lock to prevent concurrent GPS recovery attempts
	s.gpsRecoveryMutex.Lock()
	// Check if recovery is already in progress
	if s.gpsRecoveryInProgress {
		s.gpsRecoveryMutex.Unlock()
		s.Logger.Printf("GPS recovery already in progress, skipping duplicate attempt")
		return nil
	}
	s.gpsRecoveryInProgress = true
	s.gpsRecoveryMutex.Unlock()
	defer func() {
		s.gpsRecoveryMutex.Lock()
		s.gpsRecoveryInProgress = false
		s.gpsRecoveryMutex.Unlock()
	}()

	s.GPSRecoveryCount++
	s.Logger.Printf("Attempting GPS recovery (attempt %d, trigger=%v)", s.GPSRecoveryCount, trigger)

	// If we've tried GPS recovery too many times, do a full reset and gate
	// the monitor loop for 30 seconds before it's allowed to re-enable GPS.
	if s.GPSRecoveryCount > 3 {
		s.Logger.Printf("GPS recovery attempted %d times, performing full reset with longer break", s.GPSRecoveryCount)
		s.GPSRecoveryCount = 0

		// Stop gpsd and close GPS connection
		if err := s.Location.StopGPSD(); err != nil {
			s.Logger.Printf("Warning: Failed to stop gpsd: %v", err)
		}
		s.Location.Close()

		// Reset state tracking
		s.LastGPSDataTime = time.Time{}
		s.GPSEnabledTime = time.Time{}
		s.WaitingForGPSLogged = false
		s.Location.ResetTimestampTracking()

		// Gate the monitor loop rather than sleeping under the mutex.
		s.gpsRecoveryMutex.Lock()
		s.gpsRecoveryUntil = time.Now().Add(30 * time.Second)
		s.gpsRecoveryMutex.Unlock()
		s.Logger.Printf("GPS break complete, monitor will re-enable after 30s")
		return nil
	}

	// Stop gpsd before performing GPS-related modem reset
	s.Logger.Printf("Stopping gpsd service before GPS reset...")
	if err := s.Location.StopGPSD(); err != nil {
		s.Logger.Printf("Warning: Failed to stop gpsd: %v", err)
		// Continue with recovery even if gpsd stop fails
	}

	// Close existing GPS connection; gate the monitor for 2 seconds so
	// it doesn't re-enable while gpsd is still tearing down.
	s.Location.Close()
	s.gpsRecoveryMutex.Lock()
	s.gpsRecoveryUntil = time.Now().Add(2 * time.Second)
	s.gpsRecoveryMutex.Unlock()

	// Reset GPS state tracking
	s.LastGPSDataTime = time.Time{}
	s.GPSEnabledTime = time.Time{}
	s.WaitingForGPSLogged = false
	s.Location.ResetTimestampTracking()

	// Try to re-enable GPS
	modemPath, err := s.Modem.FindModem()
	if err != nil {
		return fmt.Errorf("modem not found for GPS recovery: %v", err)
	}

	if err := s.Location.EnableGPS(modemPath); err != nil {
		return fmt.Errorf("failed to re-enable GPS: %v", err)
	}

	s.GPSEnabledTime = time.Now()
	s.Logger.Printf("GPS recovery completed, waiting for fix...")
	return nil
}

// requestGPSModeForConnectivity kicks off a GPS mode change to match the new
// connectivity state. Runs asynchronously because the AT stop/start dance
// takes several seconds; we don't want to stall the modem state loop. The
// location service serializes mode changes internally via configMutex.
//
// UE-Based mode is disabled for now. In the field (2026-04-15) we observed a
// fleet scooter on Telefónica DE hang for 180 s in UE-Based after a switch,
// with no NMEA timestamp updates, until the stuck-timestamp recovery path
// fired. The stall overlapped with the user unlocking and starting to ride.
// Until we can verify UE-Based is reliable across carriers and firmware (and
// given XTRA is broken on SIM7100E), stay in standalone always. The
// classifier + plumbing are kept so flipping this back on is one constant.
const enableUEBasedMode = false

func (s *Service) requestGPSModeForConnectivity(ctx context.Context, conn connectivity.State) {
	var desired location.GPSMode
	switch {
	case enableUEBasedMode && conn == connectivity.Online:
		desired = location.ModeUEBased
	default:
		desired = location.ModeStandalone
	}

	prev := s.Location.CurrentGPSMode()
	s.Logger.Printf("gps-transition request from=%s to=%s connectivity=%s", prev, desired, conn)

	go func() {
		if err := s.Location.SetGPSMode(ctx, desired); err != nil {
			s.Logger.Printf("Failed to switch GPS to %s mode: %v", desired, err)
			return
		}
		s.publishGPSMode()
	}()
}

// publishGPSMode updates the gps.mode Redis field if the current mode has
// changed since the last publish. Serialized via modePubMu because
// requestGPSModeForConnectivity can spawn overlapping goroutines when
// connectivity flickers.
func (s *Service) publishGPSMode() {
	mode := s.Location.CurrentGPSMode()

	s.modePubMu.Lock()
	defer s.modePubMu.Unlock()

	if mode == s.lastPubGPSMode {
		return
	}
	if err := s.Redis.PublishLocationState(map[string]interface{}{
		"mode": mode.String(),
	}, false); err != nil {
		s.Logger.Printf("Failed to publish gps mode: %v", err)
		return
	}
	s.lastPubGPSMode = mode
}

// publishModemState publishes the detailed modem and derived internet state to Redis.
// It now takes the determined internetStatus as an argument.
func (s *Service) publishModemState(ctx context.Context, currentState *modem.State, internetStatus string) error {
	// Track which fields changed for consolidated logging
	var internetChanges, modemChanges []string

	// Publish internet state fields
	if s.LastState.Status != internetStatus {
		if err := s.Redis.PublishInternetState("status", internetStatus); err != nil {
			return err
		}
		internetChanges = append(internetChanges, fmt.Sprintf("status=%s", internetStatus))
		s.LastState.Status = internetStatus
	}

	if s.LastState.LastRawModemStatus != currentState.Status {
		if err := s.Redis.PublishInternetState("modem-state", currentState.Status); err != nil {
			s.Logger.Printf("Failed to publish internet modem-state: %v", err)
		}
		internetChanges = append(internetChanges, fmt.Sprintf("modem-state=%s", currentState.Status))
		s.LastState.LastRawModemStatus = currentState.Status
	}

	if s.LastState.IfIPAddr != currentState.IfIPAddr {
		if err := s.Redis.PublishInternetState("ip-address", currentState.IfIPAddr); err != nil {
			return err
		}
		internetChanges = append(internetChanges, fmt.Sprintf("ip=%s", currentState.IfIPAddr))
		s.LastState.IfIPAddr = currentState.IfIPAddr
	}

	if s.LastState.AccessTech != currentState.AccessTech {
		if err := s.Redis.PublishInternetState("access-tech", currentState.AccessTech); err != nil {
			return err
		}
		internetChanges = append(internetChanges, fmt.Sprintf("tech=%s", currentState.AccessTech))
		s.LastState.AccessTech = currentState.AccessTech
	}

	if s.LastState.SignalQuality != currentState.SignalQuality {
		if err := s.Redis.PublishInternetState("signal-quality", fmt.Sprintf("%d", currentState.SignalQuality)); err != nil {
			return err
		}
		internetChanges = append(internetChanges, fmt.Sprintf("signal=%d", currentState.SignalQuality))
		s.LastState.SignalQuality = currentState.SignalQuality
	}

	if s.LastState.IMEI != currentState.IMEI {
		if err := s.Redis.PublishInternetState("sim-imei", currentState.IMEI); err != nil {
			return err
		}
		internetChanges = append(internetChanges, fmt.Sprintf("imei=%s", currentState.IMEI))
		s.LastState.IMEI = currentState.IMEI
	}

	if s.LastState.IMSI != currentState.IMSI {
		if err := s.Redis.PublishInternetState("sim-imsi", currentState.IMSI); err != nil {
			return err
		}
		internetChanges = append(internetChanges, fmt.Sprintf("imsi=%s", currentState.IMSI))
		s.LastState.IMSI = currentState.IMSI
	}

	if s.LastState.ICCID != currentState.ICCID {
		if err := s.Redis.PublishInternetState("sim-iccid", currentState.ICCID); err != nil {
			return err
		}
		internetChanges = append(internetChanges, fmt.Sprintf("iccid=%s", currentState.ICCID))
		s.LastState.ICCID = currentState.ICCID
	}

	// Publish modem state fields
	if s.LastState.PowerState != currentState.PowerState {
		if err := s.Redis.PublishModemState("power-state", currentState.PowerState); err != nil {
			return err
		}
		modemChanges = append(modemChanges, fmt.Sprintf("power=%s", currentState.PowerState))
		s.LastState.PowerState = currentState.PowerState
	}

	if s.LastState.SIMState != currentState.SIMState {
		if err := s.Redis.PublishModemState("sim-state", currentState.SIMState); err != nil {
			return err
		}
		modemChanges = append(modemChanges, fmt.Sprintf("sim=%s", currentState.SIMState))
		s.LastState.SIMState = currentState.SIMState
	}

	if s.LastState.SIMLockStatus != currentState.SIMLockStatus {
		if err := s.Redis.PublishModemState("sim-lock", currentState.SIMLockStatus); err != nil {
			return err
		}
		modemChanges = append(modemChanges, fmt.Sprintf("sim-lock=%s", currentState.SIMLockStatus))
		s.LastState.SIMLockStatus = currentState.SIMLockStatus
	}

	if s.LastState.OperatorName != currentState.OperatorName {
		if err := s.Redis.PublishModemState("operator-name", currentState.OperatorName); err != nil {
			return err
		}
		modemChanges = append(modemChanges, fmt.Sprintf("operator=%s", currentState.OperatorName))
		s.LastState.OperatorName = currentState.OperatorName
	}

	if s.LastState.OperatorCode != currentState.OperatorCode {
		if err := s.Redis.PublishModemState("operator-code", currentState.OperatorCode); err != nil {
			return err
		}
		modemChanges = append(modemChanges, fmt.Sprintf("mcc-mnc=%s", currentState.OperatorCode))
		s.LastState.OperatorCode = currentState.OperatorCode
	}

	if s.LastState.IsRoaming != currentState.IsRoaming {
		if err := s.Redis.PublishModemState("is-roaming", fmt.Sprintf("%t", currentState.IsRoaming)); err != nil {
			return err
		}
		modemChanges = append(modemChanges, fmt.Sprintf("roaming=%t", currentState.IsRoaming))
		s.LastState.IsRoaming = currentState.IsRoaming
	}

	if s.LastState.Registration != currentState.Registration {
		if err := s.Redis.PublishModemState("registration", currentState.Registration); err != nil {
			s.Logger.Printf("Failed to publish modem registration: %v", err)
		}
		modemChanges = append(modemChanges, fmt.Sprintf("reg=%s", currentState.Registration))
		s.LastState.Registration = currentState.Registration
	}

	if s.LastState.RegistrationFail != currentState.RegistrationFail {
		if err := s.Redis.PublishModemState("registration-fail", currentState.RegistrationFail); err != nil {
			return err
		}
		modemChanges = append(modemChanges, fmt.Sprintf("reg-fail=%s", currentState.RegistrationFail))
		s.LastState.RegistrationFail = currentState.RegistrationFail
	}

	if s.LastState.ErrorState != currentState.ErrorState {
		if err := s.Redis.PublishModemState("error-state", currentState.ErrorState); err != nil {
			s.Logger.Printf("Failed to publish modem error-state: %v", err)
		}
		modemChanges = append(modemChanges, fmt.Sprintf("error=%s", currentState.ErrorState))
		s.LastState.ErrorState = currentState.ErrorState
	}

	conn := s.connClassifier.Classify(currentState.Status, currentState.SIMState)
	if conn != s.lastPubConn {
		if err := s.Redis.PublishModemState("connectivity", string(conn)); err != nil {
			s.Logger.Printf("Failed to publish modem connectivity: %v", err)
		}
		modemChanges = append(modemChanges, fmt.Sprintf("connectivity=%s", conn))
		s.lastPubConn = conn
		s.requestGPSModeForConnectivity(ctx, conn)
	}

	// Log consolidated changes
	if len(internetChanges) > 0 {
		s.Logger.Printf("internet %s", strings.Join(internetChanges, " "))
	}
	if len(modemChanges) > 0 {
		s.Logger.Printf("modem %s", strings.Join(modemChanges, " "))
	}

	return nil
}

func (s *Service) publishLocationState(ctx context.Context, loc location.Location, publishRecovery bool) error {
	data := map[string]interface{}{
		"latitude":  fmt.Sprintf("%.6f", loc.Latitude),
		"longitude": fmt.Sprintf("%.6f", loc.Longitude),
		"altitude":  fmt.Sprintf("%.6f", loc.Altitude),
		"speed":     fmt.Sprintf("%.6f", loc.Speed*3.6), // Convert m/s to km/h
		"course":    fmt.Sprintf("%.6f", loc.Course),
		"timestamp": loc.Timestamp.Format(time.RFC3339),
	}
	gpsStatus := s.Location.GetGPSStatus()
	for k, v := range gpsStatus {
		data[k] = v
	}

	if err := s.Redis.PublishLocationState(data, publishRecovery); err != nil {
		return err
	}
	if err := s.Redis.PublishGPSSnapshot(data); err != nil {
		s.Logger.Printf("Failed to publish GPS snapshot: %v", err)
	}
	return nil
}

// syncClockFromGPS feeds chrony a single time sample via `chronyc settime`.
// Returns true if chrony accepted the sample, false if the timestamp was
// rejected or the command failed (so the caller can retry on the next tick
// instead of waiting a full clockSyncInterval).
func (s *Service) syncClockFromGPS(t time.Time) bool {
	if t.IsZero() {
		return false
	}
	// Defense in depth against GPS week-rollover bugs: refuse to set the
	// system clock to a timestamp before the current rollover epoch. The TPV
	// callback already corrects rollover, but we never want a stray bad
	// value to roll a working system clock back ~20 years.
	if t.Before(location.MinValidGPSDate) {
		s.Logger.Printf("Refusing to set system time from GPS: %s is before the current GPS rollover epoch (%s)",
			t.Format(time.RFC3339), location.MinValidGPSDate.Format(time.RFC3339))
		return false
	}
	timeStr := t.Local().Format("02 Jan 2006 15:04:05")
	out, err := exec.Command("chronyc", "settime", timeStr).CombinedOutput()
	if err != nil {
		s.Logger.Printf("Failed to set system time from GPS: %v: %s", err, out)
		return false
	}
	s.Logger.Printf("System time set from GPS: %s", timeStr)
	return true
}

// shouldEscalateDisconnection decides whether the data-session stall watchdog
// should force a recovery cycle. We only escalate when the modem is still
// talking to the carrier (reg=home/roaming) and the error state isn't
// something a modem reset can't fix (SIM locked/missing, registration denied
// or outright failed). Searching/idle are left alone — those are signal or
// coverage issues, and power-cycling the modem won't conjure a tower.
func (s *Service) shouldEscalateDisconnection(state *modem.State) bool {
	if s.disconnectedSince.IsZero() {
		return false
	}
	if time.Since(s.disconnectedSince) < dataSessionStallTimeout {
		return false
	}
	if state.Registration != modem.RegistrationHome && state.Registration != modem.RegistrationRoaming {
		return false
	}
	switch state.ErrorState {
	case "sim-locked", "sim-missing", "registration-denied", "registration-failed":
		return false
	}
	return true
}

func (s *Service) checkAndPublishModemStatus(ctx context.Context) error {
	if err := s.checkHealth(ctx); err != nil {
		s.Logger.Printf("Health check failed: %v", err)
		// If health check fails, assume disconnected and publish minimal state
		s.publishModemState(ctx, modem.NewState(), "disconnected")
		s.publishHealthState(ctx)
		return err
	}

	currentState, err := s.Modem.GetModemInfo(s.Config.Interface)
	if err != nil {
		s.Logger.Printf("Failed to get modem info: %v", err)
		// Publish the state we got, even if partial, as it contains the ErrorState
		s.publishModemState(ctx, currentState, "disconnected")
		s.publishHealthState(ctx)
		return err // Return the original error from GetModemInfo
	}

	internetStatus := "disconnected"
	if currentState.Status == "connected" {
		// Modem reports connected, perform a real connectivity check
		connected, connErr := health.CheckInternetConnectivity(ctx, s.Config.Interface)
		if connected {
			internetStatus = "connected"
			s.connectivityFailures = 0        // Reset on success
			s.disconnectedSince = time.Time{} // Reset the stall watchdog
		} else {
			s.connectivityFailures++
			if connErr != nil {
				s.Logger.Printf("Internet connectivity check failed (%d/%d): %v", s.connectivityFailures, 3, connErr)
			} else {
				s.Logger.Printf("Internet connectivity check failed (%d/%d)", s.connectivityFailures, 3)
			}
			internetStatus = "disconnected"

			// Only trigger recovery after 3 consecutive failures
			if s.connectivityFailures >= 3 {
				// Publish the disconnected status immediately
				if err := s.publishModemState(ctx, currentState, internetStatus); err != nil {
					s.Logger.Printf("Failed to publish internet disconnected state: %v", err)
				}

				s.Logger.Printf("Modem reports connected but internet check failed %d times, attempting recovery", s.connectivityFailures)
				// Don't reset counter here - it will be reset on next successful connectivity check
				// This ensures persistent connectivity issues are detected if recovery fails
				recoveryErr := s.handleModemFailure(ctx, "internet_connectivity_failed")
				if recoveryErr != nil {
					s.Logger.Printf("Failed to initiate modem recovery: %v", recoveryErr)
				}

				// Return since we've already published the state
				return nil
			}
		}
	} else {
		internetStatus = "disconnected"
		s.connectivityFailures = 0 // Reset if modem not connected

		// Data-session stall watchdog: if we're registered to the carrier
		// but the modem's data session has been down for too long, force a
		// recovery cycle. Without this path nothing ever triggers recovery
		// for a wedged PDP context — the TCP probe above only runs while
		// status == "connected".
		if s.disconnectedSince.IsZero() {
			s.disconnectedSince = time.Now()
		}
		if s.shouldEscalateDisconnection(currentState) {
			stall := time.Since(s.disconnectedSince)
			// Publish the disconnected state first so consumers see it
			// before the recovery path runs.
			if err := s.publishModemState(ctx, currentState, internetStatus); err != nil {
				s.Logger.Printf("Failed to publish disconnected state before stall recovery: %v", err)
			}
			s.Logger.Printf("Data session stalled for %v with reg=%s error=%s, attempting recovery",
				stall.Round(time.Second), currentState.Registration, currentState.ErrorState)
			// Reset the clock so we don't re-trigger on the next tick while
			// recovery is working; handleModemFailure success will also
			// clear it naturally via the "connected" branch.
			s.disconnectedSince = time.Now()
			if recoveryErr := s.handleModemFailure(ctx, "data_session_stalled"); recoveryErr != nil {
				s.Logger.Printf("Failed to initiate stall recovery: %v", recoveryErr)
			}
			return nil
		}
	}

	if err := s.publishModemState(ctx, currentState, internetStatus); err != nil {
		s.Logger.Printf("Failed to publish state: %v", err)
		s.publishHealthState(ctx)
		return err
	}

	if err := s.publishHealthState(ctx); err != nil {
		s.Logger.Printf("Failed to publish health state: %v", err)
		return err
	}

	return nil
}

func (s *Service) queryCellLocation(ctx context.Context, state *modem.State) {
	modemPath, err := s.Modem.FindModem()
	if err != nil {
		return
	}

	locationData, err := s.MMClient.GetLocation(modemPath)
	if err != nil {
		if s.Config.Debug {
			s.Logger.Printf("Failed to get cell location data: %v", err)
		}
		return
	}

	tower, err := cell.ParseModemManagerLocation(locationData, state.AccessTech)
	if err != nil {
		if s.Config.Debug {
			s.Logger.Printf("Failed to parse cell info: %v", err)
		}
		return
	}

	// Skip API call if cell tower hasn't changed and we have a cached result
	if s.lastCellTower != nil && s.lastCellLoc != nil &&
		tower.CellId == s.lastCellTower.CellId &&
		tower.LocationAreaCode == s.lastCellTower.LocationAreaCode &&
		tower.MobileNetworkCode == s.lastCellTower.MobileNetworkCode &&
		tower.MobileCountryCode == s.lastCellTower.MobileCountryCode {
		return
	}

	result, err := cell.Geolocate(ctx, []cell.CellTower{*tower})
	if err != nil {
		if s.Config.Debug {
			s.Logger.Printf("BeaconDB lookup failed: %v", err)
		}
		return
	}

	s.lastCellTower = tower
	s.lastCellLoc = result
	s.Logger.Printf("Cell location: %.5f, %.5f (accuracy: %.0fm)", result.Latitude, result.Longitude, result.Accuracy)

	data := map[string]interface{}{
		"latitude":  fmt.Sprintf("%.6f", result.Latitude),
		"longitude": fmt.Sprintf("%.6f", result.Longitude),
		"accuracy":  fmt.Sprintf("%.0f", result.Accuracy),
		"source":    "cell",
	}
	if err := s.Redis.PublishCellLocationState(data); err != nil {
		s.Logger.Printf("Failed to publish cell location: %v", err)
	}
}

// resetGPSAfterModemRecovery tears down the GPS subsystem so the monitor
// loop reconfigures it from scratch on the next tick. A modem reset
// invalidates the AT command state, gpsd connection, and GPS timestamps,
// so a fresh EnableGPS is cleaner than trying to paper over stale clocks.
func (s *Service) resetGPSAfterModemRecovery() {
	s.Location.Close()
	s.LastGPSDataTime = time.Time{}
	s.GPSEnabledTime = time.Time{}
	s.WaitingForGPSLogged = false
	s.Location.ResetTimestampTracking()
}

func (s *Service) checkGPSHealth() error {
	now := time.Now()

	// Check if GPS data is stale (no data for 30 seconds)
	if !s.LastGPSDataTime.IsZero() && now.Sub(s.LastGPSDataTime) > 30*time.Second {
		return fmt.Errorf("gps_data_stale: no GPS data received for %v", now.Sub(s.LastGPSDataTime))
	}

	// Check if GPS timestamp is stuck (timestamp hasn't changed for 180 seconds)
	lastTsUpdate := s.Location.LastGPSTimestampUpdate()
	if !lastTsUpdate.IsZero() && now.Sub(lastTsUpdate) > location.GPSTimestampStaleness {
		return fmt.Errorf("gps_timestamp_stuck: GPS timestamp hasn't changed for %v", now.Sub(lastTsUpdate))
	}

	// Check if GPS fix is taking too long (no fix for 300 seconds since GPS was enabled)
	if !s.GPSEnabledTime.IsZero() && !s.Location.HasValidFix() && now.Sub(s.GPSEnabledTime) > 300*time.Second {
		return fmt.Errorf("gps_fix_timeout: no GPS fix established for %v", now.Sub(s.GPSEnabledTime))
	}

	return nil
}

func (s *Service) monitorStatus(ctx context.Context) {
	defer close(s.monitorDone)
	ticker := time.NewTicker(s.Config.InternetCheckTime)
	gpsTimer := time.NewTicker(location.GPSUpdateInterval)
	cellTimer := time.NewTicker(location.CellLocationUpdateInterval)
	defer ticker.Stop()
	defer gpsTimer.Stop()
	defer cellTimer.Stop()

	if err := s.checkAndPublishModemStatus(ctx); err != nil {
		s.Logger.Printf("Initial modem status check failed: %v", err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !s.modemEnabled.Load() {
				continue
			}
			if err := s.checkAndPublishModemStatus(ctx); err != nil {
				s.Logger.Printf("Periodic modem status check failed: %v", err)
			}
		case <-cellTimer.C:
			if !s.modemEnabled.Load() {
				continue
			}
			if s.cellLocationEnabled.Load() && !s.Location.HasValidFix() && s.LastState.Status == "connected" {
				s.queryCellLocation(ctx, s.LastState)
			}
		case <-gpsTimer.C:
			if !s.modemEnabled.Load() {
				continue
			}
			if !s.gpsEnabled.Load() {
				continue
			}
			if s.Health.State == health.StateNormal {
				modemPath, err := s.Modem.FindModem()
				if err != nil {
					continue
				}

				// Check if GPS recovery is in progress or the monitor is
				// gated waiting for recovery to settle.
				s.gpsRecoveryMutex.Lock()
				recoveryInProgress := s.gpsRecoveryInProgress
				gatedUntil := s.gpsRecoveryUntil
				s.gpsRecoveryMutex.Unlock()

				if recoveryInProgress || time.Now().Before(gatedUntil) {
					continue
				}

				if !s.Location.Enabled {
					if err := s.Location.EnableGPS(modemPath); err != nil {
						s.Logger.Printf("Failed to enable GPS: %v", err)
						continue
					}
					s.GPSEnabledTime = time.Now()
				}

				// Check for GPS health issues and try GPS-specific recovery first
				if err := s.checkGPSHealth(); err != nil {
					s.Logger.Printf("GPS health check failed: %v", err)
					if recoveryErr := s.handleGPSFailure(ctx, err); recoveryErr != nil {
						s.Logger.Printf("GPS recovery failed: %v", recoveryErr)
					}
					continue
				}

				// Always publish GPS status, even without valid fix
				gpsStatus := s.Location.GetGPSStatus()
				hasValidFix, _ := gpsStatus["active"].(bool)

				// Determine if we should publish GPS recovery notification
				// Check current internet status from LastState
				hasInternet := s.LastState.Status == "connected"
				publishRecovery := false

				if hasValidFix {
					// Bootstrap the system clock from GPS on the first fix of the
					// session so it's close to right immediately, even if NTP is
					// reachable but chrony hasn't synced yet. After bootstrap, only
					// keep feeding chrony GPS samples while we're offline — when we
					// have connectivity, the NTP pool is the more accurate source
					// and we don't want manual settime samples competing with it.
					needsClockSync := s.lastClockSync.IsZero() ||
						(!hasInternet && time.Since(s.lastClockSync) >= clockSyncInterval)
					if needsClockSync {
						currentLoc := s.Location.CurrentLoc()
						if s.syncClockFromGPS(currentLoc.Timestamp) {
							s.lastClockSync = time.Now()
						}
					}

					// GPS is now valid - check if this is a recovery event
					publishRecovery = s.Location.ShouldPublishRecovery(hasInternet)
					if publishRecovery {
						// Clear the fresh init flag after first successful publish
						s.Location.SetGPSFreshInit(false)
					}

					// Reset GPS lost time since we have a fix now
					s.Location.GPSLostTime = time.Time{}

					// Update GPS data timestamp when we have active GPS
					s.LastGPSDataTime = time.Now()

					// If we were waiting (flag is true), log that we got a fix
					if s.WaitingForGPSLogged {
						s.Logger.Printf("GPS fix established")
						s.WaitingForGPSLogged = false
						// Reset recovery counter since GPS is now working
						s.GPSRecoveryCount = 0
						// Disarm the cold-start timeout — it only guards
						// against "never got a fix"; the data-stale and
						// timestamp-stuck checks handle ongoing monitoring.
						s.GPSEnabledTime = time.Time{}
					}

					// TTFF: if a search was in progress, stop the clock
					// and publish. Mode is read here (at fix time) rather
					// than at wait-start because ProbeGPSMode may correct
					// our in-memory currentMode in the window between
					// wait-start and fix-established.
					if !s.ttffStart.IsZero() {
						ttff := time.Since(s.ttffStart)
						s.ttffStart = time.Time{}
						mode := s.Location.CurrentGPSMode()
						snr, _ := gpsStatus["snr"].(float64)
						satsUsed, _ := gpsStatus["satellites-used"].(int32)
						satsVisible, _ := gpsStatus["satellites-visible"].(int32)
						s.Logger.Printf("gps ttff=%.1fs mode=%s snr=%.1fdBHz sats=%d/%d",
							ttff.Seconds(), mode, snr, satsUsed, satsVisible)
						s.Redis.PublishLocationState(map[string]interface{}{
							"last_ttff_seconds": fmt.Sprintf("%.1f", ttff.Seconds()),
							"last_ttff_mode":    mode.String(),
						}, false)
					}

					// Log GPS diagnostics every 90 seconds
					if s.LastGPSQualityLog.IsZero() || time.Since(s.LastGPSQualityLog) >= 90*time.Second {
						s.Logger.Printf("gps state=%s fix=%s eph=%.1fm hdop=%.1f vdop=%.1f pdop=%.1f snr=%.1fdBHz sats=%d/%d",
							gpsStatus["state"], gpsStatus["fix"],
							gpsStatus["eph"], gpsStatus["hdop"], gpsStatus["vdop"], gpsStatus["pdop"],
							gpsStatus["snr"], gpsStatus["satellites-used"], gpsStatus["satellites-visible"])
						s.LastGPSQualityLog = time.Now()
					}

					if err := s.publishLocationState(ctx, s.Location.CurrentLoc(), publishRecovery); err != nil {
						s.Logger.Printf("Failed to publish location: %v", err)
					}
				} else {
					// GPS fix is lost - mark the time
					if s.Location.GPSLostTime.IsZero() {
						s.Location.GPSLostTime = time.Now()
					}

					// Update GPS data timestamp even when no fix, if GPS is connected
					isConnected, _ := gpsStatus["connected"].(bool)
					if isConnected {
						s.LastGPSDataTime = time.Now()
					}

					if !s.WaitingForGPSLogged {
						s.Logger.Printf("Waiting for valid GPS fix...")
						s.WaitingForGPSLogged = true
						// Start (or re-start) the TTFF clock. Mode will be
						// read at fix-establish time rather than here.
						s.ttffStart = time.Now()
					}

					// Log GPS diagnostics every 90 seconds while searching
					if s.LastGPSQualityLog.IsZero() || time.Since(s.LastGPSQualityLog) >= 90*time.Second {
						s.Logger.Printf("gps state=%s fix=%s snr=%.1fdBHz sats=%d/%d",
							gpsStatus["state"], gpsStatus["fix"],
							gpsStatus["snr"], gpsStatus["satellites-used"], gpsStatus["satellites-visible"])
						s.LastGPSQualityLog = time.Now()
					}

					// Publish just the status without location data (never
					// publish recovery when no fix). All SKY-derived fields
					// (sat counts, SNR, DOPs) pass through the cached atomics
					// — they stay meaningful during search and show
					// acquisition progress. EPH/EPS/EPT are TPV/fix-specific
					// (horizontal/speed/time error estimates of the current
					// fix) so they're zeroed to avoid lingering last-good-fix
					// values in the hash and pub/sub snapshot.
					data := map[string]interface{}{
						"fix":                gpsStatus["fix"],
						"snr":                gpsStatus["snr"],
						"active":             gpsStatus["active"],
						"connected":          gpsStatus["connected"],
						"state":              gpsStatus["state"],
						"hdop":               gpsStatus["hdop"],
						"vdop":               gpsStatus["vdop"],
						"pdop":               gpsStatus["pdop"],
						"eph":                float64(0),
						"eps":                float64(0),
						"ept":                float64(0),
						"satellites-used":    gpsStatus["satellites-used"],
						"satellites-visible": gpsStatus["satellites-visible"],
					}
					if err := s.Redis.PublishLocationState(data, false); err != nil {
						s.Logger.Printf("Failed to publish GPS status: %v", err)
					}

					// Pub/sub snapshot includes the (stale) last-known location
					// alongside zeroed quality fields. Subscribers key off
					// active=false to ignore the position; including it keeps
					// the snapshot a complete view of the hash state.
					loc := s.Location.CurrentLoc()
					snapshot := map[string]interface{}{
						"latitude":  fmt.Sprintf("%.6f", loc.Latitude),
						"longitude": fmt.Sprintf("%.6f", loc.Longitude),
						"altitude":  fmt.Sprintf("%.6f", loc.Altitude),
						"speed":     fmt.Sprintf("%.6f", loc.Speed*3.6),
						"course":    fmt.Sprintf("%.6f", loc.Course),
						"timestamp": loc.Timestamp.Format(time.RFC3339),
					}
					for k, v := range data {
						snapshot[k] = v
					}
					if err := s.Redis.PublishGPSSnapshot(snapshot); err != nil {
						s.Logger.Printf("Failed to publish GPS snapshot: %v", err)
					}
				}
			}
		}
	}
}

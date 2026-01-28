package service

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"modem-service/internal/config"
	"modem-service/internal/health"
	"modem-service/internal/location"
	"modem-service/internal/mm"
	"modem-service/internal/modem"
	redisClient "modem-service/internal/redis"
)

// Vehicle states that should trigger modem enable
var modemOnlineStates = map[string]bool{
	"parked":         true,
	"ready-to-drive": true,
}

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
	connectivityFailures  int        // Consecutive internet connectivity check failures

	// Modem enable/disable state
	modemEnabled      bool       // Target state: should modem be on?
	modemEnabledMutex sync.Mutex // Protects modemEnabled
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

	// Create modem manager
	modemMgr, err := modem.NewManager(logger, cfg.Debug)
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
	}

	service.Logger.Printf("modem-service v%s", version)

	return service, nil
}

func (s *Service) Run(ctx context.Context) error {
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

	s.Location.Close()

	return nil
}

// handleModemCommand handles enable/disable commands from pm-service
func (s *Service) handleModemCommand(command string) error {
	s.modemEnabledMutex.Lock()
	defer s.modemEnabledMutex.Unlock()

	switch command {
	case "enable":
		s.Logger.Printf("Received modem enable command")
		s.modemEnabled = true
		// Modem will be enabled by ensureModemEnabled or monitor loop
	case "disable":
		s.Logger.Printf("Received modem disable command")
		s.modemEnabled = false
		// Disable the modem
		go s.disableModem()
	default:
		s.Logger.Printf("Unknown modem command: %s", command)
	}
	return nil
}

// handleVehicleState handles vehicle state changes to auto-enable modem
func (s *Service) handleVehicleState(state string) error {
	if modemOnlineStates[state] {
		s.modemEnabledMutex.Lock()
		if !s.modemEnabled {
			s.Logger.Printf("Vehicle state '%s' - enabling modem", state)
			s.modemEnabled = true
		}
		s.modemEnabledMutex.Unlock()
	}
	return nil
}

// disableModem turns off the modem and publishes the off state
func (s *Service) disableModem() {
	s.Logger.Printf("Disabling modem...")

	// Close GPS first
	s.Location.Close()

	// Publish off states
	s.Redis.PublishInternetState("internet", "status", "disconnected")
	s.Redis.PublishInternetState("internet", "modem-state", "off")
	s.Redis.PublishModemState("power-state", "off")

	// Turn off the modem via GPIO (3.5 second pulse)
	if err := s.Modem.RestartModem(); err != nil {
		s.Logger.Printf("Failed to disable modem via GPIO: %v", err)
	}

	s.Logger.Printf("Modem disabled")
}

func (s *Service) ensureModemEnabled(ctx context.Context) error {
	if modem.IsInterfacePresent(s.Config.Interface) {
		s.Logger.Printf("Modem interface %s is already present", s.Config.Interface)
		return nil
	}

	if s.Modem.IsModemPresent() {
		s.Logger.Printf("Modem is already present via D-Bus")
		return nil
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

func (s *Service) checkHealth() error {
	// Skip health check if we're in a terminal state
	if s.Health.IsTerminal() {
		return fmt.Errorf("modem in terminal state: %s", s.Health.State)
	}

	_, err := s.Modem.FindModem()
	if err != nil {
		return s.handleModemFailure(fmt.Sprintf("no_modem_found: %v", err))
	}

	if err := s.Modem.CheckPrimaryPort(); err != nil {
		return s.handleModemFailure(fmt.Sprintf("wrong_primary_port: %v", err))
	}

	if err := s.Modem.CheckPowerState(); err != nil {
		return s.handleModemFailure(fmt.Sprintf("wrong_power_state: %v", err))
	}

	s.Health.MarkNormal()
	// Clear any active faults when modem is healthy
	s.Redis.RemoveFault(redisClient.FaultCodeModemRecoveryFailed)
	return nil
}

func (s *Service) handleModemFailure(reason string) error {
	s.Logger.Printf("Modem failure detected: %s", reason)

	if s.Health.IsRecovering() {
		return fmt.Errorf("recovery in progress")
	}

	// Be more forgiving - instead of entering terminal state, just wait longer
	if !s.Health.CanRecover() {
		s.Logger.Printf("Max recovery attempts reached, waiting before reset...")
		// Instead of entering terminal state, wait and reset recovery counter
		time.Sleep(2 * time.Minute)
		s.Health.RecoveryAttempts = 0 // Reset recovery attempts
		s.Logger.Printf("Recovery attempts reset, will try again")
		return nil // Don't fail permanently
	}

	return s.attemptRecovery()
}

func (s *Service) attemptRecovery() error {
	s.Health.StartRecovery()

	s.Logger.Printf("Attempting modem recovery (attempt %d/%d)",
		s.Health.RecoveryAttempts, health.MaxRecoveryAttempts)

	s.publishHealthState(context.Background())

	// Strategy 1: Try software reset first if modem is present
	_, err := s.Modem.FindModem()
	if err == nil {
		s.Logger.Printf("Attempting to reset the modem via D-Bus")
		if err := s.Modem.ResetModem(); err != nil {
			s.Logger.Printf("Failed to reset modem via D-Bus: %v", err)
		} else {
			// Wait for modem to recover
			time.Sleep(health.RecoveryWaitTime)

			if err := s.checkHealth(); err == nil {
				s.Logger.Printf("Modem recovery successful via D-Bus reset")
				s.Health.MarkNormal()
				s.GPSRecoveryCount = 0 // Reset GPS recovery counter on successful modem recovery
				s.publishHealthState(context.Background())
				return nil
			}
		}
	}

	// Strategy 2: Try USB unbind/bind recovery
	s.Logger.Printf("Attempting USB recovery (unbind/bind)...")
	if err := s.Modem.RecoverUSB(); err != nil {
		s.Logger.Printf("USB recovery failed: %v", err)
	} else {
		// Wait for modem to come back up after USB recovery
		ctx, cancel := context.WithTimeout(context.Background(), health.RecoveryWaitTime)
		defer cancel()

		if err := s.Modem.WaitForModem(ctx, s.Config.Interface); err == nil {
			if err := s.checkHealth(); err == nil {
				s.Logger.Printf("Modem recovery successful via USB recovery")
				s.Health.MarkNormal()
				s.GPSRecoveryCount = 0 // Reset GPS recovery counter
				s.publishHealthState(context.Background())
				return nil
			}
		}
	}

	// Strategy 3: Try hardware reset via GPIO
	s.Logger.Printf("Attempting modem restart (GPIO with D-Bus fallback)...")
	if err := s.Modem.RestartModem(); err != nil {
		s.Logger.Printf("GPIO restart failed: %v", err)
		// Don't return error immediately, try waiting longer
	} else {
		// Wait for modem to come back up
		ctx, cancel := context.WithTimeout(context.Background(), health.RecoveryWaitTime)
		defer cancel()

		if err := s.Modem.WaitForModem(ctx, s.Config.Interface); err == nil {
			if err := s.checkHealth(); err == nil {
				s.Logger.Printf("Modem recovery successful via GPIO restart")
				s.Health.MarkNormal()
				s.GPSRecoveryCount = 0 // Reset GPS recovery counter
				s.publishHealthState(context.Background())
				return nil
			}
		}
	}

	// Strategy 4: Just wait longer and hope the modem recovers
	s.Logger.Printf("Hardware recovery uncertain, waiting additional time for modem to stabilize...")
	time.Sleep(30 * time.Second)

	// Check if modem recovered during the wait
	if err := s.checkHealth(); err == nil {
		s.Logger.Printf("Modem recovered during extended wait")
		s.Health.MarkNormal()
		s.GPSRecoveryCount = 0
		s.publishHealthState(context.Background())
		return nil
	}

	// If we get here, this recovery attempt failed, but don't give up entirely
	s.Logger.Printf("Recovery attempt %d failed, will retry", s.Health.RecoveryAttempts)
	return fmt.Errorf("recovery attempt failed, will retry")
}

func (s *Service) publishHealthState(ctx context.Context) error {
	return s.Redis.PublishInternetState("internet", "modem-health", s.Health.State)
}

// handleGPSFailure attempts GPS-specific recovery before escalating to modem recovery
func (s *Service) handleGPSFailure(gpsErr error) error {
	s.Logger.Printf("Attempting GPS-specific recovery for: %v", gpsErr)

	// Try to restart GPS configuration without restarting the entire modem
	if err := s.attemptGPSRecovery(); err != nil {
		s.Logger.Printf("GPS-specific recovery failed: %v", err)
		// Only escalate to modem recovery for severe GPS issues after GPS recovery fails
		if recoveryErr := s.handleModemFailure(fmt.Sprintf("gps_stuck_after_gps_recovery: %v", gpsErr)); recoveryErr != nil {
			return fmt.Errorf("both GPS and modem recovery failed: %v", recoveryErr)
		}
	}

	return nil
}

// attemptGPSRecovery tries to recover GPS without restarting the modem
func (s *Service) attemptGPSRecovery() error {
	// Acquire lock to prevent concurrent GPS recovery attempts
	s.gpsRecoveryMutex.Lock()
	defer s.gpsRecoveryMutex.Unlock()

	// Check if recovery is already in progress
	if s.gpsRecoveryInProgress {
		s.Logger.Printf("GPS recovery already in progress, skipping duplicate attempt")
		return nil
	}

	// Mark recovery as in progress
	s.gpsRecoveryInProgress = true
	defer func() {
		s.gpsRecoveryInProgress = false
	}()

	s.GPSRecoveryCount++
	s.Logger.Printf("Attempting GPS recovery (attempt %d)...", s.GPSRecoveryCount)

	// If we've tried GPS recovery too many times, do a full reset and wait longer
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
		s.Location.LastGPSTimestamp = time.Time{}
		s.Location.LastGPSTimestampUpdate = time.Time{}

		// Wait longer before allowing monitor to re-enable GPS
		time.Sleep(30 * time.Second)
		s.Logger.Printf("GPS break complete, monitor will re-enable")
		return nil
	}

	// Stop gpsd before performing GPS-related modem reset
	s.Logger.Printf("Stopping gpsd service before GPS reset...")
	if err := s.Location.StopGPSD(); err != nil {
		s.Logger.Printf("Warning: Failed to stop gpsd: %v", err)
		// Continue with recovery even if gpsd stop fails
	}

	// Close existing GPS connection
	s.Location.Close()
	time.Sleep(2 * time.Second)

	// Reset GPS state tracking
	s.LastGPSDataTime = time.Time{}
	s.GPSEnabledTime = time.Time{}
	s.WaitingForGPSLogged = false
	s.Location.LastGPSTimestamp = time.Time{}
	s.Location.LastGPSTimestampUpdate = time.Time{}

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

// publishModemState publishes the detailed modem and derived internet state to Redis.
// It now takes the determined internetStatus as an argument.
func (s *Service) publishModemState(ctx context.Context, currentState *modem.State, internetStatus string) error {
	// Publish the actual internet connectivity status
	// Compare with LastState.Status which now stores the *last published internet status*
	// NOTE: LastState.Status here refers to the overall internet connectivity, not the raw modem status.
	if s.LastState.Status != internetStatus {
		s.Logger.Printf("internet status: %s", internetStatus)
		if err := s.Redis.PublishInternetState("internet", "status", internetStatus); err != nil {
			return err
		}
		s.LastState.Status = internetStatus // Store the published internet status
	}

	// Publish the raw modem state (1:1 copy of currentState.Status)
	if s.LastState.LastRawModemStatus != currentState.Status {
		s.Logger.Printf("internet modem-state: %s", currentState.Status)
		if err := s.Redis.PublishInternetState("internet", "modem-state", currentState.Status); err != nil {
			// Log error but don't necessarily fail the whole publish operation for this specific field
			s.Logger.Printf("Failed to publish internet modem-state: %v", err)
		}
		s.LastState.LastRawModemStatus = currentState.Status // Store the published raw modem status
	}

	// Publish modem's reported IP address (might be present even if ping fails)
	if s.LastState.IfIPAddr != currentState.IfIPAddr {
		s.Logger.Printf("internet ip-address: %s", currentState.IfIPAddr)
		if err := s.Redis.PublishInternetState("internet", "ip-address", currentState.IfIPAddr); err != nil {
			return err
		}
		s.LastState.IfIPAddr = currentState.IfIPAddr
	}

	if s.LastState.AccessTech != currentState.AccessTech {
		s.Logger.Printf("internet access-tech: %s", currentState.AccessTech)
		if err := s.Redis.PublishInternetState("internet", "access-tech", currentState.AccessTech); err != nil {
			return err
		}
		s.LastState.AccessTech = currentState.AccessTech
	}

	if s.LastState.SignalQuality != currentState.SignalQuality {
		s.Logger.Printf("internet signal-quality: %d", currentState.SignalQuality)
		if err := s.Redis.PublishInternetState("internet", "signal-quality", fmt.Sprintf("%d", currentState.SignalQuality)); err != nil {
			return err
		}
		s.LastState.SignalQuality = currentState.SignalQuality
	}

	if s.LastState.PowerState != currentState.PowerState {
		s.Logger.Printf("modem power-state: %s", currentState.PowerState)
		if err := s.Redis.PublishModemState("power-state", currentState.PowerState); err != nil {
			return err
		}
		s.LastState.PowerState = currentState.PowerState
	}

	if s.LastState.SIMState != currentState.SIMState {
		s.Logger.Printf("modem sim-state: %s", currentState.SIMState)
		if err := s.Redis.PublishModemState("sim-state", currentState.SIMState); err != nil {
			return err
		}
		s.LastState.SIMState = currentState.SIMState
	}

	if s.LastState.SIMLockStatus != currentState.SIMLockStatus {
		s.Logger.Printf("modem sim-lock: %s", currentState.SIMLockStatus)
		if err := s.Redis.PublishModemState("sim-lock", currentState.SIMLockStatus); err != nil {
			return err
		}
		s.LastState.SIMLockStatus = currentState.SIMLockStatus
	}

	if s.LastState.OperatorName != currentState.OperatorName {
		s.Logger.Printf("operator name: %s", currentState.OperatorName)
		if err := s.Redis.PublishModemState("operator-name", currentState.OperatorName); err != nil {
			return err
		}
		s.LastState.OperatorName = currentState.OperatorName
	}

	if s.LastState.OperatorCode != currentState.OperatorCode {
		s.Logger.Printf("operator code: %s", currentState.OperatorCode)
		if err := s.Redis.PublishModemState("operator-code", currentState.OperatorCode); err != nil {
			return err
		}
		s.LastState.OperatorCode = currentState.OperatorCode
	}

	if s.LastState.IsRoaming != currentState.IsRoaming {
		s.Logger.Printf("roaming: %t", currentState.IsRoaming)
		if err := s.Redis.PublishModemState("is-roaming", fmt.Sprintf("%t", currentState.IsRoaming)); err != nil {
			return err
		}
		s.LastState.IsRoaming = currentState.IsRoaming
	}

	if s.LastState.RegistrationFail != currentState.RegistrationFail {
		s.Logger.Printf("registration-fail: %s", currentState.RegistrationFail)
		if err := s.Redis.PublishModemState("registration-fail", currentState.RegistrationFail); err != nil {
			return err
		}
		s.LastState.RegistrationFail = currentState.RegistrationFail
	}

	if s.LastState.IMEI != currentState.IMEI {
		s.Logger.Printf("modem IMEI: %s", currentState.IMEI)
		if err := s.Redis.PublishInternetState("internet", "sim-imei", currentState.IMEI); err != nil {
			return err
		}
		s.LastState.IMEI = currentState.IMEI
	}

	if s.LastState.IMSI != currentState.IMSI {
		s.Logger.Printf("SIM IMSI: %s", currentState.IMSI)
		if err := s.Redis.PublishInternetState("internet", "sim-imsi", currentState.IMSI); err != nil {
			return err
		}
		s.LastState.IMSI = currentState.IMSI
	}

	if s.LastState.ICCID != currentState.ICCID {
		s.Logger.Printf("SIM ICCID: %s", currentState.ICCID)
		if err := s.Redis.PublishInternetState("internet", "sim-iccid", currentState.ICCID); err != nil {
			return err
		}
		s.LastState.ICCID = currentState.ICCID
	}

	// Publish the consolidated error state
	if s.LastState.ErrorState != currentState.ErrorState {
		s.Logger.Printf("modem error-state: %s", currentState.ErrorState)
		if err := s.Redis.PublishModemState("error-state", currentState.ErrorState); err != nil {
			// Log error but don't necessarily fail the whole publish operation
			s.Logger.Printf("Failed to publish modem error-state: %v", err)
		}
		s.LastState.ErrorState = currentState.ErrorState
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

	return s.Redis.PublishLocationState(data, publishRecovery)
}

func (s *Service) checkAndPublishModemStatus(ctx context.Context) error {
	if err := s.checkHealth(); err != nil {
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
			s.connectivityFailures = 0 // Reset on success
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
				recoveryErr := s.handleModemFailure("internet_connectivity_failed")
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

func (s *Service) checkGPSHealth() error {
	now := time.Now()

	// Check if GPS data is stale (no data for 30 seconds)
	if !s.LastGPSDataTime.IsZero() && now.Sub(s.LastGPSDataTime) > 30*time.Second {
		return fmt.Errorf("gps_data_stale: no GPS data received for %v", now.Sub(s.LastGPSDataTime))
	}

	// Check if GPS timestamp is stuck (timestamp hasn't changed for 180 seconds)
	if !s.Location.LastGPSTimestampUpdate.IsZero() && now.Sub(s.Location.LastGPSTimestampUpdate) > location.GPSTimestampStaleness {
		return fmt.Errorf("gps_timestamp_stuck: GPS timestamp hasn't changed for %v", now.Sub(s.Location.LastGPSTimestampUpdate))
	}

	// Check if GPS fix is taking too long (no fix for 300 seconds since GPS was enabled)
	if !s.GPSEnabledTime.IsZero() && !s.Location.HasValidFix && now.Sub(s.GPSEnabledTime) > 300*time.Second {
		return fmt.Errorf("gps_fix_timeout: no GPS fix established for %v", now.Sub(s.GPSEnabledTime))
	}

	return nil
}

func (s *Service) monitorStatus(ctx context.Context) {
	ticker := time.NewTicker(s.Config.InternetCheckTime)
	gpsTimer := time.NewTicker(location.GPSUpdateInterval)
	defer ticker.Stop()
	defer gpsTimer.Stop()

	if err := s.checkAndPublishModemStatus(ctx); err != nil {
		s.Logger.Printf("Initial modem status check failed: %v", err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.checkAndPublishModemStatus(ctx); err != nil {
				s.Logger.Printf("Periodic modem status check failed: %v", err)
			}
		case <-gpsTimer.C:
			if s.Health.State == health.StateNormal {
				modemPath, err := s.Modem.FindModem()
				if err != nil {
					continue
				}

				// Check if GPS recovery is in progress before attempting to enable GPS
				s.gpsRecoveryMutex.Lock()
				recoveryInProgress := s.gpsRecoveryInProgress
				s.gpsRecoveryMutex.Unlock()

				if !s.Location.Enabled && !recoveryInProgress {
					if err := s.Location.EnableGPS(modemPath); err != nil {
						s.Logger.Printf("Failed to enable GPS: %v", err)
						continue
					}
					s.GPSEnabledTime = time.Now()
				}

				// Check for GPS health issues and try GPS-specific recovery first
				if err := s.checkGPSHealth(); err != nil {
					s.Logger.Printf("GPS health check failed: %v", err)
					if recoveryErr := s.handleGPSFailure(err); recoveryErr != nil {
						s.Logger.Printf("GPS recovery failed: %v", recoveryErr)
					}
					continue
				}

				// Always publish GPS status, even without valid fix
				gpsStatus := s.Location.GetGPSStatus()
				hasValidFix := gpsStatus["active"].(bool)

				// Determine if we should publish GPS recovery notification
				// Check current internet status from LastState
				hasInternet := s.LastState.Status == "connected"
				publishRecovery := false

				if hasValidFix {
					// GPS is now valid - check if this is a recovery event
					publishRecovery = s.Location.ShouldPublishRecovery(hasInternet)
					if publishRecovery {
						// Clear the fresh init flag after first successful publish
						s.Location.GPSFreshInit = false
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
					}

					// Log GPS quality every 90 seconds (similar to signal quality)
					if s.LastGPSQualityLog.IsZero() || time.Since(s.LastGPSQualityLog) >= 90*time.Second {
						s.Logger.Printf("gps quality: %.2f", gpsStatus["quality"].(float64))
						s.LastGPSQualityLog = time.Now()
					}

					if err := s.publishLocationState(ctx, s.Location.CurrentLoc, publishRecovery); err != nil {
						s.Logger.Printf("Failed to publish location: %v", err)
					}
				} else {
					// GPS fix is lost - mark the time
					if s.Location.GPSLostTime.IsZero() {
						s.Location.GPSLostTime = time.Now()
					}

					// Update GPS data timestamp even when no fix, if GPS is connected
					if gpsStatus["connected"].(bool) {
						s.LastGPSDataTime = time.Now()
					}

					if !s.WaitingForGPSLogged {
						s.Logger.Printf("Waiting for valid GPS fix...")
						s.WaitingForGPSLogged = true
					}

					// Publish just the status without location data (never publish recovery when no fix)
					data := map[string]interface{}{
						"fix":       gpsStatus["fix"],
						"quality":   gpsStatus["quality"],
						"active":    gpsStatus["active"],
						"connected": gpsStatus["connected"],
					}
					if err := s.Redis.PublishLocationState(data, false); err != nil {
						s.Logger.Printf("Failed to publish GPS status: %v", err)
					}
				}
			}
		}
	}
}

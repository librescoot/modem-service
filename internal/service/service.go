package service

import (
	"context"
	"fmt"
	"log"
	"time"

	"modem-service/internal/config"
	"modem-service/internal/health"
	"modem-service/internal/location"
	"modem-service/internal/modem"
	redisClient "modem-service/internal/redis"
)

type Service struct {
	Config              *config.Config
	Redis               *redisClient.Client
	Logger              *log.Logger
	Health              *health.Health
	Location            *location.Service
	LastState           *modem.State
	WaitingForGPSLogged bool // Tracks if we've already logged the waiting for GPS message
}

func New(cfg *config.Config, logger *log.Logger, version string) (*Service, error) {
	redis, err := redisClient.New(cfg.RedisURL, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create Redis client: %v", err)
	}

	service := &Service{
		Config:              cfg,
		Redis:               redis,
		Logger:              logger,
		Health:              health.New(),
		Location:            location.NewService(logger, cfg.GpsdServer),
		LastState:           modem.NewState(),
		WaitingForGPSLogged: false,
	}

	service.Logger.Printf("rescoot-modem v%s", version)

	return service, nil
}

func (s *Service) Run(ctx context.Context) error {
	if err := s.Redis.Ping(ctx); err != nil {
		return fmt.Errorf("redis connection failed: %v", err)
	}

	// Try to enable the modem if it's not present
	if err := s.ensureModemEnabled(ctx); err != nil {
		s.Logger.Printf("SEVERE ERROR: Failed to ensure modem is enabled: %v", err)

		// If the modem interface is still not present, we cannot continue
		if !modem.IsInterfacePresent(s.Config.Interface) && !modem.IsDBusPresent() {
			s.Logger.Printf("Cannot continue without modem interface or dbus presence")
			return fmt.Errorf("modem not available: %v", err)
		}
	}

	s.Logger.Printf("Starting modem service on interface %s", s.Config.Interface)
	go s.monitorStatus(ctx)

	<-ctx.Done()

	s.Location.Close()

	return nil
}

func (s *Service) ensureModemEnabled(ctx context.Context) error {
	if modem.IsInterfacePresent(s.Config.Interface) {
		s.Logger.Printf("Modem interface %s is already present", s.Config.Interface)
		return nil
	}

	if modem.IsDBusPresent() {
		s.Logger.Printf("Modem is already present via mmcli/dbus")
		return nil
	}

	s.Logger.Printf("Modem not detected, will attempt to enable via GPIO pin %d", modem.GPIOPin)

	// Try multiple times with increasing wait times
	for attempt := range health.MaxRecoveryAttempts {
		waitTime := min(time.Duration(60*(attempt+1))*time.Second, 300*time.Second)

		s.Logger.Printf("Modem start attempt %d/%d with %v wait time",
			attempt+1, health.MaxRecoveryAttempts, waitTime)

		if err := modem.StartModem(); err != nil {
			s.Logger.Printf("Failed to start modem: %v", err)
			continue
		}

		attemptCtx, cancel := context.WithTimeout(ctx, waitTime)

		err := modem.WaitForModem(attemptCtx, s.Config.Interface, s.Logger)
		cancel()

		if err == nil {
			s.Logger.Printf("Modem successfully enabled on attempt %d", attempt+1)
			return nil
		}

		s.Logger.Printf("Modem did not come up after attempt %d: %v", attempt+1, err)

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

	return fmt.Errorf("modem failed to come up after multiple attempts, marked as potentially defective")
}

func (s *Service) checkHealth() error {
	// Skip health check if we're in a terminal state
	if s.Health.IsTerminal() {
		return fmt.Errorf("modem in terminal state: %s", s.Health.State)
	}

	modemID, err := modem.FindModemID()
	if err != nil {
		return s.handleModemFailure(fmt.Sprintf("no_modem_found: %v", err))
	}

	if err := modem.CheckPrimaryPort(modemID); err != nil {
		return s.handleModemFailure(fmt.Sprintf("wrong_primary_port: %v", err))
	}

	if err := modem.CheckPowerState(modemID); err != nil {
		return s.handleModemFailure(fmt.Sprintf("wrong_power_state: %v", err))
	}

	s.Health.MarkNormal()
	return nil
}

func (s *Service) handleModemFailure(reason string) error {
	s.Logger.Printf("Modem failure detected: %s", reason)

	if s.Health.IsRecovering() {
		return fmt.Errorf("recovery in progress")
	}

	if !s.Health.CanRecover() {
		s.Health.MarkRecoveryFailed()
		s.publishHealthState(context.Background())
		return fmt.Errorf("max recovery attempts reached")
	}

	return s.attemptRecovery()
}

func (s *Service) attemptRecovery() error {
	s.Health.StartRecovery()

	s.Logger.Printf("Attempting modem recovery (attempt %d/%d)",
		s.Health.RecoveryAttempts, health.MaxRecoveryAttempts)

	s.publishHealthState(context.Background())

	// Try software reset first if modem is present
	modemID, err := modem.FindModemID()
	if err == nil {
		// Try mmcli reset
		s.Logger.Printf("Attempting to reset the modem via mmcli")
		if err := modem.ResetModem(modemID); err != nil {
			s.Logger.Printf("Failed to reset modem via mmcli: %v", err)
		} else {
			time.Sleep(health.RecoveryWaitTime)

			if err := s.checkHealth(); err == nil {
				s.Logger.Printf("Modem recovery successful via mmcli reset")
				s.Health.MarkNormal()
				s.publishHealthState(context.Background())
				return nil
			}
		}
	}

	// If software reset failed or modem not found, try hardware reset via GPIO (which now includes mmcli fallback)
	s.Logger.Printf("Attempting modem restart (GPIO with mmcli fallback)...")
	if err := modem.RestartModem(s.Logger); err != nil {
		s.Logger.Printf("Modem restart attempt failed: %v", err)
		return err // Return the combined error from RestartModem
	}

	ctx, cancel := context.WithTimeout(context.Background(), health.RecoveryWaitTime)
	defer cancel()

	if err := modem.WaitForModem(ctx, s.Config.Interface, s.Logger); err != nil {
		s.Logger.Printf("Modem did not come up after GPIO restart: %v", err)
		return err
	}

	if err := s.checkHealth(); err != nil {
		s.Logger.Printf("Recovery failed after GPIO restart: %v", err)
		return err
	}

	s.Logger.Printf("Modem recovery successful via GPIO restart")
	s.Health.MarkNormal()
	s.publishHealthState(context.Background())
	return nil
}

func (s *Service) publishHealthState(ctx context.Context) error {
	return s.Redis.PublishInternetState(ctx, "internet", "modem-health", s.Health.State)
}

// publishModemState publishes the detailed modem and derived internet state to Redis.
// It now takes the determined internetStatus as an argument.
func (s *Service) publishModemState(ctx context.Context, currentState *modem.State, internetStatus string) error {
	// Publish the actual internet connectivity status
	// Compare with LastState.Status which now stores the *last published internet status*
	// NOTE: LastState.Status here refers to the overall internet connectivity, not the raw modem status.
	if s.LastState.Status != internetStatus {
		s.Logger.Printf("internet status: %s", internetStatus)
		if err := s.Redis.PublishInternetState(ctx, "internet", "status", internetStatus); err != nil {
			return err
		}
		s.LastState.Status = internetStatus // Store the published internet status
	}

	// Publish the raw modem state (1:1 copy of currentState.Status)
	if s.LastState.LastRawModemStatus != currentState.Status {
		s.Logger.Printf("internet modem-state: %s", currentState.Status)
		if err := s.Redis.PublishInternetState(ctx, "internet", "modem-state", currentState.Status); err != nil {
			// Log error but don't necessarily fail the whole publish operation for this specific field
			s.Logger.Printf("Failed to publish internet modem-state: %v", err)
		}
		s.LastState.LastRawModemStatus = currentState.Status // Store the published raw modem status
	}

	// Publish modem's reported IP address (might be present even if ping fails)
	if s.LastState.IfIPAddr != currentState.IfIPAddr {
		s.Logger.Printf("internet ip-address: %s", currentState.IfIPAddr)
		if err := s.Redis.PublishInternetState(ctx, "internet", "ip-address", currentState.IfIPAddr); err != nil {
			return err
		}
		s.LastState.IfIPAddr = currentState.IfIPAddr
	}

	if s.LastState.AccessTech != currentState.AccessTech {
		s.Logger.Printf("internet access-tech: %s", currentState.AccessTech)
		if err := s.Redis.PublishInternetState(ctx, "internet", "access-tech", currentState.AccessTech); err != nil {
			return err
		}
		s.LastState.AccessTech = currentState.AccessTech
	}

	if s.LastState.SignalQuality != currentState.SignalQuality {
		s.Logger.Printf("internet signal-quality: %d", currentState.SignalQuality)
		if err := s.Redis.PublishInternetState(ctx, "internet", "signal-quality", fmt.Sprintf("%d", currentState.SignalQuality)); err != nil {
			return err
		}
		s.LastState.SignalQuality = currentState.SignalQuality
	}

	if s.LastState.PowerState != currentState.PowerState {
		s.Logger.Printf("modem power-state: %s", currentState.PowerState)
		if err := s.Redis.PublishModemState(ctx, "power-state", currentState.PowerState); err != nil {
			return err
		}
		s.LastState.PowerState = currentState.PowerState
	}

	if s.LastState.SIMState != currentState.SIMState {
		s.Logger.Printf("modem sim-state: %s", currentState.SIMState)
		if err := s.Redis.PublishModemState(ctx, "sim-state", currentState.SIMState); err != nil {
			return err
		}
		s.LastState.SIMState = currentState.SIMState
	}

	if s.LastState.SIMLockStatus != currentState.SIMLockStatus {
		s.Logger.Printf("modem sim-lock: %s", currentState.SIMLockStatus)
		if err := s.Redis.PublishModemState(ctx, "sim-lock", currentState.SIMLockStatus); err != nil {
			return err
		}
		s.LastState.SIMLockStatus = currentState.SIMLockStatus
	}

	if s.LastState.OperatorName != currentState.OperatorName {
		s.Logger.Printf("operator name: %s", currentState.OperatorName)
		if err := s.Redis.PublishModemState(ctx, "operator-name", currentState.OperatorName); err != nil {
			return err
		}
		s.LastState.OperatorName = currentState.OperatorName
	}

	if s.LastState.OperatorCode != currentState.OperatorCode {
		s.Logger.Printf("operator code: %s", currentState.OperatorCode)
		if err := s.Redis.PublishModemState(ctx, "operator-code", currentState.OperatorCode); err != nil {
			return err
		}
		s.LastState.OperatorCode = currentState.OperatorCode
	}

	if s.LastState.IsRoaming != currentState.IsRoaming {
		s.Logger.Printf("roaming: %t", currentState.IsRoaming)
		if err := s.Redis.PublishModemState(ctx, "is-roaming", fmt.Sprintf("%t", currentState.IsRoaming)); err != nil {
			return err
		}
		s.LastState.IsRoaming = currentState.IsRoaming
	}

	if s.LastState.RegistrationFail != currentState.RegistrationFail {
		s.Logger.Printf("registration-fail: %s", currentState.RegistrationFail)
		if err := s.Redis.PublishModemState(ctx, "registration-fail", currentState.RegistrationFail); err != nil {
			return err
		}
		s.LastState.RegistrationFail = currentState.RegistrationFail
	}

	if s.LastState.IMEI != currentState.IMEI {
		s.Logger.Printf("modem IMEI: %s", currentState.IMEI)
		if err := s.Redis.PublishInternetState(ctx, "internet", "sim-imei", currentState.IMEI); err != nil {
			return err
		}
		s.LastState.IMEI = currentState.IMEI
	}

	if s.LastState.IMSI != currentState.IMSI {
		s.Logger.Printf("SIM IMSI: %s", currentState.IMSI)
		if err := s.Redis.PublishInternetState(ctx, "internet", "sim-imsi", currentState.IMSI); err != nil {
			return err
		}
		s.LastState.IMSI = currentState.IMSI
	}

	if s.LastState.ICCID != currentState.ICCID {
		s.Logger.Printf("SIM ICCID: %s", currentState.ICCID)
		if err := s.Redis.PublishInternetState(ctx, "internet", "sim-iccid", currentState.ICCID); err != nil {
			return err
		}
		s.LastState.ICCID = currentState.ICCID
	}

	// Publish the consolidated error state
	if s.LastState.ErrorState != currentState.ErrorState {
		s.Logger.Printf("modem error-state: %s", currentState.ErrorState)
		if err := s.Redis.PublishModemState(ctx, "error-state", currentState.ErrorState); err != nil {
			// Log error but don't necessarily fail the whole publish operation
			s.Logger.Printf("Failed to publish modem error-state: %v", err)
		}
		s.LastState.ErrorState = currentState.ErrorState
	}

	return nil
}

func (s *Service) publishLocationState(ctx context.Context, rawLoc location.Location, filteredLoc location.Location) error {
	// Publish Raw GPS Data
	rawData := map[string]interface{}{
		"latitude":  fmt.Sprintf("%.6f", rawLoc.Latitude),
		"longitude": fmt.Sprintf("%.6f", rawLoc.Longitude),
		"altitude":  fmt.Sprintf("%.6f", rawLoc.Altitude),
		"speed":     fmt.Sprintf("%.6f", rawLoc.Speed*3.6), // Convert m/s to km/h
		"course":    fmt.Sprintf("%.6f", rawLoc.Course),
		"timestamp": rawLoc.Timestamp.Format(time.RFC3339),
	}
	gpsStatus := s.Location.GetGPSStatus() // Same status for both
	for k, v := range gpsStatus {
		rawData[k] = v
	}
	if err := s.Redis.PublishLocationState(ctx, rawData); err != nil {
		s.Logger.Printf("Failed to publish raw location: %v", err)
		// Potentially return err here or just log
	}

	// Publish Filtered GPS Data
	filteredData := map[string]interface{}{
		"latitude":  fmt.Sprintf("%.6f", filteredLoc.Latitude),
		"longitude": fmt.Sprintf("%.6f", filteredLoc.Longitude),
		"altitude":  fmt.Sprintf("%.6f", filteredLoc.Altitude),
		"speed":     fmt.Sprintf("%.6f", filteredLoc.Speed*3.6), // Convert m/s to km/h
		"course":    fmt.Sprintf("%.6f", filteredLoc.Course),
		"timestamp": filteredLoc.Timestamp.Format(time.RFC3339), // Use filtered timestamp
	}
	for k, v := range gpsStatus { // Add status to filtered data as well
		filteredData[k] = v
	}
	return s.Redis.PublishFilteredLocationState(ctx, filteredData)
}

func (s *Service) checkAndPublishModemStatus(ctx context.Context) error {
	if err := s.checkHealth(); err != nil {
		s.Logger.Printf("Health check failed: %v", err)
		// If health check fails, assume disconnected and publish minimal state
		s.publishModemState(ctx, modem.NewState(), "disconnected")
		s.publishHealthState(ctx)
		return err
	}

	currentState, err := modem.GetModemInfo(s.Config.Interface, s.Logger)
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
		connected, pingErr := health.CheckInternetConnectivity(ctx, s.Config.Interface)
		if connected {
			internetStatus = "connected"
		} else {
			if pingErr != nil {
				s.Logger.Printf("Internet connectivity check failed: %v", pingErr)
			} else {
				s.Logger.Printf("Internet connectivity check failed (ping unsuccessful)")
			}
			internetStatus = "disconnected"

			// Publish the disconnected status immediately
			if err := s.publishModemState(ctx, currentState, internetStatus); err != nil {
				s.Logger.Printf("Failed to publish internet disconnected state: %v", err)
			}

			// After publishing status, trigger modem recovery
			s.Logger.Printf("Modem reports connected but internet check failed, attempting recovery")
			recoveryErr := s.handleModemFailure("internet_connectivity_failed")
			if recoveryErr != nil {
				s.Logger.Printf("Failed to initiate modem recovery: %v", recoveryErr)
			}

			// Return since we've already published the state
			return nil
		}
	} else {
		internetStatus = "disconnected"
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
				modemID, err := modem.FindModemID()
				if err != nil {
					continue
				}

				if !s.Location.Enabled {
					if err := s.Location.EnableGPS(modemID); err != nil {
						s.Logger.Printf("Failed to enable GPS: %v", err)
						continue
					}
				}

				// Always publish GPS status, even without valid fix
				gpsStatus := s.Location.GetGPSStatus()
				if gpsStatus["active"].(bool) {
					// If we were waiting (flag is true), log that we got a fix
					if s.WaitingForGPSLogged {
						s.Logger.Printf("GPS fix established")
						s.WaitingForGPSLogged = false
					}

					// s.Location.CurrentLoc is already the filtered location
					// We need the raw location before filtering to publish it.
					// This requires a change in location.Service to expose raw location or pass it through.
					// For now, let's assume s.Location.CurrentLoc is what we want for filtered,
					// and we'd need to get the raw one.
					// Let's modify location.go to store both raw and filtered, or pass raw through TPV.
					// Quickest path for now: assume CurrentLoc is filtered. We need raw.
					// The TPV handler in location.go now sets s.CurrentLoc to the filtered one.
					// We need to access the raw one that was passed to s.Filter.FilterLocation()
					// This implies a design change in how location data flows or is stored in location.Service.

					// Simplification for now: We will publish the filtered location to both endpoints.
					// This is not ideal but avoids further refactoring of location.go in this step.
					// The user can refine this later if they need distinct raw data.
					// OR, we can assume that s.Location.Filter.lastLocation is the *input* to the filter for the current s.Location.CurrentLoc
					// This is a bit of a hack.
					// A cleaner way would be for FilterLocation to return both raw and filtered, or for location.Service to store both.

					// Let's assume s.Location.CurrentLoc is the *filtered* one.
					// And s.Location.Filter.lastLocation was the *raw* input that produced the current filtered output.
					// This is not strictly true by the current filter.go logic, as lastLocation is updated with filtered.
					//
					// The TPV handler in location.go does:
					// rawLocation := Location{... from report ...}
					// s.CurrentLoc = s.Filter.FilterLocation(rawLocation)
					// So, to get the raw location that corresponds to s.CurrentLoc, we'd need to have stored `rawLocation` in the location.Service
					// or have the filter return it.
					//
					// Let's make a temporary adjustment to publishLocationState to accept only one Location
					// and publish it to both, and then I'll suggest a follow-up to properly separate raw/filtered.
					// For now, we'll publish the filtered location to both raw and filtered Redis keys.

					// The `publishLocationState` function now expects raw and filtered.
					// The `s.Location.CurrentLoc` is the filtered location.
					// We need to get the raw location. The filter doesn't directly expose the last raw input.
					// The location service's TPV handler creates `rawLocation` then passes it to the filter.
					// We need to make `rawLocation` available here.
					//
					// Let's modify location.Service to store LastRawLocation
					// Add `LastRawLocation Location` to `location.Service` struct
					// In TPV handler:
					//   s.LastRawLocation = rawLocation // Store it
					//   s.CurrentLoc = s.Filter.FilterLocation(rawLocation)
					// Then here:
					//   err := s.publishLocationState(ctx, s.Location.LastRawLocation, s.Location.CurrentLoc)

					// This requires another edit to location.go. I will do that first.
					// For now, I will proceed with the current structure and publish CurrentLoc (filtered) to both.
					// This is a known limitation to be addressed.

					// Correct approach: publishLocationState should take the filtered location.
					// The raw data is implicitly handled by the fact that CurrentLoc *is* the filtered one.
					// The request was to publish *filtered* data to `gps:filtered` and keep `gps` as raw.
					// This means `publishLocationState` needs to be called with the *raw* data for the `gps` key,
					// and then separately with *filtered* data for `gps:filtered`.
					//
					// The current `s.Location.CurrentLoc` IS the filtered location.
					// We need the raw data that was used to produce this.
					// The `location.go` TPV handler has `rawLocation`. We need to pass this up.
					//
					// Plan:
					// 1. Modify `location.Service` to have `LastRawReportedLocation Location`
					// 2. In `location.go` TPV handler, after creating `rawLocation`, set `s.LastRawReportedLocation = rawLocation`
					// 3. In `service.go` `monitorStatus`, call `s.publishLocationState(ctx, s.Location.LastRawReportedLocation, s.Location.CurrentLoc)`

					// Given I can only do one file edit at a time, I will make a placeholder here
					// and then edit location.go, then come back to service.go.
					// For now, I will call publishLocationState with s.Location.CurrentLoc for both arguments.
					// This is incorrect but allows the code to compile.
					// UPDATE: Now that LastRawReportedLocation is available in location.Service:
					if err := s.publishLocationState(ctx, s.Location.LastRawReportedLocation, s.Location.CurrentLoc); err != nil {
						s.Logger.Printf("Failed to publish location: %v", err)
					}
				} else {
					if !s.WaitingForGPSLogged {
						s.Logger.Printf("Waiting for valid GPS fix...")
						s.WaitingForGPSLogged = true
					}

					// Publish just the status without location data
					data := map[string]interface{}{
						"fix":       gpsStatus["fix"],
						"quality":   gpsStatus["quality"],
						"active":    gpsStatus["active"],
						"connected": gpsStatus["connected"],
					}
					if err := s.Redis.PublishLocationState(ctx, data); err != nil {
						s.Logger.Printf("Failed to publish GPS status: %v", err)
					}
				}
			}
		}
	}
}

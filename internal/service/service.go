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

	// If software reset failed or modem not found, try hardware reset via GPIO
	s.Logger.Printf("Attempting to restart modem via GPIO pin %d", modem.GPIOPin)
	if err := modem.RestartModem(); err != nil {
		s.Logger.Printf("Failed to restart modem via GPIO: %v", err)
		return err
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

func (s *Service) publishModemState(ctx context.Context, currentState *modem.State) error {
	if s.LastState.Status != currentState.Status {
		s.Logger.Printf("internet modem-state: %s", currentState.Status)
		if err := s.Redis.PublishInternetState(ctx, "internet", "modem-state", currentState.Status); err != nil {
			return err
		}
		s.LastState.Status = currentState.Status
	}

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

	return nil
}

func (s *Service) publishLocationState(ctx context.Context, loc location.Location) error {
	data := map[string]interface{}{
		"latitude":  fmt.Sprintf("%.6f", loc.Latitude),
		"longitude": fmt.Sprintf("%.6f", loc.Longitude),
		"altitude":  fmt.Sprintf("%.6f", loc.Altitude),
		"speed":     fmt.Sprintf("%.6f", loc.Speed),
		"course":    fmt.Sprintf("%.6f", loc.Course),
		"timestamp": loc.Timestamp.Format(time.RFC3339),
	}

	// Add GPS status fields
	gpsStatus := s.Location.GetGPSStatus()
	for k, v := range gpsStatus {
		data[k] = v
	}

	return s.Redis.PublishLocationState(ctx, data)
}

func (s *Service) checkAndPublishModemStatus(ctx context.Context) error {
	if err := s.checkHealth(); err != nil {
		s.Logger.Printf("Health check failed: %v", err)
		return err
	}

	currentState, err := modem.GetModemInfo(s.Config.Interface, s.Logger)
	if err != nil {
		s.Logger.Printf("Failed to get modem info: %v", err)
		currentState.Status = "off"
	}

	if err := s.publishModemState(ctx, currentState); err != nil {
		s.Logger.Printf("Failed to publish state: %v", err)
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

					if err := s.publishLocationState(ctx, s.Location.CurrentLoc); err != nil {
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

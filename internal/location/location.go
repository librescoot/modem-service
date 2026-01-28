package location

import (
	"context"
	"fmt"
	"log"
	"modem-service/internal/mm"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/stratoberry/go-gpsd"
)

const (
	GPSUpdateInterval     = 1 * time.Second
	GPSTimeout            = 10 * time.Minute
	GPSTimestampStaleness = 180 * time.Second
	MaxGPSRetries         = 10
	GPSRetryInterval      = 5 * time.Second
	GPSConfigTimeout      = 30 * time.Second
	MaxConfigRetries      = 3
)

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
	ModemPath              dbus.ObjectPath
	MMClient               *mm.Client
	Config                 Config
	LastFix                time.Time
	CurrentLoc             Location
	Enabled                bool
	Logger                 *log.Logger
	GpsdConn               *gpsd.Session
	GpsdServer             string
	Done                   chan bool
	HasValidFix            bool
	FixMode                string  // "none", "2d", "3d"
	Quality                float64 // DOP value
	HDOP                   float64 // Horizontal Dilution of Precision
	VDOP                   float64 // Vertical Dilution of Precision
	PDOP                   float64 // Position (3D) Dilution of Precision
	EPH                    float64 // Estimated horizontal position error (meters)
	GpsdConnected          bool
	State                  string     // "off", "searching", "fix-established", "error"
	GPSLostTime            time.Time  // Time when GPS fix was lost
	GPSFreshInit           bool       // True if GPS has just been initialized
	LastDataReceived       time.Time  // Last time any GPS data was received (even without fix)
	LastGPSTimestamp       time.Time  // Last GPS timestamp received from GPSD
	LastGPSTimestampUpdate time.Time  // When we last saw the GPS timestamp change
	configMutex            sync.Mutex   // Protects GPS configuration to prevent concurrent attempts
	monitoringActive       bool         // True if monitoring goroutine is already running
	stopChan               chan struct{} // Signals monitoring goroutine to stop
}

func NewService(logger *log.Logger, gpsdServer string, mmClient *mm.Client, suplServer string) *Service {
	if suplServer == "" {
		suplServer = "supl.google.com:7275"
	}

	return &Service{
		Config: Config{
			SuplServer:     suplServer,
			RefreshRate:    GPSUpdateInterval,
			AccuracyThresh: 50.0,
			AntennaVoltage: 3.05,
		},
		MMClient:     mmClient,
		Logger:       logger,
		GpsdServer:   gpsdServer,
		Done:         make(chan bool),
		HasValidFix:  false,
		State:        "off",
		GPSFreshInit: true,
	}
}

func (s *Service) EnableGPS(modemPath dbus.ObjectPath) error {
	s.ModemPath = modemPath
	s.Enabled = true

	// Prevent multiple monitoring goroutines from running
	if s.monitoringActive {
		s.Logger.Printf("GPS monitoring already active, skipping duplicate EnableGPS call")
		return nil
	}

	s.monitoringActive = true
	s.stopChan = make(chan struct{})

	go func() {
		defer func() {
			s.monitoringActive = false
		}()

		attempt := 0
		for {
			// Check both Enabled flag and stop channel for shutdown
			select {
			case <-s.stopChan:
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
					// (GPS might already be running from previous service instance)
					if attempt == 0 && s.GPSFreshInit {
						s.Logger.Printf("Trying to connect to existing gpsd...")
						if err := s.connectToGPSD(); err == nil {
							s.Logger.Printf("Connected to gpsd, checking for GPS data...")
							s.configMutex.Unlock()

							// Wait briefly to see if we get GPS data
							select {
							case <-s.stopChan:
								return
							case <-time.After(3 * time.Second):
							}

							if s.LastDataReceived.After(time.Now().Add(-5 * time.Second)) {
								s.Logger.Printf("GPS already running, reusing existing connection")
								s.GPSFreshInit = false
								attempt = 0
								continue
							}

							// No data, need to reconfigure
							s.Logger.Printf("No GPS data received from gpsd, will reconfigure")
							s.configMutex.Lock()
							if s.GpsdConn != nil {
								s.GpsdConn.Close()
								s.GpsdConn = nil
							}
						}
						s.GPSFreshInit = false
					}

					s.Logger.Printf("Configuring GPS (attempt %d)", attempt+1)

					if err := s.configureGPS(); err != nil {
						s.Logger.Printf("GPS configuration attempt %d failed: %v", attempt+1, err)
						s.configMutex.Unlock()
						select {
						case <-s.stopChan:
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
						case <-s.stopChan:
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

			if s.HasValidFix && time.Since(s.LastFix) > GPSTimeout {
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
			case <-s.stopChan:
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

	// Configure SUPL server
	if err := s.configureSuplServer(ctx, status); err != nil {
		return fmt.Errorf("failed to configure SUPL server: %v", err)
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

// sendATCommand is a helper to send AT commands with context and logging support
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
// Uses direct modem AT commands for comprehensive GPS setup including antenna power,
// XTRA assisted GPS, and accuracy thresholds for faster and more reliable GPS fixes
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

	// Disable automatic GPS start and standalone mode
	s.sendATCommand(ctx, "AT+CGPSAUTO=0", false)
	s.sendATCommand(ctx, "AT+CGPSMSB=0", false)

	// Set accuracy threshold (50 meters - higher = faster fix)
	accuracyMeters := int(s.Config.AccuracyThresh)
	cmd := fmt.Sprintf("AT+CGPSHOR=%d", accuracyMeters)
	s.sendATCommand(ctx, cmd, false)
	s.Logger.Printf("GPS accuracy threshold: %dm", accuracyMeters)

	// Set GPS antenna GPIO (GPIO 41 as output, high)
	s.sendATCommand(ctx, "AT+CGDRT=41,1", false)
	s.sendATCommand(ctx, "AT+CGSETV=41,1", false)

	// Set GPS clock from system time
	s.setGPSClockFromSystem(ctx)

	// Configure APN and socket context (optional)
	s.sendATCommand(ctx, `AT+CGDCONT=1,"IP","internet"`, false)
	s.sendATCommand(ctx, `AT+CGSOCKCONT=1,"IP","internet"`, false)

	// Enable GPS XTRA assisted GPS (faster fix)
	s.sendATCommand(ctx, "AT+CGPSXE=1", false)
	s.sendATCommand(ctx, "AT+CGPSSSL=0", false)
	s.sendATCommand(ctx, "AT+CGPSXD=0", false)
	s.sendATCommand(ctx, "AT+CGPSXDAUTO=1", false)
	s.Logger.Printf("GPS XTRA assisted GPS enabled")

	// Configure NMEA and positioning mode
	s.sendATCommand(ctx, "AT+CGPSNMEA=511", false)
	s.sendATCommand(ctx, "AT+CGPSPMD=7", false)
	return nil
}

// configureAntennaPower configures the GPS antenna power supply
// CRITICAL: This can reset to 2950mV after reboot which prevents GPS from working
// Must be called on every GPS enable, not just initial configuration
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
	s.updateGPSClockFromSystem(ctx)

	// Check if GPS is enabled, start it if needed
	response, err := s.sendATCommand(ctx, "AT+CGPS?", false)
	if err == nil && !strings.Contains(response, "+CGPS: 1,1") && !strings.Contains(response, "+CGPS:1,1") {
		s.sendATCommand(ctx, "AT+CGPS=1,1", false)
		s.Logger.Printf("GPS started")
	}

	// Set GPS notification mode
	s.sendATCommand(ctx, "AT+CGPSNOTIFY=0", false)

	return nil
}

// setGPSClockFromSystem sets the modem's GPS clock from system time
func (s *Service) setGPSClockFromSystem(ctx context.Context) error {
	now := time.Now().UTC()
	clockCmd := fmt.Sprintf(`AT+CCLK="%s"`, now.Format("06/01/02,15:04:05+00"))
	s.sendATCommand(ctx, clockCmd, false)
	s.Logger.Printf("Set GPS clock: %s", now.Format("2006-01-02 15:04:05 MST"))
	return nil
}

// updateGPSClockFromSystem updates the modem's GPS clock from system time (used after antenna power up)
func (s *Service) updateGPSClockFromSystem(ctx context.Context) error {
	now := time.Now().UTC()
	clockCmd := fmt.Sprintf(`AT+CCLK="%s"`, now.Format("06/01/02,15:04:05+00"))
	s.sendATCommand(ctx, clockCmd, false)
	s.Logger.Printf("Updated GPS clock: %s", now.Format("2006-01-02 15:04:05 MST"))
	return nil
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
	SuplServer     string
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

		supl, _ := s.MMClient.GetSuplServer(s.ModemPath)

		done <- result{&LocationStatus{
			EnabledSources: enabled,
			SuplServer:     supl,
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

func (s *Service) configureSuplServer(ctx context.Context, status *LocationStatus) error {
	currentSuplServer := ""
	if status != nil {
		currentSuplServer = status.SuplServer
		s.Logger.Printf("Current SUPL server: %s", currentSuplServer)
	}

	if currentSuplServer != s.Config.SuplServer {
		s.Logger.Printf("Setting SUPL server to %s", s.Config.SuplServer)

		// Disable all sources before setting SUPL server (required by ModemManager)
		if err := s.MMClient.SetupLocation(s.ModemPath, 0, false); err != nil {
			s.Logger.Printf("Warning: Failed to disable all sources: %v", err)
		}

		// Small delay before setting SUPL server
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}

		// Set SUPL server via D-Bus
		if err := s.MMClient.SetSuplServer(s.ModemPath, s.Config.SuplServer); err != nil {
			return fmt.Errorf("failed to set SUPL server: %v", err)
		}
	} else {
		s.Logger.Printf("SUPL server already set correctly")
	}

	return nil
}

func (s *Service) enableLocationSources(ctx context.Context, currentSources uint32) error {
	// Required sources: 3gpp-lac-ci, agps-msb, gps-unmanaged
	requiredSources := mm.MMModemLocationSource3gppLacCi |
		mm.MMModemLocationSourceAgpsMsb |
		mm.MMModemLocationSourceGpsUnmanaged

	// Check which sources need to be enabled
	missingSourcesDisplay := []string{}
	if currentSources&mm.MMModemLocationSource3gppLacCi == 0 {
		missingSourcesDisplay = append(missingSourcesDisplay, "3gpp-lac-ci")
	}
	if currentSources&mm.MMModemLocationSourceAgpsMsb == 0 {
		missingSourcesDisplay = append(missingSourcesDisplay, "agps-msb")
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

		// Update DOP values
		s.HDOP = report.Hdop
		s.VDOP = report.Vdop
		s.PDOP = report.Pdop
	})

	s.GpsdConn.AddFilter("TPV", func(r interface{}) {
		report, ok := r.(*gpsd.TPVReport)
		if !ok {
			s.Logger.Printf("Error: Could not cast TPV report")
			s.State = "error"
			return
		}

		// Track when we receive any GPS data (even without fix)
		s.LastDataReceived = time.Now()

		// Update fix status
		switch report.Mode {
		case 0:
			s.FixMode = "none"
			s.State = "searching"
		case 1:
			s.FixMode = "none"
			s.State = "searching"
		case 2:
			s.FixMode = "2d"
			s.State = "fix-established"
		case 3:
			s.FixMode = "3d"
			s.State = "fix-established"
		}

		// Update quality metrics from TPV report
		s.Quality = report.Ept // Using estimated time precision as quality metric
		s.EPH = report.Eph     // Horizontal position error estimate in meters

		if report.Mode == 1 || report.Mode == 0 {
			// 0=unknown, 1=no fix
			s.HasValidFix = false
			return
		}

		rawLocation := Location{
			Latitude:  report.Lat,
			Longitude: report.Lon,
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
			rawLocation.Timestamp = report.Time

			// Track GPS timestamp changes
			if s.LastGPSTimestamp.IsZero() || !report.Time.Equal(s.LastGPSTimestamp) {
				s.LastGPSTimestamp = report.Time
				s.LastGPSTimestampUpdate = time.Now()
			}
		} else {
			rawLocation.Timestamp = time.Now()
		}

		// Store raw location directly (no filtering)
		s.CurrentLoc = rawLocation
		s.LastFix = time.Now()
		s.HasValidFix = true
	})

	// Track gpsd connection state
	s.GpsdConnected = true

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
	// Signal monitoring goroutine to stop
	if s.stopChan != nil {
		close(s.stopChan)
		s.stopChan = nil
	}
	s.Enabled = false
	s.State = "off"
	s.configMutex.Lock()
	if s.GpsdConn != nil {
		s.GpsdConn.Close()
		s.GpsdConn = nil
	}
	s.configMutex.Unlock()
	s.GpsdConnected = false
}

func (s *Service) GetGPSStatus() map[string]interface{} {
	return map[string]interface{}{
		"fix":       s.FixMode,
		"quality":   s.Quality,
		"hdop":      s.HDOP,
		"vdop":      s.VDOP,
		"pdop":      s.PDOP,
		"eph":       s.EPH,
		"active":    s.HasValidFix,
		"connected": s.GpsdConnected,
		"state":     s.State,
	}
}

// ShouldPublishRecovery determines if GPS recovery notification should be published.
// This happens when GPS becomes available after being unavailable, and only if the
// outage was significant (>5 minutes) or it's the first fix after initialization.
func (s *Service) ShouldPublishRecovery(hasInternetConnection bool) bool {
	if !hasInternetConnection {
		return false
	}

	if s.GPSFreshInit {
		return true
	}

	if !s.GPSLostTime.IsZero() {
		duration := time.Since(s.GPSLostTime)
		return duration > 5*time.Minute
	}

	return false
}

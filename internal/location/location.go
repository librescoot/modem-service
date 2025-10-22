package location

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"time"

	"github.com/rescoot/go-mmcli"
	"github.com/stratoberry/go-gpsd"
)

const (
	GPSUpdateInterval = 1 * time.Second
	GPSTimeout        = 10 * time.Minute
	MaxGPSRetries     = 10
	GPSRetryInterval  = 5 * time.Second
	GPSConfigTimeout  = 30 * time.Second
	MaxConfigRetries  = 3
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
	ModemID                 string
	Config                  Config
	LastFix                 time.Time
	CurrentLoc              Location
	Enabled                 bool
	Logger                  *log.Logger
	GpsdConn                *gpsd.Session
	GpsdServer              string
	Done                    chan bool
	HasValidFix             bool
	FixMode                 string  // "none", "2d", "3d"
	Quality                 float64 // DOP value
	HDOP                    float64 // Horizontal Dilution of Precision
	VDOP                    float64 // Vertical Dilution of Precision
	PDOP                    float64 // Position (3D) Dilution of Precision
	EPH                     float64 // Estimated horizontal position error (meters)
	GpsdConnected           bool
	State                   string // "off", "searching", "fix-established", "error"
	Filter                  *GPSFilter
	LastRawReportedLocation Location
	GPSLostTime             time.Time // Time when GPS fix was lost
	GPSFreshInit            bool      // True if GPS has just been initialized
}

func NewService(logger *log.Logger, gpsdServer string) *Service {
	// TODO: Make filter config configurable from main config
	filter := NewGPSFilter(nil)
	return &Service{
		Config: Config{
			SuplServer:     "supl.google.com:7275",
			RefreshRate:    GPSUpdateInterval,
			AccuracyThresh: 50.0,
			AntennaVoltage: 3.05,
		},
		Logger:       logger,
		GpsdServer:   gpsdServer,
		Done:         make(chan bool),
		HasValidFix:  false,
		State:        "off",
		Filter:       filter,
		GPSFreshInit: true,
	}
}

func (s *Service) EnableGPS(modemID string) error {
	s.ModemID = modemID
	s.Enabled = true

	go func() {
		attempt := 0
		for {
			if !s.Enabled {
				return
			}

			if s.GpsdConn == nil {
				s.Logger.Printf("GPS not configured or gpsd connection lost, attempting configuration (attempt %d)", attempt+1)

				if err := s.configureGPS(); err != nil {
					s.Logger.Printf("GPS configuration attempt %d failed: %v", attempt+1, err)
					time.Sleep(GPSRetryInterval)
					attempt++
					continue
				}

				if err := s.connectToGPSD(); err != nil {
					s.Logger.Printf("Failed to connect to gpsd: %v", err)
					time.Sleep(GPSRetryInterval)
					attempt++
					continue
				}

				s.Logger.Printf("Successfully connected to gpsd")
				attempt = 0
			}

			if s.HasValidFix && time.Since(s.LastFix) > GPSTimeout {
				s.Logger.Printf("No GPS updates received for %v, reconnecting", GPSTimeout)
				if s.GpsdConn != nil {
					s.GpsdConn.Close()
					s.GpsdConn = nil
				}
				continue
			}

			time.Sleep(GPSRetryInterval)
		}
	}()

	return nil
}

func (s *Service) configureGPS() error {
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
	// First, get current status with timeout
	status, err := s.getLocationStatusWithTimeout(ctx)
	if err != nil {
		s.Logger.Printf("Warning: Could not get current location status: %v", err)
		// Continue with configuration anyway
	}

	sourcesEnabled := map[string]bool{
		"3gpp-lac-ci":   false,
		"agps-msb":      false,
		"gps-unmanaged": false,
		"gps-nmea":      false,
		"gps-raw":       false,
		"cdma-bs":       false,
		"agps-msa":      false,
	}

	if status != nil {
		s.Logger.Printf("Current location sources status: %+v", status)
		for _, enabled := range status.Enabled {
			sourcesEnabled[enabled] = true
		}
	}

	// Disable conflicting sources first
	if err := s.disableConflictingSources(ctx, sourcesEnabled); err != nil {
		s.Logger.Printf("Warning: Failed to disable conflicting sources: %v", err)
	}

	// Configure SUPL server
	if err := s.configureSuplServer(ctx, status); err != nil {
		return fmt.Errorf("failed to configure SUPL server: %v", err)
	}

	// Enable required location sources
	if err := s.enableLocationSources(ctx, sourcesEnabled); err != nil {
		return fmt.Errorf("failed to enable location sources: %v", err)
	}

	return nil
}

func (s *Service) getLocationStatusWithTimeout(ctx context.Context) (*mmcli.LocationStatus, error) {
	done := make(chan struct{})
	var status *mmcli.LocationStatus
	var err error

	go func() {
		defer close(done)
		status, err = mmcli.GetLocationStatus(s.ModemID)
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-done:
		return status, err
	}
}

func (s *Service) disableConflictingSources(ctx context.Context, sourcesEnabled map[string]bool) error {
	// Disable conflicting sources one by one for better reliability
	conflictingSources := []string{"gps-nmea", "gps-raw"}

	for _, source := range conflictingSources {
		if sourcesEnabled[source] {
			s.Logger.Printf("Disabling conflicting %s location source", source)
			args := []string{"-m", s.ModemID, "--location-disable-" + source}
			if err := s.runMMCLICommand(ctx, args); err != nil {
				s.Logger.Printf("Warning: Failed to disable %s: %v", source, err)
				// Continue with other sources
			}
		}
	}
	return nil
}

func (s *Service) configureSuplServer(ctx context.Context, status *mmcli.LocationStatus) error {
	currentSuplServer := ""
	if status != nil {
		currentSuplServer = status.GPS.SuplServer
		s.Logger.Printf("Current SUPL server: %s", currentSuplServer)
	}

	if currentSuplServer != s.Config.SuplServer {
		s.Logger.Printf("Setting SUPL server to %s", s.Config.SuplServer)

		// Disable all sources individually before setting SUPL server
		allSources := []string{"3gpp", "agps-msb", "gps-unmanaged", "gps-nmea", "gps-raw", "cdma-bs", "agps-msa"}

		for _, source := range allSources {
			args := []string{"-m", s.ModemID, "--location-disable-" + source}
			if err := s.runMMCLICommand(ctx, args); err != nil {
				s.Logger.Printf("Warning: Failed to disable %s: %v", source, err)
				// Continue with other sources
			}
			// Small delay between commands
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}

		// Set SUPL server
		args := []string{"-m", s.ModemID, "--location-set-supl-server", s.Config.SuplServer}
		if err := s.runMMCLICommand(ctx, args); err != nil {
			return fmt.Errorf("failed to set SUPL server: %v", err)
		}
	} else {
		s.Logger.Printf("SUPL server already set correctly")
	}

	return nil
}

func (s *Service) enableLocationSources(ctx context.Context, sourcesEnabled map[string]bool) error {
	// Define the sources we want to enable in order
	requiredSources := []struct {
		name        string
		mmcliFlag   string
		description string
		required    bool
	}{
		{"3gpp-lac-ci", "--location-enable-3gpp", "3GPP location services", true},
		{"agps-msb", "--location-enable-agps-msb", "A-GPS", true},
		{"gps-unmanaged", "--location-enable-gps-unmanaged", "GPS unmanaged", true},
	}

	for _, source := range requiredSources {
		if !sourcesEnabled[source.name] {
			s.Logger.Printf("Attempting to enable %s location source...", source.name)

			// Try enabling with retries for critical sources
			var enableErr error
			maxRetries := 1
			if source.required {
				maxRetries = 3
			}

			for attempt := 0; attempt < maxRetries; attempt++ {
				args := []string{"-m", s.ModemID, source.mmcliFlag}
				enableErr = s.runMMCLICommand(ctx, args)
				if enableErr == nil {
					s.Logger.Printf("Successfully enabled %s location source (attempt %d)", source.name, attempt+1)
					break
				}
				s.Logger.Printf("Warning: Failed to enable %s (attempt %d/%d): %v", source.name, attempt+1, maxRetries, enableErr)

				// Brief pause before retry
				if attempt < maxRetries-1 {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(1 * time.Second):
					}
				}
			}

			if enableErr != nil {
				if source.required {
					return fmt.Errorf("failed to enable required %s after %d attempts: %v", source.description, maxRetries, enableErr)
				} else {
					s.Logger.Printf("Warning: Failed to enable optional %s: %v", source.description, enableErr)
				}
			}
		} else {
			s.Logger.Printf("%s location source already enabled", source.name)
		}

		// Small delay between enabling different sources
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
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
		// Don't fail the entire operation if gpsd restart fails
	} else {
		s.Logger.Printf("Successfully restarted gpsd service")
	}

	return nil
}

func (s *Service) runMMCLICommand(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, "mmcli", args...)
	return cmd.Run()
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
		} else {
			rawLocation.Timestamp = time.Now()
		}

		// Store raw location before filtering
		s.LastRawReportedLocation = rawLocation

		// Apply filter
		s.CurrentLoc = s.Filter.FilterLocation(rawLocation)
		// Ensure the timestamp from the raw report is preserved if the filter doesn't set it
		if s.CurrentLoc.Timestamp.IsZero() {
			s.CurrentLoc.Timestamp = rawLocation.Timestamp
		}

		s.LastFix = time.Now() // This should ideally be based on report.Time if available and reliable
		s.HasValidFix = true
	})

	// Track gpsd connection state
	s.GpsdConnected = true

	s.Done = s.GpsdConn.Watch()

	return nil
}

func (s *Service) Close() {
	s.Enabled = false
	s.State = "off"
	if s.GpsdConn != nil {
		s.GpsdConn.Close()
		s.GpsdConn = nil
	}
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

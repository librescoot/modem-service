package location

import (
	"fmt"
	"log"
	"os/exec"
	"time"

	"github.com/rescoot/go-mmcli"
	"github.com/stratoberry/go-gpsd"
)

// Constants for GPS
const (
	GPSUpdateInterval = 1 * time.Second
	GPSTimeout        = 10 * time.Minute
	MaxGPSRetries     = 10
	GPSRetryInterval  = 5 * time.Second
)

// Config holds GPS configuration
type Config struct {
	SuplServer     string
	RefreshRate    time.Duration
	AccuracyThresh float64
	AntennaVoltage float64
}

// Location represents a geographic location
type Location struct {
	Latitude  float64   `json:"latitude"`
	Longitude float64   `json:"longitude"`
	Altitude  float64   `json:"altitude"`
	Speed     float64   `json:"speed"`
	Course    float64   `json:"course"`
	Timestamp time.Time `json:"timestamp"`
}

// Service manages location services
type Service struct {
	ModemID     string
	Config      Config
	LastFix     time.Time
	CurrentLoc  Location
	Enabled     bool
	Logger      *log.Logger
	GpsdConn    *gpsd.Session
	Done        chan bool
	HasValidFix bool
}

// NewService creates a new location service
func NewService(logger *log.Logger) *Service {
	return &Service{
		Config: Config{
			SuplServer:     "supl.google.com:7275",
			RefreshRate:    GPSUpdateInterval,
			AccuracyThresh: 50.0,
			AntennaVoltage: 3.05,
		},
		Logger:      logger,
		Done:        make(chan bool),
		HasValidFix: false,
	}
}

// EnableGPS enables GPS on the modem
func (s *Service) EnableGPS(modemID string) error {
	s.ModemID = modemID

	// Try configuration multiple times
	for attempt := range MaxGPSRetries {
		if err := s.configureGPS(); err != nil {
			s.Logger.Printf("GPS configuration attempt %d failed: %v", attempt+1, err)
			time.Sleep(GPSRetryInterval)
			continue
		}

		if err := s.connectToGPSD(); err != nil {
			s.Logger.Printf("Failed to connect to gpsd: %v", err)
			time.Sleep(GPSRetryInterval)
			continue
		}
		s.Enabled = true
		return nil
	}

	return fmt.Errorf("failed to configure GPS after %d attempts", MaxGPSRetries)
}

// configureGPS configures GPS on the modem
func (s *Service) configureGPS() error {
	// Map to track which sources are enabled
	sourcesEnabled := map[string]bool{
		"3gpp-lac-ci":   false,
		"agps-msb":      false,
		"gps-unmanaged": false,
		"gps-nmea":      false,
		"gps-raw":       false,
		"cdma-bs":       false,
		"agps-msa":      false,
	}

	// Get current location status
	status, err := mmcli.GetLocationStatus(s.ModemID)
	if err != nil {
		s.Logger.Printf("Warning: Could not get current location status: %v", err)
		// Continue anyway, we'll try to set everything
	} else {
		// Log current status
		s.Logger.Printf("Current location sources status: %+v", status)

		// Check which sources are already enabled
		for _, enabled := range status.Enabled {
			sourcesEnabled[enabled] = true
		}
	}

	// Check if SuplServer is already set
	currentSuplServer := ""
	if err == nil && status != nil {
		// The status object from go-mmcli should have GPS info including SuplServer
		currentSuplServer = status.GPS.SuplServer
		s.Logger.Printf("Current SUPL server: %s", currentSuplServer)
	}

	// If SUPL server is not set to our desired value
	if currentSuplServer != s.Config.SuplServer {
		s.Logger.Printf("Setting SUPL server to %s", s.Config.SuplServer)

		// Disable all location sources first
		allSources := []string{
			"--location-disable-3gpp",
			"--location-disable-agps-msb",
			"--location-disable-gps-unmanaged",
			"--location-disable-gps-nmea",
			"--location-disable-gps-raw",
			"--location-disable-cdma-bs",
			"--location-disable-agps-msa",
		}

		// Run the disable command for all sources
		args := append([]string{"-m", s.ModemID}, allSources...)
		if err := exec.Command("mmcli", args...).Run(); err != nil {
			s.Logger.Printf("Warning: Failed to disable all location sources: %v", err)
			// Continue anyway
		}

		// Configure SUPL
		if err := exec.Command("mmcli", "-m", s.ModemID,
			"--location-set-supl-server", s.Config.SuplServer).Run(); err != nil {
			return fmt.Errorf("failed to set SUPL server: %v", err)
		}
	} else {
		s.Logger.Printf("SUPL server already set correctly, skipping configuration")
	}

	// Enable the desired sources if they're not already enabled
	s.Logger.Printf("Ensuring 3gpp-lac-ci, agps-msb, and gps-unmanaged location sources are enabled")

	// Enable 3GPP location services if not already enabled
	if !sourcesEnabled["3gpp-lac-ci"] {
		if err := exec.Command("mmcli", "-m", s.ModemID,
			"--location-enable-3gpp").Run(); err != nil {
			return fmt.Errorf("failed to enable 3GPP location services: %v", err)
		}
		s.Logger.Printf("Enabled 3gpp-lac-ci location source")
	} else {
		s.Logger.Printf("3gpp-lac-ci location source already enabled")
	}

	// Enable GPS with A-GPS if not already enabled
	if !sourcesEnabled["agps-msb"] {
		if err := exec.Command("mmcli", "-m", s.ModemID,
			"--location-enable-agps-msb").Run(); err != nil {
			return fmt.Errorf("failed to enable A-GPS: %v", err)
		}
		s.Logger.Printf("Enabled agps-msb location source")
	} else {
		s.Logger.Printf("agps-msb location source already enabled")
	}

	// Enable GPS unmanaged if not already enabled
	if !sourcesEnabled["gps-unmanaged"] {
		if err := exec.Command("mmcli", "-m", s.ModemID,
			"--location-enable-gps-unmanaged").Run(); err != nil {
			return fmt.Errorf("failed to enable GPS unmanaged: %v", err)
		}
		s.Logger.Printf("Enabled gps-unmanaged location source")
	} else {
		s.Logger.Printf("gps-unmanaged location source already enabled")
	}

	return nil
}

// connectToGPSD connects to the GPSD daemon
func (s *Service) connectToGPSD() error {
	if s.GpsdConn != nil {
		return nil
	}

	s.Logger.Printf("Connecting to gpsd on localhost:2947")
	conn, err := gpsd.Dial("localhost:2947")
	if err != nil {
		return fmt.Errorf("failed to connect to gpsd: %v", err)
	}
	if conn == nil {
		return fmt.Errorf("failed to connect to gpsd")
	}

	s.GpsdConn = conn

	// Add filter for TPV reports (Time-Position-Velocity)
	s.GpsdConn.AddFilter("TPV", func(r interface{}) {
		report, ok := r.(*gpsd.TPVReport)
		if !ok {
			s.Logger.Printf("Error: Could not cast TPV report")
			return
		}

		if report.Mode == 1 || report.Mode == 0 {
			// 0=unknown, 1=no fix
			return
		}

		// Update location from TPV report
		s.CurrentLoc.Latitude = report.Lat
		s.CurrentLoc.Longitude = report.Lon

		// These fields are optional
		if report.Alt != 0 {
			s.CurrentLoc.Altitude = report.Alt
		}
		if report.Speed != 0 {
			s.CurrentLoc.Speed = report.Speed
		}
		if report.Track != 0 {
			s.CurrentLoc.Course = report.Track
		}

		// Update timestamp if available
		if !report.Time.IsZero() {
			s.CurrentLoc.Timestamp = report.Time
		} else {
			s.CurrentLoc.Timestamp = time.Now()
		}

		s.LastFix = time.Now()
		s.HasValidFix = true
		// s.Logger.Printf("Received valid GPS fix: latitude=%.6f longitude=%.6f mode=%d",
		// 	s.CurrentLoc.Latitude, s.CurrentLoc.Longitude, report.Mode)
	})

	s.Done = s.GpsdConn.Watch()

	return nil
}

// Close closes the GPSD connection
func (s *Service) Close() {
	if s.GpsdConn != nil {
		s.GpsdConn.Close()
		s.GpsdConn = nil
	}
}

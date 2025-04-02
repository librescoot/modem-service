package location

import (
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
	ModemID       string
	Config        Config
	LastFix       time.Time
	CurrentLoc    Location
	Enabled       bool
	Logger        *log.Logger
	GpsdConn      *gpsd.Session
	GpsdServer    string
	Done          chan bool
	HasValidFix   bool
	FixMode       string  // "none", "2d", "3d"
	Quality       float64 // DOP value
	GpsdConnected bool
}

func NewService(logger *log.Logger, gpsdServer string) *Service {
	return &Service{
		Config: Config{
			SuplServer:     "supl.google.com:7275",
			RefreshRate:    GPSUpdateInterval,
			AccuracyThresh: 50.0,
			AntennaVoltage: 3.05,
		},
		Logger:      logger,
		GpsdServer:  gpsdServer,
		Done:        make(chan bool),
		HasValidFix: false,
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

	if err := s.configureGPS(); err != nil {
		s.Logger.Printf("Initial GPS configuration failed: %v", err)
	}

	if err := s.connectToGPSD(); err != nil {
		s.Logger.Printf("Initial connection to gpsd failed: %v", err)
	}

	return nil
}

func (s *Service) configureGPS() error {
	sourcesEnabled := map[string]bool{
		"3gpp-lac-ci":   false,
		"agps-msb":      false,
		"gps-unmanaged": false,
		"gps-nmea":      false,
		"gps-raw":       false,
		"cdma-bs":       false,
		"agps-msa":      false,
	}

	status, err := mmcli.GetLocationStatus(s.ModemID)
	if err != nil {
		s.Logger.Printf("Warning: Could not get current location status: %v", err)
		// Continue anyway, we'll try to set everything
	} else {
		s.Logger.Printf("Current location sources status: %+v", status)

		for _, enabled := range status.Enabled {
			sourcesEnabled[enabled] = true
		}
	}

	var conflictingSources []string

	if sourcesEnabled["gps-nmea"] {
		s.Logger.Printf("Disabling conflicting gps-nmea location source")
		conflictingSources = append(conflictingSources, "--location-disable-gps-nmea")
	}

	if sourcesEnabled["gps-raw"] {
		s.Logger.Printf("Disabling conflicting gps-raw location source")
		conflictingSources = append(conflictingSources, "--location-disable-gps-raw")
	}

	if len(conflictingSources) > 0 {
		args := append([]string{"-m", s.ModemID}, conflictingSources...)
		if err := exec.Command("mmcli", args...).Run(); err != nil {
			s.Logger.Printf("Warning: Failed to disable conflicting location sources: %v", err)
		}
	}

	currentSuplServer := ""
	if err == nil && status != nil {
		currentSuplServer = status.GPS.SuplServer
		s.Logger.Printf("Current SUPL server: %s", currentSuplServer)
	}

	if currentSuplServer != s.Config.SuplServer {
		s.Logger.Printf("Setting SUPL server to %s", s.Config.SuplServer)

		allSources := []string{
			"--location-disable-3gpp",
			"--location-disable-agps-msb",
			"--location-disable-gps-unmanaged",
			"--location-disable-gps-nmea",
			"--location-disable-gps-raw",
			"--location-disable-cdma-bs",
			"--location-disable-agps-msa",
		}

		args := append([]string{"-m", s.ModemID}, allSources...)
		if err := exec.Command("mmcli", args...).Run(); err != nil {
			s.Logger.Printf("Warning: Failed to disable all location sources: %v", err)
		}

		if err := exec.Command("mmcli", "-m", s.ModemID,
			"--location-set-supl-server", s.Config.SuplServer).Run(); err != nil {
			return fmt.Errorf("failed to set SUPL server: %v", err)
		}
	} else {
		s.Logger.Printf("SUPL server already set correctly")
	}

	if !sourcesEnabled["3gpp-lac-ci"] {
		if err := exec.Command("mmcli", "-m", s.ModemID,
			"--location-enable-3gpp").Run(); err != nil {
			return fmt.Errorf("failed to enable 3GPP location services: %v", err)
		}
		s.Logger.Printf("Enabled 3gpp-lac-ci location source")
	} else {
		s.Logger.Printf("3gpp-lac-ci location source already enabled")
	}

	if !sourcesEnabled["agps-msb"] {
		if err := exec.Command("mmcli", "-m", s.ModemID,
			"--location-enable-agps-msb").Run(); err != nil {
			return fmt.Errorf("failed to enable A-GPS: %v", err)
		}
		s.Logger.Printf("Enabled agps-msb location source")
	} else {
		s.Logger.Printf("agps-msb location source already enabled")
	}

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

	s.GpsdConn.AddFilter("TPV", func(r interface{}) {
		report, ok := r.(*gpsd.TPVReport)
		if !ok {
			s.Logger.Printf("Error: Could not cast TPV report")
			return
		}

		// Update fix status
		switch report.Mode {
		case 0:
			s.FixMode = "none"
		case 1:
			s.FixMode = "none"
		case 2:
			s.FixMode = "2d"
		case 3:
			s.FixMode = "3d"
		}

		// Update quality metrics
		s.Quality = report.Ept // Using estimated time precision as quality metric

		if report.Mode == 1 || report.Mode == 0 {
			// 0=unknown, 1=no fix
			s.HasValidFix = false
			return
		}

		s.CurrentLoc.Latitude = report.Lat
		s.CurrentLoc.Longitude = report.Lon

		if report.Alt != 0 {
			s.CurrentLoc.Altitude = report.Alt
		}
		if report.Speed != 0 {
			s.CurrentLoc.Speed = report.Speed
		}
		if report.Track != 0 {
			s.CurrentLoc.Course = report.Track
		}

		if !report.Time.IsZero() {
			s.CurrentLoc.Timestamp = report.Time
		} else {
			s.CurrentLoc.Timestamp = time.Now()
		}

		s.LastFix = time.Now()
		s.HasValidFix = true
	})

	// Track gpsd connection state
	s.GpsdConnected = true

	s.Done = s.GpsdConn.Watch()

	return nil
}

func (s *Service) Close() {
	s.Enabled = false
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
		"active":    s.HasValidFix,
		"connected": s.GpsdConnected,
	}
}

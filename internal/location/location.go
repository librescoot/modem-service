package location

import (
	"context"
	"fmt"
	"log"
	"modem-service/internal/mm"
	"os/exec"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/stratoberry/go-gpsd"
)

const (
	GPSUpdateInterval = 1 * time.Second
	GPSTimeout        = 10 * time.Minute
	MaxGPSRetries     = 10
	GPSRetryInterval  = 10 * time.Second
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
	ModemPath               dbus.ObjectPath
	MMClient                *mm.Client
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
	GpsdConnected           bool
	State                   string // "off", "searching", "fix-established", "error"
	Filter                  *GPSFilter
	LastRawReportedLocation Location
}

func NewService(logger *log.Logger, gpsdServer string, mmClient *mm.Client, suplServer string) *Service {
	filter := NewGPSFilter(nil)

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
		MMClient:    mmClient,
		Logger:      logger,
		GpsdServer:  gpsdServer,
		Done:        make(chan bool),
		HasValidFix: false,
		State:       "off",
		Filter:      filter,
	}
}

func (s *Service) EnableGPS(modemPath dbus.ObjectPath) error {
	s.ModemPath = modemPath
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
	s.Logger.Printf("Configuring GPS...")

	gpsRunning, err := s.MMClient.GetGPSStatus(s.ModemPath)
	if err != nil {
		s.Logger.Printf("Warning: Failed to check GPS status: %v", err)
	}

	if !gpsRunning {
		if err := s.MMClient.ConfigureGPS(s.ModemPath); err != nil {
			s.Logger.Printf("Warning: AT command GPS configuration failed: %v", err)
		}
	}

	if gpsRunning {
		if err := s.MMClient.StopGPS(s.ModemPath); err != nil {
			s.Logger.Printf("Warning: Failed to stop GPS: %v", err)
		}
		time.Sleep(2 * time.Second)
	}

	sources := mm.MMModemLocationSource3gppLacCi |
		mm.MMModemLocationSourceGpsUnmanaged

	if err := s.MMClient.SetupLocation(s.ModemPath, sources, false); err != nil {
		return fmt.Errorf("failed to setup location sources: %v", err)
	}

	if err := s.MMClient.StartGPS(s.ModemPath, 1); err != nil {
		s.Logger.Printf("Warning: Failed to start GPS via AT command: %v", err)
	}

	s.Logger.Printf("GPS configuration complete")

	s.Logger.Printf("Restarting gpsd service...")
	restartCmd := exec.CommandContext(ctx, "systemctl", "restart", "gpsd")
	if err := restartCmd.Run(); err != nil {
		s.Logger.Printf("Warning: Failed to restart gpsd: %v", err)
	}

	return nil
}

// fallbackModemManagerConfig is used if AT command configuration fails
func (s *Service) fallbackModemManagerConfig(ctx context.Context) error {
	s.Logger.Printf("Using fallback ModemManager configuration...")

	// Enable location sources (unmanaged mode for gpsd)
	sources := mm.MMModemLocationSource3gppLacCi |
		mm.MMModemLocationSourceGpsUnmanaged

	if err := s.MMClient.SetupLocation(s.ModemPath, sources, false); err != nil {
		return fmt.Errorf("failed to setup location sources: %v", err)
	}

	return nil
}

// Removed old mmcli-based functions - now using D-Bus directly

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

		// Update quality metrics
		s.Quality = report.Ept // Using estimated time precision as quality metric

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
		"active":    s.HasValidFix,
		"connected": s.GpsdConnected,
		"state":     s.State,
	}
}

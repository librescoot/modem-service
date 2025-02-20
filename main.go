package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rescoot/go-mmcli"
)

const (
	ModemStateDefault    = "UNKNOWN"
	AccessTechDefault    = "UNKNOWN"
	SignalQualityDefault = 255
)

// Modem Health
const (
	MaxRecoveryAttempts = 2
	RecoveryWaitTime    = 30 * time.Second

	StateNormal             = "normal"
	StateRecovering         = "recovering"
	StateRecoveryFailedWait = "recovery-failed-waiting-reboot"
	StatePermanentFailure   = "permanent-failure-needs-replacement"
)

// GPS
const (
	GPSUpdateInterval = 1 * time.Second
	GPSTimeout        = 10 * time.Minute
	MaxGPSRetries     = 10
	GPSRetryInterval  = 5 * time.Second
)

type Config struct {
	redisURL          string
	pollingTime       time.Duration
	internetCheckTime time.Duration
	interface_        string
}

type ModemJSON struct {
	Modem struct {
		Generic struct {
			SignalQuality struct {
				Value string `json:"value"`
			} `json:"signal-quality"`
			State       string   `json:"state"`
			PowerState  string   `json:"power-state"`
			PrimaryPort string   `json:"primary-port"`
			EquipmentID string   `json:"equipment-identifier"`
			AccessTech  []string `json:"access-technologies"`
		} `json:"generic"`
		ThreeGPP struct {
			IMEI string `json:"imei"`
		} `json:"3gpp"`
		DBusPath string `json:"dbus-path"`
	} `json:"modem"`
}

type ModemState struct {
	status        string
	accessTech    string
	signalQuality uint8
	ipAddr        string
	ifIpAddr      string
	registration  string
	imei          string
	iccid         string
}

type ModemHealth struct {
	recoveryAttempts int
	lastRecoveryTime time.Time
	state            string
}

type ModemService struct {
	cfg            Config
	redis          *redis.Client
	logger         *log.Logger
	lastModemState ModemState
	health         *ModemHealth
	location       *LocationService
}

type GPSConfig struct {
	suplServer     string
	refreshRate    time.Duration
	accuracyThresh float64
	antennaVoltage float64
}

type Location struct {
	Latitude  float64   `json:"latitude"`
	Longitude float64   `json:"longitude"`
	Altitude  float64   `json:"altitude"`
	Speed     float64   `json:"speed"`
	Course    float64   `json:"course"`
	Timestamp time.Time `json:"timestamp"`
}

type LocationService struct {
	modemId  string
	config   GPSConfig
	lastFix  time.Time
	location Location
	enabled  bool
	logger   *log.Logger
}

func NewModemHealth() *ModemHealth {
	return &ModemHealth{
		state: StateNormal,
	}
}

func NewModemService(cfg Config, version string) *ModemService {
	opt, err := redis.ParseURL(cfg.redisURL)
	if err != nil {
		// Since this is during initialization, we should probably just panic
		panic(fmt.Sprintf("invalid redis URL: %v", err))
	}

	logger := log.New(os.Stdout, "", 0)

	service := &ModemService{
		cfg:    cfg,
		redis:  redis.NewClient(opt),
		logger: logger,
		lastModemState: ModemState{
			status:        ModemStateDefault,
			accessTech:    AccessTechDefault,
			signalQuality: SignalQualityDefault,
			ipAddr:        "UNKNOWN",
			ifIpAddr:      "UNKNOWN",
			registration:  "",
		},
		health:   NewModemHealth(),
		location: NewLocationService(logger),
	}

	service.logger.Printf("rescoot-modem v%s", version)

	return service
}

func (m *ModemService) findModemId() (string, error) {
	modemList, err := mmcli.ListModems()
	if err != nil {
		return "", fmt.Errorf("mmcli ListModems error: %v", err)
	}

	if len(modemList) == 0 {
		return "", fmt.Errorf("no modem found")
	}

	// Extract ID from DBus path
	modemPath := modemList[0]
	return strings.Split(modemPath, "/")[5], nil
}

func (m *ModemService) getModemStatus(modemId string) (*mmcli.ModemManager, error) {
	mm, err := mmcli.GetModemDetails(modemId)
	if err != nil {
		return nil, fmt.Errorf("mmcli GetModemDetails error: %v", err)
	}

	return mm, nil
}

func (m *ModemService) getModemInfo() (ModemState, error) {
	state := ModemState{
		status:        ModemStateDefault,
		accessTech:    AccessTechDefault,
		signalQuality: SignalQualityDefault,
	}

	modemId, err := m.findModemId()
	if err != nil {
		return state, err
	}

	mm, err := m.getModemStatus(modemId)
	if err != nil {
		return state, err
	}

	if quality, err := mm.SignalStrength(); err == nil {
		state.signalQuality = uint8(quality)
	}

	state.accessTech = mm.GetCurrentAccessTechnology()
	state.imei = mm.Modem.ThreeGPP.IMEI

	switch {
	case mm.Modem.Generic.PowerState != "on":
		state.status = "off"
	case mm.IsConnected():
		state.status = "connected"
	default:
		state.status = "disconnected"
	}

	if state.status == "connected" {
		if ifIP, err := m.getInterfaceIP(); err == nil {
			state.ifIpAddr = ifIP
			// if pubIP, err := m.getPublicIP(); err == nil {
			// 	state.ipAddr = pubIP
			// }
		} else {
			m.logger.Printf("Error getting interface IP: %v", err)
			state.status = "disconnected"
		}
	}

	return state, nil
}

func (m *ModemService) publishModemState(currentState ModemState) error {
	pipe := m.redis.Pipeline()
	ctx := context.Background()

	if m.lastModemState.status != currentState.status {
		m.logger.Printf("internet modem-state: %s", currentState.status)
		pipe.HSet(ctx, "internet", "modem-state", currentState.status)
		pipe.Publish(ctx, "internet", "modem-state")
		m.lastModemState.status = currentState.status
	}

	if m.lastModemState.ipAddr != currentState.ipAddr {
		m.logger.Printf("internet ip-address: %s", currentState.ipAddr)
		pipe.HSet(ctx, "internet", "ip-address", currentState.ipAddr)
		pipe.Publish(ctx, "internet", "ip-address")
		m.lastModemState.ipAddr = currentState.ipAddr
	}

	if m.lastModemState.ifIpAddr != currentState.ifIpAddr {
		m.logger.Printf("interface ip-address: %s", currentState.ifIpAddr)
		pipe.HSet(ctx, "internet", "if-ip-address", currentState.ifIpAddr)
		pipe.Publish(ctx, "internet", "if-ip-address")
		m.lastModemState.ifIpAddr = currentState.ifIpAddr
	}

	if m.lastModemState.accessTech != currentState.accessTech {
		m.logger.Printf("internet access-tech: %s", currentState.accessTech)
		pipe.HSet(ctx, "internet", "access-tech", currentState.accessTech)
		pipe.Publish(ctx, "internet", "access-tech")
		m.lastModemState.accessTech = currentState.accessTech
	}

	if m.lastModemState.signalQuality != currentState.signalQuality {
		m.logger.Printf("internet signal-quality: %d", currentState.signalQuality)
		pipe.HSet(ctx, "internet", "signal-quality", currentState.signalQuality)
		pipe.Publish(ctx, "internet", "signal-quality")
		m.lastModemState.signalQuality = currentState.signalQuality
	}

	if m.lastModemState.imei != currentState.imei {
		m.logger.Printf("IMEI: %s", currentState.imei)
		pipe.HSet(ctx, "internet", "sim-imei", currentState.imei)
		pipe.Publish(ctx, "internet", "sim-imei")
		m.lastModemState.imei = currentState.imei
	}

	if m.lastModemState.iccid != currentState.iccid {
		m.logger.Printf("ICCID: %s", currentState.iccid)
		pipe.HSet(ctx, "internet", "sim-iccid", currentState.iccid)
		pipe.Publish(ctx, "internet", "sim-iccid")
		m.lastModemState.iccid = currentState.iccid
	}

	_, err := pipe.Exec(ctx)
	if err != nil {
		m.logger.Printf("Unable to set values in redis: %v", err)
		return fmt.Errorf("cannot write to redis: %v", err)
	}

	return nil
}

func (m *ModemService) publishLocationState(loc Location) error {
	pipe := m.redis.Pipeline()
	ctx := context.Background()

	pipe.HSet(ctx, "gps", map[string]interface{}{
		"latitude":  fmt.Sprintf("%.6f", loc.Latitude),
		"longitude": fmt.Sprintf("%.6f", loc.Longitude),
		"altitude":  fmt.Sprintf("%.6f", loc.Altitude),
		"speed":     fmt.Sprintf("%.6f", loc.Speed),
		"course":    fmt.Sprintf("%.6f", loc.Course),
		"timestamp": loc.Timestamp.Format(time.RFC3339),
	})

	pipe.Publish(ctx, "gps", "location-update")

	_, err := pipe.Exec(ctx)
	return err
}

func (m *ModemService) checkHealth() error {
	// Skip health check if we're in a terminal state
	if m.health.state == StateRecoveryFailedWait || m.health.state == StatePermanentFailure {
		return fmt.Errorf("modem in terminal state: %s", m.health.state)
	}

	modemId, err := m.findModemId()
	if err != nil {
		return m.handleModemFailure(fmt.Sprintf("no_modem_found: %v", err))
	}

	// Check primary port
	if err := m.checkPrimaryPort(modemId); err != nil {
		return m.handleModemFailure(fmt.Sprintf("wrong_primary_port: %v", err))
	}

	// Check power state
	if err := m.checkPowerState(modemId); err != nil {
		return m.handleModemFailure(fmt.Sprintf("wrong_power_state: %v", err))
	}

	// If we get here, modem is healthy
	m.health.state = StateNormal
	return nil
}

func (m *ModemService) checkPrimaryPort(modemId string) error {
	mm, err := m.getModemStatus(modemId)
	if err != nil {
		return err
	}

	if mm.Modem.Generic.PrimaryPort != "cdc-wdm0" {
		return fmt.Errorf("wrong primary port: %s", mm.Modem.Generic.PrimaryPort)
	}
	return nil
}

func (m *ModemService) checkPowerState(modemId string) error {
	mm, err := m.getModemStatus(modemId)
	if err != nil {
		return err
	}

	if mm.Modem.Generic.PowerState != "on" {
		return fmt.Errorf("modem not powered on: %s", mm.Modem.Generic.PowerState)
	}
	return nil
}

func (m *ModemService) handleModemFailure(reason string) error {
	m.logger.Printf("Modem failure detected: %s", reason)

	// If we're already recovering, wait for recovery to complete
	if m.health.state == StateRecovering {
		return fmt.Errorf("recovery in progress")
	}

	// Check if we should attempt recovery
	if m.health.recoveryAttempts >= MaxRecoveryAttempts {
		if m.health.recoveryAttempts == MaxRecoveryAttempts {
			m.health.state = StateRecoveryFailedWait
		} else {
			m.health.state = StatePermanentFailure
		}
		m.publishHealthState()
		return fmt.Errorf("max recovery attempts reached")
	}

	// Start recovery process
	return m.attemptRecovery()
}

func (m *ModemService) attemptRecovery() error {
	m.health.state = StateRecovering
	m.health.recoveryAttempts++
	m.health.lastRecoveryTime = time.Now()

	m.logger.Printf("Attempting modem recovery (attempt %d/%d)",
		m.health.recoveryAttempts, MaxRecoveryAttempts)

	// Publish recovery state
	m.publishHealthState()

	// Find modem ID first
	modemId, err := m.findModemId()
	if err == nil {
		// Try mmcli reset
		m.logger.Printf("attempting to reset the modem")
		if err := exec.Command("mmcli", "-m", modemId, "--reset").Run(); err != nil {
			m.logger.Printf("Failed to reset modem: %v", err)
		}
	}

	// Wait for recovery
	time.Sleep(RecoveryWaitTime)

	// Check if recovery was successful
	if err := m.checkHealth(); err != nil {
		// TODO: try power cycling the modem to hard reset it? Does that do more than --reset?
		m.logger.Printf("Recovery failed: %v", err)
		return err
	}

	// Recovery successful
	m.logger.Printf("Modem recovery successful")
	m.health.state = StateNormal
	m.publishHealthState()
	return nil
}

func (m *ModemService) publishHealthState() error {
	ctx := context.Background()
	pipe := m.redis.Pipeline()

	pipe.HSet(ctx, "internet", "modem-health", m.health.state)
	pipe.Publish(ctx, "internet", "modem-health")

	_, err := pipe.Exec(ctx)
	return err
}

func (m *ModemService) getPublicIP() (string, error) {
	client := http.Client{
		Timeout: 10 * time.Second,
	}
	resp, err := client.Get("https://api.ipify.org/")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("bad status: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

func (m *ModemService) getInterfaceIP() (string, error) {
	iface, err := net.InterfaceByName(m.cfg.interface_)
	if err != nil {
		return "", err
	}

	addrs, err := iface.Addrs()
	if err != nil {
		return "", err
	}

	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if ok && ipNet.IP.IsGlobalUnicast() && ipNet.IP.To4() != nil {
			return ipNet.IP.String(), nil
		}
	}
	return "", fmt.Errorf("no global unicast IPv4 address found for interface %s", m.cfg.interface_)
}

func (m *ModemService) checkAndPublishModemStatus() error {
	// Check modem health
	if err := m.checkHealth(); err != nil {
		m.logger.Printf("Health check failed: %v", err)
		return err
	}

	// Get modem info
	currentState, err := m.getModemInfo()
	if err != nil {
		m.logger.Printf("Failed to get modem info: %v", err)
		currentState.status = "off"
	}

	// Publish modem state
	if err := m.publishModemState(currentState); err != nil {
		m.logger.Printf("Failed to publish state: %v", err)
		return err
	}

	// Publish health state
	if err := m.publishHealthState(); err != nil {
		m.logger.Printf("Failed to publish health state: %v", err)
		return err
	}

	return nil
}

func (m *ModemService) monitorStatus(ctx context.Context) {
	ticker := time.NewTicker(m.cfg.internetCheckTime)
	gpsTimer := time.NewTicker(GPSUpdateInterval)
	defer ticker.Stop()
	defer gpsTimer.Stop()

	if err := m.checkAndPublishModemStatus(); err != nil {
		m.logger.Printf("Initial modem status check failed: %v", err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.checkAndPublishModemStatus(); err != nil {
				m.logger.Printf("Periodic modem status check failed: %v", err)
			}
		case <-gpsTimer.C:
			if m.health.state == StateNormal {
				modemId, err := m.findModemId()
				if err != nil {
					continue
				}

				// Ensure GPS is enabled
				if !m.location.enabled {
					if err := m.location.enableGPS(modemId); err != nil {
						m.logger.Printf("Failed to enable GPS: %v", err)
						continue
					}
				}

				// Update location
				if err := m.location.updateLocation(); err != nil {
					m.logger.Printf("Failed to update location: %v", err)
					continue
				}

				// Publish to Redis
				if err := m.publishLocationState(m.location.location); err != nil {
					m.logger.Printf("Failed to publish location: %v", err)
				}
			}
		}
	}
}

func (m *ModemService) Run(ctx context.Context) error {
	if err := m.redis.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis connection failed: %v", err)
	}

	m.logger.Printf("Starting modem service on interface %s", m.cfg.interface_)
	go m.monitorStatus(ctx)

	<-ctx.Done()
	return nil
}

func NewLocationService(logger *log.Logger) *LocationService {
	return &LocationService{
		config: GPSConfig{
			suplServer:     "supl.google.com:7275",
			refreshRate:    GPSUpdateInterval,
			accuracyThresh: 50.0,
			antennaVoltage: 3.05,
		},
		logger: logger,
	}
}

func (l *LocationService) enableGPS(modemId string) error {
	l.modemId = modemId

	// Try configuration multiple times
	for attempt := 0; attempt < MaxGPSRetries; attempt++ {
		if err := l.configureGPS(); err != nil {
			l.logger.Printf("GPS configuration attempt %d failed: %v", attempt+1, err)
			time.Sleep(GPSRetryInterval)
			continue
		}
		l.enabled = true
		return nil
	}

	return fmt.Errorf("failed to configure GPS after %d attempts", MaxGPSRetries)
}

func (l *LocationService) configureGPS() error {
	// Stop any existing GPS session
	exec.Command("mmcli", "-m", l.modemId, "--location-disable-3gpp --location-disable-agps-msa --location-disable-agps-msb --location-disable-gps-nmea --location-disable-gps-raw --location-disable-cdma-bs --location-disable-gps-unmanaged").Run()

	// Configure SUPL
	if err := exec.Command("mmcli", "-m", l.modemId,
		"--location-set-supl-server", l.config.suplServer).Run(); err != nil {
		return fmt.Errorf("failed to set SUPL server: %v", err)
	}

	// Enable 3GPP location services
	if err := exec.Command("mmcli", "-m", l.modemId,
		"--location-enable-3gpp").Run(); err != nil {
		return fmt.Errorf("failed to enable 3GPP location services: %v", err)
	}

	// Enable GPS with A-GPS
	if err := exec.Command("mmcli", "-m", l.modemId,
		"--location-enable-agps-msb").Run(); err != nil {
		return fmt.Errorf("failed to enable A-GPS: %v", err)
	}

	// Enable GPS
	if err := exec.Command("mmcli", "-m", l.modemId,
		"--location-enable-gps-raw").Run(); err != nil {
		return fmt.Errorf("failed to enable raw GPS: %v", err)
	}

	// Enable NMEA
	if err := exec.Command("mmcli", "-m", l.modemId,
		"--location-enable-gps-nmea").Run(); err != nil {
		return fmt.Errorf("failed to enable NMEA GPS: %v", err)
	}

	return nil
}

// TODO also use mmcli -J here instead of parsing NMEA
func (l *LocationService) updateLocation() error {
	if !l.enabled {
		return fmt.Errorf("GPS not enabled")
	}

	// Get raw NMEA data
	out, err := exec.Command("mmcli", "-m", l.modemId, "--location-get").Output()
	if err != nil {
		return fmt.Errorf("failed to get location: %v", err)
	}

	location, err := l.parseLocationData(string(out))
	if err != nil {
		return err
	}

	l.location = *location
	l.lastFix = time.Now()
	return nil
}

func (l *LocationService) parseLocationData(data string) (*Location, error) {
	// Example NMEA parsing - we'll need to handle multiple sentence types
	location := &Location{}

	// Parse latitude
	latMatch := regexp.MustCompile(`latitude:\s*([-+]?\d*\.\d+)`).FindStringSubmatch(data)
	if len(latMatch) > 1 {
		lat, err := strconv.ParseFloat(latMatch[1], 64)
		if err == nil {
			location.Latitude = lat
		}
	}

	// Parse longitude
	lonMatch := regexp.MustCompile(`longitude:\s*([-+]?\d*\.\d+)`).FindStringSubmatch(data)
	if len(lonMatch) > 1 {
		lon, err := strconv.ParseFloat(lonMatch[1], 64)
		if err == nil {
			location.Longitude = lon
		}
	}

	// Add timestamp
	location.Timestamp = time.Now()

	return location, nil
}

var version string

func main() {
	cfg := Config{}
	flag.StringVar(&cfg.redisURL, "redis-url", "redis://127.0.0.1:6379", "Redis URL")
	flag.DurationVar(&cfg.pollingTime, "polling-time", 5*time.Second, "Polling interval")
	flag.DurationVar(&cfg.internetCheckTime, "internet-check-time", 30*time.Second, "Internet check interval")
	flag.StringVar(&cfg.interface_, "interface", "wwan0", "Network interface to monitor")
	flag.Parse()

	_, err := net.InterfaceByName(cfg.interface_)
	if err != nil {
		log.Fatalf("Specified interface %s does not exist: %v", cfg.interface_, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	service := NewModemService(cfg, version)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		cancel()
	}()

	if err := service.Run(ctx); err != nil {
		log.Fatalf("Service failed: %v", err)
	}
}

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
)

const (
	ModemStateDefault    = "UNKNOWN"
	AccessTechDefault    = "UNKNOWN"
	SignalQualityDefault = 255
)

const (
	MaxRecoveryAttempts = 2
	RecoveryWaitTime    = 30 * time.Second

	StateNormal             = "normal"
	StateRecovering         = "recovering"
	StateRecoveryFailedWait = "recovery-failed-waiting-reboot"
	StatePermanentFailure   = "permanent-failure-needs-replacement"
)

type Config struct {
	redisURL          string
	pollingTime       time.Duration
	internetCheckTime time.Duration
	interface_        string
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
}

func NewModemHealth() *ModemHealth {
	return &ModemHealth{
		state: StateNormal,
	}
}

func NewModemService(cfg Config) *ModemService {
	opt, err := redis.ParseURL(cfg.redisURL)
	if err != nil {
		// Since this is during initialization, we should probably just panic
		panic(fmt.Sprintf("invalid redis URL: %v", err))
	}

	return &ModemService{
		cfg:    cfg,
		redis:  redis.NewClient(opt),
		logger: log.New(os.Stdout, "rescoot-modem: ", log.LstdFlags),
		lastModemState: ModemState{
			status:        ModemStateDefault,
			accessTech:    AccessTechDefault,
			signalQuality: SignalQualityDefault,
			ipAddr:        "UNKNOWN",
			ifIpAddr:      "UNKNOWN",
			registration:  "",
		},
		health: NewModemHealth(),
	}
}

func (m *ModemService) findModemId() (string, error) {
	out, err := exec.Command("mmcli", "-L").Output()
	if err != nil {
		return "", fmt.Errorf("mmcli -L failed: %v", err)
	}

	re := regexp.MustCompile(`/org/freedesktop/ModemManager1/Modem/(\d+)`)
	match := re.FindStringSubmatch(string(out))
	if len(match) < 2 {
		return "", fmt.Errorf("no modem found")
	}

	return match[1], nil
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

	out, err := exec.Command("mmcli", "-m", modemId).Output()
	if err != nil {
		return state, fmt.Errorf("mmcli error: %v", err)
	}

	output := string(out)
	lines := strings.Split(output, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)

		switch {
		case strings.Contains(line, "access tech:"):
			state.accessTech = strings.TrimSpace(strings.Split(line, ":")[1])

		case strings.Contains(line, "signal quality:"):
			parts := strings.Split(line, ":")
			if len(parts) > 1 {
				qualStr := strings.TrimSpace(strings.Split(parts[1], "%")[0])
				if qual, err := strconv.Atoi(qualStr); err == nil {
					state.signalQuality = uint8(qual)
				}
			}

		case strings.Contains(line, "equipment id:"):
			state.imei = strings.TrimSpace(strings.Split(line, ":")[1])
		}
	}

	// Get sim info
	simOut, err := exec.Command("mmcli", "-i", modemId).Output()
	if err == nil {
		simInfo := string(simOut)
		for _, line := range strings.Split(simInfo, "\n") {
			if strings.Contains(line, "iccid:") {
				state.iccid = strings.TrimSpace(strings.Split(line, ":")[1])
			}
		}
	}

	if ifIP, err := m.getInterfaceIP(); err == nil {
		state.ifIpAddr = ifIP
		state.status = "connected"

		if pubIP, err := m.getPublicIP(); err == nil {
			state.ipAddr = pubIP
		}
	} else {
		state.status = "off"
	}

	return state, nil
}

func (m *ModemService) publishModemState(currentState ModemState) error {
	pipe := m.redis.Pipeline()
	ctx := context.Background()

	if m.lastModemState.status != currentState.status {
		m.logger.Printf("internet modem-state : %s", currentState.status)
		pipe.HSet(ctx, "internet", "modem-state", currentState.status)
		pipe.Publish(ctx, "internet", "modem-state")
		m.lastModemState.status = currentState.status
	}

	if m.lastModemState.ipAddr != currentState.ipAddr {
		m.logger.Printf("internet ip-address : %s", currentState.ipAddr)
		pipe.HSet(ctx, "internet", "ip-address", currentState.ipAddr)
		pipe.Publish(ctx, "internet", "ip-address")
		m.lastModemState.ipAddr = currentState.ipAddr
	}

	if m.lastModemState.ifIpAddr != currentState.ifIpAddr {
		m.logger.Printf("interface ip-address : %s", currentState.ifIpAddr)
		pipe.HSet(ctx, "internet", "if-ip-address", currentState.ifIpAddr)
		pipe.Publish(ctx, "internet", "if-ip-address")
		m.lastModemState.ifIpAddr = currentState.ifIpAddr
	}

	if m.lastModemState.accessTech != currentState.accessTech {
		m.logger.Printf("internet access-tech : %s", currentState.accessTech)
		pipe.HSet(ctx, "internet", "access-tech", currentState.accessTech)
		pipe.Publish(ctx, "internet", "access-tech")
		m.lastModemState.accessTech = currentState.accessTech
	}

	if m.lastModemState.signalQuality != currentState.signalQuality {
		m.logger.Printf("internet signal-quality : %d", currentState.signalQuality)
		pipe.HSet(ctx, "internet", "signal-quality", currentState.signalQuality)
		pipe.Publish(ctx, "internet", "signal-quality")
		m.lastModemState.signalQuality = currentState.signalQuality
	}

	if m.lastModemState.imei != currentState.imei {
		m.logger.Printf("IMEI : %s", currentState.imei)
		pipe.HSet(ctx, "internet", "sim-imei", currentState.imei)
		pipe.Publish(ctx, "internet", "sim-imei")
		m.lastModemState.imei = currentState.imei
	}

	if m.lastModemState.iccid != currentState.iccid {
		m.logger.Printf("ICCID : %s", currentState.iccid)
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

func (m *ModemService) checkHealth() error {
	// Skip health check if we're in a terminal state
	if m.health.state == StateRecoveryFailedWait || m.health.state == StatePermanentFailure {
		return fmt.Errorf("modem in terminal state: %s", m.health.state)
	}

	modemId, err := m.findModemId()
	if err != nil {
		return m.handleModemFailure("no_modem_found")
	}

	// Check primary port
	if err := m.checkPrimaryPort(modemId); err != nil {
		return m.handleModemFailure("wrong_primary_port")
	}

	// Check power state
	if err := m.checkPowerState(modemId); err != nil {
		return m.handleModemFailure("wrong_power_state")
	}

	// If we get here, modem is healthy
	m.health.state = StateNormal
	return nil
}

func (m *ModemService) checkPrimaryPort(modemId string) error {
	out, err := exec.Command("mmcli", "-m", modemId, "--simple-status").Output()
	if err != nil {
		return fmt.Errorf("failed to get modem status: %v", err)
	}

	// Check if primary port is cdc-wdm0
	if !strings.Contains(string(out), "cdc-wdm0") {
		return fmt.Errorf("wrong primary port")
	}
	return nil
}

func (m *ModemService) checkPowerState(modemId string) error {
	out, err := exec.Command("mmcli", "-m", modemId).Output()
	if err != nil {
		return fmt.Errorf("failed to get modem info: %v", err)
	}

	// Check power state
	if !strings.Contains(string(out), "power state: 'on'") {
		return fmt.Errorf("modem not powered on")
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
		if err := exec.Command("mmcli", "-m", modemId, "--reset").Run(); err != nil {
			m.logger.Printf("Failed to reset modem: %v", err)
		}
	}

	// Wait for recovery
	time.Sleep(RecoveryWaitTime)

	// Check if recovery was successful
	if err := m.checkHealth(); err != nil {
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
		if ok && !ipNet.IP.IsLoopback() && ipNet.IP.To4() != nil {
			return ipNet.IP.String(), nil
		}
	}
	return "", fmt.Errorf("no IPv4 address found for interface %s", m.cfg.interface_)
}

func (m *ModemService) monitorStatus(ctx context.Context) {
	ticker := time.NewTicker(m.cfg.internetCheckTime)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.checkHealth(); err != nil {
				m.logger.Printf("Health check failed: %v", err)
				continue
			}

			currentState, err := m.getModemInfo()
			if err != nil {
				m.logger.Printf("Failed to get modem info: %v", err)
				currentState.status = "off"
			}

			if currentState.status == "connected" {
				if _, err := m.getPublicIP(); err != nil {
					currentState.status = "disconnected"
				}
			}

			if err := m.publishModemState(currentState); err != nil {
				m.logger.Printf("Failed to publish state: %v", err)
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

func main() {
	cfg := Config{}
	flag.StringVar(&cfg.redisURL, "redis-url", "redis://127.0.0.1:6379", "Redis URL")
	flag.DurationVar(&cfg.pollingTime, "polling-time", 5*time.Second, "Polling interval")
	flag.DurationVar(&cfg.internetCheckTime, "internet-check-time", 30*time.Second, "Internet check interval")
	flag.StringVar(&cfg.interface_, "interface", "wwan0", "Network interface to monitor")
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	service := NewModemService(cfg)

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

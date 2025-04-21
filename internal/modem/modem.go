package modem

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/rescoot/go-mmcli"
)

// Constants for modem state
const (
	StateDefault         = "UNKNOWN"
	AccessTechDefault    = "UNKNOWN"
	SignalQualityDefault = 255
)

// Modem power states
const (
	PowerStateOn  = "on"
	PowerStateOff = "off"
)

// SIM states
const (
	SIMStatePresent  = "present"
	SIMStateMissing  = "missing"
	SIMStateLocked   = "locked"
	SIMStateInactive = "inactive"
)

// Registration states
const (
	RegistrationHome    = "home"
	RegistrationRoaming = "roaming"
	RegistrationDenied  = "denied"
	RegistrationFailed  = "failed"
	RegistrationUnknown = "unknown"
)

// GPIO Control constants
const (
	GPIOPin        = 110
	CheckInterval  = 5 * time.Second
	MaxStartChecks = 60 // 5 minutes (60 * 5 seconds)
)

// State represents the current state of the modem
type State struct {
	Status           string
	AccessTech       string
	SignalQuality    uint8
	IPAddr           string
	IfIPAddr         string
	Registration     string
	IMEI             string
	IMSI             string
	ICCID            string
	PowerState       string
	SIMState         string
	SIMLockStatus    string
	OperatorName     string
	OperatorCode     string
	IsRoaming        bool
	RegistrationFail string
	ErrorState       string
}

// NewState creates a new modem state with default values
func NewState() *State {
	return &State{
		Status:           StateDefault,
		AccessTech:       AccessTechDefault,
		SignalQuality:    SignalQualityDefault,
		IPAddr:           "UNKNOWN",
		IfIPAddr:         "UNKNOWN",
		Registration:     "",
		PowerState:       PowerStateOff,
		SIMState:         SIMStateMissing,
		SIMLockStatus:    "",
		OperatorName:     "",
		OperatorCode:     "",
		IsRoaming:        false,
		RegistrationFail: "",
		ErrorState:       "", // Initialize as empty
	}
}

// FindModemID finds the modem ID
func FindModemID() (string, error) {
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

// GetModemStatus gets the modem status
func GetModemStatus(modemID string) (*mmcli.ModemManager, error) {
	mm, err := mmcli.GetModemDetails(modemID)
	if err != nil {
		return nil, fmt.Errorf("mmcli GetModemDetails error: %v", err)
	}

	return mm, nil
}

// GetModemInfo gets the modem information
func GetModemInfo(interfaceName string, logger *log.Logger) (*State, error) {
	state := NewState()
	// Default error state is 'ok' unless overridden later
	state.ErrorState = "ok"

	modemID, err := FindModemID()
	if err != nil {
		state.Status = "no-modem"
		state.ErrorState = "no-modem"
		return state, err
	}

	mm, err := GetModemStatus(modemID)
	if err != nil {
		// If we can't get status, assume it's an issue, but keep 'no-modem' if that was the initial error
		if state.ErrorState == "ok" {
			state.ErrorState = "status-error"
		}
		return state, err
	}

	// Set power state
	state.PowerState = mm.Modem.Generic.PowerState

	// Handle SIM state
	if mm.Modem.Generic.SIM == "" || mm.Modem.Generic.SIM == "--" {
		state.SIMState = SIMStateMissing
	} else {
		simInfo, err := mmcli.GetSIMInfo(strings.Split(mm.Modem.Generic.SIM, "/")[5])
		if err != nil {
			state.SIMState = SIMStateMissing
		} else {
			state.SIMState = SIMStatePresent
			state.IMSI = simInfo.Properties.IMSI
			state.ICCID = simInfo.Properties.ICCID
			state.OperatorName = simInfo.Properties.OperatorName
			state.OperatorCode = simInfo.Properties.OperatorCode

			if simInfo.Properties.Active != "yes" {
				state.SIMState = SIMStateInactive
			}
		}
	}

	// Set SIM lock status
	if mm.Modem.Generic.UnlockRequired != "" && mm.Modem.Generic.UnlockRequired != "none" {
		state.SIMLockStatus = mm.Modem.Generic.UnlockRequired
	}

	// Set signal quality
	if quality, err := mm.SignalStrength(); err == nil {
		state.SignalQuality = uint8(quality)
	}

	// Set access tech and IMEI
	state.AccessTech = mm.GetCurrentAccessTechnology()
	state.IMEI = mm.Modem.ThreeGPP.IMEI

	// Set registration state
	switch mm.Modem.ThreeGPP.RegistrationState {
	case "home":
		state.Registration = RegistrationHome
		state.IsRoaming = false
	case "roaming":
		state.Registration = RegistrationRoaming
		state.IsRoaming = true
	case "denied":
		state.Registration = RegistrationDenied
		state.RegistrationFail = mm.Modem.Generic.StateFailedReason
	case "searching", "registered":
		// No special handling
	default:
		state.Registration = RegistrationUnknown
	}

	// Set connection status
	switch {
	case mm.Modem.Generic.PowerState != "on":
		state.Status = "off"
	case mm.IsConnected():
		state.Status = "connected"
	default:
		state.Status = "disconnected"
	}

	if state.Status == "connected" {
		if ifIP, err := GetInterfaceIP(interfaceName); err == nil {
			state.IfIPAddr = ifIP
		} else {
			logger.Printf("Error getting interface IP: %v", err)
			state.Status = "disconnected"
		}
	}

	// Determine consolidated error state based on priority, only if current state is 'ok'
	if state.ErrorState == "ok" {
		if state.PowerState != PowerStateOn {
			state.ErrorState = "powered-off"
		} else if state.SIMState == SIMStateMissing {
			state.ErrorState = "sim-missing"
		} else if state.SIMState == SIMStateInactive {
			state.ErrorState = "sim-inactive"
		} else if state.SIMLockStatus != "" && state.SIMLockStatus != "none" {
			state.ErrorState = "sim-locked"
		} else if state.Registration == RegistrationDenied {
			state.ErrorState = "registration-denied"
		} else if state.Registration == RegistrationFailed {
			state.ErrorState = "registration-failed"
		} else if state.Status == "disconnected" && state.Registration != RegistrationHome && state.Registration != RegistrationRoaming {
			state.ErrorState = "disconnected"
		}
	}

	return state, nil
}

// GetInterfaceIP gets the IP address of the interface
func GetInterfaceIP(interfaceName string) (string, error) {
	iface, err := net.InterfaceByName(interfaceName)
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
	return "", fmt.Errorf("no global unicast IPv4 address found for interface %s", interfaceName)
}

// CheckPrimaryPort checks if the primary port is correct
func CheckPrimaryPort(modemID string) error {
	mm, err := GetModemStatus(modemID)
	if err != nil {
		return err
	}

	if mm.Modem.Generic.PrimaryPort != "cdc-wdm0" {
		return fmt.Errorf("wrong primary port: %s", mm.Modem.Generic.PrimaryPort)
	}
	return nil
}

// CheckPowerState checks if the power state is correct
func CheckPowerState(modemID string) error {
	mm, err := GetModemStatus(modemID)
	if err != nil {
		return err
	}

	if mm.Modem.Generic.PowerState != "on" {
		return fmt.Errorf("modem not powered on: %s", mm.Modem.Generic.PowerState)
	}
	return nil
}

// StartModem starts the modem
func StartModem() error {
	if err := os.WriteFile("/sys/class/gpio/export", []byte(fmt.Sprintf("%d", GPIOPin)), 0644); err != nil {
		return fmt.Errorf("failed to export GPIO pin: %v", err)
	}

	if err := os.WriteFile(fmt.Sprintf("/sys/class/gpio/gpio%d/direction", GPIOPin), []byte("out"), 0644); err != nil {
		return fmt.Errorf("failed to set GPIO direction: %v", err)
	}

	if err := os.WriteFile(fmt.Sprintf("/sys/class/gpio/gpio%d/value", GPIOPin), []byte("1"), 0644); err != nil {
		return fmt.Errorf("failed to set GPIO value high: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	if err := os.WriteFile(fmt.Sprintf("/sys/class/gpio/gpio%d/value", GPIOPin), []byte("0"), 0644); err != nil {
		return fmt.Errorf("failed to set GPIO value low: %v", err)
	}

	return nil
}

// RestartModem restarts the modem, attempting GPIO first and falling back to mmcli reset.
func RestartModem(logger *log.Logger) error {
	logger.Printf("Attempting modem restart via GPIO pin %d...", GPIOPin)
	gpioErr := attemptGPIORestart()

	if gpioErr == nil {
		logger.Printf("Modem restart via GPIO successful.")
		return nil
	}

	// GPIO failed, log warning and attempt mmcli reset as fallback
	logger.Printf("WARN: Modem restart via GPIO failed (%v), attempting fallback via mmcli reset...", gpioErr)

	modemID, findErr := FindModemID()
	if findErr != nil {
		// Cannot find modem to reset via mmcli either
		return fmt.Errorf("GPIO restart failed (%v) and cannot find modem for mmcli reset (%v)", gpioErr, findErr)
	}

	logger.Printf("Found modem %s, attempting mmcli reset.", modemID)
	resetErr := ResetModem(modemID)
	if resetErr != nil {
		return fmt.Errorf("GPIO restart failed (%v) and mmcli reset failed (%v)", gpioErr, resetErr)
	}

	logger.Printf("Modem restart via mmcli reset successful.")
	return nil
}

// attemptGPIORestart performs the modem restart sequence using GPIO.
func attemptGPIORestart() error {
	_, err := os.Stat(fmt.Sprintf("/sys/class/gpio/gpio%d", GPIOPin))
	isAlreadyExported := err == nil

	if !isAlreadyExported {
		if err := os.WriteFile("/sys/class/gpio/export", []byte(fmt.Sprintf("%d", GPIOPin)), 0644); err != nil {
			// Check if the error is specifically "Device or resource busy" which often means already exported
			if !strings.Contains(err.Error(), "Device or resource busy") {
				return fmt.Errorf("failed to export GPIO pin %d: %w", GPIOPin, err)
			}
			// If it is busy/exported, ignore the error and proceed
		}
	}

	// Set direction (might also fail if already set correctly by another process)
	directionPath := fmt.Sprintf("/sys/class/gpio/gpio%d/direction", GPIOPin)
	if err := os.WriteFile(directionPath, []byte("out"), 0644); err != nil {
		// Read current direction to see if it's already 'out'
		currentDirection, readErr := os.ReadFile(directionPath)
		if readErr != nil || strings.TrimSpace(string(currentDirection)) != "out" {
			return fmt.Errorf("failed to set GPIO direction for pin %d: %w (current: %s, readErr: %v)", GPIOPin, err, string(currentDirection), readErr)
		}
		// If already 'out', ignore the WriteFile error
	}

	// Set value high
	valuePath := fmt.Sprintf("/sys/class/gpio/gpio%d/value", GPIOPin)
	if err := os.WriteFile(valuePath, []byte("1"), 0644); err != nil {
		return fmt.Errorf("failed to set GPIO value high for pin %d: %w", GPIOPin, err)
	}

	time.Sleep(3500 * time.Millisecond)

	// Set value low
	if err := os.WriteFile(valuePath, []byte("0"), 0644); err != nil {
		return fmt.Errorf("failed to set GPIO value low for pin %d: %w", GPIOPin, err)
	}

	// if !isAlreadyExported {
	// 	if err := os.WriteFile("/sys/class/gpio/unexport", []byte(fmt.Sprintf("%d", GPIOPin)), 0644); err != nil {
	// 		// Log warning, but don't fail the restart for unexport failure
	// 		fmt.Printf("WARN: Failed to unexport GPIO pin %d: %v\n", GPIOPin, err)
	// 	}
	// }

	return nil
}

// IsInterfacePresent checks if the interface is present
func IsInterfacePresent(interfaceName string) bool {
	_, err := net.InterfaceByName(interfaceName)
	return err == nil
}

// IsDBusPresent checks if the DBus is present
func IsDBusPresent() bool {
	_, err := FindModemID()
	return err == nil
}

// WaitForModem waits for the modem to come up
func WaitForModem(ctx context.Context, interfaceName string, logger *log.Logger) error {
	logger.Printf("Waiting for modem to come up...")

	ticker := time.NewTicker(CheckInterval)
	defer ticker.Stop()

	count := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if IsInterfacePresent(interfaceName) {
				logger.Printf("Modem interface %s is now present", interfaceName)
				return nil
			}

			if IsDBusPresent() {
				logger.Printf("Modem is now present via mmcli/dbus")
				return nil
			}

			count++
			if count >= MaxStartChecks {
				return fmt.Errorf("modem did not come up after %d checks", MaxStartChecks)
			}
		}
	}
}

// ResetModem resets the modem
func ResetModem(modemID string) error {
	return exec.Command("mmcli", "-m", modemID, "--reset").Run()
}

package modem

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
	Status             string // Raw status from modem: "off", "connected", "disconnected", "no-modem", "UNKNOWN"
	LastRawModemStatus string // Used by service layer to cache last published raw modem status
	AccessTech         string
	SignalQuality      uint8
	IPAddr             string
	IfIPAddr           string
	Registration       string
	IMEI               string
	IMSI               string
	ICCID              string
	PowerState         string
	SIMState           string
	SIMLockStatus      string
	OperatorName       string
	OperatorCode       string
	IsRoaming          bool
	RegistrationFail   string
	ErrorState         string
}

// NewState creates a new modem state with default values
func NewState() *State {
	return &State{
		Status:             StateDefault,
		LastRawModemStatus: StateDefault, // Initialize to UNKNOWN
		AccessTech:         AccessTechDefault,
		SignalQuality:      SignalQualityDefault,
		IPAddr:             "UNKNOWN",
		IfIPAddr:           "UNKNOWN",
		Registration:       "",
		PowerState:         PowerStateOff,
		SIMState:           SIMStateMissing,
		SIMLockStatus:      "",
		OperatorName:       "",
		OperatorCode:       "",
		IsRoaming:          false,
		RegistrationFail:   "",
		ErrorState:         "", // Initialize as empty
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

// toggleGPIO performs the GPIO toggle sequence with a configurable timespan.
// It handles export, setting direction, toggling the pin, and unexport in a safe way.
func toggleGPIO(pin int, toggleDuration time.Duration) error {
	// Try to export GPIO pin, but continue even if it fails (might already be exported)
	exportErr := os.WriteFile("/sys/class/gpio/export", []byte(fmt.Sprintf("%d", pin)), 0644)
	wasExported := exportErr == nil

	// Set direction to output and continue anyway
	directionPath := fmt.Sprintf("/sys/class/gpio/gpio%d/direction", pin)
	dirErr := os.WriteFile(directionPath, []byte("out"), 0644)

	// Only fail if we can't access the pin at all
	if exportErr != nil && dirErr != nil {
		return fmt.Errorf("failed to initialize GPIO pin %d: export error: %v, direction error: %v",
			pin, exportErr, dirErr)
	}

	// Set value high
	valuePath := fmt.Sprintf("/sys/class/gpio/gpio%d/value", pin)
	if err := os.WriteFile(valuePath, []byte("1"), 0644); err != nil {
		return fmt.Errorf("failed to set GPIO value high for pin %d: %v", pin, err)
	}

	time.Sleep(toggleDuration)

	// Set value low
	if err := os.WriteFile(valuePath, []byte("0"), 0644); err != nil {
		return fmt.Errorf("failed to set GPIO value low for pin %d: %v", pin, err)
	}

	// Always try to unexport the pin if we successfully exported it
	if wasExported {
		if err := os.WriteFile("/sys/class/gpio/unexport", []byte(fmt.Sprintf("%d", pin)), 0644); err != nil {
			// Log warning, but don't fail the operation
			fmt.Printf("WARN: Failed to unexport GPIO pin %d: %v\n", pin, err)
		}
	}

	return nil
}

// StartModem starts the modem with an initial quick pulse.
// If that fails, tries the full restart sequence as a fallback.
func StartModem(logger *log.Logger) error {
	// First try a quick pulse to turn ON
	if err := toggleGPIO(GPIOPin, 500*time.Millisecond); err == nil {
		return nil
	}

	// If the quick start failed, try the full restart sequence
	if logger != nil {
		logger.Printf("Initial modem start failed, attempting full restart sequence")
	}
	return RestartModem(logger)
}

// RestartModem restarts the modem by first turning it OFF with a 3500ms pulse, then turning it ON with a 500ms pulse.
// Falls back to mmcli reset if GPIO method fails.
func RestartModem(logger *log.Logger) error {
	// First turn the modem OFF with a 3500ms pulse
	logger.Printf("Pulsing LTE_POWER for 3500ms to turn modem OFF")
	offErr := toggleGPIO(GPIOPin, 3500*time.Millisecond)
	if offErr != nil {
		logger.Printf("Failed to turn modem OFF: %v", offErr)
	} else {
		// Give the modem a moment to fully shut down
		time.Sleep(15 * time.Second)
	}

	// Then turn the modem ON with a 500ms pulse (same as StartModem)
	logger.Printf("Pulsing LTE_POWER for 500ms to turn modem ON")
	onErr := toggleGPIO(GPIOPin, 500*time.Millisecond)

	if onErr == nil && offErr == nil {
		logger.Printf("Modem restart via GPIO successful (OFF then ON).")
		return nil
	}

	// GPIO failed, log warning and attempt mmcli reset as fallback
	logger.Printf("WARN: Modem restart via GPIO failed (OFF error: %v, ON error: %v), attempting fallback via mmcli reset...", offErr, onErr)

	modemID, findErr := FindModemID()
	if findErr != nil {
		// Cannot find modem to reset via mmcli either
		return fmt.Errorf("GPIO restart failed (OFF: %v, ON: %v) and cannot find modem for mmcli reset (%v)",
			offErr, onErr, findErr)
	}

	logger.Printf("Found modem %s, attempting mmcli reset.", modemID)
	resetErr := ResetModem(modemID)
	if resetErr != nil {
		return fmt.Errorf("GPIO restart failed (OFF: %v, ON: %v) and mmcli reset failed (%v)",
			offErr, onErr, resetErr)
	}

	logger.Printf("Modem restart via mmcli reset successful.")
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

// RecoverUSB attempts to recover the modem by unbinding and rebinding the USB device
func RecoverUSB(logger *log.Logger) error {
	const waitTimeBindUnbind = 30
	const waitTimeMmcli = 30
	const usbDevice = "1-1" // USB device path for the modem

	logger.Printf("USB recovery: 'unbind'")
	if err := writeToFile("/sys/bus/usb/drivers/usb/unbind", usbDevice); err != nil {
		logger.Printf("Warning: Failed to unbind USB device: %v", err)
		// Continue anyway, might still work
	}

	time.Sleep(waitTimeBindUnbind * time.Second)

	if isUSBDevicePresent() {
		logger.Printf("USB recovery: USB device still found after 'unbind'")
	}

	logger.Printf("USB recovery: 'bind'")
	if err := writeToFile("/sys/bus/usb/drivers/usb/bind", usbDevice); err != nil {
		logger.Printf("Warning: Failed to bind USB device: %v", err)
		return fmt.Errorf("failed to bind USB device: %v", err)
	}

	// Wait for USB device to come back
	time.Sleep(waitTimeBindUnbind * time.Second)

	if isUSBDevicePresent() {
		logger.Printf("USB recovery: USB device found after 'bind', modem seems to be ON")
		logger.Printf("USB recovery: waiting %ds for 'mmcli -L'...", waitTimeMmcli)
		time.Sleep(waitTimeMmcli * time.Second)
	} else {
		logger.Printf("USB recovery: USB device not found after 'bind', modem seems to be OFF")
	}

	logger.Printf("USB recovery: modem state after recovery:")
	logger.Printf("USB recovery: usbFound: %t", isUSBDevicePresent())
	logger.Printf("USB recovery: isModemFound: %t", IsDBusPresent())

	return nil
}

// writeToFile writes data to a file (helper for USB recovery)
func writeToFile(filename, data string) error {
	file, err := os.OpenFile(filename, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = file.WriteString(data + "\n")
	return err
}

// isUSBDevicePresent checks if the USB device is present
func isUSBDevicePresent() bool {
	// Check if USB device files exist (this is a simplified check)
	// In a real implementation, you might want to check /sys/bus/usb/devices/ or use lsusb
	if _, err := os.Stat("/sys/bus/usb/devices/1-1"); err == nil {
		return true
	}
	// Also check for modem-specific USB devices
	patterns := []string{
		"/sys/bus/usb/devices/*/idVendor",
		"/sys/bus/usb/devices/*/idProduct",
	}
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(pattern)
		for _, match := range matches {
			if content, err := os.ReadFile(match); err == nil {
				// Check for common modem vendor IDs (Simcom, etc.)
				vendorProduct := strings.TrimSpace(string(content))
				if vendorProduct == "1e0e" || vendorProduct == "05c6" { // Common modem vendor IDs
					return true
				}
			}
		}
	}
	return false
}

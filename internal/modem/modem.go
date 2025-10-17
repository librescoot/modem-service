package modem

import (
	"context"
	"fmt"
	"log"
	"modem-service/internal/gpio"
	"modem-service/internal/mm"
	"modem-service/internal/usb"
	"net"
	"os"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
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

// Timing constants
const (
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

// Manager manages modem operations via D-Bus
type Manager struct {
	client   *mm.Client
	gpio     *gpio.PowerController
	usb      *usb.Recovery
	logger   *log.Logger
	modemPath dbus.ObjectPath
}

// NewManager creates a new modem manager
func NewManager(logger *log.Logger, debug bool) (*Manager, error) {
	client, err := mm.NewClient(debug, logger.Printf)
	if err != nil {
		return nil, fmt.Errorf("failed to create ModemManager client: %v", err)
	}

	gpioCtrl, err := gpio.NewPowerController(logger.Printf)
	if err != nil {
		logger.Printf("Warning: GPIO controller init failed: %v", err)
	}

	usbRecovery := usb.NewRecovery(logger.Printf)

	return &Manager{
		client: client,
		gpio:   gpioCtrl,
		usb:    usbRecovery,
		logger: logger,
	}, nil
}

// Close closes the manager and releases resources
func (m *Manager) Close() error {
	if m.gpio != nil {
		m.gpio.Close()
	}
	if m.client != nil {
		return m.client.Close()
	}
	return nil
}

// NewState creates a new modem state with default values
func NewState() *State {
	return &State{
		Status:             StateDefault,
		LastRawModemStatus: StateDefault,
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
		ErrorState:         "",
	}
}

// FindModem finds the modem via D-Bus
func (m *Manager) FindModem() (dbus.ObjectPath, error) {
	path, err := m.client.FindModem()
	if err == nil {
		m.modemPath = path
	}
	return path, err
}

// GetModemInfo gets comprehensive modem information
func (m *Manager) GetModemInfo(interfaceName string) (*State, error) {
	state := NewState()
	state.ErrorState = "ok"

	// Find modem
	modemPath, err := m.FindModem()
	if err != nil {
		state.Status = "no-modem"
		state.ErrorState = "no-modem"
		return state, err
	}

	// Get power state
	powerVar, err := m.client.GetProperty(modemPath, mm.ModemInterface, "PowerState")
	if err == nil {
		if powerState, ok := powerVar.Value().(uint32); ok {
			state.PowerState = mm.PowerStateToString(int32(powerState))
		}
	}

	// Get modem state
	stateVar, err := m.client.GetProperty(modemPath, mm.ModemInterface, "State")
	if err == nil {
		if modemState, ok := stateVar.Value().(int32); ok {
			switch {
			case state.PowerState != PowerStateOn:
				state.Status = "off"
			case modemState == mm.MMModemStateConnected:
				state.Status = "connected"
			default:
				state.Status = "disconnected"
			}
		}
	}

	// Get SIM information
	simVar, err := m.client.GetProperty(modemPath, mm.ModemInterface, "Sim")
	if err == nil {
		if simPath, ok := simVar.Value().(dbus.ObjectPath); ok && string(simPath) != "/" {
			state.SIMState = SIMStatePresent

			// Get IMSI
			if imsiVar, err := m.client.GetProperty(simPath, "org.freedesktop.ModemManager1.Sim", "Imsi"); err == nil {
				if imsi, ok := imsiVar.Value().(string); ok {
					state.IMSI = imsi
				}
			}

			// Get ICCID (also try via AT command)
			if iccid, err := m.client.GetICCID(modemPath); err == nil && iccid != "" {
				state.ICCID = iccid
			} else if iccidVar, err := m.client.GetProperty(simPath, "org.freedesktop.ModemManager1.Sim", "SimIdentifier"); err == nil {
				if iccid, ok := iccidVar.Value().(string); ok {
					state.ICCID = iccid
				}
			}

			// Get operator info
			if opVar, err := m.client.GetProperty(simPath, "org.freedesktop.ModemManager1.Sim", "OperatorName"); err == nil {
				if op, ok := opVar.Value().(string); ok {
					state.OperatorName = op
				}
			}
			if opCodeVar, err := m.client.GetProperty(simPath, "org.freedesktop.ModemManager1.Sim", "OperatorIdentifier"); err == nil {
				if opCode, ok := opCodeVar.Value().(string); ok {
					state.OperatorCode = opCode
				}
			}
		} else {
			state.SIMState = SIMStateMissing
		}
	}

	// Get SIM lock status
	if lockVar, err := m.client.GetProperty(modemPath, mm.ModemInterface, "UnlockRequired"); err == nil {
		if lock, ok := lockVar.Value().(uint32); ok {
			lockStr := mm.LockReasonToString(lock)
			if lockStr != "none" && lockStr != "unknown" {
				state.SIMLockStatus = lockStr
				state.SIMState = SIMStateLocked
			}
		}
	}

	// Get signal quality
	if qualVar, err := m.client.GetProperty(modemPath, mm.ModemInterface, "SignalQuality"); err == nil {
		if qual, ok := qualVar.Value().(uint32); ok {
			state.SignalQuality = uint8(qual)
		}
	}

	// Get IMEI (try AT command first, fallback to property)
	if imei, err := m.client.GetIMEI(modemPath); err == nil && imei != "" {
		state.IMEI = imei
	} else if imeiVar, err := m.client.GetProperty(modemPath, mm.ModemInterface, "EquipmentIdentifier"); err == nil {
		if imei, ok := imeiVar.Value().(string); ok {
			state.IMEI = imei
		}
	}

	// Get access technology
	if techVar, err := m.client.GetProperty(modemPath, mm.ModemInterface, "AccessTechnologies"); err == nil {
		if tech, ok := techVar.Value().(uint32); ok {
			state.AccessTech = mm.AccessTechnologyToString(tech)
		}
	}

	// Get 3GPP registration state
	if regVar, err := m.client.GetProperty(modemPath, mm.Modem3gppInterface, "RegistrationState"); err == nil {
		if reg, ok := regVar.Value().(uint32); ok {
			state.Registration = mm.RegistrationStateToString(reg)
			state.IsRoaming = (reg == mm.MMModem3gppRegistrationStateRoaming)
		}
	}

	// Get operator name from 3GPP if not from SIM
	if state.OperatorName == "" {
		if opVar, err := m.client.GetProperty(modemPath, mm.Modem3gppInterface, "OperatorName"); err == nil {
			if op, ok := opVar.Value().(string); ok {
				state.OperatorName = op
			}
		}
	}
	if state.OperatorCode == "" {
		if opVar, err := m.client.GetProperty(modemPath, mm.Modem3gppInterface, "OperatorCode"); err == nil {
			if op, ok := opVar.Value().(string); ok {
				state.OperatorCode = op
			}
		}
	}

	// Get interface IP if connected
	if state.Status == "connected" {
		if ifIP, err := GetInterfaceIP(interfaceName); err == nil {
			state.IfIPAddr = ifIP
		} else {
			m.logger.Printf("Error getting interface IP: %v", err)
			state.Status = "disconnected"
		}
	}

	// Determine consolidated error state
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
func (m *Manager) CheckPrimaryPort() error {
	if m.modemPath == "" {
		if _, err := m.FindModem(); err != nil {
			return err
		}
	}

	portVar, err := m.client.GetProperty(m.modemPath, mm.ModemInterface, "PrimaryPort")
	if err != nil {
		return err
	}

	if port, ok := portVar.Value().(string); ok {
		if port != "cdc-wdm0" {
			return fmt.Errorf("wrong primary port: %s", port)
		}
	}

	return nil
}

// CheckPowerState checks if the power state is correct
func (m *Manager) CheckPowerState() error {
	if m.modemPath == "" {
		if _, err := m.FindModem(); err != nil {
			return err
		}
	}

	powerVar, err := m.client.GetProperty(m.modemPath, mm.ModemInterface, "PowerState")
	if err != nil {
		return err
	}

	if power, ok := powerVar.Value().(int32); ok {
		if power != mm.MMModemPowerStateOn {
			return fmt.Errorf("modem not powered on: %s", mm.PowerStateToString(power))
		}
	}

	return nil
}

// StartModem starts the modem via GPIO
func (m *Manager) StartModem() error {
	if m.gpio == nil {
		return fmt.Errorf("GPIO controller not initialized")
	}

	if err := m.gpio.Init(); err != nil {
		return fmt.Errorf("failed to init GPIO: %v", err)
	}
	defer m.gpio.Close()

	return m.gpio.PowerOn()
}

// RestartModem restarts the modem (power cycle)
func (m *Manager) RestartModem() error {
	if m.gpio == nil {
		return fmt.Errorf("GPIO controller not initialized")
	}

	if err := m.gpio.Init(); err != nil {
		return fmt.Errorf("failed to init GPIO: %v", err)
	}
	defer m.gpio.Close()

	// Full power cycle
	if err := m.gpio.Cycle(); err != nil {
		// Fallback to D-Bus reset
		m.logger.Printf("GPIO power cycle failed, attempting D-Bus reset...")
		if m.modemPath == "" {
			if _, err := m.FindModem(); err != nil {
				return fmt.Errorf("GPIO failed and cannot find modem: %v", err)
			}
		}
		return m.client.Reset(m.modemPath)
	}

	return nil
}

// ResetModem resets the modem via D-Bus
func (m *Manager) ResetModem() error {
	if m.modemPath == "" {
		if _, err := m.FindModem(); err != nil {
			return err
		}
	}

	return m.client.Reset(m.modemPath)
}

// RecoverUSB performs USB recovery
func (m *Manager) RecoverUSB() error {
	return m.usb.Recover()
}

// IsInterfacePresent checks if the interface is present
func IsInterfacePresent(interfaceName string) bool {
	_, err := net.InterfaceByName(interfaceName)
	return err == nil
}

// IsModemPresent checks if modem is present via D-Bus
func (m *Manager) IsModemPresent() bool {
	_, err := m.FindModem()
	return err == nil
}

// WaitForModem waits for the modem to come up
func (m *Manager) WaitForModem(ctx context.Context, interfaceName string) error {
	m.logger.Printf("Waiting for modem to come up...")

	ticker := time.NewTicker(CheckInterval)
	defer ticker.Stop()

	count := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if IsInterfacePresent(interfaceName) {
				m.logger.Printf("Modem interface %s is now present", interfaceName)
				return nil
			}

			if m.IsModemPresent() {
				m.logger.Printf("Modem is now present via D-Bus")
				return nil
			}

			count++
			if count >= MaxStartChecks {
				return fmt.Errorf("modem did not come up after %d checks", MaxStartChecks)
			}
		}
	}
}

// IsUSBDevicePresent checks if the USB device is present
func IsUSBDevicePresent() bool {
	if _, err := os.Stat("/sys/bus/usb/devices/1-1"); err == nil {
		return true
	}
	return false
}

// Convenience functions for backward compatibility

// FindModemID finds the modem and returns a string ID (for compatibility)
func FindModemID(m *Manager) (string, error) {
	path, err := m.FindModem()
	if err != nil {
		return "", err
	}
	// Extract last component of path as ID
	pathStr := string(path)
	parts := strings.Split(pathStr, "/")
	if len(parts) > 0 {
		return parts[len(parts)-1], nil
	}
	return pathStr, nil
}

package mm

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/pkg/errors"
)

const (
	ModemManagerService   = "org.freedesktop.ModemManager1"
	ModemManagerPath      = "/org/freedesktop/ModemManager1"
	ModemManagerInterface = "org.freedesktop.ModemManager1"

	ModemInterface         = "org.freedesktop.ModemManager1.Modem"
	Modem3gppInterface     = "org.freedesktop.ModemManager1.Modem.Modem3gpp"
	ModemLocationInterface = "org.freedesktop.ModemManager1.Modem.Location"
	ModemSimpleInterface   = "org.freedesktop.ModemManager1.Modem.Simple"
	SimInterface           = "org.freedesktop.ModemManager1.Sim"

	ModemMessagingInterface = "org.freedesktop.ModemManager1.Modem.Messaging"
	SmsInterface            = "org.freedesktop.ModemManager1.Sms"

	// MobileEquipment error names returned by ModemManager when SIM PIN
	// operations fail. Used by IsWrongPinError and IsPukRequiredError.
	ErrIncorrectPassword = "org.freedesktop.ModemManager1.Error.MobileEquipment.IncorrectPassword"
	ErrSimPuk            = "org.freedesktop.ModemManager1.Error.MobileEquipment.SimPuk"
	ErrSimPuk2           = "org.freedesktop.ModemManager1.Error.MobileEquipment.SimPuk2"

	// MMBearerAllowedAuth flags (from MM-enums.h). Used in the "allowed-auth"
	// field of InitialEpsBearerSettings.
	BearerAuthNone uint32 = 1 << 0
	BearerAuthPAP  uint32 = 1 << 1
	BearerAuthCHAP uint32 = 1 << 2

	// MMBearerIpFamily values. Used in the "ip-type" field of
	// InitialEpsBearerSettings.
	BearerIPv4   uint32 = 1 << 0
	BearerIPv6   uint32 = 1 << 1
	BearerIPv4v6 uint32 = 1 << 2

	DBusPropertiesInterface = "org.freedesktop.DBus.Properties"
	DBusObjectManager       = "org.freedesktop.DBus.ObjectManager"
)

// Client is a D-Bus client for ModemManager
type Client struct {
	conn   *dbus.Conn
	debug  bool
	logger func(string, ...interface{})
}

// NewClient creates a new ModemManager D-Bus client
func NewClient(debug bool, logger func(string, ...interface{})) (*Client, error) {
	conn, err := dbus.SystemBus()
	if err != nil {
		return nil, errors.Wrap(err, "failed to connect to system bus")
	}

	if logger == nil {
		logger = func(string, ...interface{}) {}
	}

	return &Client{
		conn:   conn,
		debug:  debug,
		logger: logger,
	}, nil
}

// Close closes the D-Bus connection
func (c *Client) Close() error {
	return c.conn.Close()
}

// FindModem finds the first available modem
func (c *Client) FindModem() (dbus.ObjectPath, error) {
	obj := c.conn.Object(ModemManagerService, ModemManagerPath)

	var managedObjects map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	err := obj.Call(DBusObjectManager+".GetManagedObjects", 0).Store(&managedObjects)
	if err != nil {
		return "", errors.Wrap(err, "failed to get managed objects")
	}

	for path, interfaces := range managedObjects {
		if _, hasModem := interfaces[ModemInterface]; hasModem {
			return path, nil
		}
	}

	return "", errors.New("no modem found")
}

// GetProperty gets a property from the modem
func (c *Client) GetProperty(modemPath dbus.ObjectPath, iface, property string) (dbus.Variant, error) {
	obj := c.conn.Object(ModemManagerService, modemPath)

	var value dbus.Variant
	err := obj.Call(DBusPropertiesInterface+".Get", 0, iface, property).Store(&value)
	if err != nil {
		return value, errors.Wrapf(err, "failed to get property %s.%s", iface, property)
	}

	c.log("Get %s.%s = %v", iface, property, value.Value())
	return value, nil
}

// SetProperty sets a property on the modem
func (c *Client) SetProperty(modemPath dbus.ObjectPath, iface, property string, value interface{}) error {
	obj := c.conn.Object(ModemManagerService, modemPath)

	call := obj.Call(DBusPropertiesInterface+".Set", 0, iface, property, dbus.MakeVariant(value))
	if call.Err != nil {
		return errors.Wrapf(call.Err, "failed to set property %s.%s", iface, property)
	}

	c.log("Set %s.%s = %v", iface, property, value)
	return nil
}

// SendCommand sends an AT command to the modem
func (c *Client) SendCommand(modemPath dbus.ObjectPath, command string, timeout time.Duration) (string, error) {
	obj := c.conn.Object(ModemManagerService, modemPath)

	timeoutSec := uint32(timeout.Seconds())
	if timeoutSec == 0 {
		timeoutSec = 120
	}

	c.log(">> %s (timeout: %ds)", command, timeoutSec)

	var response string
	err := obj.Call(ModemInterface+".Command", 0, command, timeoutSec).Store(&response)
	if err != nil {
		return "", errors.Wrapf(err, "AT command failed: %s", command)
	}

	c.log("<< %s", strings.TrimSpace(response))
	return response, nil
}

// CallMethod calls a method on the modem
func (c *Client) CallMethod(modemPath dbus.ObjectPath, iface, method string, args ...interface{}) *dbus.Call {
	obj := c.conn.Object(ModemManagerService, modemPath)
	fullMethod := iface + "." + method
	c.log("Call %s(%v)", fullMethod, args)
	return obj.Call(fullMethod, 0, args...)
}

// Enable enables the modem
func (c *Client) Enable(modemPath dbus.ObjectPath, enable bool) error {
	call := c.CallMethod(modemPath, ModemInterface, "Enable", enable)
	return call.Err
}

// Reset resets the modem
func (c *Client) Reset(modemPath dbus.ObjectPath) error {
	call := c.CallMethod(modemPath, ModemInterface, "Reset")
	return call.Err
}


// SendPin sends a PIN to unlock the SIM. The pin string is never logged.
func (c *Client) SendPin(simPath dbus.ObjectPath, pin string) error {
	obj := c.conn.Object(ModemManagerService, simPath)
	c.log("Call %s.SendPin([REDACTED])", SimInterface)
	return obj.Call(SimInterface+".SendPin", 0, pin).Err
}

// EnablePin enables or disables the PIN lock on the SIM. The pin string is
// never logged.
func (c *Client) EnablePin(simPath dbus.ObjectPath, pin string, enabled bool) error {
	obj := c.conn.Object(ModemManagerService, simPath)
	c.log("Call %s.EnablePin([REDACTED], %v)", SimInterface, enabled)
	return obj.Call(SimInterface+".EnablePin", 0, pin, enabled).Err
}

// GetUnlockRetries reads modem.UnlockRetries (a{uu}), keyed by MMLock*.
// Missing entries mean "unknown"; callers should treat absent keys
// conservatively (e.g. as zero remaining attempts).
func (c *Client) GetUnlockRetries(modemPath dbus.ObjectPath) (map[uint32]uint32, error) {
	variant, err := c.GetProperty(modemPath, ModemInterface, "UnlockRetries")
	if err != nil {
		return nil, err
	}
	if retries, ok := variant.Value().(map[uint32]uint32); ok {
		return retries, nil
	}
	return nil, errors.New("invalid UnlockRetries type")
}

// GetEnabledFacilityLocks reads modem3gpp.EnabledFacilityLocks (u, bitmask of
// MMModem3gppFacility flags). Returns 0 with no error when the modem doesn't
// expose the property (rare but possible during early init).
func (c *Client) GetEnabledFacilityLocks(modemPath dbus.ObjectPath) (uint32, error) {
	variant, err := c.GetProperty(modemPath, Modem3gppInterface, "EnabledFacilityLocks")
	if err != nil {
		return 0, err
	}
	if locks, ok := variant.Value().(uint32); ok {
		return locks, nil
	}
	return 0, errors.New("invalid EnabledFacilityLocks type")
}

// IsWrongPinError reports whether err is a ModemManager IncorrectPassword.
func IsWrongPinError(err error) bool {
	if err == nil {
		return false
	}
	if dbusErr, ok := err.(dbus.Error); ok {
		return dbusErr.Name == ErrIncorrectPassword
	}
	return false
}

// IsPukRequiredError reports whether err indicates the SIM is PUK-locked.
func IsPukRequiredError(err error) bool {
	if err == nil {
		return false
	}
	if dbusErr, ok := err.(dbus.Error); ok {
		return dbusErr.Name == ErrSimPuk || dbusErr.Name == ErrSimPuk2
	}
	return false
}

// GetInitialEpsBearerSettings reads modem3gpp.InitialEpsBearerSettings (a{sv}).
// Returned dict keys include "apn" (s), "user" (s), "password" (s),
// "ip-type" (u), "allowed-auth" (u). Missing keys mean the modem has no
// value set for that field — callers should treat them as empty/zero.
func (c *Client) GetInitialEpsBearerSettings(modemPath dbus.ObjectPath) (map[string]dbus.Variant, error) {
	variant, err := c.GetProperty(modemPath, Modem3gppInterface, "InitialEpsBearerSettings")
	if err != nil {
		return nil, err
	}
	if settings, ok := variant.Value().(map[string]dbus.Variant); ok {
		return settings, nil
	}
	return nil, errors.New("invalid InitialEpsBearerSettings type")
}

// SetInitialEpsBearerSettings calls
// org.freedesktop.ModemManager1.Modem.Modem3gpp.SetInitialEpsBearerSettings.
// Persists the LTE attach APN/credentials in the modem (survives reboot).
// The password value is never logged.
func (c *Client) SetInitialEpsBearerSettings(modemPath dbus.ObjectPath, settings map[string]dbus.Variant) error {
	obj := c.conn.Object(ModemManagerService, modemPath)
	c.log("Call %s.SetInitialEpsBearerSettings([apn/user/password redacted])", Modem3gppInterface)
	return obj.Call(Modem3gppInterface+".SetInitialEpsBearerSettings", 0, settings).Err
}

// SetupLocation configures location services
func (c *Client) SetupLocation(modemPath dbus.ObjectPath, sources uint32, signalLocation bool) error {
	call := c.CallMethod(modemPath, ModemLocationInterface, "Setup", sources, signalLocation)
	return call.Err
}

// SetSuplServer sets the SUPL server for A-GPS
func (c *Client) SetSuplServer(modemPath dbus.ObjectPath, server string) error {
	call := c.CallMethod(modemPath, ModemLocationInterface, "SetSuplServer", server)
	return call.Err
}

// GetLocation gets the current location
func (c *Client) GetLocation(modemPath dbus.ObjectPath) (map[uint32]dbus.Variant, error) {
	call := c.CallMethod(modemPath, ModemLocationInterface, "GetLocation")
	if call.Err != nil {
		return nil, call.Err
	}

	var location map[uint32]dbus.Variant
	err := call.Store(&location)
	return location, err
}

// SetGPSRefreshRate sets the GPS refresh rate in seconds
func (c *Client) SetGPSRefreshRate(modemPath dbus.ObjectPath, seconds uint32) error {
	call := c.CallMethod(modemPath, ModemLocationInterface, "SetGpsRefreshRate", seconds)
	return call.Err
}

// GetLocationCapabilities returns the available location sources
func (c *Client) GetLocationCapabilities(modemPath dbus.ObjectPath) (uint32, error) {
	variant, err := c.GetProperty(modemPath, ModemLocationInterface, "Capabilities")
	if err != nil {
		return 0, err
	}
	if caps, ok := variant.Value().(uint32); ok {
		return caps, nil
	}
	return 0, errors.New("invalid capabilities type")
}

// GetEnabledLocationSources returns the currently enabled location sources
func (c *Client) GetEnabledLocationSources(modemPath dbus.ObjectPath) (uint32, error) {
	variant, err := c.GetProperty(modemPath, ModemLocationInterface, "Enabled")
	if err != nil {
		return 0, err
	}
	if enabled, ok := variant.Value().(uint32); ok {
		return enabled, nil
	}
	return 0, errors.New("invalid enabled type")
}

// GetSuplServer returns the current SUPL server
func (c *Client) GetSuplServer(modemPath dbus.ObjectPath) (string, error) {
	variant, err := c.GetProperty(modemPath, ModemLocationInterface, "SuplServer")
	if err != nil {
		return "", err
	}
	if server, ok := variant.Value().(string); ok {
		return server, nil
	}
	return "", errors.New("invalid SUPL server type")
}

// WatchModems sets up signal watching for modem added/removed
func (c *Client) WatchModems(ctx context.Context, onAdded func(dbus.ObjectPath), onRemoved func(dbus.ObjectPath)) error {
	// Use a larger buffer to handle signal bursts and prevent D-Bus blocking
	signals := make(chan *dbus.Signal, 100)
	c.conn.Signal(signals)

	matchRules := []string{
		fmt.Sprintf("type='signal',sender='%s',interface='%s',member='InterfacesAdded'",
			ModemManagerService, DBusObjectManager),
		fmt.Sprintf("type='signal',sender='%s',interface='%s',member='InterfacesRemoved'",
			ModemManagerService, DBusObjectManager),
	}

	for _, rule := range matchRules {
		if err := c.conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, rule).Err; err != nil {
			return errors.Wrap(err, "failed to add match rule")
		}
	}

	go func() {
		defer close(signals)
		defer c.conn.RemoveSignal(signals)
		for {
			select {
			case <-ctx.Done():
				return
			case signal, ok := <-signals:
				if !ok {
					return
				}
				switch signal.Name {
				case DBusObjectManager + ".InterfacesAdded":
					if len(signal.Body) >= 2 {
						if path, ok := signal.Body[0].(dbus.ObjectPath); ok {
							if interfaces, ok := signal.Body[1].(map[string]map[string]dbus.Variant); ok {
								if _, hasModem := interfaces[ModemInterface]; hasModem {
									c.log("Modem added: %s", path)
									if onAdded != nil {
										onAdded(path)
									}
								}
							}
						}
					}
				case DBusObjectManager + ".InterfacesRemoved":
					if len(signal.Body) >= 2 {
						if path, ok := signal.Body[0].(dbus.ObjectPath); ok {
							if interfaces, ok := signal.Body[1].([]string); ok {
								for _, iface := range interfaces {
									if iface == ModemInterface {
										c.log("Modem removed: %s", path)
										if onRemoved != nil {
											onRemoved(path)
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}()

	return nil
}

// WatchPropertyChanges watches for property changes on a modem
func (c *Client) WatchPropertyChanges(ctx context.Context, modemPath dbus.ObjectPath, onChange func(string, string, dbus.Variant)) error {
	// Use a larger buffer to handle signal bursts and prevent D-Bus blocking
	signals := make(chan *dbus.Signal, 100)
	c.conn.Signal(signals)

	rule := fmt.Sprintf("type='signal',sender='%s',path='%s',interface='%s',member='PropertiesChanged'",
		ModemManagerService, modemPath, DBusPropertiesInterface)

	if err := c.conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, rule).Err; err != nil {
		return errors.Wrap(err, "failed to add match rule")
	}

	go func() {
		defer close(signals)
		defer c.conn.RemoveSignal(signals)
		for {
			select {
			case <-ctx.Done():
				return
			case signal, ok := <-signals:
				if !ok {
					return
				}
				if signal.Name == DBusPropertiesInterface+".PropertiesChanged" && signal.Path == modemPath {
					if len(signal.Body) >= 2 {
						if iface, ok := signal.Body[0].(string); ok {
							if changed, ok := signal.Body[1].(map[string]dbus.Variant); ok {
								for prop, value := range changed {
									c.log("Property changed: %s.%s = %v", iface, prop, value.Value())
									if onChange != nil {
										onChange(iface, prop, value)
									}
								}
							}
						}
					}
				}
			}
		}
	}()

	return nil
}

// SMSProperties holds the subset of org.freedesktop.ModemManager1.Sms
// properties the service cares about. ModemManager exposes no "direction"
// property; PduType distinguishes inbound (DELIVER) from outbound (SUBMIT) —
// see SmsPduTypeIsIncoming.
type SMSProperties struct {
	Number    string // sender (inbound) or recipient (outbound) number
	Text      string // message body
	State     uint32 // MMSmsState
	PduType   uint32 // MMSmsPduType
	Timestamp string // network timestamp (ISO 8601); empty for locally-created
}

// ListSMS returns the D-Bus paths of all SMS objects currently in modem
// storage (Modem.Messaging.List).
func (c *Client) ListSMS(modemPath dbus.ObjectPath) ([]dbus.ObjectPath, error) {
	call := c.CallMethod(modemPath, ModemMessagingInterface, "List")
	if call.Err != nil {
		return nil, call.Err
	}
	var paths []dbus.ObjectPath
	err := call.Store(&paths)
	return paths, err
}

// CreateSMS creates a new outbound SMS object and returns its D-Bus path.
// Messaging.Create takes a properties dict; "number" and "text" are the only
// keys we set. The object is not transmitted until SendSMS is called on it.
func (c *Client) CreateSMS(modemPath dbus.ObjectPath, number, text string) (dbus.ObjectPath, error) {
	properties := map[string]dbus.Variant{
		"number": dbus.MakeVariant(number),
		"text":   dbus.MakeVariant(text),
	}
	call := c.CallMethod(modemPath, ModemMessagingInterface, "Create", properties)
	if call.Err != nil {
		return "", call.Err
	}
	var smsPath dbus.ObjectPath
	err := call.Store(&smsPath)
	return smsPath, err
}

// DeleteSMS removes an SMS object from modem storage (Modem.Messaging.Delete).
func (c *Client) DeleteSMS(modemPath, smsPath dbus.ObjectPath) error {
	call := c.CallMethod(modemPath, ModemMessagingInterface, "Delete", smsPath)
	return call.Err
}

// SendSMS transmits a previously-created SMS object. Send is a method on the
// Sms object itself, so it is called on smsPath rather than the modem path.
func (c *Client) SendSMS(smsPath dbus.ObjectPath) error {
	call := c.CallMethod(smsPath, SmsInterface, "Send")
	return call.Err
}

// GetSMSProperties reads the relevant properties of an Sms object via
// org.freedesktop.DBus.Properties.GetAll. Absent keys are left as zero values
// (some are only populated for certain states/directions).
func (c *Client) GetSMSProperties(smsPath dbus.ObjectPath) (SMSProperties, error) {
	obj := c.conn.Object(ModemManagerService, smsPath)

	var props map[string]dbus.Variant
	if err := obj.Call(DBusPropertiesInterface+".GetAll", 0, SmsInterface).Store(&props); err != nil {
		return SMSProperties{}, errors.Wrap(err, "failed to get SMS properties")
	}

	var p SMSProperties
	if v, ok := props["Number"]; ok {
		p.Number, _ = v.Value().(string)
	}
	if v, ok := props["Text"]; ok {
		p.Text, _ = v.Value().(string)
	}
	if v, ok := props["State"]; ok {
		p.State, _ = v.Value().(uint32)
	}
	if v, ok := props["PduType"]; ok {
		p.PduType, _ = v.Value().(uint32)
	}
	if v, ok := props["Timestamp"]; ok {
		p.Timestamp, _ = v.Value().(string)
	}
	return p, nil
}

// WatchSMSAdded subscribes to Modem.Messaging "Added" signals on modemPath and
// invokes onAdded(smsPath, received) for each one; received is TRUE for
// messages delivered by the network (inbound). The watch runs until ctx is
// cancelled, so it can be re-armed after a modem reset rebinds the D-Bus path.
// Mirrors WatchPropertyChanges.
func (c *Client) WatchSMSAdded(ctx context.Context, modemPath dbus.ObjectPath, onAdded func(dbus.ObjectPath, bool)) error {
	// Use a larger buffer to handle signal bursts and prevent D-Bus blocking
	signals := make(chan *dbus.Signal, 100)
	c.conn.Signal(signals)

	rule := fmt.Sprintf("type='signal',sender='%s',path='%s',interface='%s',member='Added'",
		ModemManagerService, modemPath, ModemMessagingInterface)

	if err := c.conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, rule).Err; err != nil {
		// Unregister the channel we just added so the connection doesn't keep
		// dispatching signals into a channel nobody reads.
		c.conn.RemoveSignal(signals)
		return errors.Wrap(err, "failed to add match rule")
	}

	go func() {
		defer close(signals)
		defer c.conn.RemoveSignal(signals)
		// Drop the match rule when the watch is cancelled so repeated re-arms
		// (after modem recovery) don't accumulate rules on the connection.
		defer c.conn.BusObject().Call("org.freedesktop.DBus.RemoveMatch", 0, rule)
		for {
			select {
			case <-ctx.Done():
				return
			case signal, ok := <-signals:
				if !ok {
					return
				}
				if signal.Name == ModemMessagingInterface+".Added" && signal.Path == modemPath {
					if len(signal.Body) >= 2 {
						smsPath, _ := signal.Body[0].(dbus.ObjectPath)
						received, _ := signal.Body[1].(bool)
						c.log("SMS added: %s (received=%v)", smsPath, received)
						if onAdded != nil {
							onAdded(smsPath, received)
						}
					}
				}
			}
		}
	}()

	return nil
}

func (c *Client) log(format string, args ...interface{}) {
	if c.debug {
		c.logger("[MM] "+format, args...)
	}
}

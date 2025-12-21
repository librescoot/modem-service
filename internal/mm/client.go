package mm

import (
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
func (c *Client) WatchModems(onAdded func(dbus.ObjectPath), onRemoved func(dbus.ObjectPath)) error {
	signals := make(chan *dbus.Signal, 10)
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
		for signal := range signals {
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
	}()

	return nil
}

// WatchPropertyChanges watches for property changes on a modem
func (c *Client) WatchPropertyChanges(modemPath dbus.ObjectPath, onChange func(string, string, dbus.Variant)) error {
	signals := make(chan *dbus.Signal, 10)
	c.conn.Signal(signals)

	rule := fmt.Sprintf("type='signal',sender='%s',path='%s',interface='%s',member='PropertiesChanged'",
		ModemManagerService, modemPath, DBusPropertiesInterface)

	if err := c.conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, rule).Err; err != nil {
		return errors.Wrap(err, "failed to add match rule")
	}

	go func() {
		for signal := range signals {
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
	}()

	return nil
}

func (c *Client) log(format string, args ...interface{}) {
	if c.debug {
		c.logger("[MM] "+format, args...)
	}
}

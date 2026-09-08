package mm

import (
	"context"
	"fmt"

	"github.com/godbus/dbus/v5"
	"github.com/pkg/errors"
)

func (c *Client) watchSignals(ctx context.Context, rules []string, handle func(*dbus.Signal)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	signals := make(chan *dbus.Signal, 100)
	c.conn.Signal(signals)
	added := make([]string, 0, len(rules))
	for _, rule := range rules {
		// Await the reply: cancellation could hide a successful AddMatch from cleanup.
		if err := c.conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, rule).Err; err != nil {
			c.removeSignalWatch(signals, added)
			return errors.Wrap(err, "failed to add match rule")
		}
		added = append(added, rule)
	}
	go c.watchSignalLoop(ctx, signals, added, handle)
	return nil
}

func (c *Client) removeSignalWatch(signals chan *dbus.Signal, rules []string) {
	// godbus owns channel closure, including when the connection closes first.
	c.conn.RemoveSignal(signals)
	for _, rule := range rules {
		c.conn.BusObject().Call("org.freedesktop.DBus.RemoveMatch", 0, rule)
	}
}

func (c *Client) watchSignalLoop(ctx context.Context, signals chan *dbus.Signal, rules []string, handle func(*dbus.Signal)) {
	defer c.removeSignalWatch(signals, rules)
	for {
		select {
		case <-ctx.Done():
			return
		case signal, ok := <-signals:
			if !ok || ctx.Err() != nil {
				return
			}
			if signal != nil {
				handle(signal)
			}
		}
	}
}

// WatchModems watches modem addition and removal until ctx is cancelled.
func (c *Client) WatchModems(ctx context.Context, onAdded, onRemoved func(dbus.ObjectPath)) error {
	rules := []string{
		fmt.Sprintf("type='signal',sender='%s',interface='%s',member='InterfacesAdded'", ModemManagerService, DBusObjectManager),
		fmt.Sprintf("type='signal',sender='%s',interface='%s',member='InterfacesRemoved'", ModemManagerService, DBusObjectManager),
	}
	return c.watchSignals(ctx, rules, func(signal *dbus.Signal) {
		if len(signal.Body) < 2 {
			return
		}
		path, ok := signal.Body[0].(dbus.ObjectPath)
		if !ok {
			return
		}
		switch signal.Name {
		case DBusObjectManager + ".InterfacesAdded":
			interfaces, _ := signal.Body[1].(map[string]map[string]dbus.Variant)
			if _, hasModem := interfaces[ModemInterface]; hasModem && onAdded != nil {
				c.log("Modem added: %s", path)
				onAdded(path)
			}
		case DBusObjectManager + ".InterfacesRemoved":
			interfaces, _ := signal.Body[1].([]string)
			for _, iface := range interfaces {
				if iface == ModemInterface && onRemoved != nil {
					c.log("Modem removed: %s", path)
					onRemoved(path)
				}
			}
		}
	})
}

func (c *Client) WatchPropertyChanges(ctx context.Context, modemPath dbus.ObjectPath, onChange func(string, string, dbus.Variant)) error {
	rule := fmt.Sprintf("type='signal',sender='%s',path='%s',interface='%s',member='PropertiesChanged'", ModemManagerService, modemPath, DBusPropertiesInterface)
	return c.watchSignals(ctx, []string{rule}, func(signal *dbus.Signal) {
		if signal.Name != DBusPropertiesInterface+".PropertiesChanged" || signal.Path != modemPath || len(signal.Body) < 2 {
			return
		}
		iface, ok := signal.Body[0].(string)
		if !ok {
			return
		}
		changed, _ := signal.Body[1].(map[string]dbus.Variant)
		for property, value := range changed {
			c.log("Property changed: %s.%s = %v", iface, property, propertyLogValue(property, value))
			if onChange != nil {
				onChange(iface, property, value)
			}
		}
	})
}

func (c *Client) WatchSMSAdded(ctx context.Context, modemPath dbus.ObjectPath, onAdded func(dbus.ObjectPath, bool)) error {
	rule := fmt.Sprintf("type='signal',sender='%s',path='%s',interface='%s',member='Added'", ModemManagerService, modemPath, ModemMessagingInterface)
	return c.watchSignals(ctx, []string{rule}, func(signal *dbus.Signal) {
		if signal.Name != ModemMessagingInterface+".Added" || signal.Path != modemPath || len(signal.Body) < 2 {
			return
		}
		path, pathOK := signal.Body[0].(dbus.ObjectPath)
		received, receivedOK := signal.Body[1].(bool)
		if pathOK && receivedOK && onAdded != nil {
			c.log("SMS added: %s (received=%v)", path, received)
			onAdded(path, received)
		}
	})
}

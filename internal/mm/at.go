package mm

import (
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

// AT command helpers for SIM7100E modem via ModemManager D-Bus

// GetIMEI gets the modem IMEI
func (c *Client) GetIMEI(modemPath dbus.ObjectPath) (string, error) {
	resp, err := c.SendCommand(modemPath, "AT+GSN", 5*time.Second)
	if err != nil {
		return "", err
	}
	return extractValue(resp), nil
}

// GetICCID gets the SIM ICCID
func (c *Client) GetICCID(modemPath dbus.ObjectPath) (string, error) {
	resp, err := c.SendCommand(modemPath, "AT+ICCID", 5*time.Second)
	if err != nil {
		return "", err
	}
	return extractPrefixedValue(resp, "+ICCID:"), nil
}

// Utility functions

func extractValue(resp string) string {
	lines := strings.Split(resp, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && line != "OK" && !strings.HasPrefix(line, "AT") && !strings.HasPrefix(line, "+") {
			return line
		}
	}
	return ""
}

func extractPrefixedValue(resp, prefix string) string {
	lines := strings.Split(resp, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			value := strings.TrimPrefix(line, prefix)
			return strings.TrimSpace(value)
		}
	}
	return ""
}

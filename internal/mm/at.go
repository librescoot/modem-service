package mm

import (
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

// atFailure never returns the modem's error detail, which can echo credentials.
func atFailure(response string) string {
	for _, line := range strings.FieldsFunc(response, func(r rune) bool { return r == '\r' || r == '\n' }) {
		line = strings.ToUpper(strings.TrimSpace(line))
		if line == "ERROR" {
			return "ERROR"
		}
		for _, prefix := range []string{"+CME ERROR", "+CMS ERROR"} {
			if line == prefix || strings.HasPrefix(line, prefix+":") {
				return prefix
			}
		}
	}
	return ""
}

// GetIMEI returns the modem IMEI.
func (c *Client) GetIMEI(modemPath dbus.ObjectPath) (string, error) {
	resp, err := c.SendCommand(modemPath, "AT+GSN", 5*time.Second)
	if err != nil {
		return "", err
	}
	return extractValue(resp), nil
}

// GetICCID returns the SIM ICCID.
func (c *Client) GetICCID(modemPath dbus.ObjectPath) (string, error) {
	resp, err := c.SendCommand(modemPath, "AT+ICCID", 5*time.Second)
	if err != nil {
		return "", err
	}
	return extractPrefixedValue(resp, "+ICCID:"), nil
}

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

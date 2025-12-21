package mm

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/pkg/errors"
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

// GetIMSI gets the SIM IMSI
func (c *Client) GetIMSI(modemPath dbus.ObjectPath) (string, error) {
	resp, err := c.SendCommand(modemPath, "AT+CIMI", 5*time.Second)
	if err != nil {
		return "", err
	}
	return extractValue(resp), nil
}

// GPS Configuration

// ConfigureGPS configures GPS with optimal settings for SIM7100E
func (c *Client) ConfigureGPS(modemPath dbus.ObjectPath) error {
	c.log("Configuring GPS...")

	// Stop GPS if running
	c.SendCommand(modemPath, "AT+CGPS=0", 5*time.Second)

	// Disable auto-start
	if _, err := c.SendCommand(modemPath, "AT+CGPSAUTO=0", 5*time.Second); err != nil {
		return errors.Wrap(err, "failed to disable GPS auto-start")
	}

	// Set GPS mode (0 = network assisted, not standalone)
	if _, err := c.SendCommand(modemPath, "AT+CGPSMSB=0", 5*time.Second); err != nil {
		return errors.Wrap(err, "failed to set GPS mode")
	}

	// Set accuracy threshold (50 meters)
	if _, err := c.SendCommand(modemPath, "AT+CGPSHOR=50", 5*time.Second); err != nil {
		return errors.Wrap(err, "failed to set GPS accuracy threshold")
	}

	// Set positioning mode (7 = all techniques)
	if _, err := c.SendCommand(modemPath, "AT+CGPSPMD=7", 5*time.Second); err != nil {
		c.log("Warning: failed to set GPS positioning mode: %v", err)
	}

	// Set GPS antenna voltage (3.05V)
	if _, err := c.SendCommand(modemPath, "AT+CVAUXV=3050", 5*time.Second); err != nil {
		return errors.Wrap(err, "failed to set GPS antenna voltage")
	}

	// Enable GPS antenna supply
	if _, err := c.SendCommand(modemPath, "AT+CVAUXS=1", 5*time.Second); err != nil {
		return errors.Wrap(err, "failed to enable GPS antenna supply")
	}

	// Set clock for GPS
	now := time.Now()
	_, offset := now.Zone()
	offsetHours := offset / 3600
	timeStr := now.Format("06/01/02,15:04:05")
	tzStr := fmt.Sprintf("%+03d", offsetHours)
	clockCmd := fmt.Sprintf("AT+CCLK=\"%s%s\"", timeStr, tzStr)
	if _, err := c.SendCommand(modemPath, clockCmd, 5*time.Second); err != nil {
		c.log("Warning: failed to set clock: %v", err)
	}

	// Enable XTRA assistance
	if _, err := c.SendCommand(modemPath, "AT+CGPSXE=1", 5*time.Second); err != nil {
		c.log("Warning: failed to enable XTRA: %v", err)
	}

	// Set XTRA source (0 = HTTP)
	if _, err := c.SendCommand(modemPath, "AT+CGPSXD=0", 5*time.Second); err != nil {
		c.log("Warning: failed to set XTRA source: %v", err)
	}

	// Enable XTRA auto-download
	if _, err := c.SendCommand(modemPath, "AT+CGPSXDAUTO=1", 5*time.Second); err != nil {
		c.log("Warning: failed to enable XTRA auto-download: %v", err)
	}

	// Disable SSL for GPS (simpler)
	c.SendCommand(modemPath, "AT+CGPSSSL=0", 5*time.Second)

	c.log("GPS configuration complete")
	return nil
}

// StartGPS starts the GPS engine
func (c *Client) StartGPS(modemPath dbus.ObjectPath, mode int) error {
	cmd := fmt.Sprintf("AT+CGPS=1,%d", mode)
	_, err := c.SendCommand(modemPath, cmd, 10*time.Second)
	return err
}

// StopGPS stops the GPS engine
func (c *Client) StopGPS(modemPath dbus.ObjectPath) error {
	_, err := c.SendCommand(modemPath, "AT+CGPS=0", 5*time.Second)
	return err
}

// GetGPSInfo gets raw GPS data via AT+CGPSINFO
func (c *Client) GetGPSInfo(modemPath dbus.ObjectPath) (string, error) {
	resp, err := c.SendCommand(modemPath, "AT+CGPSINFO", 5*time.Second)
	if err != nil {
		return "", err
	}
	return extractPrefixedValue(resp, "+CGPSINFO:"), nil
}

// GetGPSStatus checks if GPS is running
func (c *Client) GetGPSStatus(modemPath dbus.ObjectPath) (bool, error) {
	resp, err := c.SendCommand(modemPath, "AT+CGPS?", 5*time.Second)
	if err != nil {
		return false, err
	}

	value := extractPrefixedValue(resp, "+CGPS:")
	parts := strings.Split(value, ",")
	if len(parts) < 1 {
		return false, nil
	}

	mode := strings.TrimSpace(parts[0])
	return mode == "1", nil
}

// ParseGPSInfo parses AT+CGPSINFO response
// Format: +CGPSINFO: lat,N/S,lon,E/W,date,time,alt,speed,course
type GPSInfo struct {
	Latitude  float64
	Longitude float64
	Altitude  float64
	Speed     float64
	Course    float64
	Timestamp time.Time
	Valid     bool
}

func ParseGPSInfo(info string) (*GPSInfo, error) {
	parts := strings.Split(strings.TrimSpace(info), ",")
	if len(parts) < 9 {
		return &GPSInfo{Valid: false}, nil
	}

	// Check if we have valid data
	if parts[0] == "" || parts[0] == "0.0" {
		return &GPSInfo{Valid: false}, nil
	}

	gps := &GPSInfo{Valid: true}

	// Parse latitude (NMEA format: ddmm.mmmmmm)
	if lat, err := strconv.ParseFloat(parts[0], 64); err == nil {
		gps.Latitude = nmeaToDecimal(lat)
		if parts[1] == "S" {
			gps.Latitude = -gps.Latitude
		}
	}

	// Parse longitude (NMEA format: dddmm.mmmmmm)
	if lon, err := strconv.ParseFloat(parts[2], 64); err == nil {
		gps.Longitude = nmeaToDecimal(lon)
		if parts[3] == "W" {
			gps.Longitude = -gps.Longitude
		}
	}

	// Parse altitude
	if alt, err := strconv.ParseFloat(parts[6], 64); err == nil {
		gps.Altitude = alt
	}

	// Parse speed (convert to km/h)
	if speed, err := strconv.ParseFloat(parts[7], 64); err == nil {
		gps.Speed = speed // Already in km/h
	}

	// Parse course
	if course, err := strconv.ParseFloat(parts[8], 64); err == nil {
		gps.Course = course
	}

	// Parse timestamp
	if len(parts[4]) == 6 && len(parts[5]) >= 6 {
		dateStr := parts[4] // DDMMYY
		timeStr := parts[5] // HHMMSS.S

		day, _ := strconv.Atoi(dateStr[0:2])
		month, _ := strconv.Atoi(dateStr[2:4])
		year, _ := strconv.Atoi(dateStr[4:6])
		year += 2000

		hour, _ := strconv.Atoi(timeStr[0:2])
		min, _ := strconv.Atoi(timeStr[2:4])
		sec, _ := strconv.Atoi(timeStr[4:6])

		gps.Timestamp = time.Date(year, time.Month(month), day, hour, min, sec, 0, time.UTC)
	}

	return gps, nil
}

// nmeaToDecimal converts NMEA coordinate to decimal degrees
func nmeaToDecimal(nmea float64) float64 {
	// NMEA format: dddmm.mmmmmm (degrees + minutes)
	degrees := int(nmea / 100)
	minutes := nmea - float64(degrees*100)
	return float64(degrees) + (minutes / 60.0)
}

// Network Commands

// GetSignalQuality gets signal quality (0-100%)
func (c *Client) GetSignalQuality(modemPath dbus.ObjectPath) (int, error) {
	resp, err := c.SendCommand(modemPath, "AT+CSQ", 5*time.Second)
	if err != nil {
		return 0, err
	}

	value := extractPrefixedValue(resp, "+CSQ:")
	parts := strings.Split(value, ",")
	if len(parts) < 1 {
		return 0, fmt.Errorf("invalid CSQ response: %s", resp)
	}

	rssi, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, err
	}

	// Convert RSSI to percentage (0-31 -> 0-100)
	if rssi == 99 {
		return 0, nil // Unknown
	}
	return (rssi * 100) / 31, nil
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

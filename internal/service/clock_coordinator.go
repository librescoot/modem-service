package service

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"modem-service/internal/location"
)

type clockQuality struct {
	BootID      string  `json:"boot_id"`
	Generation  uint64  `json:"generation"`
	Source      string  `json:"source"`
	External    bool    `json:"external"`
	AgeNS       int64   `json:"age_ns"`
	Residual    float64 `json:"residual_seconds"`
	Uncertainty float64 `json:"uncertainty_seconds"`
	Outcome     string  `json:"outcome"`
	Preparing   bool    `json:"preparing"`
}
type clockResponse struct {
	OK      bool         `json:"ok"`
	Error   string       `json:"error"`
	Quality clockQuality `json:"quality"`
}

func clockRequest(request any) (clockResponse, error) {
	var response clockResponse
	var started, finished unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &started); err != nil {
		return response, err
	}
	c, err := net.DialTimeout("unix", "/run/clock-coordinator/control.sock", time.Second)
	if err != nil {
		return response, err
	}
	defer c.Close()
	if err = c.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return response, err
	}
	if err = json.NewEncoder(c).Encode(request); err != nil {
		return response, err
	}
	var raw struct {
		OK      bool            `json:"ok"`
		Error   string          `json:"error"`
		Quality json.RawMessage `json:"quality"`
	}
	if err = json.NewDecoder(c).Decode(&raw); err != nil {
		return response, fmt.Errorf("unknown clock request outcome: %w", err)
	}
	if err = json.Unmarshal(raw.Quality, &response.Quality); err != nil {
		return response, err
	}
	response.OK, response.Error = raw.OK, raw.Error
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw.Quality, &fields); err != nil {
		return response, err
	}
	for _, name := range []string{"boot_id", "generation", "source", "external", "age_ns", "residual_seconds", "uncertainty_seconds", "outcome", "preparing"} {
		if v, ok := fields[name]; !ok || string(v) == "null" {
			return response, fmt.Errorf("incomplete coordinator quality: %s", name)
		}
	}
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &finished); err != nil {
		return response, err
	}
	elapsed := finished.Nano() - started.Nano()
	if elapsed < 0 || elapsed > int64(30*time.Second) || response.Quality.AgeNS < 0 || response.Quality.AgeNS > int64(30*time.Second)-elapsed {
		return response, fmt.Errorf("stale coordinator response")
	}
	response.Quality.AgeNS += elapsed
	if !response.OK {
		return response, fmt.Errorf("coordinator rejected request: %s", response.Error)
	}
	return response, nil
}
func gpsProgresses(a, b location.Location) bool {
	if !a.TimeFromGPS || a.GPSSession == 0 || a.GPSSession != b.GPSSession || a.ReceiptBootNS <= 0 || b.ReceiptBootNS <= a.ReceiptBootNS || b.ReceiptMonoNS <= a.ReceiptMonoNS {
		return false
	}
	elapsed := time.Duration(b.ReceiptBootNS - a.ReceiptBootNS)
	active := time.Duration(b.ReceiptMonoNS - a.ReceiptMonoNS)
	delta := b.Timestamp.Sub(a.Timestamp)
	return elapsed <= 5*time.Second && elapsed-active < 500*time.Millisecond && delta > 0 && (delta-elapsed).Abs() < 2*time.Second
}
func gpsReceiptFresh(loc location.Location, nowBoot, nowMono int64) bool {
	bootAge, monoAge := nowBoot-loc.ReceiptBootNS, nowMono-loc.ReceiptMonoNS
	return loc.ReceiptBootNS > 0 && loc.ReceiptMonoNS > 0 && bootAge >= 0 && monoAge >= 0 && bootAge <= int64(2*time.Second) && bootAge-monoAge <= int64(500*time.Millisecond)
}
func (s *Service) submitGPSClock(loc location.Location) error {
	var now unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &now); err != nil {
		return err
	}
	var mono unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &mono); err != nil {
		return err
	}
	if !gpsReceiptFresh(loc, now.Nano(), mono.Nano()) {
		return fmt.Errorf("stale GPS receipt")
	}

	status, err := clockRequest(map[string]any{"op": "status"})
	if err != nil {
		return err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}
	q := status.Quality
	if q.BootID != strings.TrimSpace(string(boot)) || q.BootID == "" || q.Generation == 0 {
		return fmt.Errorf("invalid coordinator identity")
	}
	if gpsClockConfirmed(q) && time.Since(s.lastClockSync) >= 30*time.Second {
		if err := s.Redis.PublishClockSync("gps", time.Now()); err != nil {
			s.Logger.Printf("GPS clock confirmation: %v", err)
		} else {
			s.lastClockSync = time.Now()
		}
	}
	_, err = clockRequest(map[string]any{"op": "submit", "boot_id": q.BootID, "generation": q.Generation, "source": "gps", "utc_unix_ns": loc.Timestamp.UnixNano(), "receipt_boot_ns": loc.ReceiptBootNS, "deadline_boot_ns": loc.ReceiptBootNS + int64(2*time.Second), "uncertainty_ns": loc.TimeUncertaintyNS})
	return err
}
func gpsClockConfirmed(q clockQuality) bool {
	return !q.Preparing && q.Source == "gps" && q.External && q.AgeNS >= 0 && q.AgeNS <= int64(30*time.Second) && q.Outcome != "unknown" && q.Outcome != "in-flight" && q.Outcome != "" && !math.IsNaN(q.Residual) && !math.IsNaN(q.Uncertainty) && q.Uncertainty >= 0 && math.Abs(q.Residual)+q.Uncertainty <= 5
}

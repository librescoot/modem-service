package service

import (
	"testing"
	"time"

	"modem-service/internal/location"
)

func TestGPSClockTimeRejectsSystemFallback(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		loc  location.Location
		want bool
	}{
		{"fallback", location.Location{Timestamp: now}, false},
		{"missing", location.Location{TimeFromGPS: true}, false},
		{"before rollover epoch", location.Location{Timestamp: location.MinValidGPSDate.Add(-time.Second), TimeFromGPS: true}, false},
		{"corrected gps", location.Location{Timestamp: now, TimeFromGPS: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, got := gpsClockTime(tc.loc)
			if got != tc.want {
				t.Fatalf("valid=%v, want %v", got, tc.want)
			}
		})
	}
}

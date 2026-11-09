package service

import (
	"testing"
	"time"

	"modem-service/internal/location"
)

func TestGPSIntervalFollowsVehicleState(t *testing.T) {
	s := &Service{gpsIntervalChanged: make(chan struct{}, 1)}
	s.modemEnabled.Store(true)
	for _, tc := range []struct {
		state   string
		want    time.Duration
		changed bool
	}{
		{"", time.Second, false},
		{"stand-by", 10 * time.Second, true},
		{"hibernating", 10 * time.Second, false},
		{"parked", time.Second, true},
		{"ready-to-drive", time.Second, false},
		{"stand-by", 10 * time.Second, true},
		{"shutting-down", time.Second, true},
		{"stand-by", 10 * time.Second, true},
		{"", time.Second, true},
	} {
		if err := s.handleVehicleState(tc.state); err != nil {
			t.Fatal(err)
		}
		if got := s.gpsUpdateInterval(); got != tc.want {
			t.Errorf("state %q: interval = %v, want %v", tc.state, got, tc.want)
		}
		select {
		case <-s.gpsIntervalChanged:
			if !tc.changed {
				t.Errorf("state %q: unexpected change", tc.state)
			}
		default:
			if tc.changed {
				t.Errorf("state %q: missing change", tc.state)
			}
		}
	}
}

// The gpsd stream is decoded at the cadence the results are used at.
func TestGPSStreamDecodeIntervalFollowsVehicleState(t *testing.T) {
	s := &Service{gpsIntervalChanged: make(chan struct{}, 1), Location: &location.Service{}}
	s.modemEnabled.Store(true)
	for state, want := range map[string]time.Duration{
		"stand-by":       gpsLowPowerInterval,
		"parked":         0,
		"hibernating":    gpsLowPowerInterval,
		"ready-to-drive": 0,
	} {
		if err := s.handleVehicleState(state); err != nil {
			t.Fatal(err)
		}
		if got := s.Location.ReportInterval(); got != want {
			t.Errorf("state %q: decode interval %v, want %v", state, got, want)
		}
	}
}

package location

import (
	"errors"
	"io"
	"log"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

func TestCloseClearsFixState(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	s := NewService(logger, "", nil, "")

	// Simulate a valid fix captured just before teardown (e.g. parking
	// outdoors before the scooter suspends).
	s.hasValidFix.Store(true)
	s.stateMutex.Lock()
	s.currentLoc = Location{Latitude: 52.5, Longitude: 13.4, Timestamp: time.Now()}
	s.lastFix = time.Now()
	s.stateMutex.Unlock()

	s.Close()

	if s.HasValidFix() {
		t.Error("HasValidFix() = true after Close(), want false (stale fix would be replayed into the clock on resume)")
	}
	if ts := s.CurrentLoc().Timestamp; !ts.IsZero() {
		t.Errorf("CurrentLoc().Timestamp = %v after Close(), want zero", ts)
	}
	// Last known position is deliberately kept for last-known consumers.
	if loc := s.CurrentLoc(); loc.Latitude == 0 || loc.Longitude == 0 {
		t.Errorf("CurrentLoc() lat/lng cleared = %+v, want preserved", loc)
	}
}

func TestIsStalePathError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("connection refused"), false},
		{"mm-rebind text", errors.New(`AT command failed: AT+CGPS?: Object does not exist at path "/org/freedesktop/ModemManager1/Modem/1"`), true},
		{"dbus name", errors.New("org.freedesktop.DBus.Error.UnknownObject: not found"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isStalePathError(tc.err); got != tc.want {
				t.Fatalf("isStalePathError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestRefreshModemPathIfStale(t *testing.T) {
	logger := log.New(io.Discard, "", 0)

	t.Run("no resolver leaves path unchanged", func(t *testing.T) {
		s := &Service{Logger: logger, ModemPath: "/Modem/1"}
		if s.refreshModemPathIfStale(errors.New("Object does not exist at path")) {
			t.Fatal("expected false when no resolver wired")
		}
		if s.ModemPath != "/Modem/1" {
			t.Fatalf("ModemPath unexpectedly changed to %q", s.ModemPath)
		}
	})

	t.Run("non-stale error is ignored", func(t *testing.T) {
		called := 0
		s := &Service{
			Logger:    logger,
			ModemPath: "/Modem/1",
			ResolveModemPath: func() (dbus.ObjectPath, error) {
				called++
				return "/Modem/2", nil
			},
		}
		if s.refreshModemPathIfStale(errors.New("timeout")) {
			t.Fatal("expected false for non-stale error")
		}
		if called != 0 {
			t.Fatalf("resolver called %d times, want 0", called)
		}
	})

	t.Run("stale error triggers re-resolve and update", func(t *testing.T) {
		s := &Service{
			Logger:    logger,
			ModemPath: "/Modem/1",
			ResolveModemPath: func() (dbus.ObjectPath, error) {
				return "/Modem/2", nil
			},
		}
		if !s.refreshModemPathIfStale(errors.New(`Object does not exist at path "/Modem/1"`)) {
			t.Fatal("expected true after rebind")
		}
		if s.ModemPath != "/Modem/2" {
			t.Fatalf("ModemPath = %q, want /Modem/2", s.ModemPath)
		}
	})

	t.Run("resolver error leaves path unchanged", func(t *testing.T) {
		s := &Service{
			Logger:    logger,
			ModemPath: "/Modem/1",
			ResolveModemPath: func() (dbus.ObjectPath, error) {
				return "", errors.New("no modem")
			},
		}
		if s.refreshModemPathIfStale(errors.New("Object does not exist at path")) {
			t.Fatal("expected false when resolver fails")
		}
		if s.ModemPath != "/Modem/1" {
			t.Fatalf("ModemPath unexpectedly changed to %q", s.ModemPath)
		}
	})

	t.Run("unchanged path returns false", func(t *testing.T) {
		s := &Service{
			Logger:    logger,
			ModemPath: "/Modem/1",
			ResolveModemPath: func() (dbus.ObjectPath, error) {
				return "/Modem/1", nil
			},
		}
		if s.refreshModemPathIfStale(errors.New("Object does not exist at path")) {
			t.Fatal("expected false when path unchanged")
		}
	})
}

func TestConfigRetryDelayBacksOffAndCaps(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, GPSRetryInterval},
		{1, 10 * time.Second},
		{2, 20 * time.Second},
		{3, 40 * time.Second},
		{4, MaxGPSRetryInterval},
		{50, MaxGPSRetryInterval},
	}
	for _, c := range cases {
		if got := configRetryDelay(c.attempt); got != c.want {
			t.Errorf("configRetryDelay(%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
}

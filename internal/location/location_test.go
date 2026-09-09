package location

import (
	"context"
	"errors"
	"io"
	"log"
	"modem-service/internal/mm"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

func TestEnsureModemPathResolvesEmptyPath(t *testing.T) {
	s := NewService(log.New(io.Discard, "", 0), "", nil, "")
	s.ResolveModemPath = func() (dbus.ObjectPath, error) {
		return dbus.ObjectPath("/Modem/1"), nil
	}
	if err := s.ensureModemPath(); err != nil {
		t.Fatal(err)
	}
	if s.ModemPath != "/Modem/1" {
		t.Fatalf("modem path = %q", s.ModemPath)
	}
}

func TestCloseJoinsBlockedMonitor(t *testing.T) {
	s := NewService(log.New(io.Discard, "", 0), "", nil, "")
	entered := make(chan struct{})
	release := make(chan struct{})
	s.beforeConfigure = func() {
		close(entered)
		<-release
	}
	if err := s.EnableGPS(dbus.ObjectPath("/Modem/1")); err != nil {
		t.Fatal(err)
	}
	<-entered
	modeCtx, cancelMode := s.modeContext(context.Background())
	defer cancelMode()
	closed := make(chan struct{})
	go func() {
		s.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close returned before blocked monitor could exit")
	case <-time.After(20 * time.Millisecond):
	}
	select {
	case <-modeCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel mode-change context")
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not join monitor after it unblocked")
	}
}

func TestCloseCancelsActiveModeChange(t *testing.T) {
	s := NewService(log.New(io.Discard, "", 0), "", nil, "")
	monitorEntered := make(chan struct{})
	releaseMonitor := make(chan struct{})
	s.beforeConfigure = func() {
		close(monitorEntered)
		<-releaseMonitor
	}
	gpsStopped := make(chan struct{}, 1)
	s.sendATCommandFn = func(_ context.Context, command string) (string, error) {
		if command == "AT+CGPS?" {
			return "+CGPS: 1,2", nil
		}
		if command == "AT+CGPS=0" {
			gpsStopped <- struct{}{}
		}
		return "", nil
	}
	if err := s.EnableGPS("/Modem/1"); err != nil {
		t.Fatal(err)
	}
	<-monitorEntered
	modeDone := make(chan error, 1)
	go func() { modeDone <- s.SetGPSMode(context.Background(), ModeStandalone) }()
	<-gpsStopped
	closeDone := make(chan struct{})
	go func() {
		s.Close()
		close(closeDone)
	}()
	select {
	case err := <-modeDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("mode change error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("active mode change was not cancelled by Close")
	}
	close(releaseMonitor)
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Close did not finish after mode change and monitor exited")
	}
}

func TestConcurrentEnableCloseHandoff(t *testing.T) {
	s := NewService(log.New(io.Discard, "", 0), "", nil, "")
	for i := 0; i < 50; i++ {
		start := make(chan struct{})
		done := make(chan struct{}, 2)
		go func() {
			<-start
			_ = s.EnableGPS(dbus.ObjectPath("/Modem/1"))
			done <- struct{}{}
		}()
		go func() {
			<-start
			s.Close()
			done <- struct{}{}
		}()
		close(start)
		<-done
		<-done
		s.Close()
		if s.IsEnabled() {
			t.Fatal("GPS monitor remained active after concurrent handoff")
		}
	}
}

func TestRapidEnableCloseHandoff(t *testing.T) {
	s := NewService(log.New(io.Discard, "", 0), "", nil, "")

	for i := 0; i < 20; i++ {
		if err := s.EnableGPS(dbus.ObjectPath("/Modem/1")); err != nil {
			t.Fatal(err)
		}
		if !s.IsEnabled() {
			t.Fatal("GPS monitor did not start")
		}
		s.Close()
		if s.IsEnabled() {
			t.Fatal("GPS monitor remained active after Close")
		}
	}
}

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

func TestIsConfiguringTracksConfigurationLock(t *testing.T) {
	s := NewService(log.New(io.Discard, "", 0), "", nil, "")
	if s.IsConfiguring() {
		t.Fatal("new service reports configuration in progress")
	}

	s.configMutex.Lock()
	defer s.configMutex.Unlock()
	if !s.IsConfiguring() {
		t.Fatal("service does not report configuration while lock is held")
	}
}

func TestLocationSourceMasksKeepGPSIndependentFromCellFallback(t *testing.T) {
	current := mm.MMModemLocationSourceGpsNmea |
		mm.MMModemLocationSourceAgpsMsb |
		mm.MMModemLocationSource3gppLacCi
	gpsSources, allSources := locationSourceMasks(current)

	if gpsSources&mm.MMModemLocationSourceGpsUnmanaged == 0 {
		t.Error("GPS-only mask does not enable gps-unmanaged")
	}
	if gpsSources&mm.MMModemLocationSource3gppLacCi != 0 {
		t.Error("GPS-only mask unexpectedly requires 3gpp-lac-ci")
	}
	if gpsSources&mm.MMModemLocationSourceGpsNmea != 0 {
		t.Error("GPS-only mask preserves conflicting gps-nmea")
	}
	if gpsSources&mm.MMModemLocationSourceAgpsMsb == 0 {
		t.Error("GPS-only mask does not preserve unrelated sources")
	}
	if allSources != gpsSources|mm.MMModemLocationSource3gppLacCi {
		t.Errorf("all-sources mask = 0x%x, want GPS mask plus 3gpp-lac-ci", allSources)
	}
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

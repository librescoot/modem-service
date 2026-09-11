package location

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"modem-service/internal/mm"
	"strings"
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
	if s.ReadyForModeSwitch() {
		t.Fatal("mode switches enabled before GPS initialization")
	}
	atCalled := false
	s.sendATCommandFn = func(context.Context, string) (string, error) {
		atCalled = true
		return "", nil
	}
	if err := s.SetGPSMode(context.Background(), ModeStandalone); err != nil {
		t.Fatal(err)
	}
	if atCalled {
		t.Fatal("mode switch issued an AT command before GPS initialization")
	}
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
	if s.ReadyForModeSwitch() {
		t.Fatal("mode switches enabled after Close")
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
	s.modeSwitchReady.Store(true)
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

func TestQueuedModeSwitchRechecksReadiness(t *testing.T) {
	s := NewService(log.New(io.Discard, "", 0), "", nil, "")
	s.modeSwitchReady.Store(true)
	atCalled := false
	s.sendATCommandFn = func(context.Context, string) (string, error) {
		atCalled = true
		return "", nil
	}

	s.configMutex.Lock()
	done := make(chan error, 1)
	go func() { done <- s.SetGPSMode(context.Background(), ModeStandalone) }()
	s.modeSwitchReady.Store(false)
	s.configMutex.Unlock()

	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if atCalled {
		t.Fatal("queued mode switch issued an AT command after readiness was cleared")
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

func TestParseCGPSResponse(t *testing.T) {
	cases := []struct {
		name     string
		response string
		running  bool
		mode     GPSMode
		ok       bool
	}{
		{"standalone", "AT+CGPS?\r\n+CGPS: 1,1\r\nOK", true, ModeStandalone, true},
		{"compact ue based", "+CGPS:1,2", true, ModeUEBased, true},
		{"stopped", "+CGPS: 0", false, ModeStandalone, true},
		{"missing mode", "+CGPS: 1", false, ModeStandalone, false},
		{"unsupported mode", "+CGPS: 1,3", true, ModeStandalone, false},
		{"unrelated", "OK", false, ModeStandalone, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			running, mode, ok := parseCGPSResponse(tc.response)
			if running != tc.running || mode != tc.mode || ok != tc.ok {
				t.Fatalf("parseCGPSResponse(%q) = (%v, %v, %v), want (%v, %v, %v)",
					tc.response, running, mode, ok, tc.running, tc.mode, tc.ok)
			}
		})
	}
}

func TestGPSChipStateReuseEligibility(t *testing.T) {
	eligible, err := parseGPSChipState(
		"+CGPS: 1,1", "+CVAUXV: 3050", "+CGPSPMD: 7")
	if err != nil {
		t.Fatal(err)
	}
	if ok, reason := eligible.reuseEligibility(); !ok {
		t.Fatalf("eligible chip state rejected: %s", reason)
	}

	cases := []struct {
		name  string
		state gpsChipState
	}{
		{"stopped", gpsChipState{mode: ModeStandalone, antennaMillivolts: 3050, powerMode: 7}},
		{"wrong mode", gpsChipState{running: true, mode: ModeUEBased, antennaMillivolts: 3050, powerMode: 7}},
		{"wrong voltage", gpsChipState{running: true, mode: ModeStandalone, antennaMillivolts: 3000, powerMode: 7}},
		{"wrong power mode", gpsChipState{running: true, mode: ModeStandalone, antennaMillivolts: 3050, powerMode: 6}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if ok, reason := tc.state.reuseEligibility(); ok || reason == "" {
				t.Fatalf("reuseEligibility() = (%v, %q), want false with reason", ok, reason)
			}
		})
	}

	if _, err := parseGPSChipState("+CGPS: 1,1", "+CVAUXV: bad", "+CGPSPMD: 7"); err == nil {
		t.Fatal("malformed chip response was accepted")
	}
}

func TestQueryGPSChipStateUsesSupportedQueries(t *testing.T) {
	s := NewService(log.New(io.Discard, "", 0), "", nil, "")
	s.ModemPath = "/Modem/1"
	var commands []string
	s.sendATCommandFn = func(_ context.Context, command string) (string, error) {
		commands = append(commands, command)
		switch command {
		case "AT+CGPS?":
			return "+CGPS: 1,1", nil
		case "AT+CVAUXV?":
			return "+CVAUXV: 3050", nil
		case "AT+CGPSPMD?":
			return "+CGPSPMD: 7", nil
		default:
			t.Fatalf("unexpected command %q", command)
			return "", nil
		}
	}
	state, err := s.queryGPSChipState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ok, reason := state.reuseEligibility(); !ok {
		t.Fatalf("queried state rejected: %s", reason)
	}
	want := []string{"AT+CGPS?", "AT+CVAUXV?", "AT+CGPSPMD?"}
	if len(commands) != len(want) {
		t.Fatalf("commands = %v, want %v", commands, want)
	}
	for i := range want {
		if commands[i] != want[i] {
			t.Fatalf("commands = %v, want %v", commands, want)
		}
	}
}

func TestConfigureGPSPowerModeRequiresVerifiedValue(t *testing.T) {
	cases := []struct {
		name      string
		setErr    error
		queryResp string
		queryErr  error
		wantErr   bool
	}{
		{name: "verified", queryResp: "+CGPSPMD: 7"},
		{name: "set failed", setErr: errors.New("set failed"), wantErr: true},
		{name: "query failed", queryErr: errors.New("query failed"), wantErr: true},
		{name: "wrong value", queryResp: "+CGPSPMD: 6", wantErr: true},
		{name: "malformed value", queryResp: "OK", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewService(log.New(io.Discard, "", 0), "", nil, "")
			s.ModemPath = "/Modem/1"
			s.sendATCommandFn = func(_ context.Context, command string) (string, error) {
				switch command {
				case "AT+CGPSPMD=7":
					return "", tc.setErr
				case "AT+CGPSPMD?":
					return tc.queryResp, tc.queryErr
				default:
					t.Fatalf("unexpected command %q", command)
					return "", nil
				}
			}
			err := s.configureGPSPowerMode(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("configureGPSPowerMode() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestConfigureReceiverOrdersCommandsAndEnforcesRestartFloor(t *testing.T) {
	s := NewService(log.New(io.Discard, "", 0), "", nil, "")
	s.ModemPath = "/Modem/1"
	base := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	now := base
	s.nowFn = func() time.Time { return now }

	var commands []string
	var startIssuedAt time.Time
	queryCount := 0
	s.sendATCommandFn = func(_ context.Context, command string) (string, error) {
		commands = append(commands, command)
		if command == "AT+CGPS=1,1" {
			startIssuedAt = now
		}
		now = now.Add(100 * time.Millisecond)
		switch command {
		case "AT+CGPS?":
			queryCount++
			if queryCount == 1 {
				return "+CGPS: 1,1", nil
			}
			if queryCount == 2 {
				return "+CGPS: 0", nil
			}
			return "+CGPS: 1,1", nil
		case "AT+CGPSPMD?":
			return "+CGPSPMD: 7", nil
		case "AT+CVAUXV?":
			return "+CVAUXV: 3050", nil
		default:
			return "", nil
		}
	}

	var waited time.Duration
	s.waitFn = func(ctx context.Context, remaining time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		waited += remaining
		now = now.Add(remaining)
		return nil
	}

	if err := s.configureReceiver(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantStart := base.Add(gpsRestartMinimumInterval)
	if !startIssuedAt.Equal(wantStart) {
		t.Fatalf("receiver started at %v, want not-before time %v", startIssuedAt, wantStart)
	}
	if waited != 500*time.Millisecond {
		t.Fatalf("total wait = %v, want 500ms across stop polling and restart floor", waited)
	}

	normalized := make([]string, len(commands))
	for i, command := range commands {
		if strings.HasPrefix(command, "AT+CCLK=") {
			normalized[i] = "AT+CCLK"
		} else {
			normalized[i] = command
		}
	}
	want := []string{
		"AT+CGPS=0", "AT+CGPS?", "AT+CGPS?", "AT+CGPSPMD=7", "AT+CGPSPMD?",
		"AT+CGPSAUTO=0", "AT+CGPSHOR=50", "AT+CGDRT=41,1", "AT+CGSETV=41,1",
		"AT+CCLK", "AT+CGPSNMEA=511", "AT+CVAUXV?", "AT+CVAUXV=3050",
		"AT+CVAUXS=1", "AT+CCLK", "AT+CGPS=1,1", "AT+CGPS?", "AT+CGPSNOTIFY=0",
	}
	if len(normalized) != len(want) {
		t.Fatalf("commands = %v, want %v", normalized, want)
	}
	for i := range want {
		if normalized[i] != want[i] {
			t.Fatalf("commands = %v, want %v", normalized, want)
		}
	}
}

func TestConfigureGPSViaATCommandsWaitsPastRestartFloorForStoppedState(t *testing.T) {
	s := NewService(log.New(io.Discard, "", 0), "", nil, "")
	s.ModemPath = "/Modem/1"
	base := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	now := base
	s.nowFn = func() time.Time { return now }
	s.waitFn = func(ctx context.Context, delay time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		now = now.Add(delay)
		return nil
	}
	pmdSet := false
	s.sendATCommandFn = func(_ context.Context, command string) (string, error) {
		switch command {
		case "AT+CGPS?":
			if now.Before(base.Add(3 * time.Second)) {
				return "+CGPS: 1,1", nil
			}
			return "+CGPS: 0", nil
		case "AT+CGPSPMD=7":
			pmdSet = true
		case "AT+CGPSPMD?":
			return "+CGPSPMD: 7", nil
		}
		return "", nil
	}
	restartNotBefore, err := s.configureGPSViaATCommands(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !pmdSet {
		t.Fatal("PMD was not configured after delayed stop confirmation")
	}
	if now.Before(base.Add(3 * time.Second)) {
		t.Fatalf("stop accepted at %v, want at least 3s", now.Sub(base))
	}
	if !restartNotBefore.Equal(base.Add(gpsRestartMinimumInterval)) {
		t.Fatalf("restart floor = %v, want %v", restartNotBefore, base.Add(gpsRestartMinimumInterval))
	}
}

func TestConfigureGPSViaATCommandsDoesNotWritePMDWithoutStoppedState(t *testing.T) {
	cases := []struct {
		name      string
		queryResp string
		queryErr  error
	}{
		{name: "verification failed", queryErr: errors.New("query failed")},
		{name: "still running", queryResp: "+CGPS: 1,1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewService(log.New(io.Discard, "", 0), "", nil, "")
			s.ModemPath = "/Modem/1"
			var commands []string
			s.sendATCommandFn = func(_ context.Context, command string) (string, error) {
				commands = append(commands, command)
				if command == "AT+CGPS?" {
					return tc.queryResp, tc.queryErr
				}
				return "", nil
			}
			s.waitFn = func(context.Context, time.Duration) error { return context.Canceled }
			if _, err := s.configureGPSViaATCommands(context.Background()); err == nil {
				t.Fatal("configuration unexpectedly succeeded")
			}
			for _, command := range commands {
				if command == "AT+CGPSPMD=7" {
					t.Fatalf("PMD write issued after failed stop verification: %v", commands)
				}
			}
			if len(commands) != 2 || commands[0] != "AT+CGPS=0" || commands[1] != "AT+CGPS?" {
				t.Fatalf("commands = %v, want stop then verification only", commands)
			}
		})
	}
}

func TestStartStandaloneReceiverSkipsElapsedRestartWait(t *testing.T) {
	s := NewService(log.New(io.Discard, "", 0), "", nil, "")
	s.ModemPath = "/Modem/1"
	base := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return base.Add(gpsRestartMinimumInterval + time.Second) }
	s.waitFn = func(context.Context, time.Duration) error {
		t.Fatal("wait called after restart floor had elapsed")
		return nil
	}
	s.sendATCommandFn = func(_ context.Context, command string) (string, error) {
		if command == "AT+CGPS?" {
			return "+CGPS: 1,1", nil
		}
		return "", nil
	}
	if err := s.startStandaloneReceiver(context.Background(), base.Add(gpsRestartMinimumInterval)); err != nil {
		t.Fatal(err)
	}
}

func TestStartStandaloneReceiverRestartWaitIsCancellable(t *testing.T) {
	for _, elapsed := range []bool{false, true} {
		t.Run(fmt.Sprintf("elapsed=%v", elapsed), func(t *testing.T) {
			s := NewService(log.New(io.Discard, "", 0), "", nil, "")
			s.ModemPath = "/Modem/1"
			base := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
			now := base
			if elapsed {
				now = base.Add(gpsRestartMinimumInterval + time.Second)
			}
			s.nowFn = func() time.Time { return now }
			s.sendATCommandFn = func(_ context.Context, command string) (string, error) {
				t.Fatalf("command %q issued after cancellation", command)
				return "", nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := s.startStandaloneReceiver(ctx, base.Add(gpsRestartMinimumInterval))
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("startStandaloneReceiver() error = %v, want context.Canceled", err)
			}
		})
	}
}

func TestStartStandaloneReceiverRequiresPostStartQuery(t *testing.T) {
	cases := []struct {
		name      string
		startErr  error
		queryResp string
		queryErr  error
		wantErr   bool
	}{
		{name: "verified start", queryResp: "+CGPS: 1,1"},
		{name: "query failed", queryErr: errors.New("query failed"), wantErr: true},
		{name: "command and query failed", startErr: errors.New("start failed"), queryErr: errors.New("query failed"), wantErr: true},
		{name: "command failed and receiver stopped", startErr: errors.New("start failed"), queryResp: "+CGPS: 0", wantErr: true},
		{name: "command failed but verified", startErr: errors.New("start failed"), queryResp: "+CGPS: 1,1"},
		{name: "wrong mode", queryResp: "+CGPS: 1,2", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewService(log.New(io.Discard, "", 0), "", nil, "")
			s.ModemPath = "/Modem/1"
			queryCalled := false
			s.sendATCommandFn = func(_ context.Context, command string) (string, error) {
				switch command {
				case "AT+CGPS=1,1":
					return "", tc.startErr
				case "AT+CGPS?":
					queryCalled = true
					return tc.queryResp, tc.queryErr
				default:
					t.Fatalf("unexpected command %q", command)
					return "", nil
				}
			}
			err := s.startStandaloneReceiver(context.Background(), time.Time{})
			if !queryCalled {
				t.Fatal("post-start CGPS query was not issued")
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("startStandaloneReceiver() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

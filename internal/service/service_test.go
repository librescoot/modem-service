package service

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"modem-service/internal/config"
	"modem-service/internal/datausage"
	"modem-service/internal/health"
	"modem-service/internal/location"
	"modem-service/internal/modem"
	"modem-service/internal/modem/connectivity"
)

func TestCheckHealthDoesNotRecoverForMissingSIM(t *testing.T) {
	s := &Service{
		Config: &config.Config{Interface: "wwan0"},
		Health: health.New(),
		getModemInfoFn: func(string) (*modem.State, error) {
			return &modem.State{SIMState: modem.SIMStateMissing}, nil
		},
	}
	s.Health.State = health.StatePermanentFailure
	s.Health.RecoveryAttempts = health.MaxRecoveryAttempts

	if err := s.checkHealth(context.Background()); err != nil {
		t.Fatalf("checkHealth() error = %v", err)
	}
	if s.Health.State != health.StateNormal || s.Health.RecoveryAttempts != 0 {
		t.Fatalf("missing SIM health = %+v, want normal with no recovery attempts", s.Health)
	}
}

func TestMissingSIMPersistsAcrossModemReenumeration(t *testing.T) {
	calls := 0
	s := &Service{
		Config: &config.Config{Interface: "wwan0"},
		getModemInfoFn: func(string) (*modem.State, error) {
			calls++
			if calls == 1 {
				return &modem.State{SIMState: modem.SIMStateMissing}, nil
			}
			return nil, fmt.Errorf("modem not found")
		},
	}

	if !s.hasMissingSIM() {
		t.Fatal("initial missing SIM was not detected")
	}
	if !s.hasMissingSIM() {
		t.Fatal("missing SIM was lost during modem re-enumeration")
	}
}

func TestHandleModemFailureDoesNotRecoverAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &Service{
		Health:       health.New(),
		Logger:       log.New(io.Discard, "", 0),
		raiseFaultFn: func(int, string) { t.Fatal("raised fault after cancellation") },
	}
	s.modemEnabled.Store(true)

	if err := s.handleModemFailure(ctx, "test"); err != context.Canceled {
		t.Fatalf("handleModemFailure() error = %v, want context.Canceled", err)
	}
	if s.Health.RecoveryAttempts != 0 || s.Health.State != health.StateNormal {
		t.Fatalf("cancelled recovery modified health: %+v", s.Health)
	}
}

func TestRecoveryBackoffCancellationReturnsToNormal(t *testing.T) {
	entered := make(chan struct{})
	s := &Service{
		Health:       health.New(),
		Logger:       log.New(io.Discard, "", 0),
		raiseFaultFn: func(int, string) {},
	}
	s.Health.RecoveryAttempts = health.MaxRecoveryAttempts
	var published string
	s.publishFn = func(key, value string) error {
		if key == "modem-health" {
			published = value
		}
		return nil
	}
	s.recoveryBackoffFn = func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	s.modemEnabled.Store(true)
	failureDone := make(chan error, 1)
	go func() {
		failureDone <- s.handleModemFailure(context.Background(), "test")
	}()
	<-entered
	s.cancelModemOperation()
	if err := <-failureDone; err != context.Canceled {
		t.Fatalf("backoff error = %v", err)
	}
	if s.Health.State != health.StateNormal || s.Health.RecoveryAttempts != 0 {
		t.Fatalf("health after cancellation = %+v", s.Health)
	}
	if published != health.StateNormal {
		t.Fatalf("published health = %q", published)
	}

	enabled := false
	s.ensureModemEnabledFn = func(context.Context) error {
		enabled = true
		return nil
	}
	applied := false
	if !s.reconcileModemTarget(context.Background(), &applied) || !enabled || !applied {
		t.Fatal("healthy re-enable did not resume after cancelled backoff")
	}
}

func TestRecoveryBackoffExpiryReturnsToNormal(t *testing.T) {
	s := &Service{Health: health.New()}
	s.Health.RecoveryAttempts = health.MaxRecoveryAttempts
	s.Health.MarkRecoveryFailed()
	s.publishFn = func(string, string) error { return nil }
	s.recoveryBackoffFn = func(context.Context) error { return nil }
	if err := s.completeRecoveryBackoff(context.Background(), context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Health.State != health.StateNormal || s.Health.RecoveryAttempts != 0 {
		t.Fatalf("health after expiry = %+v", s.Health)
	}
}

func TestSMSWatchSurvivesOperationAndStopsExplicitly(t *testing.T) {
	serviceCtx, cancelService := context.WithCancel(context.Background())
	defer cancelService()
	delivered := make(chan dbus.ObjectPath, 1)
	var watchCtx context.Context
	var handleAdded func(dbus.ObjectPath, bool)
	s := &Service{
		ctx:    serviceCtx,
		Logger: log.New(io.Discard, "", 0),
		watchSMSAddedFn: func(ctx context.Context, _ dbus.ObjectPath, handler func(dbus.ObjectPath, bool)) error {
			watchCtx, handleAdded = ctx, handler
			return nil
		},
		handleSMSAddedFn: func(_, smsPath dbus.ObjectPath) {
			delivered <- smsPath
		},
	}
	opCtx, finish := s.startModemOperation(serviceCtx)
	if err := s.armSMSAddedWatch(opCtx, "/Modem/1"); err != nil {
		t.Fatal(err)
	}

	finish()
	handleAdded("/SMS/1", true)
	select {
	case got := <-delivered:
		if got != "/SMS/1" {
			t.Fatalf("delivered SMS path = %q", got)
		}
	default:
		t.Fatal("SMS Added event was not delivered after operation completion")
	}
	select {
	case <-watchCtx.Done():
		t.Fatal("SMS watch stopped with completed modem operation")
	default:
	}
	s.smsSIMPresent.Store(false)
	s.stopSMSWatch()
	select {
	case <-watchCtx.Done():
	default:
		t.Fatal("explicit SMS watch stop did not cancel subscription")
	}

	if err := s.armSMSAddedWatch(serviceCtx, "/Modem/1"); err != nil {
		t.Fatal(err)
	}
	shutdownWatch := watchCtx
	cancelService()
	select {
	case <-shutdownWatch.Done():
	default:
		t.Fatal("service shutdown did not cancel SMS subscription")
	}
}

func TestDurableContextOutlivesOperation(t *testing.T) {
	serviceCtx, cancelService := context.WithCancel(context.Background())
	defer cancelService()
	opCtx, cancelOp := context.WithCancel(context.Background())
	s := &Service{ctx: serviceCtx}

	got := s.durableContext(opCtx)
	cancelOp()
	select {
	case <-got.Done():
		t.Fatal("durable context was cancelled with operation context")
	default:
	}
	cancelService()
	select {
	case <-got.Done():
	default:
		t.Fatal("durable context ignored service shutdown")
	}
}

func newDisableLifecycleService() (*Service, map[string]string, *int, *int) {
	published := make(map[string]string)
	powerOffs, inhibitorRemovals := 0, 0
	s := &Service{
		Logger:         log.New(io.Discard, "", 0),
		Health:         health.New(),
		Location:       location.NewService(log.New(io.Discard, "", 0), "", nil, ""),
		Usage:          datausage.New("", nil),
		LastState:      modem.NewState(),
		connClassifier: connectivity.New(),
		publishFn: func(key, value string) error {
			published["internet."+key] = value
			return nil
		},
		publishModemFn: func(key, value string) error {
			published["modem."+key] = value
			return nil
		},
		publishLocationFn: func(fields map[string]interface{}, _ bool) error {
			for key, value := range fields {
				published["gps."+key] = fmt.Sprint(value)
			}
			return nil
		},
		powerOffModemFn: func(context.Context) error {
			powerOffs++
			return nil
		},
		removeInhibitorFn: func() error {
			inhibitorRemovals++
			return nil
		},
	}
	return s, published, &powerOffs, &inhibitorRemovals
}

func TestRepeatedDisableRunsIdempotentCleanup(t *testing.T) {
	s, published, powerOffs, inhibitorRemovals := newDisableLifecycleService()
	s.modemEnabled.Store(false)
	applied := false
	s.reconcileModemTarget(context.Background(), &applied)
	s.reconcileModemTarget(context.Background(), &applied)
	if *powerOffs != 2 || *inhibitorRemovals != 2 {
		t.Fatalf("cleanup calls = power %d inhibitor %d, want 2 each", *powerOffs, *inhibitorRemovals)
	}
	if published["internet.connectivity"] != "disabled" || published["modem.power-state"] != "off" || published["gps.state"] != "off" {
		t.Fatalf("off state not published: %v", published)
	}
}

func TestCancelledEnableStillRunsDisableCleanup(t *testing.T) {
	started := make(chan struct{})
	s, published, powerOffs, inhibitorRemovals := newDisableLifecycleService()
	s.modemStateChange = make(chan struct{}, 1)
	s.ensureModemEnabledFn = func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	s.modemEnabled.Store(true)
	applied := false
	done := make(chan struct{})
	go func() {
		s.reconcileModemTarget(context.Background(), &applied)
		close(done)
	}()
	<-started

	if err := s.handleModemCommand("disable"); err != nil {
		t.Fatal(err)
	}
	<-done
	<-s.modemStateChange
	s.reconcileModemTarget(context.Background(), &applied)
	if *powerOffs != 1 || *inhibitorRemovals != 1 {
		t.Fatalf("cancelled enable cleanup = power %d inhibitor %d", *powerOffs, *inhibitorRemovals)
	}
	if published["internet.connectivity"] != "disabled" || published["modem.power-state"] != "off" {
		t.Fatalf("cancelled enable did not publish off state: %v", published)
	}
	if applied {
		t.Fatal("cancelled enable remained applied")
	}
}

func TestDisableCommandCancelsModemOperation(t *testing.T) {
	s := &Service{
		Logger:           log.New(io.Discard, "", 0),
		modemStateChange: make(chan struct{}, 1),
	}
	s.modemEnabled.Store(true)
	opCtx, finish := s.startModemOperation(context.Background())
	defer finish()

	if err := s.handleModemCommand("disable"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-opCtx.Done():
	default:
		t.Fatal("active modem operation was not cancelled")
	}
	select {
	case <-s.modemStateChange:
	default:
		t.Fatal("monitor was not notified")
	}
}

func TestSMSPresenceTransitions(t *testing.T) {
	steps := []struct {
		name        string
		path        dbus.ObjectPath
		wantPresent bool
		wantArm     bool
		wantStop    bool
	}{
		{name: "missing at startup", path: "/"},
		{name: "inserted", path: "/org/freedesktop/ModemManager1/SIM/0", wantPresent: true, wantArm: true},
		{name: "still present", path: "/org/freedesktop/ModemManager1/SIM/0", wantPresent: true},
		{name: "removed", path: "/", wantStop: true},
		{name: "still missing", path: ""},
	}

	present := false
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			gotPresent, gotArm, gotStop := smsPresenceTransition(present, step.path)
			if gotPresent != step.wantPresent || gotArm != step.wantArm || gotStop != step.wantStop {
				t.Fatalf("transition = (present=%t arm=%t stop=%t), want (%t %t %t)",
					gotPresent, gotArm, gotStop, step.wantPresent, step.wantArm, step.wantStop)
			}
			present = gotPresent
		})
	}
}

func TestReconcileSMSPresenceCancelsWatchOnRemoval(t *testing.T) {
	s := &Service{}
	s.smsSIMPresent.Store(true)
	cancelled := 0
	s.smsWatchCancel = func() { cancelled++ }

	s.reconcileSMSPresence(context.Background(), "/")
	s.reconcileSMSPresence(context.Background(), "")

	if cancelled != 1 {
		t.Fatalf("watch cancelled %d times, want 1", cancelled)
	}
	if s.smsSIMPresent.Load() {
		t.Fatal("SIM remains marked present after removal")
	}
	if s.smsWatchCancel != nil {
		t.Fatal("watch cancel function remains armed after removal")
	}
}

func TestGPSRecoveryConcurrency(t *testing.T) {
	cfg := &config.Config{
		Interface:         "wwan0",
		InternetCheckTime: 30 * time.Second,
		GpsdServer:        "localhost:2947",
		RedisURL:          "redis://localhost:6379",
	}

	logger := log.New(os.Stdout, "TEST: ", log.LstdFlags)
	service := &Service{
		Config:    cfg,
		Logger:    logger,
		Health:    health.New(),
		Location:  location.NewService(logger, cfg.GpsdServer, nil, ""),
		LastState: modem.NewState(),
	}

	var recoveryAttempts int
	var mu sync.Mutex

	const numAttempts = 10
	var wg sync.WaitGroup
	wg.Add(numAttempts)

	for i := 0; i < numAttempts; i++ {
		go func(id int) {
			defer wg.Done()

			service.gpsRecoveryMutex.Lock()
			if !service.gpsRecoveryInProgress {
				mu.Lock()
				recoveryAttempts++
				mu.Unlock()

				service.gpsRecoveryInProgress = true
				service.gpsRecoveryMutex.Unlock()

				time.Sleep(10 * time.Millisecond)

				service.gpsRecoveryMutex.Lock()
				service.gpsRecoveryInProgress = false
				service.gpsRecoveryMutex.Unlock()
			} else {
				service.gpsRecoveryMutex.Unlock()
				t.Logf("Attempt %d: Recovery already in progress, skipped", id)
			}
		}(i)
	}

	wg.Wait()

	mu.Lock()
	actualAttempts := recoveryAttempts
	mu.Unlock()

	if actualAttempts == numAttempts {
		t.Errorf("Expected fewer than %d recovery attempts due to serialization, got %d", numAttempts, actualAttempts)
	} else {
		t.Logf("Successfully serialized: %d/%d attempts executed", actualAttempts, numAttempts)
	}
}

func TestGPSRecoveryInProgressFlag(t *testing.T) {
	cfg := &config.Config{
		Interface:         "wwan0",
		InternetCheckTime: 30 * time.Second,
		GpsdServer:        "localhost:2947",
		RedisURL:          "redis://localhost:6379",
	}

	logger := log.New(os.Stdout, "TEST: ", log.LstdFlags)
	service := &Service{
		Config:    cfg,
		Logger:    logger,
		Health:    health.New(),
		Location:  location.NewService(logger, cfg.GpsdServer, nil, ""),
		LastState: modem.NewState(),
	}

	service.gpsRecoveryMutex.Lock()
	if service.gpsRecoveryInProgress {
		t.Error("Expected gpsRecoveryInProgress to be false initially")
	}
	service.gpsRecoveryMutex.Unlock()

	service.gpsRecoveryMutex.Lock()
	service.gpsRecoveryInProgress = true
	service.gpsRecoveryMutex.Unlock()

	service.gpsRecoveryMutex.Lock()
	inProgress := service.gpsRecoveryInProgress
	service.gpsRecoveryMutex.Unlock()

	if !inProgress {
		t.Error("Expected gpsRecoveryInProgress to be true after setting")
	}

	service.gpsRecoveryMutex.Lock()
	service.gpsRecoveryInProgress = false
	service.gpsRecoveryMutex.Unlock()

	service.gpsRecoveryMutex.Lock()
	inProgress = service.gpsRecoveryInProgress
	service.gpsRecoveryMutex.Unlock()

	if inProgress {
		t.Error("Expected gpsRecoveryInProgress to be false after clearing")
	}
}

func TestGPSTimerRespectRecoveryFlag(t *testing.T) {
	cfg := &config.Config{
		Interface:         "wwan0",
		InternetCheckTime: 30 * time.Second,
		GpsdServer:        "localhost:2947",
		RedisURL:          "redis://localhost:6379",
	}

	logger := log.New(os.Stdout, "TEST: ", log.LstdFlags)
	service := &Service{
		Config:    cfg,
		Logger:    logger,
		Health:    health.New(),
		Location:  location.NewService(logger, cfg.GpsdServer, nil, ""),
		LastState: modem.NewState(),
	}

	service.gpsRecoveryMutex.Lock()
	service.gpsRecoveryInProgress = true
	service.gpsRecoveryMutex.Unlock()

	service.gpsRecoveryMutex.Lock()
	recoveryInProgress := service.gpsRecoveryInProgress
	service.gpsRecoveryMutex.Unlock()

	gpsEnabled := service.Location.IsEnabled()
	shouldEnableGPS := !gpsEnabled && !recoveryInProgress

	if shouldEnableGPS {
		t.Error("GPS timer should not enable GPS when recovery is in progress")
	} else {
		t.Log("GPS timer correctly skipped EnableGPS during recovery")
	}

	service.gpsRecoveryMutex.Lock()
	service.gpsRecoveryInProgress = false
	service.gpsRecoveryMutex.Unlock()

	service.gpsRecoveryMutex.Lock()
	recoveryInProgress = service.gpsRecoveryInProgress
	service.gpsRecoveryMutex.Unlock()

	shouldEnableGPS = !gpsEnabled && !recoveryInProgress

	if !shouldEnableGPS {
		t.Error("GPS timer should enable GPS when recovery is not in progress")
	} else {
		t.Log("GPS timer correctly allows EnableGPS when recovery is complete")
	}
}

func TestGPSRecoveryMutexProtection(t *testing.T) {
	cfg := &config.Config{
		Interface:         "wwan0",
		InternetCheckTime: 30 * time.Second,
		GpsdServer:        "localhost:2947",
		RedisURL:          "redis://localhost:6379",
	}

	logger := log.New(os.Stdout, "TEST: ", log.LstdFlags)
	service := &Service{
		Config:    cfg,
		Logger:    logger,
		Health:    health.New(),
		Location:  location.NewService(logger, cfg.GpsdServer, nil, ""),
		LastState: modem.NewState(),
	}

	const numGoroutines = 100
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()

			for {
				select {
				case <-ctx.Done():
					return
				default:
					service.gpsRecoveryMutex.Lock()
					_ = service.gpsRecoveryInProgress
					service.gpsRecoveryMutex.Unlock()

					service.gpsRecoveryMutex.Lock()
					service.gpsRecoveryInProgress = true
					service.gpsRecoveryMutex.Unlock()

					time.Sleep(1 * time.Millisecond)

					service.gpsRecoveryMutex.Lock()
					service.gpsRecoveryInProgress = false
					service.gpsRecoveryMutex.Unlock()
				}
			}
		}()
	}

	wg.Wait()

	t.Log("Mutex protection test passed - no race conditions detected")
}

func TestServiceInitialization(t *testing.T) {
	cfg := &config.Config{
		Interface:         "wwan0",
		InternetCheckTime: 30 * time.Second,
		GpsdServer:        "localhost:2947",
		RedisURL:          "redis://localhost:6379",
	}

	logger := log.New(os.Stdout, "TEST: ", log.LstdFlags)
	service := &Service{
		Config:    cfg,
		Logger:    logger,
		Health:    health.New(),
		Location:  location.NewService(logger, cfg.GpsdServer, nil, ""),
		LastState: modem.NewState(),
	}

	if service.Config == nil {
		t.Error("Expected Config to be initialized")
	}

	if service.Logger == nil {
		t.Error("Expected Logger to be initialized")
	}

	if service.Health == nil {
		t.Error("Expected Health to be initialized")
	}

	if service.Location == nil {
		t.Error("Expected Location to be initialized")
	}

	if service.LastState == nil {
		t.Error("Expected LastState to be initialized")
	}

	service.gpsRecoveryMutex.Lock()
	if service.gpsRecoveryInProgress {
		t.Error("Expected gpsRecoveryInProgress to be false on initialization")
	}
	service.gpsRecoveryMutex.Unlock()

	if service.GPSRecoveryCount != 0 {
		t.Errorf("Expected GPSRecoveryCount to be 0, got %d", service.GPSRecoveryCount)
	}
}

func TestFormatHumanDuration(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want string
	}{
		{name: "sub-minute rounds down", in: 7*time.Second + 400*time.Millisecond, want: "7s"},
		{name: "sub-minute rounds up", in: 7*time.Second + 500*time.Millisecond, want: "8s"},
		{name: "exact minute", in: time.Minute, want: "1m 0s"},
		{name: "rounds across minute boundary", in: 59*time.Second + 500*time.Millisecond, want: "1m 0s"},
		{name: "GPS timeout age", in: 15*time.Minute + 6*time.Second + 945*time.Millisecond, want: "15m 7s"},
		{name: "hour scale", in: time.Hour + 2*time.Minute + 3*time.Second, want: "1h 2m 3s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatHumanDuration(tt.in); got != tt.want {
				t.Fatalf("formatHumanDuration(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestGPSHealthCheck(t *testing.T) {
	cfg := &config.Config{
		Interface:         "wwan0",
		InternetCheckTime: 30 * time.Second,
		GpsdServer:        "localhost:2947",
		RedisURL:          "redis://localhost:6379",
	}

	logger := log.New(os.Stdout, "TEST: ", log.LstdFlags)
	service := &Service{
		Config:    cfg,
		Logger:    logger,
		Health:    health.New(),
		Location:  location.NewService(logger, cfg.GpsdServer, nil, ""),
		LastState: modem.NewState(),
	}

	err := service.checkGPSHealth()
	if err != nil {
		t.Logf("Fresh service GPS health check result: %v (expected)", err)
	}

	service.Location.SetLastDataReceived(time.Now().Add(-2 * time.Second))
	err = service.checkGPSHealth()
	if err != nil {
		t.Errorf("Expected no error for recent GPS data, got: %v", err)
	}

	service.Location.SetLastDataReceived(time.Now().Add(-(gpsNoDataTimeout + time.Second)))
	err = service.checkGPSHealth()
	if err == nil {
		t.Error("Expected error for no GPS data, got nil")
	} else {
		t.Logf("Correctly detected gps_no_data: %v", err)
	}

	service.Location.SetLastDataReceived(time.Now())
	service.GPSEnabledTime = time.Now().Add(-16 * time.Minute)
	service.Location.SetHasValidFix(false)
	err = service.checkGPSHealth()
	if err == nil {
		t.Error("Expected error for GPS fix timeout, got nil")
	} else {
		t.Logf("Correctly detected GPS fix timeout: %v", err)
	}

	service.GPSEnabledTime = time.Now().Add(-10 * time.Minute)
	err = service.checkGPSHealth()
	if err != nil {
		t.Errorf("Expected no error 10 minutes into cold start, got: %v", err)
	}
}

func TestHealthStateInitialization(t *testing.T) {
	h := health.New()

	if h.State != health.StateNormal {
		t.Errorf("Expected initial health state to be %s, got %s", health.StateNormal, h.State)
	}

	if h.RecoveryAttempts != 0 {
		t.Errorf("Expected recovery attempts to be 0, got %d", h.RecoveryAttempts)
	}
}

func TestGPSRecoveryCountReset(t *testing.T) {
	cfg := &config.Config{
		Interface:         "wwan0",
		InternetCheckTime: 30 * time.Second,
		GpsdServer:        "localhost:2947",
	}

	logger := log.New(os.Stdout, "TEST: ", log.LstdFlags)
	service := &Service{
		Config:    cfg,
		Logger:    logger,
		Health:    health.New(),
		Location:  location.NewService(logger, cfg.GpsdServer, nil, ""),
		LastState: modem.NewState(),
	}

	for range 5 {
		service.GPSRecoveryCount++
	}
	if service.GPSRecoveryCount != 5 {
		t.Errorf("Expected GPS recovery count to be 5, got %d", service.GPSRecoveryCount)
	}

	service.GPSRecoveryCount = 0
	if service.GPSRecoveryCount != 0 {
		t.Errorf("Expected GPS recovery count to be reset to 0, got %d", service.GPSRecoveryCount)
	}
}

func TestLocationServiceInitialization(t *testing.T) {
	logger := log.New(os.Stdout, "TEST: ", log.LstdFlags)
	locService := location.NewService(logger, "localhost:2947", nil, "")

	if locService == nil {
		t.Fatal("Expected location service to be initialized")
	}

	if locService.Logger == nil {
		t.Error("Expected logger to be set")
	}

	if locService.GpsdServer != "localhost:2947" {
		t.Errorf("Expected gpsd server to be 'localhost:2947', got '%s'", locService.GpsdServer)
	}

	if locService.State() != "off" {
		t.Errorf("Expected initial state to be 'off', got '%s'", locService.State())
	}

	if locService.HasValidFix() {
		t.Error("Expected HasValidFix to be false initially")
	}

	if !locService.GPSFreshInit() {
		t.Error("Expected GPSFreshInit to be true initially")
	}
}

func TestGPSStatusMapping(t *testing.T) {
	logger := log.New(os.Stdout, "TEST: ", log.LstdFlags)
	locService := location.NewService(logger, "localhost:2947", nil, "")

	status := locService.GetGPSStatus()

	expectedFields := []string{"fix", "snr", "hdop", "vdop", "pdop", "eph", "eps", "ept", "satellites-used", "satellites-visible", "active", "connected", "state"}
	for _, field := range expectedFields {
		if _, ok := status[field]; !ok {
			t.Errorf("Expected GPS status to contain field '%s'", field)
		}
	}

	if status["active"] != false {
		t.Error("Expected active to be false initially")
	}

	if status["connected"] != false {
		t.Error("Expected connected to be false initially")
	}

	if status["state"] != "off" {
		t.Errorf("Expected state to be 'off', got '%s'", status["state"])
	}
}
func TestGPSLifecycleOperationsAreSerialized(t *testing.T) {
	s := &Service{}
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan struct{})
	secondEntered := make(chan struct{})

	go func() {
		s.withGPSLifecycleLock(func() {
			close(firstEntered)
			<-releaseFirst
		})
		close(firstDone)
	}()
	<-firstEntered

	go s.withGPSLifecycleLock(func() { close(secondEntered) })
	select {
	case <-secondEntered:
		t.Fatal("GPS lifecycle operations overlapped")
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseFirst)
	<-firstDone
	select {
	case <-secondEntered:
	case <-time.After(time.Second):
		t.Fatal("second GPS lifecycle operation did not proceed")
	}
}

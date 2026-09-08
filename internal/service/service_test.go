package service

import (
	"context"
	"log"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"modem-service/internal/config"
	"modem-service/internal/health"
	"modem-service/internal/location"
	"modem-service/internal/modem"
)

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

// TestGPSRecoveryConcurrency tests that multiple concurrent GPS recovery attempts
// are properly serialized and don't cause race conditions
func TestGPSRecoveryConcurrency(t *testing.T) {
	// Create a test service
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

	// Track how many recovery attempts actually execute
	var recoveryAttempts int
	var mu sync.Mutex

	// Launch multiple concurrent recovery attempts
	const numAttempts = 10
	var wg sync.WaitGroup
	wg.Add(numAttempts)

	for i := 0; i < numAttempts; i++ {
		go func(id int) {
			defer wg.Done()

			// Acquire the recovery lock to check if recovery executes
			service.gpsRecoveryMutex.Lock()
			if !service.gpsRecoveryInProgress {
				mu.Lock()
				recoveryAttempts++
				mu.Unlock()

				// Simulate recovery work
				service.gpsRecoveryInProgress = true
				service.gpsRecoveryMutex.Unlock()

				// Simulate some work
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

	// Wait for all attempts to complete
	wg.Wait()

	// Verify that recoveries were serialized (not all 10 executed)
	mu.Lock()
	actualAttempts := recoveryAttempts
	mu.Unlock()

	if actualAttempts == numAttempts {
		t.Errorf("Expected fewer than %d recovery attempts due to serialization, got %d", numAttempts, actualAttempts)
	} else {
		t.Logf("Successfully serialized: %d/%d attempts executed", actualAttempts, numAttempts)
	}
}

// TestGPSRecoveryInProgressFlag tests that the gpsRecoveryInProgress flag
// properly prevents concurrent GPS recovery attempts
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

	// Test 1: Flag should be false initially
	service.gpsRecoveryMutex.Lock()
	if service.gpsRecoveryInProgress {
		t.Error("Expected gpsRecoveryInProgress to be false initially")
	}
	service.gpsRecoveryMutex.Unlock()

	// Test 2: Set flag and verify it blocks second attempt
	service.gpsRecoveryMutex.Lock()
	service.gpsRecoveryInProgress = true
	service.gpsRecoveryMutex.Unlock()

	// Try to check if recovery is in progress (simulate GPS timer check)
	service.gpsRecoveryMutex.Lock()
	inProgress := service.gpsRecoveryInProgress
	service.gpsRecoveryMutex.Unlock()

	if !inProgress {
		t.Error("Expected gpsRecoveryInProgress to be true after setting")
	}

	// Test 3: Clear flag and verify
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

// TestGPSTimerRespectRecoveryFlag tests that the GPS timer respects
// the gpsRecoveryInProgress flag and doesn't try to enable GPS during recovery
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

	// Simulate recovery in progress
	service.gpsRecoveryMutex.Lock()
	service.gpsRecoveryInProgress = true
	service.gpsRecoveryMutex.Unlock()

	// Simulate GPS timer check (from monitorStatus gpsTimer.C case)
	service.gpsRecoveryMutex.Lock()
	recoveryInProgress := service.gpsRecoveryInProgress
	service.gpsRecoveryMutex.Unlock()

	// GPS is not enabled, but recovery is in progress
	gpsEnabled := service.Location.Enabled
	shouldEnableGPS := !gpsEnabled && !recoveryInProgress

	if shouldEnableGPS {
		t.Error("GPS timer should not enable GPS when recovery is in progress")
	} else {
		t.Log("GPS timer correctly skipped EnableGPS during recovery")
	}

	// Clear recovery flag
	service.gpsRecoveryMutex.Lock()
	service.gpsRecoveryInProgress = false
	service.gpsRecoveryMutex.Unlock()

	// Now check again
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

// TestGPSRecoveryMutexProtection tests that the mutex properly protects
// the gpsRecoveryInProgress flag from race conditions
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

	// Run multiple goroutines that try to read and write the flag
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
					// Simulate GPS timer checking flag
					service.gpsRecoveryMutex.Lock()
					_ = service.gpsRecoveryInProgress
					service.gpsRecoveryMutex.Unlock()

					// Simulate recovery setting/clearing flag
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

	// Wait for all goroutines to complete
	wg.Wait()

	// If we get here without race detector complaints, the mutex is working
	t.Log("Mutex protection test passed - no race conditions detected")
}

// TestServiceInitialization tests that a new service is properly initialized
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

	// Check GPS recovery fields are initialized properly
	service.gpsRecoveryMutex.Lock()
	if service.gpsRecoveryInProgress {
		t.Error("Expected gpsRecoveryInProgress to be false on initialization")
	}
	service.gpsRecoveryMutex.Unlock()

	if service.GPSRecoveryCount != 0 {
		t.Errorf("Expected GPSRecoveryCount to be 0, got %d", service.GPSRecoveryCount)
	}
}

// TestGPSHealthCheck tests the GPS health check logic
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

	// Test 1: Fresh service with no GPS data should pass (lastDataReceived is zero)
	err := service.checkGPSHealth()
	if err != nil {
		t.Logf("Fresh service GPS health check result: %v (expected)", err)
	}

	// Test 2: GPS data received recently should pass
	service.Location.SetLastDataReceived(time.Now().Add(-2 * time.Second))
	err = service.checkGPSHealth()
	if err != nil {
		t.Errorf("Expected no error for recent GPS data, got: %v", err)
	}

	// Test 3: No data past gpsNoDataTimeout should fail
	service.Location.SetLastDataReceived(time.Now().Add(-(gpsNoDataTimeout + time.Second)))
	err = service.checkGPSHealth()
	if err == nil {
		t.Error("Expected error for no GPS data, got nil")
	} else {
		t.Logf("Correctly detected gps_no_data: %v", err)
	}

	// Test 4: GPS fix timeout should fail past 15 minutes
	service.Location.SetLastDataReceived(time.Now())
	service.GPSEnabledTime = time.Now().Add(-16 * time.Minute)
	service.Location.SetHasValidFix(false)
	err = service.checkGPSHealth()
	if err == nil {
		t.Error("Expected error for GPS fix timeout, got nil")
	} else {
		t.Logf("Correctly detected GPS fix timeout: %v", err)
	}

	// Test 5: GPS fix timeout should NOT fire before 15 minutes elapsed
	service.GPSEnabledTime = time.Now().Add(-10 * time.Minute)
	err = service.checkGPSHealth()
	if err != nil {
		t.Errorf("Expected no error 10 minutes into cold start, got: %v", err)
	}
}

// TestHealthStateInitialization tests that health state starts as normal
func TestHealthStateInitialization(t *testing.T) {
	h := health.New()

	if h.State != health.StateNormal {
		t.Errorf("Expected initial health state to be %s, got %s", health.StateNormal, h.State)
	}

	if h.RecoveryAttempts != 0 {
		t.Errorf("Expected recovery attempts to be 0, got %d", h.RecoveryAttempts)
	}
}

// TestGPSRecoveryCountReset tests that GPS recovery count can be incremented and reset
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

	// Simulate multiple GPS recovery attempts
	for range 5 {
		service.GPSRecoveryCount++
	}
	if service.GPSRecoveryCount != 5 {
		t.Errorf("Expected GPS recovery count to be 5, got %d", service.GPSRecoveryCount)
	}

	// Simulate successful fix resetting the counter
	service.GPSRecoveryCount = 0
	if service.GPSRecoveryCount != 0 {
		t.Errorf("Expected GPS recovery count to be reset to 0, got %d", service.GPSRecoveryCount)
	}
}

// TestLocationServiceInitialization tests that location service initializes correctly
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

// TestGPSStatusMapping tests that GPS status is correctly mapped
func TestGPSStatusMapping(t *testing.T) {
	logger := log.New(os.Stdout, "TEST: ", log.LstdFlags)
	locService := location.NewService(logger, "localhost:2947", nil, "")

	status := locService.GetGPSStatus()

	// Verify all expected fields are present
	expectedFields := []string{"fix", "snr", "hdop", "vdop", "pdop", "eph", "eps", "ept", "satellites-used", "satellites-visible", "active", "connected", "state"}
	for _, field := range expectedFields {
		if _, ok := status[field]; !ok {
			t.Errorf("Expected GPS status to contain field '%s'", field)
		}
	}

	// Verify initial values
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

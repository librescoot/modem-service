package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/godbus/dbus/v5"

	"modem-service/internal/apn"
	"modem-service/internal/cell"
	"modem-service/internal/config"
	"modem-service/internal/datausage"
	"modem-service/internal/health"
	"modem-service/internal/location"
	"modem-service/internal/mm"
	"modem-service/internal/modem"
	"modem-service/internal/modem/connectivity"
	"modem-service/internal/modem/link"
	redisClient "modem-service/internal/redis"
	"modem-service/internal/sim"
	"modem-service/internal/sms"
	"modem-service/internal/usb"
)

// nmWWANConnection is the NetworkManager connection name for the cellular
// data bearer. Matches what mdb-netconfig provisions.
const nmWWANConnection = "wwan"

// Vehicle states that should trigger modem enable
var modemOnlineStates = map[string]bool{
	"parked":         true,
	"ready-to-drive": true,
}

// clockSyncInterval is how often we re-feed the system clock from rollover-corrected
// GPS time while the scooter is offline. When connectivity is available, chrony's
// NTP pool takes over and we suppress manual settime samples to avoid competing
// with the higher-precision source.
const clockSyncInterval = 60 * time.Second

// Remedy cooldowns. A persistent fault must not loop: each remedy may fire at
// most once per window. The modem-reset window is deliberately long, since a
// reset drops the data session and the GPS fix along with it.
// remedyConfirmations is how many consecutive assessments must report the same
// failing layer before any action is taken.
const remedyConfirmations = 2

// Remedy cooldowns. A persistent fault must not loop: each remedy may fire at
// most once per window. The modem-reset window is deliberately long, since a
// reset drops the data session and the GPS fix along with it.
const (
	reattachCooldown     = 5 * time.Minute
	bearerBounceCooldown = 5 * time.Minute
	modemResetCooldown   = 30 * time.Minute
)

type Service struct {
	Config                *config.Config
	Redis                 *redisClient.Client
	Logger                *log.Logger
	Health                *health.Health
	Location              *location.Service
	Modem                 *modem.Manager
	MMClient              *mm.Client
	Sim                   *sim.Manager
	simPin                atomic.Value // string; PIN configured via cellular.sim-pin setting
	Apn                   *apn.Manager
	apnAPN                atomic.Value // string; cellular.apn
	apnUsername           atomic.Value // string; cellular.username
	apnPassword           atomic.Value // string; cellular.password
	apnAuth               atomic.Value // string; cellular.auth ("none"|"pap"|"chap")
	SMS                   *sms.Manager
	smsWatchMu            sync.Mutex         // guards smsWatchCancel; startSMSWatch runs from the Run, monitor, and watchdog goroutines
	smsWatchCancel        context.CancelFunc // cancels the active inbound-SMS watch; re-armed on modem recovery
	smsSIMPresent         atomic.Bool
	unreadSMS             atomic.Int64 // inbound messages since start; published as sms.unread-count
	ownMSISDN             atomic.Value // string; own phone number for the voice-call keepalive, resolved via AT+CNUM
	lastCSActivity        atomic.Int64 // UnixNano of last confirmed CS event (any SMS sent/received)
	LastState             *modem.State
	WaitingForGPSLogged   bool       // Tracks if we've already logged the waiting for GPS message
	GPSEnabledTime        time.Time  // When GPS was first enabled
	GPSRecoveryCount      int        // Number of GPS recovery attempts
	LastGPSQualityLog     time.Time  // Last time GPS quality was logged
	gpsRecoveryMutex      sync.Mutex // Prevents concurrent GPS recovery/configuration attempts
	gpsRecoveryInProgress bool       // Tracks if GPS recovery is currently running
	gpsRecoveryUntil      time.Time  // Protected by gpsRecoveryMutex; monitor skips EnableGPS until this passes
	// Layered connectivity assessment. link answers "is the local stack
	// healthy" from ModemManager and sysfs alone, and is the only thing
	// permitted to trigger a modem action. prober answers "did anything
	// answer", which affects only what we publish: a fleet whose APN routes
	// through a restrictive tunnel is unreachable by design and permanently
	// healthy, and must never be power-cycled for it.
	link           *link.Assessor
	prober         *health.Prober
	wantATCheck    bool
	remedyCooldown map[link.Remedy]time.Time
	probeInterval  time.Duration
	nextProbeAt    time.Time
	lastProbe      health.Result
	lastAssessment link.Assessment

	// Change-gating for the additive diagnostic fields.
	lastReachability string
	lastLinkLayer    string

	// Cellular byte accounting. The counter owns its own persistence; the
	// service only decides when to publish and when to ask it to write.
	Usage        *datausage.Counter
	lastPubUsage datausage.Totals
	havePubUsage bool

	// Debounce: how many consecutive assessments have reported this same
	// failing layer. Layers 0-6 act on a single observation otherwise, and a
	// snapshot taken mid-bringup would bounce a connection that was already
	// coming up.
	pendingLayer  link.Layer
	pendingRepeat int

	// Injection seams for tests.
	applyRemedyFn  func(link.Remedy)
	publishFn      func(field, value string) error
	publishUsageFn func(map[string]interface{}) error
	now            func() time.Time

	lastClockSync time.Time // Last time syncClockFromGPS successfully fed chrony

	// Settings (from Redis) — atomic so the Redis watcher goroutine can
	// update them without racing the monitor goroutine that reads them.
	gpsEnabled          atomic.Bool
	cellLocationEnabled atomic.Bool
	lastCellTower       *cell.CellTower
	lastCellLoc         *cell.CellLocation

	// Modem enable/disable target state. Atomic because it's read from the
	// monitor goroutine and written from the Redis command/vehicle-state
	// watcher goroutines.
	modemEnabled atomic.Bool

	// Service-level context, captured in Run() so handlers spawned from
	// Redis watchers (e.g. disableModem goroutine) can respect shutdown.
	ctx context.Context

	// Connectivity classifier derives online/searching/offline/no-sim from
	// raw modem state with hysteresis to avoid thrashing on coverage flickers.
	connClassifier *connectivity.Classifier
	lastPubConn    connectivity.State // last value published to Redis

	// TTFF measurement. ttffStart is the moment we went from "no fix" to
	// "actively searching". Mode is read at fix time rather than wait-start,
	// because startup paths sometimes don't know the real modem mode until
	// ProbeGPSMode has run — capturing at wait-start gave misleading labels.
	ttffStart time.Time

	// lastPubGPSMode is the last value of gps.mode published to Redis.
	// Guarded by modePubMu because requestGPSModeForConnectivity can spawn
	// overlapping goroutines on rapid connectivity changes.
	modePubMu      sync.Mutex
	lastPubGPSMode location.GPSMode

	// monitorDone is closed by monitorStatus when it returns, so Run() can
	// wait for it before closing MMClient/Modem/Redis. Prevents the "D-Bus
	// call fails mid-shutdown" noise where the monitor goroutine is in the
	// middle of an AT command when its transport gets closed out from under
	// it.
	monitorDone chan struct{}
}

func New(cfg *config.Config, logger *log.Logger, version string) (*Service, error) {
	redis, err := redisClient.New(cfg.RedisURL, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create Redis client: %v", err)
	}

	// Create ModemManager D-Bus client
	mmClient, err := mm.NewClient(cfg.Debug, logger.Printf)
	if err != nil {
		return nil, fmt.Errorf("failed to create ModemManager client: %v", err)
	}

	// Create modem manager, sharing the same D-Bus client.
	modemMgr, err := modem.NewManager(mmClient, logger)
	if err != nil {
		mmClient.Close()
		return nil, fmt.Errorf("failed to create modem manager: %v", err)
	}

	service := &Service{
		Config:              cfg,
		Redis:               redis,
		Logger:              logger,
		Health:              health.New(),
		Modem:               modemMgr,
		MMClient:            mmClient,
		Sim:                 sim.New(mmClient, logger),
		Apn:                 apn.New(mmClient, apn.NewNMCli(), nmWWANConnection, logger),
		Location:            location.NewService(logger, cfg.GpsdServer, mmClient, cfg.SuplServer),
		Usage:               datausage.New(cfg.DataUsageFile, logger),
		LastState:           modem.NewState(),
		WaitingForGPSLogged: false,
		connClassifier:      connectivity.New(),
		monitorDone:         make(chan struct{}),

		link: link.New(),
		prober: health.NewProberWithVerification(
			cfg.Interface,
			cfg.ConnectivityTargets(),
			cfg.ConnectivityVerificationName,
			cfg.ConnectivityVerificationValue,
		),
		remedyCooldown: map[link.Remedy]time.Time{},
		probeInterval:  cfg.InternetCheckTime,
	}
	// Let the location service re-resolve the modem D-Bus path when MM
	// rebinds the modem (e.g. after mmcli --reset or AT+CFUN=0/1).
	// The SMS manager delivers inbound messages through the service so a
	// failed Redis write keeps the message in modem storage for retry.
	service.SMS = sms.New(mmClient, logger, service.deliverIncomingSMS)
	service.Location.ResolveModemPath = modemMgr.FindModem
	service.gpsEnabled.Store(true)           // default: GPS on
	service.cellLocationEnabled.Store(false) // default: cell location off
	service.modemEnabled.Store(true)         // default: modem on until pm-service says otherwise
	service.simPin.Store("")                 // default: no PIN configured
	service.apnAPN.Store("")
	service.apnUsername.Store("")
	service.apnPassword.Store("")
	service.apnAuth.Store("")

	cell.SetVersion(version)
	service.Logger.Printf("modem-service %s", version)

	return service, nil
}

func (s *Service) Run(ctx context.Context) error {
	// Capture the service context so handlers spawned from Redis watchers
	// (e.g. the disableModem goroutine) can respect shutdown.
	s.ctx = ctx

	if err := s.Redis.Ping(); err != nil {
		return fmt.Errorf("redis connection failed: %v", err)
	}

	// Start listening for modem enable/disable commands from pm-service
	if err := s.Redis.StartModemCommandHandler(s.handleModemCommand); err != nil {
		s.Logger.Printf("Failed to start modem command handler: %v", err)
	}

	// Start listening for outbound SMS requests
	if err := s.Redis.StartSMSCommandHandler(s.handleSMSCommand); err != nil {
		s.Logger.Printf("Failed to start SMS command handler: %v", err)
	}
	// Reset SMS operational state so a stale "sending" or "error" from a previous
	// crash doesn't persist in Redis indefinitely.
	s.Redis.PublishSMSState("state", "idle")

	// Start watching vehicle state to auto-enable modem
	if err := s.Redis.StartVehicleStateWatcher(s.handleVehicleState); err != nil {
		s.Logger.Printf("Failed to start vehicle state watcher: %v", err)
	}

	// Watch location settings
	s.Redis.StartSettingsWatcher("modem.gps", func(value string) error {
		enabled := value != "false"
		s.gpsEnabled.Store(enabled)
		s.Logger.Printf("GPS %s", map[bool]string{true: "enabled", false: "disabled"}[enabled])
		return nil
	})
	s.Redis.StartSettingsWatcher("modem.cell-location", func(value string) error {
		enabled := value == "true"
		s.cellLocationEnabled.Store(enabled)
		s.Logger.Printf("Cell location %s", map[bool]string{true: "enabled", false: "disabled"}[enabled])
		return nil
	})
	// SIM PIN setting. The value is sensitive — never logged. The next monitor
	// tick reads simPin via Load() and feeds it to sim.Manager.Reconcile.
	s.Redis.StartSettingsWatcher("cellular.sim-pin", func(value string) error {
		s.simPin.Store(value)
		if value == "" {
			s.Logger.Printf("SIM PIN cleared")
		} else {
			s.Logger.Printf("SIM PIN configured")
		}
		return nil
	})
	// APN settings. Values are stored and consumed by apn.Manager on the
	// next reconcile cycle. cellular.password is never logged.
	s.Redis.StartSettingsWatcher("cellular.apn", func(value string) error {
		s.apnAPN.Store(value)
		s.Logger.Printf("APN set: %q", value)
		return nil
	})
	s.Redis.StartSettingsWatcher("cellular.username", func(value string) error {
		s.apnUsername.Store(value)
		s.Logger.Printf("APN username set: %q", value)
		return nil
	})
	s.Redis.StartSettingsWatcher("cellular.password", func(value string) error {
		s.apnPassword.Store(value)
		if value == "" {
			s.Logger.Printf("APN password cleared")
		} else {
			s.Logger.Printf("APN password configured")
		}
		return nil
	})
	s.Redis.StartSettingsWatcher("cellular.auth", func(value string) error {
		s.apnAuth.Store(value)
		s.Logger.Printf("APN auth set: %q", value)
		return nil
	})
	s.Redis.StartSettingsWatching()

	// Try to enable the modem if it's not present
	if err := s.ensureModemEnabled(ctx); err != nil {
		s.Logger.Printf("SEVERE ERROR: Failed to ensure modem is enabled: %v", err)

		// If the modem interface is still not present, we cannot continue
		if !modem.IsInterfacePresent(s.Config.Interface) && !s.Modem.IsModemPresent() {
			s.Logger.Printf("Cannot continue without modem interface or D-Bus presence")
			return fmt.Errorf("modem not available: %v", err)
		}
	}

	// Hold a power inhibitor while the modem is up so pm-service won't suspend
	// the MDB until the modem has been told to shut down and confirmed off.
	if err := s.Redis.AddModemInhibitor(); err != nil {
		s.Logger.Printf("Failed to register modem power inhibitor: %v", err)
	}

	// Periodically check that the SGs CS registration at the MSC/VLR is still
	// alive and force a fresh combined attach if it has expired (LAC=0xFFFE).
	// Opt-in: the keepalive targets operators with a short CS implicit-detach
	// timer (seen on O2/Lebara DE) and briefly drops to 2G for each self-call,
	// so it must not run fleet-wide by default.
	if s.Config.SMSKeepalive {
		s.startSMSRegistrationWatchdog(ctx)
	} else {
		s.Logger.Printf("sms: SGs keepalive disabled (enable with -sms-keepalive if inbound SMS stops after ~15 min idle)")
	}

	s.Logger.Printf("Starting modem service on interface %s", s.Config.Interface)
	go s.monitorStatus(ctx)

	<-ctx.Done()

	// Give the monitor goroutine a chance to exit its current tick before
	// we tear down MMClient/Modem/Redis. Without this, an AT command or
	// D-Bus call in-flight will error out when its transport is closed,
	// producing noise in the journal. Bounded so shutdown stays snappy.
	select {
	case <-s.monitorDone:
	case <-time.After(10 * time.Second):
		s.Logger.Printf("Monitor goroutine did not exit within 10s; proceeding with shutdown")
	}

	// Stop the inbound-SMS watch. It also stops when ctx is cancelled (its
	// context is derived from ctx), but cancel explicitly for a tidy shutdown.
	s.smsSIMPresent.Store(false)
	s.stopSMSWatch()

	// Graceful shutdown: keep last lat/lng in Redis as a useful fallback
	// for consumers, but clear the fix indicators so nobody treats the
	// stale coords as a current position.
	s.Location.Close()
	s.Redis.PublishLocationState(map[string]interface{}{
		"state":              "off",
		"fix":                "none",
		"active":             false,
		"connected":          false,
		"snr":                float64(0),
		"hdop":               float64(0),
		"vdop":               float64(0),
		"pdop":               float64(0),
		"eph":                float64(0),
		"satellites-used":    int32(0),
		"satellites-visible": int32(0),
	}, false)

	// Persist the byte totals. The monitor goroutine has stopped by now, so
	// this writes everything it counted.
	if err := s.Usage.Flush(); err != nil {
		s.Logger.Printf("Failed to persist data usage: %v", err)
	}

	// Release service-owned resources. Modem.Close does not close the
	// shared mm.Client — service owns it and closes it below.
	if err := s.Modem.Close(); err != nil {
		s.Logger.Printf("Error closing modem manager: %v", err)
	}
	if err := s.MMClient.Close(); err != nil {
		s.Logger.Printf("Error closing ModemManager D-Bus client: %v", err)
	}
	// Drop the power inhibitor so a restart doesn't leave a stale entry in
	// power:inhibits blocking suspend.
	if err := s.Redis.RemoveModemInhibitor(); err != nil {
		s.Logger.Printf("Error clearing modem power inhibitor: %v", err)
	}
	if err := s.Redis.Close(); err != nil {
		s.Logger.Printf("Error closing Redis client: %v", err)
	}

	return nil
}

// handleModemCommand handles enable/disable commands from pm-service
func (s *Service) handleModemCommand(command string) error {
	switch command {
	case "enable":
		s.Logger.Printf("Received modem enable command")
		s.modemEnabled.Store(true)
		// Re-arm the power inhibitor for the next suspend cycle. Idempotent.
		if err := s.Redis.AddModemInhibitor(); err != nil {
			s.Logger.Printf("Failed to register modem power inhibitor: %v", err)
		}
		// Modem will be enabled by ensureModemEnabled or monitor loop
	case "disable":
		s.Logger.Printf("Received modem disable command")
		s.modemEnabled.Store(false)
		// Disable the modem
		go s.disableModem(s.ctx)
	default:
		s.Logger.Printf("Unknown modem command: %s", command)
	}
	return nil
}

// handleVehicleState handles vehicle state changes to auto-enable modem
func (s *Service) handleVehicleState(state string) error {
	if modemOnlineStates[state] {
		if s.modemEnabled.CompareAndSwap(false, true) {
			s.Logger.Printf("Vehicle state '%s' - enabling modem", state)
			// Re-arm the power inhibitor for the next suspend cycle. Idempotent.
			if err := s.Redis.AddModemInhibitor(); err != nil {
				s.Logger.Printf("Failed to register modem power inhibitor: %v", err)
			}
		}
	}
	return nil
}

// disableModem turns off the modem and publishes the off state
func (s *Service) disableModem(ctx context.Context) {
	s.Logger.Printf("Disabling modem...")

	s.reconcileSMSPresence(ctx, "")

	// Close GPS first. Preserve last lat/lng but clear the fix indicators
	// so consumers don't treat stale coords as a current position.
	s.withGPSLifecycleLock(s.Location.Close)
	s.Redis.PublishLocationState(map[string]interface{}{
		"state":              "off",
		"fix":                "none",
		"active":             false,
		"connected":          false,
		"snr":                float64(0),
		"hdop":               float64(0),
		"vdop":               float64(0),
		"pdop":               float64(0),
		"eph":                float64(0),
		"satellites-used":    int32(0),
		"satellites-visible": int32(0),
	}, false)

	// Publish off states
	s.Redis.PublishInternetState("status", "disconnected")
	s.Redis.PublishInternetState("modem-state", "off")
	s.Redis.PublishModemState("power-state", "off")

	// The monitor loop skips status checks while disabled, so the connectivity
	// classifier won't run to observe the power-off. Publish "disabled"
	// explicitly and force the classifier so it reports correctly on resume.
	s.Redis.PublishInternetState("connectivity", string(connectivity.Disabled))
	s.connClassifier.Force(connectivity.Disabled)
	s.lastPubConn = connectivity.Disabled

	// Keep the in-memory cache in sync with what we just wrote to Redis.
	// publishModemState() only writes a field when it differs from LastState,
	// so if we leave LastState holding the pre-disable values (e.g. "connected")
	// the monitor loop will treat the post-resume reconnection as "no change"
	// and never re-publish "connected"/the real modem-state — leaving the UI
	// stuck on the disconnected icon after the modem comes back. Syncing here
	// ensures the recovery transition is detected and re-published.
	s.LastState.Status = "disconnected"
	s.LastState.LastRawModemStatus = "off"
	s.LastState.PowerState = "off"

	// Clear the clock-sync bookkeeping so the first GPS fix after resume
	// bootstraps the system clock again. While suspended the system clock can
	// drift (no NTP, RTC may be imprecise), and without this reset the
	// IsZero() bootstrap branch in the monitor loop wouldn't re-fire — leaving
	// an online device to wait for chrony/NTP and an offline device to wait up
	// to a full clockSyncInterval before correcting.
	s.lastClockSync = time.Time{}

	if err := s.Modem.PowerOffModem(ctx); err != nil {
		s.Logger.Printf("Failed to disable modem via GPIO: %v", err)
	}

	// The modem is off, so the totals are final for this power cycle. This is
	// the intended write point: pm-service disables the modem before every
	// suspend, hibernate and poweroff, and it happens while we still hold the
	// inhibitor, so the write lands before the system goes down.
	if err := s.Usage.Flush(); err != nil {
		s.Logger.Printf("Failed to persist data usage: %v", err)
	}

	// Modem is off: drop the power inhibitor so pm-service can proceed to
	// suspend. Released here (after PowerOffModem) so suspend can't race the
	// modem still being powered.
	if err := s.Redis.RemoveModemInhibitor(); err != nil {
		s.Logger.Printf("Failed to clear modem power inhibitor: %v", err)
	}

	s.Logger.Printf("Modem disabled")
}

// handleSMSCommand processes one outbound SMS request from the scooter:sms
// queue. The payload is JSON: {"id":"optional-token","to":"+49...","text":"..."}.
// Every parsed request gets a terminal outcome on the sms:sent stream/channel
// (correlated by the caller's id token); send progress is also reflected in
// the sms.state field (sending → idle on success, error on failure).
func (s *Service) handleSMSCommand(payload string) error {
	var req sms.SendRequest
	if err := json.Unmarshal([]byte(payload), &req); err != nil {
		// Nothing to correlate a stream entry with — flag it on the hash only.
		s.Logger.Printf("sms: invalid command payload: %v", err)
		s.Redis.PublishSMSState("state", "error")
		return fmt.Errorf("invalid sms command: %w", err)
	}
	if req.To == "" {
		s.Logger.Printf("sms: command missing recipient")
		s.recordSendResult(req, "missing recipient")
		return fmt.Errorf("sms command missing recipient")
	}

	modemPath, err := s.Modem.FindModem()
	if err != nil {
		s.Logger.Printf("sms: cannot send, no modem: %v", err)
		s.recordSendResult(req, fmt.Sprintf("no modem available: %v", err))
		return fmt.Errorf("no modem available: %w", err)
	}

	s.Redis.PublishSMSState("state", "sending")
	outcome, sendErr := s.SMS.Send(modemPath, req)
	if outcome != sms.OutcomeOK {
		s.Logger.Printf("sms: send failed (%s): %v", outcome, sendErr)
		s.recordSendResult(req, sendErr.Error())
		return sendErr
	}

	// An MO SMS goes out over SGs, so it resets the operator's CS idle timer
	// just like an inbound one; without this the watchdog would fire a
	// redundant keepalive after a send.
	s.touchCSActivity()

	s.recordSendResult(req, "")
	s.Logger.Printf("sms: sent to %s", req.To)
	return nil
}

// recordSendResult publishes the terminal outcome of one send request to the
// sms:sent stream/channel and updates the sms hash. An empty errStr means the
// send succeeded.
func (s *Service) recordSendResult(req sms.SendRequest, errStr string) {
	res := redisClient.SMSSendResult{
		RequestID: req.ID,
		To:        req.To,
		Text:      req.Text,
		Outcome:   "sent",
		Error:     errStr,
		Timestamp: time.Now().Format(time.RFC3339),
	}
	if errStr != "" {
		res.Outcome = "error"
	}
	if err := s.Redis.PublishSMSSendResult(res); err != nil {
		s.Logger.Printf("sms: failed to record send result: %v", err)
	}
}

func smsPresenceTransition(previous bool, simPath dbus.ObjectPath) (present, arm, stop bool) {
	present = simPath != "" && simPath != "/"
	return present, present && !previous, !present && previous
}

func (s *Service) reconcileSMSPresence(ctx context.Context, simPath dbus.ObjectPath) {
	present := simPath != "" && simPath != "/"
	previous := s.smsSIMPresent.Swap(present)
	_, arm, stop := smsPresenceTransition(previous, simPath)
	if stop {
		s.stopSMSWatch()
	} else if arm {
		s.startSMSWatch(ctx)
	}
}

func (s *Service) stopSMSWatch() {
	s.smsWatchMu.Lock()
	defer s.smsWatchMu.Unlock()
	if s.smsSIMPresent.Load() {
		return
	}
	if s.smsWatchCancel != nil {
		s.smsWatchCancel()
		s.smsWatchCancel = nil
	}
}

// startSMSWatch replaces the active watcher and drains stored messages.
func (s *Service) startSMSWatch(ctx context.Context) {
	s.smsWatchMu.Lock()
	defer s.smsWatchMu.Unlock()

	if s.smsWatchCancel != nil {
		s.smsWatchCancel()
		s.smsWatchCancel = nil
	}
	if !s.smsSIMPresent.Load() {
		return
	}

	// Modem just came up (boot or post-resume recovery): it performed a fresh
	// Combined Attach which re-establishes SGs, so reset the idle clock. Without
	// this the watchdog would see stale idle time and fire a redundant keepalive.
	s.touchCSActivity()

	modemPath, err := s.Modem.FindModem()
	if err != nil {
		s.Logger.Printf("sms: no modem yet, deferring SMS watch: %v", err)
		return
	}

	// Resolve own MSISDN once so the voice-call keepalive knows what number to dial.
	if s.ownNumber() == "" {
		if msisdn := s.queryOwnMSISDN(modemPath); msisdn != "" {
			s.ownMSISDN.Store(msisdn)
			s.Logger.Printf("sms: own MSISDN: %s", msisdn)
		}
	}

	// Arm the watch BEFORE draining: a message that arrives during setup then
	// still triggers the signal, and the drain below catches whatever is
	// already stored. Draining first would leave a race window where an inbound
	// SMS lands after the drain but before the watch and goes unnoticed until
	// the next message or the periodic poll.
	watchCtx, cancel := context.WithCancel(ctx)
	if err := s.MMClient.WatchSMSAdded(watchCtx, modemPath, func(smsPath dbus.ObjectPath, received bool) {
		s.Logger.Printf("sms: Added signal path=%s received=%v", smsPath, received)
		if !received {
			return // an outbound object we created; ignore
		}
		// Process the signalled object directly. Some modems deliver a received
		// message as a transient object that never lands in storage, so a
		// re-list (drainSMS) would miss it.
		s.SMS.HandleAdded(modemPath, smsPath)
	}); err != nil {
		s.Logger.Printf("sms: failed to start SMS watch: %v", err)
		cancel()
		// Still drain once so messages already in storage aren't left behind.
		s.drainSMS(modemPath)
		return
	}
	s.smsWatchCancel = cancel
	s.Logger.Printf("sms: watching for incoming messages on %s", modemPath)

	// Make sure the modem actually tells MM about inbound SMS, and log its SMS
	// config/storage for diagnosis.
	s.configureAndDiagnoseSMS(modemPath)

	// Pick up anything already in storage (received while offline, or before
	// this (re-)arm).
	s.drainSMS(modemPath)
}

// configureAndDiagnoseSMS enables new-message indications on the modem and logs
// its SMS configuration and stored messages. On some modems ModemManager is
// never told about inbound SMS unless AT+CNMI is set so the modem emits +CMTI on
// receipt; we set it here (best-effort, via MM's command interface — the same
// path the APN code uses). The logged AT+CMGL dump shows, from the journal
// alone, whether inbound messages are landing in the modem's AT-readable store,
// which tells us how to finish wiring up receive if MM still doesn't surface
// them. Every command is best-effort: if MM restricts Modem.Command the errors
// are logged and nothing else is affected.
func (s *Service) configureAndDiagnoseSMS(modemPath dbus.ObjectPath) {
	if v, err := s.MMClient.GetProperty(modemPath, mm.ModemMessagingInterface, "DefaultStorage"); err == nil {
		s.Logger.Printf("sms: MM default-storage=%v", v.Value())
	}
	if v, err := s.MMClient.GetProperty(modemPath, mm.ModemMessagingInterface, "SupportedStorages"); err == nil {
		s.Logger.Printf("sms: MM supported-storages=%v", v.Value())
	}

	// Enable new-message indications so the modem notifies MM of inbound SMS.
	if _, err := s.MMClient.SendCommand(modemPath, "AT+CNMI=2,1,0,0,0", 5*time.Second); err != nil {
		s.Logger.Printf("sms: enabling new-message indications (CNMI) failed: %v", err)
	} else {
		s.Logger.Printf("sms: enabled new-message indications (AT+CNMI=2,1,0,0,0)")
	}

	// Diagnostics: current indication/storage config.
	for _, q := range []string{"AT+CPMS?", "AT+CNMI?"} {
		if resp, err := s.MMClient.SendCommand(modemPath, q, 5*time.Second); err == nil {
			s.Logger.Printf("sms: %s -> %s", q, strings.TrimSpace(resp))
		} else {
			s.Logger.Printf("sms: %s failed: %v", q, err)
		}
	}
	// The raw store dump contains full message PDUs (bodies, hex-encoded), so
	// it stays behind -debug: the normal logging policy is to never write
	// message content to the journal.
	if !s.Config.Debug {
		return
	}
	if _, err := s.MMClient.SendCommand(modemPath, "AT+CMGF=0", 5*time.Second); err == nil {
		if resp, err := s.MMClient.SendCommand(modemPath, "AT+CMGL=4", 10*time.Second); err == nil {
			s.Logger.Printf("sms: AT+CMGL=4 (modem SMS store) -> %s", strings.TrimSpace(resp))
		} else {
			s.Logger.Printf("sms: AT+CMGL=4 failed: %v", err)
		}
	}
}

// drainSMS delivers every inbound message currently in modem storage.
func (s *Service) drainSMS(modemPath dbus.ObjectPath) {
	if err := s.SMS.DrainReceived(modemPath); err != nil {
		s.Logger.Printf("sms: drain failed: %v", err)
	}
}

// deliverIncomingSMS is the SMS manager's delivery callback: it commits one
// received message to the sms:received stream (with a channel notification
// and hash convenience fields). An error return makes the manager keep the
// message in modem storage, so the periodic drain retries the delivery.
func (s *Service) deliverIncomingSMS(msg *sms.Message) error {
	s.touchCSActivity()
	count := s.unreadSMS.Add(1)
	if err := s.Redis.PublishIncomingSMS(redisClient.IncomingSMS{
		From:      msg.Number,
		Text:      msg.Text,
		Timestamp: msg.Timestamp.Format(time.RFC3339),
	}, count); err != nil {
		s.unreadSMS.Add(-1)
		return err
	}
	return nil
}

func (s *Service) touchCSActivity() {
	s.lastCSActivity.Store(time.Now().UnixNano())
}

// ownNumber returns the scooter's own MSISDN, or "" while it is unresolved.
func (s *Service) ownNumber() string {
	v, _ := s.ownMSISDN.Load().(string)
	return v
}

// queryOwnMSISDN asks the modem for its own subscriber number via AT+CNUM.
// Returns "" if the command fails or the SIM doesn't have an MSISDN stored.
func (s *Service) queryOwnMSISDN(modemPath dbus.ObjectPath) string {
	resp, err := s.MMClient.SendCommand(modemPath, "AT+CNUM", 5*time.Second)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(resp, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "+CNUM:") {
			continue
		}
		// +CNUM: "","number",129  or  +CNUM: "","+number",145
		parts := strings.SplitN(line[6:], ",", 3)
		if len(parts) < 2 {
			continue
		}
		num := strings.Trim(strings.TrimSpace(parts[1]), "\"")
		if num == "" {
			continue
		}
		// Only type 145 (international) numbers may gain a missing "+".
		// A national-format entry (type 129, e.g. "0157...") must be dialed
		// as stored; prepending "+" would make it undialable.
		if len(parts) >= 3 && strings.TrimSpace(parts[2]) == "145" && !strings.HasPrefix(num, "+") {
			num = "+" + num
		}
		return num
	}
	return ""
}

// refreshSGsViaVoiceCall refreshes the SGs association by placing a brief MO
// voice call to the scooter's own number. The call setup sends an Extended
// Service Request (MO_CS_FB) to the MME via LTE NAS; the MME forwards it to
// the MSC/VLR via SGs (SGsAP-SERVICE-REQUEST), resetting the implicit IMSI
// detach timer. The modem does CSFB to EDGE for the duration and returns to
// LTE after hangup — internet stays connected throughout, IP unchanged.
//
// The self-call never connects (the same line cannot answer an incoming call
// while it is placing an outgoing one), so no call charges are incurred.
func (s *Service) refreshSGsViaVoiceCall(ctx context.Context) bool {
	msisdn := s.ownNumber()
	if msisdn == "" {
		s.Logger.Printf("sms: SGs keepalive via voice call skipped: own MSISDN unknown")
		return false
	}
	modemPath, err := s.Modem.FindModem()
	if err != nil {
		s.Logger.Printf("sms: SGs keepalive via voice call skipped, no modem: %v", err)
		return false
	}

	s.Logger.Printf("sms: SGs keepalive — MO call to self (CSFB→EDGE, free)")
	start := time.Now()

	callPath, err := s.MMClient.CreateCall(modemPath, msisdn)
	if err != nil {
		s.Logger.Printf("sms: SGs keepalive voice call create failed: %v", err)
		return false
	}
	defer s.MMClient.DeleteCall(modemPath, callPath)

	if err := s.MMClient.StartCall(callPath); err != nil {
		s.Logger.Printf("sms: SGs keepalive voice call start failed: %v", err)
		return false
	}

	// Hold for 2 s so the SGsAP-SERVICE-REQUEST reaches the MSC before we
	// tear down. The Extended Service Request goes out synchronously on Start,
	// so this is a generous margin.
	select {
	case <-ctx.Done():
		s.MMClient.HangupCall(callPath)
		return false
	case <-time.After(2 * time.Second):
	}

	if err := s.MMClient.HangupCall(callPath); err != nil {
		s.Logger.Printf("sms: SGs keepalive voice call hangup error (CS signaling already sent): %v", err)
	}

	s.touchCSActivity()
	s.Logger.Printf("sms: SGs keepalive complete — voice call in %.1fs (CSFB, free)", time.Since(start).Seconds())
	return true
}

// startSMSRegistrationWatchdog keeps the SGs association at the MSC/VLR alive
// so that SMS delivery works indefinitely on LTE.
//
// On LTE with CEMODE=2 the modem does a combined EPS+IMSI Attach at boot,
// registering with both the LTE core (PS) and the MSC/VLR via SGs (CS). That
// SGs association is the path through which the MSC pages the UE for incoming
// SMS. O2/Lebara DE's MSC runs a ~15-minute implicit detach timer and silently
// drops the SGs record; the modem is never notified.
//
// The watchdog polls every minute and sends a keepalive only if no CS event
// has been observed in the last 13 minutes (2-minute margin before the 15-min
// timer expires). Any real incoming SMS counts as a CS event and resets the
// idle clock, so active scooters that receive fleet SMS regularly pay nothing.
//
// Primary keepalive — refreshSGsViaVoiceCall: place a brief MO call to own
// number. The call setup sends Extended Service Request (MO_CS_FB) to the MME
// via NAS; MME forwards SGsAP-SERVICE-REQUEST to the MSC, resetting the timer.
// Modem does CSFB to EDGE for ~2 s then returns to LTE. IP unchanged, free.
//
// Fallback — refreshSGsViaCFUN4: if both above fail (SGs already expired),
// AT+CFUN=4/1 forces a fresh Combined Attach (~6 s SMS downtime, ~29 s
// internet downtime). On this hardware CFUN=4 clears NAS context so CFUN=1
// always triggers a full Combined Attach rather than a lightweight TAU.
//
// Last resort — refreshSGsViaRadioCycle: MM Enable(false→true), same outcome.
func (s *Service) startSMSRegistrationWatchdog(ctx context.Context) {
	// SGs was just established by the Combined Attach at boot; start the idle
	// clock from now so we don't fire a redundant keepalive immediately.
	s.touchCSActivity()
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		msisdnWarned := false
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !s.modemEnabled.Load() {
					continue // modem is off (suspend/disable); nothing to keep alive
				}
				lastActivity := time.Unix(0, s.lastCSActivity.Load())
				idle := time.Since(lastActivity)
				if idle < 13*time.Minute {
					continue // CS was active recently; SGs timer not at risk
				}
				// No MSISDN means the voice-call keepalive can never work, and
				// that is a property of the SIM, not a transient failure. Do
				// NOT escalate to CFUN/radio cycles in that case: they cost
				// ~30 s of connectivity per round and would repeat forever.
				if s.ownNumber() == "" {
					if !msisdnWarned {
						s.Logger.Printf("sms: SGs keepalive disabled: own MSISDN unknown (SIM has no number stored); inbound SMS may stop after CS idle timeout")
						msisdnWarned = true
					}
					continue
				}
				msisdnWarned = false
				s.Logger.Printf("sms: no CS activity for %.0f min — sending SGs keepalive", idle.Minutes())
				if s.refreshSGsViaVoiceCall(ctx) {
					continue
				}
				s.Logger.Printf("sms: voice call keepalive failed, falling back to CFUN=4/1 cycle")
				if !s.refreshSGsViaCFUN4(ctx) {
					s.Logger.Printf("sms: CFUN=4/1 refresh failed, falling back to radio cycle")
					s.refreshSGsViaRadioCycle(ctx)
				}
			}
		}
	}()
}

// refreshSGsViaCFUN4 refreshes the SGs association using AT+CFUN=4 (fly mode)
// followed immediately by AT+CFUN=1. On the SIM7100E, CFUN=4 clears NAS
// context, so CFUN=1 triggers a fresh Combined Attach (not a lightweight TAU),
// causing ~6 s of SMS downtime and ~29 s of internet downtime. Used as a
// fallback when the voice-call keepalive fails.
//
// Returns true if the modem reconnected and SMS was re-configured successfully.
// Returns false if the approach fails so the caller can fall back.
func (s *Service) refreshSGsViaCFUN4(ctx context.Context) bool {
	modemPath, err := s.Modem.FindModem()
	if err != nil {
		s.Logger.Printf("sms: SGs refresh via CFUN=4/1 skipped, no modem: %v", err)
		return false
	}

	s.Logger.Printf("sms: SGs refresh — AT+CFUN=4/1 fly-mode cycle (TAU, ~3-10s downtime)")

	if _, err := s.MMClient.SendCommand(modemPath, "AT+CFUN=4", 5*time.Second); err != nil {
		s.Logger.Printf("sms: SGs refresh: AT+CFUN=4 failed: %v", err)
		return false
	}

	// Brief fly-mode period. 500 ms is enough for the network to see the modem
	// as unreachable; NAS context is held in modem RAM throughout.
	select {
	case <-ctx.Done():
		s.MMClient.SendCommand(modemPath, "AT+CFUN=1", 5*time.Second) //nolint:errcheck
		return false
	case <-time.After(500 * time.Millisecond):
	}

	// Restore radio. If MM transitioned the modem to its own disabled state
	// due to deregistration URCs, SendCommand will fail; fall back to Enable.
	if _, err := s.MMClient.SendCommand(modemPath, "AT+CFUN=1", 5*time.Second); err != nil {
		s.Logger.Printf("sms: SGs refresh: AT+CFUN=1 failed (%v), trying MM Enable(true)", err)
		if enableErr := s.MMClient.Enable(modemPath, true); enableErr != nil {
			s.Logger.Printf("sms: SGs refresh: MM Enable(true) also failed: %v", enableErr)
			return false
		}
	}

	// Poll until modem reports connected or the 20-second window closes.
	// A TAU completes in ~2-5 s; a full attach takes ~26 s.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(1 * time.Second):
		}
		state, err := s.Modem.GetModemInfo(s.Config.Interface)
		if err == nil && state.Status == "connected" {
			break
		}
	}

	// The cycle can make MM re-register the modem under a new D-Bus path, so
	// fully re-arm the SMS watch instead of just re-applying CNMI: startSMSWatch
	// re-resolves the path, re-subscribes the Added signal, reconfigures the
	// modem, drains storage, and resets the CS idle clock so the watchdog
	// doesn't fire again on its next tick.
	if _, err := s.Modem.FindModem(); err != nil {
		s.Logger.Printf("sms: SGs refresh: modem gone after CFUN cycle: %v", err)
		return false
	}
	s.startSMSWatch(ctx)
	s.Logger.Printf("sms: SGs refresh complete (CFUN=4/1 fly-mode cycle)")
	return true
}

func (s *Service) refreshSGsViaRadioCycle(ctx context.Context) {
	modemPath, err := s.Modem.FindModem()
	if err != nil {
		s.Logger.Printf("sms: SGs refresh skipped, no modem: %v", err)
		return
	}

	s.Logger.Printf("sms: SGs refresh — cycling modem radio (Enable false→true) for fresh combined attach")

	if err := s.MMClient.Enable(modemPath, false); err != nil {
		s.Logger.Printf("sms: SGs refresh: disable failed (%v), falling back to firmware reset", err)
		if err := s.handleModemFailure(ctx, "sgs_refresh"); err != nil {
			s.Logger.Printf("sms: SGs refresh firmware reset failed: %v", err)
		}
		return
	}

	// Brief pause to let the network clear the UE's registration state before
	// re-enabling so the modem performs a fresh Combined Attach (not a TAU).
	select {
	case <-ctx.Done():
		return
	case <-time.After(3 * time.Second):
	}

	if err := s.MMClient.Enable(modemPath, true); err != nil {
		s.Logger.Printf("sms: SGs refresh: enable failed (%v), falling back to firmware reset", err)
		if err := s.handleModemFailure(ctx, "sgs_refresh"); err != nil {
			s.Logger.Printf("sms: SGs refresh firmware reset failed: %v", err)
		}
		return
	}

	// Wait for the modem to complete network registration and APN reconnection
	// before re-arming the SMS watch and draining any queued messages.
	select {
	case <-ctx.Done():
		return
	case <-time.After(30 * time.Second):
	}

	// Re-arm rather than just reconfigure: the Enable cycle can rebind the
	// modem's D-Bus path, and startSMSWatch also resets the CS idle clock so
	// the watchdog doesn't immediately fire another refresh.
	s.startSMSWatch(ctx)
	s.Logger.Printf("sms: SGs refresh complete (radio cycle)")
}

func (s *Service) ensureModemEnabled(ctx context.Context) error {
	if s.Modem.IsModemPresent() {
		s.Logger.Printf("Modem is already present via D-Bus")
		return nil
	}

	if modem.IsInterfacePresent(s.Config.Interface) {
		s.Logger.Printf("Modem interface %s is present, waiting for ModemManager...", s.Config.Interface)
		waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := s.Modem.WaitForModem(waitCtx, s.Config.Interface); err == nil {
			return nil
		}
		s.Logger.Printf("ModemManager did not register modem, proceeding to GPIO recovery")
	}

	s.Logger.Printf("Modem not detected, will attempt to enable via GPIO")

	// Try multiple times with increasing wait times
	for attempt := range health.MaxRecoveryAttempts {
		waitTime := min(time.Duration(60*(attempt+1))*time.Second, 300*time.Second)

		s.Logger.Printf("Modem start attempt %d/%d with %v wait time",
			attempt+1, health.MaxRecoveryAttempts, waitTime)

		if err := s.Modem.StartModem(); err != nil {
			continue
		}

		attemptCtx, cancel := context.WithTimeout(ctx, waitTime)

		err := s.Modem.WaitForModem(attemptCtx, s.Config.Interface)
		cancel()

		if err == nil {
			s.Logger.Printf("Modem successfully enabled on attempt %d", attempt+1)
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Continue to next attempt
		}
	}

	// If we get here, all attempts failed
	s.Logger.Printf("SEVERE ERROR: Modem failed to come up after %d attempts with up to 5 minute wait times",
		health.MaxRecoveryAttempts)

	// Mark modem as potentially defective in Redis
	s.Health.State = health.StatePermanentFailure
	s.publishHealthState(ctx)

	s.Redis.RaiseFault(redisClient.FaultCodeModemRecoveryFailed, "Modem recovery failed")

	return fmt.Errorf("modem failed to come up after multiple attempts, marked as potentially defective")
}

// probeHealth checks whether the modem is currently healthy on D-Bus without
// triggering any recovery machinery. Used inside attemptRecovery to verify a
// strategy succeeded; going through checkHealth there would re-enter
// handleModemFailure and collapse the escalation sequence.
func (s *Service) probeHealth() bool {
	if _, err := s.Modem.FindModem(); err != nil {
		return false
	}
	if err := s.Modem.CheckPrimaryPort(); err != nil {
		return false
	}
	if err := s.Modem.CheckPowerState(); err != nil {
		return false
	}
	return true
}

// recoverySucceeded is the shared post-strategy bookkeeping: mark healthy,
// reset GPS, clear the fault. Called whenever probeHealth returns true after
// a recovery strategy.
func (s *Service) recoverySucceeded(ctx context.Context, strategy string) {
	s.Logger.Printf("Modem recovery successful via %s", strategy)
	s.Health.MarkNormal()
	s.GPSRecoveryCount = 0
	s.resetGPSAfterModemRecovery()
	s.publishHealthState(ctx)
	s.Redis.ClearFault(redisClient.FaultCodeModemRecoveryFailed)

	// The modem's D-Bus path can change across a reset; re-arm the inbound-SMS
	// watch on the new path (and drain anything that queued meanwhile).
	s.startSMSWatch(ctx)
}

func (s *Service) checkHealth(ctx context.Context) error {
	// Skip health check if we're in a terminal state
	if s.Health.IsTerminal() {
		return fmt.Errorf("modem in terminal state: %s", s.Health.State)
	}

	if !s.probeHealth() {
		return s.handleModemFailure(ctx, "probe_failed")
	}

	s.Health.MarkNormal()
	s.Redis.ClearFault(redisClient.FaultCodeModemRecoveryFailed)
	return nil
}

func (s *Service) handleModemFailure(ctx context.Context, reason string) error {
	s.Logger.Printf("Modem failure detected: %s", reason)

	if s.Health.IsRecovering() {
		return fmt.Errorf("recovery in progress")
	}

	// Exhausted retries: publish terminal Wait state, back off 2 minutes,
	// then reset and allow recovery to try again on the next failure.
	// StatePermanentFailure is reached only from ensureModemEnabled when
	// the modem never responded at all.
	if !s.Health.CanRecover() {
		s.Health.MarkRecoveryFailed()
		s.publishHealthState(ctx)
		s.Redis.RaiseFault(redisClient.FaultCodeModemRecoveryFailed,
			fmt.Sprintf("Max recovery attempts (%d) exhausted, entering recovery-failed-wait", health.MaxRecoveryAttempts))
		s.Logger.Printf("Max recovery attempts reached, entering %s state", s.Health.State)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Minute):
		}
		s.Health.MarkNormal() // clears both state and RecoveryAttempts
		s.publishHealthState(ctx)
		s.Logger.Printf("Recovery-failed-wait expired, will retry on next failure")
		return nil
	}

	return s.attemptRecovery(ctx)
}

func (s *Service) attemptRecovery(ctx context.Context) error {
	s.Health.StartRecovery()

	s.Logger.Printf("Attempting modem recovery (attempt %d/%d)",
		s.Health.RecoveryAttempts, health.MaxRecoveryAttempts)

	s.publishHealthState(ctx)

	// Strategy 1: Try software reset first if modem is present
	_, err := s.Modem.FindModem()
	if err == nil {
		s.Logger.Printf("Attempting to reset the modem via D-Bus")
		if err := s.Modem.ResetModem(); err != nil {
			s.Logger.Printf("Failed to reset modem via D-Bus: %v", err)
		} else {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(health.RecoveryWaitTime):
			}

			if s.probeHealth() {
				s.recoverySucceeded(ctx, "D-Bus reset")
				return nil
			}
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// Strategy 2: Try USB unbind/bind recovery
	s.Logger.Printf("Attempting USB recovery (unbind/bind)...")
	if err := s.Modem.RecoverUSB(); err != nil {
		if errors.Is(err, usb.ErrDeviceNotPresent) {
			// Modem is transiently off the bus (typical mid-reset). Wait
			// briefly for it to reappear; if it does, skip USB recovery.
			s.Logger.Printf("USB device not present, waiting up to %v for modem to reappear on D-Bus", health.RecoveryWaitTime)
			waitCtx, waitCancel := context.WithTimeout(ctx, health.RecoveryWaitTime)
			err := s.Modem.WaitForModem(waitCtx, s.Config.Interface)
			waitCancel()
			if err == nil && s.probeHealth() {
				s.recoverySucceeded(ctx, "MM rebind wait")
				return nil
			}
		} else {
			s.Logger.Printf("USB recovery failed: %v", err)
		}
	} else {
		usbCtx, usbCancel := context.WithTimeout(ctx, health.RecoveryWaitTime)
		err := s.Modem.WaitForModem(usbCtx, s.Config.Interface)
		usbCancel()

		if err == nil && s.probeHealth() {
			s.recoverySucceeded(ctx, "USB recovery")
			return nil
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// Strategy 3: Try hardware reset via GPIO
	s.Logger.Printf("Attempting modem restart (GPIO with D-Bus fallback)...")
	if err := s.Modem.RestartModem(ctx); err != nil {
		s.Logger.Printf("GPIO restart failed: %v", err)
	} else {
		gpioCtx, gpioCancel := context.WithTimeout(ctx, health.RecoveryWaitTime)
		err := s.Modem.WaitForModem(gpioCtx, s.Config.Interface)
		gpioCancel()

		if err == nil && s.probeHealth() {
			s.recoverySucceeded(ctx, "GPIO restart")
			return nil
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// Strategy 4: Just wait longer and hope the modem recovers
	s.Logger.Printf("Hardware recovery uncertain, waiting additional time for modem to stabilize...")
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(30 * time.Second):
	}

	if s.probeHealth() {
		s.recoverySucceeded(ctx, "extended wait")
		return nil
	}

	s.Logger.Printf("Recovery attempt %d failed, will retry", s.Health.RecoveryAttempts)
	return fmt.Errorf("recovery attempt failed, will retry")
}

func (s *Service) publishHealthState(ctx context.Context) error {
	return s.Redis.PublishInternetState("modem-health", s.Health.State)
}

// handleGPSFailure attempts GPS-specific recovery before escalating to modem recovery
func (s *Service) handleGPSFailure(ctx context.Context, gpsErr error) error {
	s.Logger.Printf("Attempting GPS-specific recovery for: %v", gpsErr)

	// Try to restart GPS configuration without restarting the entire modem
	if err := s.attemptGPSRecovery(gpsErr); err != nil {
		s.Logger.Printf("GPS-specific recovery failed: %v", err)
		// Only escalate to modem recovery for severe GPS issues after GPS recovery fails
		if recoveryErr := s.handleModemFailure(ctx, fmt.Sprintf("gps_stuck_after_gps_recovery: %v", gpsErr)); recoveryErr != nil {
			return fmt.Errorf("both GPS and modem recovery failed: %v", recoveryErr)
		}
	}

	return nil
}

// attemptGPSRecovery tries to recover GPS without restarting the modem.
// trigger is the underlying failure that caused recovery to be requested
// (e.g. gps_data_stale, gps_timestamp_stuck, gps_fix_timeout). Logged so
// we can correlate unexpected GPS restarts with the triggering check.
//
// Pacing is handled via s.gpsRecoveryUntil (see the monitor loop) — this
// function does not sleep while holding any mutex.
func (s *Service) attemptGPSRecovery(trigger error) error {
	if !s.modemEnabled.Load() {
		s.Logger.Printf("GPS recovery skipped, modem is disabled")
		return nil
	}

	// Acquire lock to prevent concurrent GPS recovery attempts
	s.gpsRecoveryMutex.Lock()
	// Check if recovery is already in progress
	if s.gpsRecoveryInProgress {
		s.gpsRecoveryMutex.Unlock()
		s.Logger.Printf("GPS recovery already in progress, skipping duplicate attempt")
		return nil
	}
	s.gpsRecoveryInProgress = true
	s.gpsRecoveryMutex.Unlock()
	defer func() {
		s.gpsRecoveryMutex.Lock()
		s.gpsRecoveryInProgress = false
		s.gpsRecoveryMutex.Unlock()
	}()

	s.GPSRecoveryCount++
	s.Logger.Printf("Attempting GPS recovery (attempt %d, trigger=%v)", s.GPSRecoveryCount, trigger)

	// If we've tried GPS recovery too many times, do a full reset and gate
	// the monitor loop for 30 seconds before it's allowed to re-enable GPS.
	if s.GPSRecoveryCount > 3 {
		s.Logger.Printf("GPS recovery attempted %d times, performing full reset with longer break", s.GPSRecoveryCount)
		s.GPSRecoveryCount = 0

		// Stop gpsd and close GPS connection
		if err := s.Location.StopGPSD(); err != nil {
			s.Logger.Printf("Warning: Failed to stop gpsd: %v", err)
		}
		s.Location.Close()

		// Reset state tracking
		s.GPSEnabledTime = time.Time{}
		s.WaitingForGPSLogged = false
		s.Location.SetLastDataReceived(time.Time{})

		// Gate the monitor loop rather than sleeping under the mutex.
		s.gpsRecoveryMutex.Lock()
		s.gpsRecoveryUntil = time.Now().Add(30 * time.Second)
		s.gpsRecoveryMutex.Unlock()
		s.Logger.Printf("GPS break complete, monitor will re-enable after 30s")
		return nil
	}

	// Stop gpsd before performing GPS-related modem reset
	s.Logger.Printf("Stopping gpsd service before GPS reset...")
	if err := s.Location.StopGPSD(); err != nil {
		s.Logger.Printf("Warning: Failed to stop gpsd: %v", err)
		// Continue with recovery even if gpsd stop fails
	}

	// Close existing GPS connection; gate the monitor for 2 seconds so
	// it doesn't re-enable while gpsd is still tearing down.
	s.Location.Close()
	s.gpsRecoveryMutex.Lock()
	s.gpsRecoveryUntil = time.Now().Add(2 * time.Second)
	s.gpsRecoveryMutex.Unlock()

	// Reset GPS state tracking
	s.GPSEnabledTime = time.Time{}
	s.WaitingForGPSLogged = false
	s.Location.SetLastDataReceived(time.Time{})

	var recoveryErr error
	reenabled := false
	s.withGPSLifecycleLock(func() {
		if !s.modemEnabled.Load() {
			return
		}
		modemPath, err := s.Modem.FindModem()
		if err != nil {
			recoveryErr = fmt.Errorf("modem not found for GPS recovery: %v", err)
			return
		}
		if err := s.Location.EnableGPS(modemPath); err != nil {
			recoveryErr = fmt.Errorf("failed to re-enable GPS: %v", err)
			return
		}
		reenabled = true
	})
	if recoveryErr != nil {
		return recoveryErr
	}
	if !reenabled {
		s.Logger.Printf("GPS recovery aborted, modem was disabled while recovering")
		return nil
	}

	s.GPSEnabledTime = time.Now()
	s.Logger.Printf("GPS recovery completed, waiting for fix...")
	return nil
}

func (s *Service) withGPSLifecycleLock(fn func()) {
	s.gpsRecoveryMutex.Lock()
	defer s.gpsRecoveryMutex.Unlock()
	fn()
}

// requestGPSModeForConnectivity kicks off a GPS mode change to match the new
// connectivity state. Runs asynchronously because the AT stop/start dance
// takes several seconds; we don't want to stall the modem state loop. The
// location service serializes mode changes internally via configMutex.
//
// UE-Based mode is disabled for now. In the field (2026-04-15) we observed a
// fleet scooter on Telefónica DE hang for 180 s in UE-Based after a switch,
// with no NMEA timestamp updates, until the stuck-timestamp recovery path
// fired. The stall overlapped with the user unlocking and starting to ride.
// Until we can verify UE-Based is reliable across carriers and firmware (and
// given XTRA is broken on SIM7100E), stay in standalone always. The
// classifier + plumbing are kept so flipping this back on is one constant.
//
// Re-tested 2026-05-13 on deep-blue (O2 DE, MCC-MNC 26203):
//   - Tried supl.storo.cloud:7276 and (per past testing) supl.google.com:7276.
//     Both: TCP reachable, modem accepts CGPSURL, enters CGPS=1,2 cleanly,
//     then produces no NMEA / no fix until the CGPSMSB=1 fallback finally
//     drops to standalone after ~10 minutes. SUPL is dead on this firmware
//     regardless of server.
//   - Tried re-enabling XTRA (AT+CGPSXE=1, AT+CGPSXDAUTO=1). Chip emits
//     "+CGPSXD: 2" ("Assistant file check error" per SIMCom AT manual
//     V1.01 §19.20) on every attempt. Verified the host can fetch the
//     same xtra2.bin from xtrapath1.izatcloud.net via the modem
//     interface (HTTP 200, 34 KB), and chip clock is correct
//     (CTZU=1 → NITZ-synced). HTP time
//     sync (AT+CHTPSERV/CHTPUPDATE) makes no difference; the chip rejects
//     the file. Most plausible cause: Qualcomm rotated XTRA signing keys
//     post-2018, SIM7100E firmware has the old root in ROM. Not fixable
//     from outside the modem firmware.
const enableUEBasedMode = false

func (s *Service) requestGPSModeForConnectivity(ctx context.Context, conn connectivity.State) {
	var desired location.GPSMode
	switch {
	case enableUEBasedMode && conn == connectivity.Connected:
		desired = location.ModeUEBased
	default:
		desired = location.ModeStandalone
	}

	prev := s.Location.CurrentGPSMode()
	s.Logger.Printf("gps-transition request from=%s to=%s connectivity=%s", prev, desired, conn)

	go func() {
		if err := s.Location.SetGPSMode(ctx, desired); err != nil {
			s.Logger.Printf("Failed to switch GPS to %s mode: %v", desired, err)
			return
		}
		s.publishGPSMode()
	}()
}

// publishGPSMode updates the gps.mode Redis field if the current mode has
// changed since the last publish. Serialized via modePubMu because
// requestGPSModeForConnectivity can spawn overlapping goroutines when
// connectivity flickers.
func (s *Service) publishGPSMode() {
	mode := s.Location.CurrentGPSMode()

	s.modePubMu.Lock()
	defer s.modePubMu.Unlock()

	if mode == s.lastPubGPSMode {
		return
	}
	if err := s.Redis.PublishLocationState(map[string]interface{}{
		"mode": mode.String(),
	}, false); err != nil {
		s.Logger.Printf("Failed to publish gps mode: %v", err)
		return
	}
	s.lastPubGPSMode = mode
}

// publishModemState publishes the detailed modem and derived internet state to Redis.
// It now takes the determined internetStatus as an argument.
func (s *Service) publishModemState(ctx context.Context, currentState *modem.State, internetStatus string) error {
	// Track which fields changed for consolidated logging
	var internetChanges, modemChanges []string

	// Publish internet state fields
	if s.LastState.Status != internetStatus {
		if err := s.Redis.PublishInternetState("status", internetStatus); err != nil {
			return err
		}
		internetChanges = append(internetChanges, fmt.Sprintf("status=%s", internetStatus))
		s.LastState.Status = internetStatus
	}

	if s.LastState.LastRawModemStatus != currentState.Status {
		if err := s.Redis.PublishInternetState("modem-state", currentState.Status); err != nil {
			s.Logger.Printf("Failed to publish internet modem-state: %v", err)
		}
		internetChanges = append(internetChanges, fmt.Sprintf("modem-state=%s", currentState.Status))
		s.LastState.LastRawModemStatus = currentState.Status
	}

	if s.LastState.IfIPAddr != currentState.IfIPAddr {
		if err := s.Redis.PublishInternetState("ip-address", currentState.IfIPAddr); err != nil {
			return err
		}
		internetChanges = append(internetChanges, fmt.Sprintf("ip=%s", currentState.IfIPAddr))
		s.LastState.IfIPAddr = currentState.IfIPAddr
	}

	if s.LastState.AccessTech != currentState.AccessTech {
		if err := s.Redis.PublishInternetState("access-tech", currentState.AccessTech); err != nil {
			return err
		}
		internetChanges = append(internetChanges, fmt.Sprintf("tech=%s", currentState.AccessTech))
		s.LastState.AccessTech = currentState.AccessTech
	}

	if s.LastState.SignalQuality != currentState.SignalQuality {
		if err := s.Redis.PublishInternetState("signal-quality", fmt.Sprintf("%d", currentState.SignalQuality)); err != nil {
			return err
		}
		internetChanges = append(internetChanges, fmt.Sprintf("signal=%d", currentState.SignalQuality))
		s.LastState.SignalQuality = currentState.SignalQuality
	}

	if s.LastState.IMEI != currentState.IMEI {
		if err := s.Redis.PublishInternetState("sim-imei", currentState.IMEI); err != nil {
			return err
		}
		internetChanges = append(internetChanges, fmt.Sprintf("imei=%s", currentState.IMEI))
		s.LastState.IMEI = currentState.IMEI
	}

	if s.LastState.IMSI != currentState.IMSI {
		if err := s.Redis.PublishInternetState("sim-imsi", currentState.IMSI); err != nil {
			return err
		}
		internetChanges = append(internetChanges, fmt.Sprintf("imsi=%s", currentState.IMSI))
		s.LastState.IMSI = currentState.IMSI
	}

	if s.LastState.ICCID != currentState.ICCID {
		if err := s.Redis.PublishInternetState("sim-iccid", currentState.ICCID); err != nil {
			return err
		}
		internetChanges = append(internetChanges, fmt.Sprintf("iccid=%s", currentState.ICCID))
		s.LastState.ICCID = currentState.ICCID
	}

	// Publish modem state fields
	if s.LastState.PowerState != currentState.PowerState {
		if err := s.Redis.PublishModemState("power-state", currentState.PowerState); err != nil {
			return err
		}
		modemChanges = append(modemChanges, fmt.Sprintf("power=%s", currentState.PowerState))
		s.LastState.PowerState = currentState.PowerState
	}

	if s.LastState.SIMState != currentState.SIMState {
		if err := s.Redis.PublishModemState("sim-state", currentState.SIMState); err != nil {
			return err
		}
		modemChanges = append(modemChanges, fmt.Sprintf("sim=%s", currentState.SIMState))
		s.LastState.SIMState = currentState.SIMState
	}

	if s.LastState.SIMLockStatus != currentState.SIMLockStatus {
		if err := s.Redis.PublishModemState("sim-lock", currentState.SIMLockStatus); err != nil {
			return err
		}
		modemChanges = append(modemChanges, fmt.Sprintf("sim-lock=%s", currentState.SIMLockStatus))
		s.LastState.SIMLockStatus = currentState.SIMLockStatus
	}

	if s.LastState.PinAction != currentState.PinAction {
		if err := s.Redis.PublishModemState("pin-action", currentState.PinAction); err != nil {
			return err
		}
		modemChanges = append(modemChanges, fmt.Sprintf("pin-action=%s", currentState.PinAction))
		s.LastState.PinAction = currentState.PinAction
	}

	if s.LastState.ApnAction != currentState.ApnAction {
		if err := s.Redis.PublishModemState("apn-action", currentState.ApnAction); err != nil {
			return err
		}
		modemChanges = append(modemChanges, fmt.Sprintf("apn-action=%s", currentState.ApnAction))
		s.LastState.ApnAction = currentState.ApnAction
	}

	if s.LastState.OperatorName != currentState.OperatorName {
		if err := s.Redis.PublishModemState("operator-name", currentState.OperatorName); err != nil {
			return err
		}
		modemChanges = append(modemChanges, fmt.Sprintf("operator=%s", currentState.OperatorName))
		s.LastState.OperatorName = currentState.OperatorName
	}

	if s.LastState.OperatorCode != currentState.OperatorCode {
		if err := s.Redis.PublishModemState("operator-code", currentState.OperatorCode); err != nil {
			return err
		}
		modemChanges = append(modemChanges, fmt.Sprintf("mcc-mnc=%s", currentState.OperatorCode))
		s.LastState.OperatorCode = currentState.OperatorCode
	}

	if s.LastState.IsRoaming != currentState.IsRoaming {
		if err := s.Redis.PublishModemState("is-roaming", fmt.Sprintf("%t", currentState.IsRoaming)); err != nil {
			return err
		}
		modemChanges = append(modemChanges, fmt.Sprintf("roaming=%t", currentState.IsRoaming))
		s.LastState.IsRoaming = currentState.IsRoaming
	}

	if s.LastState.Registration != currentState.Registration {
		if err := s.Redis.PublishModemState("registration", currentState.Registration); err != nil {
			s.Logger.Printf("Failed to publish modem registration: %v", err)
		}
		modemChanges = append(modemChanges, fmt.Sprintf("reg=%s", currentState.Registration))
		s.LastState.Registration = currentState.Registration
	}

	if s.LastState.RegistrationFail != currentState.RegistrationFail {
		if err := s.Redis.PublishModemState("registration-fail", currentState.RegistrationFail); err != nil {
			return err
		}
		modemChanges = append(modemChanges, fmt.Sprintf("reg-fail=%s", currentState.RegistrationFail))
		s.LastState.RegistrationFail = currentState.RegistrationFail
	}

	if s.LastState.ErrorState != currentState.ErrorState {
		if err := s.Redis.PublishModemState("error-state", currentState.ErrorState); err != nil {
			s.Logger.Printf("Failed to publish modem error-state: %v", err)
		}
		modemChanges = append(modemChanges, fmt.Sprintf("error=%s", currentState.ErrorState))
		s.LastState.ErrorState = currentState.ErrorState
	}

	conn := s.connClassifier.Classify(connectivity.Inputs{
		ModemStatus:  currentState.Status,
		SIMState:     currentState.SIMState,
		Registration: currentState.Registration,
		Enabled:      s.modemEnabled.Load(),
		HardFailed:   s.Health.IsTerminal(),
	})
	if conn != s.lastPubConn {
		if err := s.Redis.PublishInternetState("connectivity", string(conn)); err != nil {
			s.Logger.Printf("Failed to publish internet connectivity: %v", err)
		}
		internetChanges = append(internetChanges, fmt.Sprintf("connectivity=%s", conn))
		s.lastPubConn = conn
		s.requestGPSModeForConnectivity(ctx, conn)
	}

	// Log consolidated changes
	if len(internetChanges) > 0 {
		s.Logger.Printf("internet %s", strings.Join(internetChanges, " "))
	}
	if len(modemChanges) > 0 {
		s.Logger.Printf("modem %s", strings.Join(modemChanges, " "))
	}

	return nil
}

func (s *Service) publishLocationState(ctx context.Context, loc location.Location, publishRecovery bool) error {
	data := map[string]interface{}{
		"latitude":  fmt.Sprintf("%.6f", loc.Latitude),
		"longitude": fmt.Sprintf("%.6f", loc.Longitude),
		"altitude":  fmt.Sprintf("%.6f", loc.Altitude),
		"speed":     fmt.Sprintf("%.6f", loc.Speed*3.6), // Convert m/s to km/h
		"course":    fmt.Sprintf("%.6f", loc.Course),
		"timestamp": loc.Timestamp.Format(time.RFC3339),
	}
	gpsStatus := s.Location.GetGPSStatus()
	for k, v := range gpsStatus {
		data[k] = v
	}

	if err := s.Redis.PublishLocationState(data, publishRecovery); err != nil {
		return err
	}
	if err := s.Redis.PublishGPSSnapshot(data); err != nil {
		s.Logger.Printf("Failed to publish GPS snapshot: %v", err)
	}
	return nil
}

// syncClockFromGPS feeds chrony a single time sample via `chronyc settime`.
// Returns true if chrony accepted the sample, false if the timestamp was
// rejected or the command failed (so the caller can retry on the next tick
// instead of waiting a full clockSyncInterval).
func (s *Service) syncClockFromGPS(t time.Time) bool {
	if t.IsZero() {
		return false
	}
	// Defense in depth against GPS week-rollover bugs: refuse to set the
	// system clock to a timestamp before the current rollover epoch. The TPV
	// callback already corrects rollover, but we never want a stray bad
	// value to roll a working system clock back ~20 years.
	if t.Before(location.MinValidGPSDate) {
		s.Logger.Printf("Refusing to set system time from GPS: %s is before the current GPS rollover epoch (%s)",
			t.Format(time.RFC3339), location.MinValidGPSDate.Format(time.RFC3339))
		return false
	}
	timeStr := t.Local().Format("02 Jan 2006 15:04:05")
	out, err := exec.Command("chronyc", "settime", timeStr).CombinedOutput()
	if err != nil {
		s.Logger.Printf("Failed to set system time from GPS: %v: %s", err, out)
		return false
	}
	s.Logger.Printf("System time set from GPS: %s", timeStr)
	return true
}

// reachabilityField distinguishes the two ways a probe can fail. "unreachable"
// means nothing answered while the local stack is healthy, which is the
// correct and permanent steady state for a scooter on a restricted APN.
// "no-path" means the local stack is broken, which is a real fault. The old
// code could not tell these apart, and treated both as broken hardware.
func reachabilityField(reachable bool, a link.Assessment) string {
	if reachable {
		return "ok"
	}
	if a.Healthy {
		return "unreachable"
	}
	return "no-path"
}

// linkLayerField renders the assessment for the Redis diagnostic field.
func linkLayerField(a link.Assessment) string {
	if a.Healthy {
		return "ok"
	}
	if a.Reason == "" {
		return a.FailedLayer.String()
	}
	return a.FailedLayer.String() + ": " + a.Reason
}

// publishIfChanged writes a diagnostic field only when its value moved.
// PublishInternetState pipelines HSET + PUBLISH, so an unconditional write
// wakes every subscriber of the internet channel on every tick.
func (s *Service) publishIfChanged(field, value string, last *string) {
	if *last == value {
		return
	}
	publish := s.publishFn
	if publish == nil {
		publish = s.Redis.PublishInternetState
	}
	if err := publish(field, value); err != nil {
		if s.Logger != nil {
			s.Logger.Printf("Failed to publish %s: %v", field, err)
		}
		// Leave *last untouched so the write is retried next tick.
		return
	}
	*last = value
}

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func remedyCooldownFor(r link.Remedy) time.Duration {
	switch r {
	case link.RemedyReattach:
		return reattachCooldown
	case link.RemedyBearerBounce:
		return bearerBounceCooldown
	case link.RemedyModemReset:
		return modemResetCooldown
	}
	return 0
}

// handleAssessment applies the remedy for a failing layer, subject to that
// remedy's cooldown. A healthy assessment does nothing.
//
// The probe result is deliberately not a parameter. Whether some destination
// answered is not evidence about the modem, and treating it as such is the
// defect this whole change exists to remove: one fleet's APN drops everything
// outside its tunnel, and the old code read that as broken hardware and reset
// a healthy modem every tick, 184 times and counting on one vehicle.
func (s *Service) handleAssessment(a link.Assessment) {
	if a.Healthy || a.Remedy == link.RemedyNone {
		s.pendingRepeat = 0
		return
	}

	// Debounce. The machinery this replaced tolerated a stalled data session
	// for 15 minutes precisely so tunnels, underground parking and handoffs
	// did not trigger action, and acting on a single snapshot would throw that
	// tolerance away. Requiring the same failing layer twice also covers the
	// window right after startup and after a recovery, when NetworkManager may
	// not have finished bringing the connection up yet.
	if a.FailedLayer != s.pendingLayer {
		s.pendingLayer, s.pendingRepeat = a.FailedLayer, 1
	} else {
		s.pendingRepeat++
	}
	if s.pendingRepeat < remedyConfirmations {
		if s.Logger != nil {
			s.Logger.Printf("link: %s failed (%s), awaiting confirmation (%d/%d)",
				a.FailedLayer, a.Reason, s.pendingRepeat, remedyConfirmations)
		}
		return
	}

	now := s.clock()
	if until, ok := s.remedyCooldown[a.Remedy]; ok && now.Before(until) {
		if s.Logger != nil {
			s.Logger.Printf("link: %s failed (%s), %s suppressed for another %v",
				a.FailedLayer, a.Reason, a.Remedy, until.Sub(now).Round(time.Second))
		}
		return
	}
	s.remedyCooldown[a.Remedy] = now.Add(remedyCooldownFor(a.Remedy))
	if s.Logger != nil {
		s.Logger.Printf("link: %s failed (%s), applying %s", a.FailedLayer, a.Reason, a.Remedy)
	}
	if s.applyRemedyFn != nil {
		s.applyRemedyFn(a.Remedy)
	} else {
		s.applyRemedy(a.Remedy)
	}
	// Only a remedy that actually ran advances the ladder. See
	// link.Assessor.NoteRemedyApplied.
	if s.link != nil {
		s.link.NoteRemedyApplied(a.FailedLayer, a.Remedy)
	}
}

// applyRemedy performs the action. Split from handleAssessment so the policy,
// meaning the cooldowns and the logging, is testable without a modem.
func (s *Service) applyRemedy(r link.Remedy) {
	switch r {
	case link.RemedyReattach:
		modemPath, err := s.Modem.FindModem()
		if err != nil {
			s.Logger.Printf("link: reattach skipped, no modem: %v", err)
			return
		}
		if err := s.Apn.Reattach(modemPath); err != nil {
			s.Logger.Printf("link: reattach failed: %v", err)
		}
	case link.RemedyBearerBounce:
		if err := apn.NewNMCli().Reapply(nmWWANConnection); err != nil {
			s.Logger.Printf("link: bearer bounce failed: %v", err)
		}
	case link.RemedyModemReset:
		if err := s.handleModemFailure(s.ctx, "link_layer_failure"); err != nil {
			s.Logger.Printf("link: modem reset failed: %v", err)
		}
	}
}

// nextProbeInterval doubles up to maxInterval, regardless of whether the probe
// succeeded. Only the network probe backs off; the local layer checks run every
// tick regardless, and a change in the local assessment resets this to base.
func nextProbeInterval(cur, base, maxInterval time.Duration) time.Duration {
	next := cur * 2
	if next > maxInterval {
		return maxInterval
	}
	if next < base {
		return base
	}
	return next
}

// assignedResolvers gathers the resolvers the network handed us. No single
// source is reliable: on an affected vehicle the ModemManager bearer read came
// back empty while resolv.conf had them all along, so all three are consulted.
func (s *Service) assignedResolvers() []string {
	return health.ResolverSources{Sources: []func() []string{
		func() []string {
			modemPath, err := s.Modem.FindModem()
			if err != nil {
				return nil
			}
			bearer, err := s.MMClient.DataBearer(modemPath)
			if err != nil {
				return nil
			}
			return bearer.IP4.DNS
		},
		func() []string { return health.ResolvectlSource(s.Config.Interface) },
		health.ResolvConfSource,
	}}.Resolvers()
}

func (s *Service) checkAndPublishModemStatus(ctx context.Context) error {
	if err := s.checkHealth(ctx); err != nil {
		s.Logger.Printf("Health check failed: %v", err)
		// If health check fails, assume disconnected and publish minimal state
		s.publishModemState(ctx, modem.NewState(), "disconnected")
		s.publishHealthState(ctx)
		return err
	}

	currentState, err := s.Modem.GetModemInfo(s.Config.Interface)
	if err != nil {
		s.Logger.Printf("Failed to get modem info: %v", err)
		// Publish the state we got, even if partial, as it contains the ErrorState
		s.publishModemState(ctx, currentState, "disconnected")
		s.publishHealthState(ctx)
		return err // Return the original error from GetModemInfo
	}

	s.reconcileSMSPresence(ctx, currentState.SIMPath)

	// Reconcile SIM PIN state with the configured cellular.sim-pin setting.
	// The manager owns retry-counter gating so the service can never push the
	// SIM into PUK lock by itself.
	pin, _ := s.simPin.Load().(string)
	currentState.PinAction = string(s.Sim.Reconcile(sim.Input{
		SIMPath:           currentState.SIMPath,
		LockStatus:        currentState.SIMLockStatus,
		LockStatusKnown:   currentState.SIMLockStatusKnown,
		SIMPinLockEnabled: currentState.SIMPinLockEnabled,
		SIMPinLockKnown:   currentState.SIMPinLockKnown,
		UnlockRetriesPin:  currentState.UnlockRetriesPin,
		ConfiguredPIN:     pin,
	}))

	// Reconcile APN settings against the modem and NetworkManager. Only
	// runs once we have an ICCID — apn.Manager handles the no-SIM case
	// itself but skipping here avoids a redundant log line on every tick
	// before the SIM comes up.
	if currentState.ICCID != "" && currentState.SIMLockStatus == "" {
		modemPath, _ := s.Modem.FindModem()
		outcome := s.Apn.Reconcile(apn.Input{
			ICCID:     currentState.ICCID,
			ModemPath: modemPath,
			Desired: apn.Config{
				APN:      s.apnAPN.Load().(string),
				Username: s.apnUsername.Load().(string),
				Password: s.apnPassword.Load().(string),
				Auth:     s.apnAuth.Load().(string),
			},
		})
		currentState.ApnAction = string(outcome)
		// Force LTE reattach when we actually wrote something — a new
		// CGDCONT=1 only takes effect on the next attach. Done in a
		// goroutine because COPS=2/0 can block for tens of seconds; we
		// don't want to stall the monitor loop. modemPath is captured
		// by value so a concurrent modem rebind doesn't race us.
		if outcome == apn.OutcomeApplied || outcome == apn.OutcomeICCIDChangedClear {
			if modemPath != "" {
				go func(p dbus.ObjectPath) {
					if err := s.Apn.Reattach(p); err != nil {
						s.Logger.Printf("apn: reattach failed: %v", err)
					}
				}(modemPath)
			}
		}
	}

	// Layers 0-7: local signals only, no destination involved. This is the
	// only input allowed to trigger a modem action.
	//
	// currentState was already fetched above; pass it through rather than
	// making LinkSnapshot repeat those D-Bus reads.
	snap, usage := s.Modem.LinkSnapshot(currentState, s.Config.Interface, s.wantATCheck)
	assessment := s.link.Assess(snap)
	s.wantATCheck = assessment.WantATCheck

	// Byte accounting rides along on the bearer read the snapshot just did.
	// Skipped when no bearer could be read: zeroes would look like a counter
	// reset and get counted twice.
	if usage.Valid {
		s.Usage.Observe(datausage.Sample{
			BearerPath: usage.Path,
			RxBytes:    usage.RxBytes,
			TxBytes:    usage.TxBytes,
			Roaming:    currentState.IsRoaming,
		})
	}
	s.publishDataUsage()
	// Persisting happens at power transitions (disableModem) and shutdown.
	// This only catches a unit that stays up for days without either.
	s.Usage.Backstop()

	// Layer 8: reachability. Backed off while healthy, because at a 30s
	// interval across a fleet this is real query load on the operator's
	// resolvers. Any change in the local assessment forces an immediate
	// re-probe, so a genuine fault is still seen within one tick.
	now := s.clock()
	assessmentChanged := assessment != s.lastAssessment
	// Skipped when the local stack is already known broken: reachabilityField
	// reports no-path from the assessment alone in that case, so the probe
	// would buy nothing and can block the tick for many seconds.
	if !assessment.Healthy {
		// Do not carry the last good probe result across a local fault. The
		// stack is known broken, so publishing the previous success would
		// report status=connected on a modem whose bearer has just died,
		// which is a worse lie than the one this change set out to remove.
		s.lastProbe = health.Result{Detail: "not probed: " + linkLayerField(assessment)}
	}
	if assessment.Healthy && (now.After(s.nextProbeAt) || assessmentChanged) {
		s.lastProbe = s.prober.Probe(ctx, s.assignedResolvers())
		// Backs off on both outcomes. Resetting on failure would keep a unit
		// whose destinations are silent probing at full rate forever, which is
		// the fleet this change exists for, and buys nothing: a probe failure
		// is never a fault under this design. A change in the local assessment
		// still forces an immediate re-probe, which is what actually needs to
		// be responsive.
		s.probeInterval = nextProbeInterval(s.probeInterval,
			s.Config.InternetCheckTime, s.Config.InternetCheckMaxInterval)
		s.nextProbeAt = now.Add(s.probeInterval)
		if !s.lastProbe.Reachable {
			s.Logger.Printf("connectivity: unreachable (%s)", s.lastProbe.Detail)
		}
	}
	if assessmentChanged {
		s.probeInterval = s.Config.InternetCheckTime
	}
	s.lastAssessment = assessment

	internetStatus := "disconnected"
	if s.lastProbe.Reachable {
		internetStatus = "connected"
	}

	// Registered before any error return below. A remedy and a Redis write
	// are independent concerns: a failed publish must not suppress recovery
	// of a wedged modem. Deferred rather than called inline so it still runs
	// after everything is published, since the reset ladder blocks for minutes
	// and acting first would leave consumers reading stale state throughout.
	//
	// Deliberately not passed the probe result: reachability may never
	// trigger a remedy.
	defer s.handleAssessment(assessment)

	if err := s.publishModemState(ctx, currentState, internetStatus); err != nil {
		s.Logger.Printf("Failed to publish state: %v", err)
		s.publishHealthState(ctx)
		return err
	}

	// Additive diagnostics. status keeps its existing values so consumers are
	// unaffected; these two carry the distinction that was previously
	// impossible to see from Redis.
	// Change-gated: PublishInternetState pipelines HSET + PUBLISH, so writing
	// unconditionally would wake every subscriber of the internet channel
	// twice per tick forever. Every other field here is gated the same way.
	s.publishIfChanged("reachability", reachabilityField(s.lastProbe.Reachable, assessment),
		&s.lastReachability)
	s.publishIfChanged("link-layer", linkLayerField(assessment), &s.lastLinkLayer)

	if err := s.publishHealthState(ctx); err != nil {
		s.Logger.Printf("Failed to publish health state: %v", err)
		return err
	}

	return nil
}

// publishDataUsage writes the cellular byte totals when they have moved. The
// hash is written silently, so this gate is about the Redis round trip rather
// than about waking subscribers: an idle modem republishes nothing.
func (s *Service) publishDataUsage() {
	totals := s.Usage.Totals()
	if s.havePubUsage && totals == s.lastPubUsage {
		return
	}
	publish := s.publishUsageFn
	if publish == nil {
		publish = s.Redis.PublishDataUsage
	}
	err := publish(map[string]interface{}{
		"rx-bytes":         strconv.FormatUint(totals.RxBytes, 10),
		"tx-bytes":         strconv.FormatUint(totals.TxBytes, 10),
		"rx-bytes-roaming": strconv.FormatUint(totals.RxBytesRoaming, 10),
		"tx-bytes-roaming": strconv.FormatUint(totals.TxBytesRoaming, 10),
		"since":            totals.Since,
	})
	if err != nil {
		// Left ungated so the next tick retries.
		s.Logger.Printf("Failed to publish data usage: %v", err)
		return
	}
	s.lastPubUsage, s.havePubUsage = totals, true
}

func (s *Service) queryCellLocation(ctx context.Context, state *modem.State) {
	modemPath, err := s.Modem.FindModem()
	if err != nil {
		return
	}

	locationData, err := s.MMClient.GetLocation(modemPath)
	if err != nil {
		if s.Config.Debug {
			s.Logger.Printf("Failed to get cell location data: %v", err)
		}
		return
	}

	tower, err := cell.ParseModemManagerLocation(locationData, state.AccessTech)
	if err != nil {
		if s.Config.Debug {
			s.Logger.Printf("Failed to parse cell info: %v", err)
		}
		return
	}

	// Skip API call if cell tower hasn't changed and we have a cached result
	if s.lastCellTower != nil && s.lastCellLoc != nil &&
		tower.CellId == s.lastCellTower.CellId &&
		tower.LocationAreaCode == s.lastCellTower.LocationAreaCode &&
		tower.MobileNetworkCode == s.lastCellTower.MobileNetworkCode &&
		tower.MobileCountryCode == s.lastCellTower.MobileCountryCode {
		return
	}

	result, err := cell.Geolocate(ctx, []cell.CellTower{*tower})
	if err != nil {
		if s.Config.Debug {
			s.Logger.Printf("BeaconDB lookup failed: %v", err)
		}
		return
	}

	s.lastCellTower = tower
	s.lastCellLoc = result
	s.Logger.Printf("Cell location: %.5f, %.5f (accuracy: %.0fm)", result.Latitude, result.Longitude, result.Accuracy)

	data := map[string]interface{}{
		"latitude":  fmt.Sprintf("%.6f", result.Latitude),
		"longitude": fmt.Sprintf("%.6f", result.Longitude),
		"accuracy":  fmt.Sprintf("%.0f", result.Accuracy),
		"source":    "cell",
	}
	if err := s.Redis.PublishCellLocationState(data); err != nil {
		s.Logger.Printf("Failed to publish cell location: %v", err)
	}
}

// resetGPSAfterModemRecovery tears down the GPS subsystem so the monitor
// loop reconfigures it from scratch on the next tick. A modem reset
// invalidates the AT command state, gpsd connection, and GPS timestamps,
// so a fresh EnableGPS is cleaner than trying to paper over stale clocks.
func (s *Service) resetGPSAfterModemRecovery() {
	s.Location.Close()
	s.GPSEnabledTime = time.Time{}
	s.WaitingForGPSLogged = false
	s.Location.SetLastDataReceived(time.Time{})
}

const (
	// gpsNoDataTimeout is how long we tolerate silence from the modem's GPS
	// before declaring it wedged. The chip emits NMEA at 1 Hz, so anything
	// past a handful of seconds means it's stopped talking and we should
	// reinit. Independent of fix state.
	gpsNoDataTimeout = 5 * time.Second

	// gpsFixTimeout is how long we wait for a fix once GPS is enabled.
	// Sized for cold-start: SIM7100E in standalone mode (UE-Based is
	// disabled for this hardware) needs the full GPS almanac broadcast,
	// which is 12.5 minutes minimum. Aggressive recovery during this
	// window discards partial almanac/ephemeris pages and resets the
	// download — counter-productive.
	gpsFixTimeout = 15 * time.Minute
)

func (s *Service) checkGPSHealth() error {
	if s.Location.IsConfiguring() {
		return nil
	}

	now := time.Now()

	// "Are we getting any NMEA from the chip?" — fed on every TPV/SKY
	// callback in location.go regardless of fix mode. If this trips the
	// chip is wedged; recovery should reinit.
	lastData := s.Location.LastDataReceived()
	if !lastData.IsZero() && now.Sub(lastData) > gpsNoDataTimeout {
		return fmt.Errorf("gps_no_data: no GPS stanzas received for %v", now.Sub(lastData))
	}

	// "Did we ever achieve a fix this session?" — long timeout because
	// cold-start without SUPL is bounded below by the almanac broadcast
	// cycle. Disarmed once a fix is established (see GPSEnabledTime
	// reset in the monitor loop).
	if !s.GPSEnabledTime.IsZero() && !s.Location.HasValidFix() && now.Sub(s.GPSEnabledTime) > gpsFixTimeout {
		return fmt.Errorf("gps_fix_timeout: no GPS fix established for %v", now.Sub(s.GPSEnabledTime))
	}

	return nil
}

func (s *Service) monitorStatus(ctx context.Context) {
	defer close(s.monitorDone)
	ticker := time.NewTicker(s.Config.InternetCheckTime)
	gpsTimer := time.NewTicker(location.GPSUpdateInterval)
	cellTimer := time.NewTicker(location.CellLocationUpdateInterval)
	defer ticker.Stop()
	defer gpsTimer.Stop()
	defer cellTimer.Stop()

	if err := s.checkAndPublishModemStatus(ctx); err != nil {
		s.Logger.Printf("Initial modem status check failed: %v", err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !s.modemEnabled.Load() {
				continue
			}
			if err := s.checkAndPublishModemStatus(ctx); err != nil {
				s.Logger.Printf("Periodic modem status check failed: %v", err)
			}
			// Poll for inbound SMS as a reliable fallback to the Added signal:
			// some modems don't emit Added for SIM-stored messages, so a
			// signal-only design can silently miss them.
			if s.smsSIMPresent.Load() {
				if modemPath, err := s.Modem.FindModem(); err == nil {
					s.drainSMS(modemPath)
				}
			}
		case <-cellTimer.C:
			if !s.modemEnabled.Load() {
				continue
			}
			if s.cellLocationEnabled.Load() && !s.Location.HasValidFix() && s.LastState.Status == "connected" {
				s.queryCellLocation(ctx, s.LastState)
			}
		case <-gpsTimer.C:
			if !s.modemEnabled.Load() {
				continue
			}
			if !s.gpsEnabled.Load() {
				continue
			}
			if s.Health.State == health.StateNormal {
				modemPath, err := s.Modem.FindModem()
				if err != nil {
					continue
				}

				// Check if GPS recovery is in progress or the monitor is
				// gated waiting for recovery to settle.
				s.gpsRecoveryMutex.Lock()
				recoveryInProgress := s.gpsRecoveryInProgress
				gatedUntil := s.gpsRecoveryUntil
				s.gpsRecoveryMutex.Unlock()

				if recoveryInProgress || time.Now().Before(gatedUntil) {
					continue
				}

				if !s.Location.Enabled {
					if err := s.Location.EnableGPS(modemPath); err != nil {
						s.Logger.Printf("Failed to enable GPS: %v", err)
						continue
					}
					s.GPSEnabledTime = time.Now()
				}

				// Check for GPS health issues and try GPS-specific recovery first
				if err := s.checkGPSHealth(); err != nil {
					s.Logger.Printf("GPS health check failed: %v", err)
					if recoveryErr := s.handleGPSFailure(ctx, err); recoveryErr != nil {
						s.Logger.Printf("GPS recovery failed: %v", recoveryErr)
					}
					continue
				}

				// Always publish GPS status, even without valid fix
				gpsStatus := s.Location.GetGPSStatus()
				hasValidFix, _ := gpsStatus["active"].(bool)

				// Determine if we should publish GPS recovery notification
				// Check current internet status from LastState
				hasInternet := s.LastState.Status == "connected"
				publishRecovery := false

				if hasValidFix {
					// Bootstrap the system clock from GPS on the first fix of the
					// session so it's close to right immediately, even if NTP is
					// reachable but chrony hasn't synced yet. After bootstrap, only
					// keep feeding chrony GPS samples while we're offline — when we
					// have connectivity, the NTP pool is the more accurate source
					// and we don't want manual settime samples competing with it.
					needsClockSync := s.lastClockSync.IsZero() ||
						(!hasInternet && time.Since(s.lastClockSync) >= clockSyncInterval)
					if needsClockSync {
						currentLoc := s.Location.CurrentLoc()
						if s.syncClockFromGPS(currentLoc.Timestamp) {
							s.lastClockSync = time.Now()
						}
					}

					// GPS is now valid - check if this is a recovery event
					publishRecovery = s.Location.ShouldPublishRecovery(hasInternet)
					if publishRecovery {
						// Clear the fresh init flag after first successful publish
						s.Location.SetGPSFreshInit(false)
					}

					// Reset GPS lost time since we have a fix now
					s.Location.GPSLostTime = time.Time{}

					// If we were waiting (flag is true), log that we got a fix
					if s.WaitingForGPSLogged {
						s.Logger.Printf("GPS fix established")
						s.WaitingForGPSLogged = false
						// Reset recovery counter since GPS is now working
						s.GPSRecoveryCount = 0
						// Disarm the cold-start timeout — it only guards
						// against "never got a fix"; the data-stale and
						// timestamp-stuck checks handle ongoing monitoring.
						s.GPSEnabledTime = time.Time{}
					}

					// TTFF: if a search was in progress, stop the clock
					// and publish. Mode is read here (at fix time) rather
					// than at wait-start because ProbeGPSMode may correct
					// our in-memory currentMode in the window between
					// wait-start and fix-established.
					if !s.ttffStart.IsZero() {
						ttff := time.Since(s.ttffStart)
						s.ttffStart = time.Time{}
						mode := s.Location.CurrentGPSMode()
						snr, _ := gpsStatus["snr"].(float64)
						satsUsed, _ := gpsStatus["satellites-used"].(int32)
						satsVisible, _ := gpsStatus["satellites-visible"].(int32)
						s.Logger.Printf("gps ttff=%.1fs mode=%s snr=%.1fdBHz sats=%d/%d",
							ttff.Seconds(), mode, snr, satsUsed, satsVisible)
						s.Redis.PublishLocationState(map[string]interface{}{
							"last_ttff_seconds": fmt.Sprintf("%.1f", ttff.Seconds()),
							"last_ttff_mode":    mode.String(),
						}, false)
					}

					// Log GPS diagnostics every 90 seconds
					if s.LastGPSQualityLog.IsZero() || time.Since(s.LastGPSQualityLog) >= 90*time.Second {
						s.Logger.Printf("gps state=%s fix=%s eph=%.1fm hdop=%.1f vdop=%.1f pdop=%.1f snr=%.1fdBHz sats=%d/%d",
							gpsStatus["state"], gpsStatus["fix"],
							gpsStatus["eph"], gpsStatus["hdop"], gpsStatus["vdop"], gpsStatus["pdop"],
							gpsStatus["snr"], gpsStatus["satellites-used"], gpsStatus["satellites-visible"])
						s.LastGPSQualityLog = time.Now()
					}

					if err := s.publishLocationState(ctx, s.Location.CurrentLoc(), publishRecovery); err != nil {
						s.Logger.Printf("Failed to publish location: %v", err)
					}
				} else {
					// GPS fix is lost - mark the time
					if s.Location.GPSLostTime.IsZero() {
						s.Location.GPSLostTime = time.Now()
					}

					if !s.WaitingForGPSLogged {
						s.Logger.Printf("Waiting for valid GPS fix...")
						s.WaitingForGPSLogged = true
						// Start (or re-start) the TTFF clock. Mode will be
						// read at fix-establish time rather than here.
						s.ttffStart = time.Now()
					}

					// Log GPS diagnostics every 90 seconds while searching
					if s.LastGPSQualityLog.IsZero() || time.Since(s.LastGPSQualityLog) >= 90*time.Second {
						s.Logger.Printf("gps state=%s fix=%s snr=%.1fdBHz sats=%d/%d",
							gpsStatus["state"], gpsStatus["fix"],
							gpsStatus["snr"], gpsStatus["satellites-used"], gpsStatus["satellites-visible"])
						s.LastGPSQualityLog = time.Now()
					}

					// Publish just the status without location data (never
					// publish recovery when no fix). All SKY-derived fields
					// (sat counts, SNR, DOPs) pass through the cached atomics
					// — they stay meaningful during search and show
					// acquisition progress. EPH/EPS/EPT are TPV/fix-specific
					// (horizontal/speed/time error estimates of the current
					// fix) so they're zeroed to avoid lingering last-good-fix
					// values in the hash and pub/sub snapshot.
					data := map[string]interface{}{
						"fix":                gpsStatus["fix"],
						"snr":                gpsStatus["snr"],
						"active":             gpsStatus["active"],
						"connected":          gpsStatus["connected"],
						"state":              gpsStatus["state"],
						"hdop":               gpsStatus["hdop"],
						"vdop":               gpsStatus["vdop"],
						"pdop":               gpsStatus["pdop"],
						"eph":                float64(0),
						"eps":                float64(0),
						"ept":                float64(0),
						"satellites-used":    gpsStatus["satellites-used"],
						"satellites-visible": gpsStatus["satellites-visible"],
					}
					if err := s.Redis.PublishLocationState(data, false); err != nil {
						s.Logger.Printf("Failed to publish GPS status: %v", err)
					}

					// Pub/sub snapshot includes the (stale) last-known location
					// alongside zeroed quality fields. Subscribers key off
					// active=false to ignore the position; including it keeps
					// the snapshot a complete view of the hash state.
					loc := s.Location.CurrentLoc()
					snapshot := map[string]interface{}{
						"latitude":  fmt.Sprintf("%.6f", loc.Latitude),
						"longitude": fmt.Sprintf("%.6f", loc.Longitude),
						"altitude":  fmt.Sprintf("%.6f", loc.Altitude),
						"speed":     fmt.Sprintf("%.6f", loc.Speed*3.6),
						"course":    fmt.Sprintf("%.6f", loc.Course),
						"timestamp": loc.Timestamp.Format(time.RFC3339),
					}
					for k, v := range data {
						snapshot[k] = v
					}
					if err := s.Redis.PublishGPSSnapshot(snapshot); err != nil {
						s.Logger.Printf("Failed to publish GPS snapshot: %v", err)
					}
				}
			}
		}
	}
}

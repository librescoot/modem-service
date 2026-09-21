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

var modemOnlineStates = map[string]bool{
	"parked":         true,
	"ready-to-drive": true,
}

// clockValidationInterval paces GPS-vs-system clock checks.
const clockValidationInterval = 5 * time.Minute

// clockStepTolerance is the GPS/system disagreement beyond which the clock is
// stepped. Smaller offsets are harmless; stepping for them fights NTP and
// disturbs wall-clock deadlines.
const clockStepTolerance = 30 * time.Second

// clockStepConfirmations is how many consecutive checks must agree on a gross
// disagreement before stepping, so one bad fix cannot move the clock.
const clockStepConfirmations = 3

// Require two identical failures so transient bring-up states do not trigger recovery.
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
	WaitingForGPSLogged   bool
	GPSEnabledTime        time.Time
	GPSRecoveryCount      int
	LastGPSQualityLog     time.Time
	gpsRecoveryMutex      sync.Mutex // serializes GPS recovery and configuration
	gpsRecoveryInProgress bool
	gpsRecoveryUntil      time.Time // Protected by gpsRecoveryMutex; monitor skips EnableGPS until this passes
	// Only local link assessments may trigger recovery; remote reachability
	// can be permanently false on a restricted APN.
	link           *link.Assessor
	prober         *health.Prober
	wantATCheck    bool
	remedyCooldown map[link.Remedy]time.Time
	probeInterval  time.Duration
	nextProbeAt    time.Time
	lastProbe      health.Result
	lastAssessment link.Assessment

	lastReachability string
	lastLinkLayer    string

	Usage        *datausage.Counter
	lastPubUsage datausage.Totals
	havePubUsage bool

	// Consecutive matching failures debounce snapshots taken during bring-up.
	pendingLayer  link.Layer
	pendingRepeat int

	applyRemedyFn  func(link.Remedy)
	publishFn      func(field, value string) error
	publishModemFn func(field, value string) error
	publishUsageFn func(map[string]interface{}) error
	now            func() time.Time

	lastClockSync    time.Time
	lastClockCheck   time.Time
	clockOffsetCount int
	gpsFaultActive   atomic.Bool

	// Settings (from Redis) — atomic so the Redis watcher goroutine can
	// update them without racing the monitor goroutine that reads them.
	gpsEnabled          atomic.Bool
	cellLocationEnabled atomic.Bool
	lastCellTower       *cell.CellTower
	lastCellLoc         *cell.CellLocation

	modemEnabled         atomic.Bool
	simMissing           atomic.Bool
	modemStateChange     chan struct{}
	smsRefreshRequest    chan chan struct{}
	recoveryRunMu        sync.Mutex
	modemOpCancelMu      sync.Mutex
	modemOpCancel        context.CancelFunc
	modemOpGeneration    uint64
	ensureModemEnabledFn func(context.Context) error
	getModemInfoFn       func(string) (*modem.State, error)
	isInterfacePresentFn func(string) bool
	probeHealthErrorFn   func() error
	disableModemFn       func(context.Context)
	powerOffModemFn      func(context.Context) error
	removeInhibitorFn    func() error
	publishLocationFn    func(map[string]interface{}, bool) error
	watchSMSAddedFn      func(context.Context, dbus.ObjectPath, func(dbus.ObjectPath, bool)) error
	handleSMSAddedFn     func(dbus.ObjectPath, dbus.ObjectPath)
	raiseFaultFn         func(int, string)
	recoveryBackoffFn    func(context.Context) error

	ctx context.Context

	connClassifier *connectivity.Classifier
	lastPubConn    connectivity.State // last value published to Redis

	// Mode is sampled at fix time because startup probing may change it.
	ttffStart time.Time

	// modePubMu protects publication from overlapping mode-change goroutines.
	modePubMu      sync.Mutex
	lastPubGPSMode location.GPSMode

	// monitorDone prevents transports closing during an in-flight monitor call.
	monitorDone chan struct{}
}

func New(cfg *config.Config, logger *log.Logger, version string) (*Service, error) {
	redis, err := redisClient.New(cfg.RedisURL, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create Redis client: %v", err)
	}

	mmClient, err := mm.NewClient(cfg.Debug, logger.Printf)
	if err != nil {
		redis.Close()
		return nil, fmt.Errorf("failed to create ModemManager client: %v", err)
	}

	// The service owns the D-Bus client shared with the modem manager.
	modemMgr, err := modem.NewManager(mmClient, logger)
	if err != nil {
		mmClient.Close()
		redis.Close()
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
		modemStateChange:    make(chan struct{}, 1),
		smsRefreshRequest:   make(chan chan struct{}),

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
	s.ctx = ctx

	if err := s.Redis.Ping(); err != nil {
		return fmt.Errorf("redis connection failed: %v", err)
	}
	s.Redis.ClearFault(redisClient.FaultCodeGPSUnavailable)

	if err := s.Redis.StartModemCommandHandler(s.handleModemCommand); err != nil {
		s.Logger.Printf("Failed to start modem command handler: %v", err)
	}

	if err := s.Redis.StartSMSCommandHandler(s.handleSMSCommand); err != nil {
		s.Logger.Printf("Failed to start SMS command handler: %v", err)
	}
	// Reset SMS operational state so a stale "sending" or "error" from a previous
	// crash doesn't persist in Redis indefinitely.
	s.Redis.PublishSMSState("state", "idle")

	if err := s.Redis.StartVehicleStateWatcher(s.handleVehicleState); err != nil {
		s.Logger.Printf("Failed to start vehicle state watcher: %v", err)
	}

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

	if s.modemEnabled.Load() {
		opCtx, finish := s.startModemOperation(ctx)
		err := s.runEnsureModemEnabled(opCtx)
		finish()
		if err != nil && s.modemEnabled.Load() {
			s.Logger.Printf("SEVERE ERROR: Failed to ensure modem is enabled: %v", err)
			if !modem.IsInterfacePresent(s.Config.Interface) && !s.Modem.IsModemPresent() {
				s.Logger.Printf("Cannot continue without modem interface or D-Bus presence")
				return fmt.Errorf("modem not available: %v", err)
			}
		}
	}

	if s.modemEnabled.Load() {
		if err := s.Redis.AddModemInhibitor(); err != nil {
			s.Logger.Printf("Failed to register modem power inhibitor: %v", err)
		}
	}

	// Opt-in: refresh SGs before an operator's short implicit-detach timeout.
	// Each self-call briefly drops to 2G, so this must not run fleet-wide.
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

	// The stopped monitor can no longer race this final persistence point.
	if err := s.Usage.Flush(); err != nil {
		s.Logger.Printf("Failed to persist data usage: %v", err)
	}

	if err := s.Modem.Close(); err != nil {
		s.Logger.Printf("Error closing modem manager: %v", err)
	}
	if err := s.MMClient.Close(); err != nil {
		s.Logger.Printf("Error closing ModemManager D-Bus client: %v", err)
	}
	if err := s.Redis.RemoveModemInhibitor(); err != nil {
		s.Logger.Printf("Error clearing modem power inhibitor: %v", err)
	}
	if err := s.Redis.Close(); err != nil {
		s.Logger.Printf("Error closing Redis client: %v", err)
	}

	return nil
}

func (s *Service) runEnsureModemEnabled(ctx context.Context) error {
	if s.ensureModemEnabledFn != nil {
		return s.ensureModemEnabledFn(ctx)
	}
	return s.ensureModemEnabled(ctx)
}

func (s *Service) getModemInfo() (*modem.State, error) {
	if s.getModemInfoFn != nil {
		return s.getModemInfoFn(s.Config.Interface)
	}
	return s.Modem.GetModemInfo(s.Config.Interface)
}

// hasMissingSIM distinguishes a detected modem with no SIM from a modem
// failure. Preserve a missing-SIM result across a transient D-Bus gap only
// while the USB network interface still exists. Once it disappears, the modem
// itself is unavailable and must use the normal recovery path.
func (s *Service) hasMissingSIM() bool {
	state, err := s.getModemInfo()
	if err != nil || state == nil {
		interfacePresent := modem.IsInterfacePresent
		if s.isInterfacePresentFn != nil {
			interfacePresent = s.isInterfacePresentFn
		}
		if !interfacePresent(s.Config.Interface) {
			s.simMissing.Store(false)
			return false
		}
		return s.simMissing.Load()
	}

	missing := state.SIMState == modem.SIMStateMissing
	s.simMissing.Store(missing)
	return missing
}

func (s *Service) runDisableModem(ctx context.Context) {
	if s.disableModemFn != nil {
		s.disableModemFn(ctx)
		return
	}
	s.disableModem(ctx)
}

func (s *Service) powerOffModem(ctx context.Context) error {
	if s.powerOffModemFn != nil {
		return s.powerOffModemFn(ctx)
	}
	return s.Modem.PowerOffModem(ctx)
}

func (s *Service) removeModemInhibitor() error {
	if s.removeInhibitorFn != nil {
		return s.removeInhibitorFn()
	}
	return s.Redis.RemoveModemInhibitor()
}

func (s *Service) publishLocation(fields map[string]interface{}, snapshot bool) error {
	if s.publishLocationFn != nil {
		return s.publishLocationFn(fields, snapshot)
	}
	return s.Redis.PublishLocationState(fields, snapshot)
}

func (s *Service) signalModemStateChange() {
	select {
	case s.modemStateChange <- struct{}{}:
	default:
	}
}

func (s *Service) cancelModemOperation() {
	s.modemOpCancelMu.Lock()
	cancel := s.modemOpCancel
	s.modemOpCancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Service) startModemOperation(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	s.modemOpCancelMu.Lock()
	s.modemOpGeneration++
	generation := s.modemOpGeneration
	s.modemOpCancel = cancel
	s.modemOpCancelMu.Unlock()
	return ctx, func() {
		cancel()
		s.modemOpCancelMu.Lock()
		if s.modemOpGeneration == generation {
			s.modemOpCancel = nil
		}
		s.modemOpCancelMu.Unlock()
	}
}

func (s *Service) handleModemCommand(command string) error {
	switch command {
	case "enable":
		s.Logger.Printf("Received modem enable command")
		s.modemEnabled.Store(true)
		if err := s.Redis.AddModemInhibitor(); err != nil {
			s.Logger.Printf("Failed to register modem power inhibitor: %v", err)
		}
		s.signalModemStateChange()
	case "disable":
		s.Logger.Printf("Received modem disable command")
		s.modemEnabled.Store(false)
		s.cancelModemOperation()
		s.signalModemStateChange()
	default:
		s.Logger.Printf("Unknown modem command: %s", command)
	}
	return nil
}

// handleVehicleState enables the modem in online vehicle states.
func (s *Service) handleVehicleState(state string) error {
	if modemOnlineStates[state] {
		if s.modemEnabled.CompareAndSwap(false, true) {
			s.Logger.Printf("Vehicle state '%s' - enabling modem", state)
			if err := s.Redis.AddModemInhibitor(); err != nil {
				s.Logger.Printf("Failed to register modem power inhibitor: %v", err)
			}
			s.signalModemStateChange()
		}
	}
	return nil
}

// disableModem powers off the modem and publishes the resulting state.
func (s *Service) disableModem(ctx context.Context) {
	s.Logger.Printf("Disabling modem...")

	s.reconcileSMSPresence(ctx, "")

	// Close GPS first. Preserve last lat/lng but clear the fix indicators
	// so consumers don't treat stale coords as a current position.
	s.withGPSLifecycleLock(s.Location.Close)
	s.publishLocation(map[string]interface{}{
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

	publishInternet := s.publishFn
	if publishInternet == nil {
		publishInternet = s.Redis.PublishInternetState
	}
	publishModem := s.publishModemFn
	if publishModem == nil {
		publishModem = s.Redis.PublishModemState
	}
	publishInternet("status", "disconnected")
	publishInternet("modem-state", "off")
	publishModem("power-state", "off")

	// The monitor loop skips status checks while disabled, so the connectivity
	// classifier won't run to observe the power-off. Publish "disabled"
	// explicitly and force the classifier so it reports correctly on resume.
	publishInternet("connectivity", string(connectivity.Disabled))
	s.connClassifier.Force(connectivity.Disabled)
	s.lastPubConn = connectivity.Disabled

	// Match the publication cache to Redis so resume changes are not suppressed.
	s.LastState.Status = "disconnected"
	s.LastState.LastRawModemStatus = "off"
	s.LastState.PowerState = "off"

	// Force the first post-resume fix to bootstrap the potentially drifted clock.
	s.lastClockSync = time.Time{}
	s.lastClockCheck = time.Time{}
	s.clockOffsetCount = 0

	if err := s.powerOffModem(ctx); err != nil {
		s.Logger.Printf("Failed to disable modem via GPIO: %v", err)
	}

	// Persist before releasing the inhibitor that permits suspend.
	if err := s.Usage.Flush(); err != nil {
		s.Logger.Printf("Failed to persist data usage: %v", err)
	}

	// Release only after power-off so suspend cannot race the modem.
	if err := s.removeModemInhibitor(); err != nil {
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

func (s *Service) durableContext(fallback context.Context) context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return fallback
}

func (s *Service) installSMSWatchContext(ctx context.Context) (context.Context, context.CancelFunc) {
	watchCtx, cancel := context.WithCancel(ctx)
	s.smsWatchCancel = cancel
	return watchCtx, cancel
}

// armSMSAddedWatch processes Added objects directly because some never reach storage.
func (s *Service) armSMSAddedWatch(ctx context.Context, modemPath dbus.ObjectPath) error {
	watchCtx, cancel := s.installSMSWatchContext(s.durableContext(ctx))
	watch := s.watchSMSAddedFn
	if watch == nil {
		watch = s.MMClient.WatchSMSAdded
	}
	if err := watch(watchCtx, modemPath, s.smsAddedHandler(modemPath)); err != nil {
		cancel()
		s.smsWatchCancel = nil
		return err
	}
	return nil
}

func (s *Service) smsAddedHandler(modemPath dbus.ObjectPath) func(dbus.ObjectPath, bool) {
	return func(smsPath dbus.ObjectPath, received bool) {
		s.Logger.Printf("sms: Added signal path=%s received=%v", smsPath, received)
		if !received {
			return
		}
		if s.handleSMSAddedFn != nil {
			s.handleSMSAddedFn(modemPath, smsPath)
			return
		}
		s.SMS.HandleAdded(modemPath, smsPath)
	}
}

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
	if err := s.armSMSAddedWatch(ctx, modemPath); err != nil {
		s.Logger.Printf("sms: failed to start SMS watch: %v", err)
		s.drainSMS(modemPath)
		return
	}
	s.Logger.Printf("sms: watching for incoming messages on %s", modemPath)

	s.configureAndDiagnoseSMS(modemPath)

	s.drainSMS(modemPath)
}

// configureAndDiagnoseSMS best-effort enables +CMTI indications required by
// modems that otherwise do not expose inbound SMS to ModemManager.
func (s *Service) configureAndDiagnoseSMS(modemPath dbus.ObjectPath) {
	if v, err := s.MMClient.GetProperty(modemPath, mm.ModemMessagingInterface, "DefaultStorage"); err == nil {
		s.Logger.Printf("sms: MM default-storage=%v", v.Value())
	}
	if v, err := s.MMClient.GetProperty(modemPath, mm.ModemMessagingInterface, "SupportedStorages"); err == nil {
		s.Logger.Printf("sms: MM supported-storages=%v", v.Value())
	}

	if _, err := s.MMClient.SendCommand(modemPath, "AT+CNMI=2,1,0,0,0", 5*time.Second); err != nil {
		s.Logger.Printf("sms: enabling new-message indications (CNMI) failed: %v", err)
	} else {
		s.Logger.Printf("sms: enabled new-message indications (AT+CNMI=2,1,0,0,0)")
	}

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

// startSMSRegistrationWatchdog refreshes the SGs association before the
// operator's silent 15-minute implicit detach. CS activity resets the clock;
// recovery escalates from a free self-call to CFUN and radio cycles.
func (s *Service) startSMSRegistrationWatchdog(ctx context.Context) {
	// Boot's combined attach established SGs now.
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
				done := make(chan struct{})
				select {
				case <-ctx.Done():
					return
				case s.smsRefreshRequest <- done:
				}
				select {
				case <-ctx.Done():
					return
				case <-done:
				}
			}
		}
	}()
}

func (s *Service) refreshSMSRegistration(ctx context.Context) {
	lastActivity := time.Unix(0, s.lastCSActivity.Load())
	s.Logger.Printf("sms: no CS activity for %.0f min — sending SGs keepalive", time.Since(lastActivity).Minutes())
	if s.refreshSGsViaVoiceCall(ctx) {
		return
	}
	s.Logger.Printf("sms: voice call keepalive failed, falling back to CFUN=4/1 cycle")
	if !s.refreshSGsViaCFUN4(ctx) {
		s.Logger.Printf("sms: CFUN=4/1 refresh failed, falling back to radio cycle")
		s.refreshSGsViaRadioCycle(ctx)
	}
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
	s.startSMSWatch(s.durableContext(ctx))
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
	s.startSMSWatch(s.durableContext(ctx))
	s.Logger.Printf("sms: SGs refresh complete (radio cycle)")
}

func (s *Service) ensureModemEnabled(ctx context.Context) error {
	if s.Modem.IsModemPresent() {
		if err := s.Modem.CheckReadyState(); err == nil {
			s.Logger.Printf("Modem is already present and ready via D-Bus")
			return nil
		} else {
			s.Logger.Printf("Modem is present via D-Bus but not ready: %v", err)
			if s.hasMissingSIM() {
				s.Logger.Printf("Modem has no SIM; leaving it powered on without recovery")
				return nil
			}
		}
	}

	interfacePresent := modem.IsInterfacePresent(s.Config.Interface)
	if interfacePresent || s.Modem.IsModemPresent() {
		if interfacePresent {
			s.Logger.Printf("Modem interface %s is present, waiting for ModemManager...", s.Config.Interface)
		} else {
			s.Logger.Printf("Modem is present on D-Bus without interface %s, waiting until ready...", s.Config.Interface)
		}
		waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := s.Modem.WaitForModem(waitCtx, s.Config.Interface); err == nil {
			return nil
		}
		s.Logger.Printf("ModemManager did not register modem, proceeding to GPIO recovery")
	}

	s.Logger.Printf("Modem not detected, will attempt to enable via GPIO")

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
		if s.hasMissingSIM() {
			s.Logger.Printf("Modem has no SIM; leaving it powered on without further recovery")
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}

	s.Logger.Printf("SEVERE ERROR: Modem failed to come up after %d attempts with up to 5 minute wait times",
		health.MaxRecoveryAttempts)

	s.Health.State = health.StatePermanentFailure
	s.publishHealthState(ctx)

	s.Redis.RaiseFault(redisClient.FaultCodeModemRecoveryFailed, "Modem recovery failed")

	return fmt.Errorf("modem failed to come up after multiple attempts, marked as potentially defective")
}

func (s *Service) probeHealthError() error {
	if s.probeHealthErrorFn != nil {
		return s.probeHealthErrorFn()
	}
	if _, err := s.Modem.FindModem(); err != nil {
		return fmt.Errorf("modem not found: %w", err)
	}
	if err := s.Modem.CheckPrimaryPort(); err != nil {
		return fmt.Errorf("primary port: %w", err)
	}
	if err := s.Modem.CheckPowerState(); err != nil {
		return fmt.Errorf("power state: %w", err)
	}
	if err := s.Modem.CheckReadyState(); err != nil {
		return s.modemReadinessHealthError(err)
	}
	return nil
}

func (s *Service) modemReadinessHealthError(err error) error {
	if err == nil || s.hasMissingSIM() {
		return nil
	}
	return fmt.Errorf("modem state: %w", err)
}

func (s *Service) probeHealth() bool {
	return s.probeHealthError() == nil
}

func (s *Service) recoverySucceeded(ctx context.Context, strategy string) {
	s.Logger.Printf("Modem recovery successful via %s", strategy)
	s.Health.MarkNormal()
	s.GPSRecoveryCount = 0
	s.resetGPSAfterModemRecovery()
	s.publishHealthState(ctx)
	s.Redis.ClearFault(redisClient.FaultCodeModemUnavailable)
	s.Redis.ClearFault(redisClient.FaultCodeModemRecoveryFailed)

	// The modem's D-Bus path can change across a reset; re-arm the inbound-SMS
	// watch on the new path (and drain anything that queued meanwhile).
	s.startSMSWatch(s.durableContext(ctx))
}

func (s *Service) checkHealth(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	probeErr := s.probeHealthError()
	if probeErr != nil {
		if s.Health.IsTerminal() {
			return fmt.Errorf("modem in terminal state: %s", s.Health.State)
		}
		return s.handleModemFailure(ctx, fmt.Sprintf("probe_failed: %v", probeErr))
	}

	s.Health.MarkNormal()
	if s.hasMissingSIM() {
		return nil
	}
	s.Redis.ClearFault(redisClient.FaultCodeModemUnavailable)
	s.Redis.ClearFault(redisClient.FaultCodeModemRecoveryFailed)
	return nil
}

func (s *Service) raiseFault(code int, description string) {
	if s.raiseFaultFn != nil {
		s.raiseFaultFn(code, description)
		return
	}
	s.Redis.RaiseFault(code, description)
}

func (s *Service) handleModemFailure(ctx context.Context, reason string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.Logger.Printf("Modem failure detected: %s", reason)
	s.raiseFault(redisClient.FaultCodeModemUnavailable, "Modem unavailable: "+reason)

	if !s.recoveryRunMu.TryLock() {
		return fmt.Errorf("recovery in progress")
	}
	defer s.recoveryRunMu.Unlock()
	if !s.modemEnabled.Load() {
		return fmt.Errorf("modem disabled")
	}

	recoveryCtx, finish := s.startModemOperation(ctx)
	defer finish()

	// Exhausted retries: publish terminal Wait state, back off 2 minutes,
	// then reset and allow recovery to try again on the next failure.
	// StatePermanentFailure is reached only from ensureModemEnabled when
	// the modem never responded at all.
	if !s.Health.CanRecover() {
		s.Health.MarkRecoveryFailed()
		s.publishHealthState(ctx)
		s.raiseFault(redisClient.FaultCodeModemRecoveryFailed,
			fmt.Sprintf("Max recovery attempts (%d) exhausted, entering recovery-failed-wait", health.MaxRecoveryAttempts))
		s.Logger.Printf("Max recovery attempts reached, entering %s state", s.Health.State)
		err := s.completeRecoveryBackoff(ctx, recoveryCtx)
		if err == nil {
			s.Logger.Printf("Recovery-failed-wait expired, will retry on next failure")
		}
		return err
	}

	return s.attemptRecovery(recoveryCtx)
}

func (s *Service) completeRecoveryBackoff(publishCtx, waitCtx context.Context) error {
	var err error
	if s.recoveryBackoffFn != nil {
		err = s.recoveryBackoffFn(waitCtx)
	} else {
		select {
		case <-waitCtx.Done():
			err = waitCtx.Err()
		case <-time.After(2 * time.Minute):
		}
	}
	s.Health.MarkNormal()
	s.publishHealthState(publishCtx)
	return err
}

func (s *Service) attemptRecovery(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.Health.StartRecovery()
	defer func() {
		if s.Health.FinishRecoveryAttempt() {
			s.publishHealthState(ctx)
		}
	}()

	s.Logger.Printf("Attempting modem recovery (attempt %d/%d)",
		s.Health.RecoveryAttempts, health.MaxRecoveryAttempts)

	s.publishHealthState(ctx)

	// Avoid restarting an external reset already in progress.
	stabilizationWaited := false
	_, findErr := s.Modem.FindModem()
	waitReason := findErr
	if findErr == nil {
		waitReason = s.Modem.CheckReadyState()
	}
	if waitReason != nil {
		stabilizationWaited = true
		s.Logger.Printf("Modem is unavailable (%v); waiting up to %v for it to stabilize", waitReason, health.RecoveryWaitTime)
		waitCtx, waitCancel := context.WithTimeout(ctx, health.RecoveryWaitTime)
		waitErr := s.Modem.WaitForModem(waitCtx, s.Config.Interface)
		waitCancel()
		if waitErr == nil && s.probeHealth() {
			s.recoverySucceeded(ctx, "MM stabilization wait")
			return nil
		}
		if waitErr != nil {
			s.Logger.Printf("Modem did not stabilize on its own: %v", waitErr)
		}
	}

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

	s.Logger.Printf("Attempting USB recovery (unbind/bind)...")
	if err := s.Modem.RecoverUSB(); err != nil {
		if errors.Is(err, usb.ErrDeviceNotPresent) && !stabilizationWaited {
			s.Logger.Printf("USB device not present, waiting up to %v for modem to reappear on D-Bus", health.RecoveryWaitTime)
			waitCtx, waitCancel := context.WithTimeout(ctx, health.RecoveryWaitTime)
			err := s.Modem.WaitForModem(waitCtx, s.Config.Interface)
			waitCancel()
			if err == nil && s.probeHealth() {
				s.recoverySucceeded(ctx, "MM rebind wait")
				return nil
			}
		} else if !errors.Is(err, usb.ErrDeviceNotPresent) {
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
	if s.publishFn != nil {
		return s.publishFn("modem-health", s.Health.State)
	}
	return s.Redis.PublishInternetState("modem-health", s.Health.State)
}

func (s *Service) clearGPSFault() {
	if !s.gpsFaultActive.CompareAndSwap(true, false) {
		return
	}
	if err := s.Redis.ClearFault(redisClient.FaultCodeGPSUnavailable); err != nil {
		s.gpsFaultActive.Store(true)
	}
}

func (s *Service) handleGPSFailure(ctx context.Context, gpsErr error) error {
	// GPS silence can be the first sign of a whole-modem reset.
	if modemErr := s.probeHealthError(); modemErr != nil {
		s.Logger.Printf("GPS recovery deferred because modem is unavailable: %v", modemErr)
		return s.handleModemFailure(ctx, fmt.Sprintf("modem_unavailable_during_gps_failure: %v; gps: %v", modemErr, gpsErr))
	}

	s.Logger.Printf("Attempting GPS-specific recovery for: %v", gpsErr)
	if err := s.Redis.RaiseFault(redisClient.FaultCodeGPSUnavailable, "GPS unavailable: "+gpsErr.Error()); err == nil {
		s.gpsFaultActive.Store(true)
	}

	if err := s.attemptGPSRecovery(gpsErr); err != nil {
		s.Logger.Printf("GPS-specific recovery failed: %v", err)
		if recoveryErr := s.handleModemFailure(ctx, fmt.Sprintf("gps_stuck_after_gps_recovery: %v", gpsErr)); recoveryErr != nil {
			return fmt.Errorf("both GPS and modem recovery failed: %v", recoveryErr)
		}
	}

	return nil
}

func (s *Service) attemptGPSRecovery(trigger error) error {
	if !s.modemEnabled.Load() {
		s.Logger.Printf("GPS recovery skipped, modem is disabled")
		return nil
	}

	s.gpsRecoveryMutex.Lock()
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

		if err := s.Location.StopGPSD(); err != nil {
			s.Logger.Printf("Warning: Failed to stop gpsd: %v", err)
		}
		s.Location.Close()

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

	s.Logger.Printf("Stopping gpsd service before GPS reset...")
	if err := s.Location.StopGPSD(); err != nil {
		s.Logger.Printf("Warning: Failed to stop gpsd: %v", err)
	}

	// Close existing GPS connection; gate the monitor for 2 seconds so
	// it doesn't re-enable while gpsd is still tearing down.
	s.Location.Close()
	s.gpsRecoveryMutex.Lock()
	s.gpsRecoveryUntil = time.Now().Add(2 * time.Second)
	s.gpsRecoveryMutex.Unlock()

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

// UE-based GPS is disabled because this firmware accepts SUPL configuration
// but then emits no NMEA data or fix for about ten minutes. XTRA assistance
// also fails in the receiver, so standalone is the reliable live invariant.
const enableUEBasedMode = false

func (s *Service) requestGPSModeForConnectivity(ctx context.Context, conn connectivity.State) {
	if ctx.Err() != nil || !s.Location.ReadyForModeSwitch() {
		return
	}
	var desired location.GPSMode
	switch {
	case enableUEBasedMode && conn == connectivity.Connected:
		desired = location.ModeUEBased
	default:
		desired = location.ModeStandalone
	}

	prev := s.Location.CurrentGPSMode()
	s.Logger.Printf("gps-transition request from=%s to=%s connectivity=%s", prev, desired, conn)

	if err := s.Location.SetGPSMode(ctx, desired); err != nil {
		s.Logger.Printf("Failed to switch GPS to %s mode: %v", desired, err)
		return
	}
	s.publishGPSMode()
}

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

// publishModemState publishes modem details and derived internet state.
func (s *Service) publishModemState(ctx context.Context, currentState *modem.State, internetStatus string) error {
	publishInternet := s.publishFn
	if publishInternet == nil {
		publishInternet = s.Redis.PublishInternetState
	}
	publishModem := s.publishModemFn
	if publishModem == nil {
		publishModem = s.Redis.PublishModemState
	}
	var internetChanges, modemChanges []string

	if s.LastState.Status != internetStatus {
		if err := publishInternet("status", internetStatus); err != nil {
			return err
		}
		internetChanges = append(internetChanges, fmt.Sprintf("status=%s", internetStatus))
		s.LastState.Status = internetStatus
	}

	if s.LastState.LastRawModemStatus != currentState.Status {
		if err := publishInternet("modem-state", currentState.Status); err != nil {
			return err
		}
		internetChanges = append(internetChanges, fmt.Sprintf("modem-state=%s", currentState.Status))
		s.LastState.LastRawModemStatus = currentState.Status
	}

	if s.LastState.IfIPAddr != currentState.IfIPAddr {
		if err := publishInternet("ip-address", currentState.IfIPAddr); err != nil {
			return err
		}
		internetChanges = append(internetChanges, fmt.Sprintf("ip=%s", currentState.IfIPAddr))
		s.LastState.IfIPAddr = currentState.IfIPAddr
	}

	if s.LastState.AccessTech != currentState.AccessTech {
		if err := publishInternet("access-tech", currentState.AccessTech); err != nil {
			return err
		}
		internetChanges = append(internetChanges, fmt.Sprintf("tech=%s", currentState.AccessTech))
		s.LastState.AccessTech = currentState.AccessTech
	}

	if s.LastState.SignalQuality != currentState.SignalQuality {
		if err := publishInternet("signal-quality", fmt.Sprintf("%d", currentState.SignalQuality)); err != nil {
			return err
		}
		internetChanges = append(internetChanges, fmt.Sprintf("signal=%d", currentState.SignalQuality))
		s.LastState.SignalQuality = currentState.SignalQuality
	}

	if s.LastState.IMEI != currentState.IMEI {
		if err := publishInternet("sim-imei", currentState.IMEI); err != nil {
			return err
		}
		internetChanges = append(internetChanges, fmt.Sprintf("imei=%s", currentState.IMEI))
		s.LastState.IMEI = currentState.IMEI
	}

	if s.LastState.IMSI != currentState.IMSI {
		if err := publishInternet("sim-imsi", currentState.IMSI); err != nil {
			return err
		}
		internetChanges = append(internetChanges, fmt.Sprintf("imsi=%s", currentState.IMSI))
		s.LastState.IMSI = currentState.IMSI
	}

	if s.LastState.ICCID != currentState.ICCID {
		if err := publishInternet("sim-iccid", currentState.ICCID); err != nil {
			return err
		}
		internetChanges = append(internetChanges, fmt.Sprintf("iccid=%s", currentState.ICCID))
		s.LastState.ICCID = currentState.ICCID
	}

	if s.LastState.PowerState != currentState.PowerState {
		if err := publishModem("power-state", currentState.PowerState); err != nil {
			return err
		}
		modemChanges = append(modemChanges, fmt.Sprintf("power=%s", currentState.PowerState))
		s.LastState.PowerState = currentState.PowerState
	}

	if s.LastState.SIMState != currentState.SIMState {
		if err := publishModem("sim-state", currentState.SIMState); err != nil {
			return err
		}
		modemChanges = append(modemChanges, fmt.Sprintf("sim=%s", currentState.SIMState))
		s.LastState.SIMState = currentState.SIMState
	}

	if s.LastState.SIMLockStatus != currentState.SIMLockStatus {
		if err := publishModem("sim-lock", currentState.SIMLockStatus); err != nil {
			return err
		}
		modemChanges = append(modemChanges, fmt.Sprintf("sim-lock=%s", currentState.SIMLockStatus))
		s.LastState.SIMLockStatus = currentState.SIMLockStatus
	}

	if s.LastState.PinAction != currentState.PinAction {
		if err := publishModem("pin-action", currentState.PinAction); err != nil {
			return err
		}
		modemChanges = append(modemChanges, fmt.Sprintf("pin-action=%s", currentState.PinAction))
		s.LastState.PinAction = currentState.PinAction
	}

	if s.LastState.ApnAction != currentState.ApnAction {
		if err := publishModem("apn-action", currentState.ApnAction); err != nil {
			return err
		}
		modemChanges = append(modemChanges, fmt.Sprintf("apn-action=%s", currentState.ApnAction))
		s.LastState.ApnAction = currentState.ApnAction
	}

	if s.LastState.OperatorName != currentState.OperatorName {
		if err := publishModem("operator-name", currentState.OperatorName); err != nil {
			return err
		}
		modemChanges = append(modemChanges, fmt.Sprintf("operator=%s", currentState.OperatorName))
		s.LastState.OperatorName = currentState.OperatorName
	}

	if s.LastState.OperatorCode != currentState.OperatorCode {
		if err := publishModem("operator-code", currentState.OperatorCode); err != nil {
			return err
		}
		modemChanges = append(modemChanges, fmt.Sprintf("mcc-mnc=%s", currentState.OperatorCode))
		s.LastState.OperatorCode = currentState.OperatorCode
	}

	if s.LastState.IsRoaming != currentState.IsRoaming {
		if err := publishModem("is-roaming", fmt.Sprintf("%t", currentState.IsRoaming)); err != nil {
			return err
		}
		modemChanges = append(modemChanges, fmt.Sprintf("roaming=%t", currentState.IsRoaming))
		s.LastState.IsRoaming = currentState.IsRoaming
	}

	if s.LastState.Registration != currentState.Registration {
		if err := publishModem("registration", currentState.Registration); err != nil {
			return err
		}
		modemChanges = append(modemChanges, fmt.Sprintf("reg=%s", currentState.Registration))
		s.LastState.Registration = currentState.Registration
	}

	if s.LastState.RegistrationFail != currentState.RegistrationFail {
		if err := publishModem("registration-fail", currentState.RegistrationFail); err != nil {
			return err
		}
		modemChanges = append(modemChanges, fmt.Sprintf("reg-fail=%s", currentState.RegistrationFail))
		s.LastState.RegistrationFail = currentState.RegistrationFail
	}

	if s.LastState.ErrorState != currentState.ErrorState {
		if err := publishModem("error-state", currentState.ErrorState); err != nil {
			return err
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
		if err := publishInternet("connectivity", string(conn)); err != nil {
			return err
		}
		internetChanges = append(internetChanges, fmt.Sprintf("connectivity=%s", conn))
		s.lastPubConn = conn
		s.requestGPSModeForConnectivity(ctx, conn)
	}

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
		"speed":     fmt.Sprintf("%.6f", loc.Speed*3.6), // m/s to km/h
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
	s.publishClockSync(t)
	return true
}

// publishClockSync records a GPS clock validation for low-power consumers.
func (s *Service) publishClockSync(at time.Time) {
	if err := s.Redis.PublishClockSync("gps", at); err != nil {
		s.Logger.Printf("Failed to publish clock sync state: %v", err)
	}
}

// nextClockOffsetCount advances the consecutive gross-disagreement counter. A
// clock that has never been synced steps on the first gross offset; afterwards
// it takes clockStepConfirmations consecutive samples.
func nextClockOffsetCount(previous int, offset time.Duration, synced bool) (int, bool) {
	if offset.Abs() <= clockStepTolerance {
		return 0, false
	}
	if !synced {
		return 0, true
	}
	previous++
	return previous, previous >= clockStepConfirmations
}

// validateClock steps the system clock when a fresh GPS fix persistently
// disagrees with it beyond clockStepTolerance. Runs online and offline, so an
// already-synced clock is re-validated instead of trusted indefinitely.
func (s *Service) validateClock() {
	now := s.clock()
	if !s.lastClockCheck.IsZero() && now.Sub(s.lastClockCheck) < clockValidationInterval {
		return
	}
	s.lastClockCheck = now

	gpsTime := s.Location.CurrentLoc().Timestamp
	if gpsTime.IsZero() || gpsTime.Before(location.MinValidGPSDate) {
		return
	}

	offset := gpsTime.Sub(now)
	count, step := nextClockOffsetCount(s.clockOffsetCount, offset, !s.lastClockSync.IsZero())
	s.clockOffsetCount = count
	if !step {
		if offset.Abs() <= clockStepTolerance && s.lastClockSync.IsZero() {
			s.lastClockSync = now
			s.publishClockSync(gpsTime)
			return
		}
		if offset.Abs() > clockStepTolerance {
			s.Logger.Printf("GPS clock offset %s, confirming (%d/%d)",
				offset.Round(time.Second), s.clockOffsetCount, clockStepConfirmations)
		}
		return
	}

	if !s.syncClockFromGPS(gpsTime) {
		return
	}
	s.lastClockSync = now
	s.clockOffsetCount = 0
	s.Logger.Printf("System clock stepped from GPS, offset was %s", offset.Round(time.Second))
}

// reachabilityField separates a silent restricted APN from a broken local path.
func reachabilityField(reachable bool, a link.Assessment) string {
	if reachable {
		return "ok"
	}
	if a.Healthy {
		return "unreachable"
	}
	return "no-path"
}

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

// handleAssessment applies a cooldown-limited remedy. Remote probe results
// are deliberately excluded because a restricted APN is not a modem fault.
func (s *Service) handleAssessment(a link.Assessment) {
	if a.Healthy || a.Remedy == link.RemedyNone {
		s.pendingRepeat = 0
		return
	}

	// Require repeated evidence through handoffs and NetworkManager bring-up.
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

// assignedResolvers combines all available network resolver sources.
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
		s.publishModemState(ctx, modem.NewState(), "disconnected")
		s.publishHealthState(ctx)
		return err
	}

	currentState, err := s.getModemInfo()
	if err != nil {
		s.Logger.Printf("Failed to get modem info: %v", err)
		// Preserve ErrorState from the partial snapshot.
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

	// Only these local layers may trigger modem action.
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

	// Back off remote probes; local assessment changes still force one immediately.
	now := s.clock()
	assessmentChanged := assessment != s.lastAssessment
	// Skipped when the local stack is already known broken: reachabilityField
	// reports no-path from the assessment alone in that case, so the probe
	// would buy nothing and can block the tick for many seconds.
	if !assessment.Healthy {
		// A prior success cannot remain valid across a known local fault.
		s.lastProbe = health.Result{Detail: "not probed: " + linkLayerField(assessment)}
	}
	if assessment.Healthy && (now.After(s.nextProbeAt) || assessmentChanged) {
		s.lastProbe = s.prober.Probe(ctx, s.assignedResolvers())
		// Failure also backs off because silence is valid on restricted APNs.
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

	// Change-gate diagnostics to avoid waking subscribers every tick.
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
	// Allow ModemManager time to expose a whole-modem reset before GPS recovery.
	gpsNoDataTimeout = 15 * time.Second
)

func formatHumanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	hours := d / time.Hour
	minutes := d % time.Hour / time.Minute
	seconds := d % time.Minute / time.Second

	if hours > 0 {
		return fmt.Sprintf("%dh %dm %ds", hours, minutes, seconds)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm %ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}

func (s *Service) checkGPSHealth() error {
	if s.Location.IsConfiguring() {
		return nil
	}

	now := time.Now()

	lastData := s.Location.LastDataReceived()
	if !lastData.IsZero() && now.Sub(lastData) > gpsNoDataTimeout {
		return fmt.Errorf("gps_no_data: no GPS stanzas received for %v", now.Sub(lastData))
	}
	if lastData.IsZero() && !s.GPSEnabledTime.IsZero() && now.Sub(s.GPSEnabledTime) > gpsNoDataTimeout {
		return fmt.Errorf("gps_no_data: no GPS stanzas received since GPS was enabled")
	}

	// A receiver can search indefinitely indoors. Fresh TPV/SKY reports mean
	// the GPS pipeline is alive even when RF conditions cannot produce a fix.
	return nil
}

func (s *Service) reconcileModemTarget(ctx context.Context, applied *bool) bool {
	if !s.modemEnabled.Load() {
		*applied = false
		s.runDisableModem(ctx)
		return false
	}
	if *applied {
		return false
	}
	opCtx, finish := s.startModemOperation(ctx)
	err := s.runEnsureModemEnabled(opCtx)
	finish()
	if err != nil {
		s.Logger.Printf("Failed to enable modem: %v", err)
		return false
	}
	*applied = true
	return true
}

func (s *Service) monitorStatus(ctx context.Context) {
	defer close(s.monitorDone)
	ticker := time.NewTicker(s.Config.InternetCheckTime)
	gpsTimer := time.NewTicker(location.GPSUpdateInterval)
	cellTimer := time.NewTicker(location.CellLocationUpdateInterval)
	defer ticker.Stop()
	defer gpsTimer.Stop()
	defer cellTimer.Stop()

	type modemEvent struct {
		kind string
		path dbus.ObjectPath
	}
	modemEvents := make(chan modemEvent, 1)
	notifyModem := func(event modemEvent) {
		select {
		case modemEvents <- event:
		default:
		}
	}
	if err := s.MMClient.WatchModems(ctx,
		func(path dbus.ObjectPath) { notifyModem(modemEvent{kind: "added", path: path}) },
		func(path dbus.ObjectPath) { notifyModem(modemEvent{kind: "removed", path: path}) },
	); err != nil {
		s.Logger.Printf("Failed to watch ModemManager events: %v", err)
	}

	appliedModemEnabled := s.modemEnabled.Load()
	if appliedModemEnabled {
		if err := s.checkAndPublishModemStatus(ctx); err != nil {
			s.Logger.Printf("Initial modem status check failed: %v", err)
		}
	} else {
		s.runDisableModem(ctx)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case done := <-s.smsRefreshRequest:
			if s.modemEnabled.Load() {
				refreshCtx, finish := s.startModemOperation(ctx)
				s.refreshSMSRegistration(refreshCtx)
				finish()
			}
			close(done)
		case <-s.modemStateChange:
			if !s.reconcileModemTarget(ctx, &appliedModemEnabled) {
				continue
			}
			if err := s.checkAndPublishModemStatus(ctx); err != nil {
				s.Logger.Printf("Post-enable modem status check failed: %v", err)
			}
		case event := <-modemEvents:
			if !s.modemEnabled.Load() {
				continue
			}
			s.Logger.Printf("ModemManager modem %s: %s", event.kind, event.path)
			if err := s.checkAndPublishModemStatus(ctx); err != nil {
				s.Logger.Printf("Event-driven modem status check failed: %v", err)
			}
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

				s.gpsRecoveryMutex.Lock()
				recoveryInProgress := s.gpsRecoveryInProgress
				gatedUntil := s.gpsRecoveryUntil
				s.gpsRecoveryMutex.Unlock()

				if recoveryInProgress || time.Now().Before(gatedUntil) {
					continue
				}

				if !s.Location.IsEnabled() {
					if err := s.Location.EnableGPS(modemPath); err != nil {
						s.Logger.Printf("Failed to enable GPS: %v", err)
						continue
					}
					s.GPSEnabledTime = time.Now()
				}

				if err := s.checkGPSHealth(); err != nil {
					s.Logger.Printf("GPS health check failed: %v", err)
					if recoveryErr := s.handleGPSFailure(ctx, err); recoveryErr != nil {
						s.Logger.Printf("GPS recovery failed: %v", recoveryErr)
					}
					continue
				}
				if !s.Location.LastDataReceived().IsZero() {
					s.clearGPSFault()
				}

				gpsStatus := s.Location.GetGPSStatus()
				hasValidFix, _ := gpsStatus["active"].(bool)

				hasInternet := s.LastState.Status == "connected"
				publishRecovery := false

				if hasValidFix {
					s.validateClock()

					publishRecovery = s.Location.ShouldPublishRecovery(hasInternet)
					if publishRecovery {
						s.Location.SetGPSFreshInit(false)
					}

					s.Location.GPSLostTime = time.Time{}

					if s.WaitingForGPSLogged {
						s.Logger.Printf("GPS fix established")
						s.WaitingForGPSLogged = false
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
					if s.Location.GPSLostTime.IsZero() {
						s.Location.GPSLostTime = time.Now()
					}

					if !s.WaitingForGPSLogged {
						s.Logger.Printf("Waiting for valid GPS fix...")
						s.WaitingForGPSLogged = true
						// Read mode at fix time because startup probing may still change it.
						s.ttffStart = time.Now()
					}

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

// Package apn reconciles the modem's LTE attach context and NetworkManager profile.
package apn

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"modem-service/internal/mm"
)

const (
	atQuickTimeout     = 5 * time.Second
	atDeregisterTime   = 10 * time.Second
	atAutoRegisterMax  = 60 * time.Second
	transactionTimeout = 3 * time.Minute
	rollbackTimeout    = 90 * time.Second
	maxApplyAttempts   = 3
	retryDelay         = 30 * time.Second
)

type Outcome string

const (
	OutcomeNoSIM             Outcome = "no-sim"
	OutcomeUnconfigured      Outcome = "unconfigured"
	OutcomeOK                Outcome = "ok"
	OutcomeApplied           Outcome = "applied"
	OutcomeICCIDChangedClear Outcome = "iccid-changed-cleared"
	OutcomeError             Outcome = "error"
)

type Config struct {
	APN      string
	Username string
	Password string
	Auth     string
}

func (c Config) IsEmpty() bool {
	return c.APN == "" && c.Username == "" && c.Password == "" && (c.Auth == "" || c.Auth == "none")
}

func (c Config) EqualNM(o Config) bool {
	return c.APN == o.APN && c.Username == o.Username && c.Password == o.Password
}

func (c Config) Equal(o Config) bool {
	a, b := c.Auth, o.Auth
	if a == "" {
		a = "none"
	}
	if b == "" {
		b = "none"
	}
	return c.EqualNM(o) && a == b
}

type Input struct {
	ICCID     string
	ModemPath dbus.ObjectPath
	Desired   Config
}

type MMDBus interface {
	GetInitialEpsBearerSettingsContext(context.Context, dbus.ObjectPath) (map[string]dbus.Variant, error)
	SetInitialEpsBearerSettingsContext(context.Context, dbus.ObjectPath, map[string]dbus.Variant) error
	SendCommandContext(context.Context, dbus.ObjectPath, string, time.Duration) (string, error)
}

type NM interface {
	GetGSM(context.Context, string) (Config, error)
	SetGSM(context.Context, string, Config) error
	State(context.Context, string) (ConnectionState, error)
	SetAutoconnect(context.Context, string, bool) error
	Disconnect(context.Context, string) error
	Connect(context.Context, string) error
	Commit(context.Context, string, bool) error
}

type attachContext struct {
	Present bool
	PDPType string
	APN     string
}

func desiredContext(cfg Config) attachContext {
	pdp := "IP"
	if cfg.APN == "" {
		pdp = "IPV4V6"
	}
	return attachContext{Present: true, PDPType: pdp, APN: cfg.APN}
}

type snapshot struct {
	nm  Config
	mm  map[string]dbus.Variant
	cid attachContext
}

type pendingRollback struct {
	before snapshot
	state  ConnectionState
	iccid  string
}

type Manager struct {
	mm                MMDBus
	nm                NM
	logger            *log.Logger
	connection        string
	mu                sync.Mutex
	observer          OperationObserver
	operation         *OperationStatus
	pending           *pendingRollback
	lastAppliedICCID  string
	lastAppliedPath   dbus.ObjectPath
	lastAppliedConfig Config
	lastAppliedCID    attachContext
	blockedICCID      string
	blockedConfig     Config
	retryInput        Input
	attempts          int
	retryAt           time.Time
	now               func() time.Time
}

func New(mmClient MMDBus, nm NM, connection string, logger *log.Logger) *Manager {
	if logger == nil {
		logger = log.Default()
	}
	return &Manager{mm: mmClient, nm: nm, logger: logger, connection: connection, now: time.Now}
}

// ResetRetry allows a fresh bounded attempt sequence after modem recovery.
func (m *Manager) ResetRetry() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attempts = 0
	m.retryAt = time.Time{}
	m.lastAppliedPath = ""
}

func (m *Manager) Reconcile(ctx context.Context, in Input) (outcome Outcome) {
	if in.ICCID == "" {
		return OutcomeNoSIM
	}
	if !m.mu.TryLock() {
		return OutcomeError
	}
	defer m.mu.Unlock()
	if ctx.Err() != nil || in.ModemPath == "" {
		return OutcomeError
	}
	if err := validateConfig(in.Desired); err != nil {
		m.logger.Printf("apn: invalid configuration: %v", err)
		return OutcomeError
	}
	if in.ICCID != m.retryInput.ICCID || in.ModemPath != m.retryInput.ModemPath || !in.Desired.Equal(m.retryInput.Desired) {
		m.retryInput, m.attempts, m.retryAt = in, 0, time.Time{}
	}

	clearSIM := m.lastAppliedICCID != "" && m.lastAppliedICCID != in.ICCID
	cfg := in.Desired
	if clearSIM {
		cfg = Config{}
	} else if m.blockedICCID == in.ICCID {
		if m.blockedConfig.Equal(in.Desired) {
			return OutcomeUnconfigured
		}
		m.blockedICCID = ""
	}

	ctx, cancel := context.WithTimeout(ctx, transactionTimeout)
	defer cancel()
	if err := m.beginOperation(ctx, in, cfg); err != nil {
		m.logger.Printf("apn: transaction status unavailable")
		return OutcomeError
	}
	defer func() {
		if err := m.finishOperation(ctx, outcome); err != nil {
			m.logger.Printf("apn: transaction completion status unavailable")
			outcome = OutcomeError
		}
	}()
	before, err := m.readCurrent(ctx, in.ModemPath)
	if err != nil {
		m.logger.Printf("apn: read current failed: %v", err)
		return OutcomeError
	}
	mmCfg := parseMMSettings(before.mm)
	verified := m.lastAppliedICCID == in.ICCID && m.lastAppliedPath == in.ModemPath && m.lastAppliedConfig.Equal(cfg)
	cidMatches := before.cid == desiredContext(cfg) || cfg.IsEmpty() && (before.cid.APN == "" || verified && before.cid == m.lastAppliedCID)
	if m.attempts == 0 && m.pending == nil && before.nm.EqualNM(cfg) && cidMatches && (mmCfg.Equal(cfg) || verified) {
		m.lastAppliedCID = before.cid
		m.recordSuccess(in, cfg)
		if clearSIM {
			m.blockedICCID, m.blockedConfig = in.ICCID, in.Desired
			return OutcomeICCIDChangedClear
		}
		if cfg.IsEmpty() {
			return OutcomeUnconfigured
		}
		return OutcomeOK
	}
	if m.attempts >= maxApplyAttempts || m.now().Before(m.retryAt) {
		return OutcomeError
	}

	if err := m.observeOperation(ctx, PhaseApplying); err != nil {
		m.logger.Printf("apn: transaction status unavailable")
		return OutcomeError
	}
	m.logger.Printf("apn: reconciling NM profile and CID1 (attempt %d/%d)", m.attempts+1, maxApplyAttempts)
	if err := m.apply(ctx, in, cfg, before, clearSIM); err != nil {
		m.attempts++
		m.retryAt = m.now().Add(retryDelay << (m.attempts - 1))
		m.logger.Printf("apn: transaction failed: %v", err)
		return OutcomeError
	}
	m.recordSuccess(in, cfg)
	if clearSIM {
		m.blockedICCID, m.blockedConfig = in.ICCID, in.Desired
		return OutcomeICCIDChangedClear
	}
	return OutcomeApplied
}

func (m *Manager) recordSuccess(in Input, cfg Config) {
	m.lastAppliedICCID, m.lastAppliedPath, m.lastAppliedConfig = in.ICCID, in.ModemPath, cfg
	m.attempts, m.retryAt = 0, time.Time{}
}

func (m *Manager) readCurrent(ctx context.Context, path dbus.ObjectPath) (snapshot, error) {
	var s snapshot
	var err error
	if s.nm, err = m.nm.GetGSM(ctx, m.connection); err != nil {
		return s, fmt.Errorf("NM read: %w", err)
	}
	queryCtx, cancel := context.WithTimeout(ctx, atQuickTimeout)
	s.mm, err = m.mm.GetInitialEpsBearerSettingsContext(queryCtx, path)
	cancel()
	if err != nil {
		return s, fmt.Errorf("MM read: %w", err)
	}
	s.cid, err = m.readCID(ctx, path)
	return s, err
}

func (m *Manager) readCID(ctx context.Context, path dbus.ObjectPath) (attachContext, error) {
	response, err := m.mm.SendCommandContext(ctx, path, "AT+CGDCONT?", atQuickTimeout)
	if err != nil {
		return attachContext{}, fmt.Errorf("CID1 read: %w", err)
	}
	return parseCID1(response)
}

func parseCID1(response string) (attachContext, error) {
	var result attachContext
	for _, line := range strings.Split(response, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "+CGDCONT:") {
			continue
		}
		r := csv.NewReader(strings.NewReader(strings.TrimSpace(strings.TrimPrefix(line, "+CGDCONT:"))))
		r.TrimLeadingSpace = true
		fields, err := r.Read()
		if err != nil || len(fields) < 3 {
			return result, errors.New("malformed CGDCONT response")
		}
		if strings.TrimSpace(fields[0]) != "1" {
			continue
		}
		if result.Present {
			return result, errors.New("duplicate CID1 in CGDCONT response")
		}
		result = attachContext{Present: true, PDPType: strings.ToUpper(fields[1]), APN: fields[2]}
		if err := validateATValue(result.APN); err != nil {
			return attachContext{}, errors.New("invalid CID1 APN")
		}
		switch result.PDPType {
		case "IP", "IPV6", "IPV4V6", "PPP":
		default:
			return attachContext{}, errors.New("unsupported CID1 PDP type")
		}
	}
	return result, nil
}

func (m *Manager) setMM(ctx context.Context, path dbus.ObjectPath, settings map[string]dbus.Variant) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return m.mm.SetInitialEpsBearerSettingsContext(ctx, path, settings)
}

func (m *Manager) apply(ctx context.Context, in Input, cfg Config, before snapshot, clearSIM bool) (err error) {
	path := in.ModemPath
	state, err := m.nm.State(ctx, m.connection)
	if err != nil {
		return fmt.Errorf("NM state: %w", err)
	}
	changed, detached := m.pending != nil, m.pending != nil
	if m.pending != nil {
		state = m.pending.state
		if m.pending.iccid == in.ICCID {
			before = m.pending.before
		} else {
			clearSIM = true
		}
	}
	if clearSIM {
		// A SIM swap must never roll back to the previous SIM's credentials.
		before = snapshot{nm: Config{}, mm: encodeMMSettings(Config{}), cid: desiredContext(Config{})}
		changed = true
	}
	if err := validateConfig(parseMMSettings(before.mm)); err != nil {
		return errors.New("cannot safely restore prior authentication settings")
	}
	m.pending = &pendingRollback{before: before, state: state, iccid: in.ICCID}
	defer func() {
		if err != nil {
			limit := rollbackTimeout
			if ctx.Err() != nil {
				limit = 30 * time.Second
			}
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), limit)
			defer cancel()
			if statusErr := m.observeOperation(cleanupCtx, PhaseRollingBack); statusErr != nil {
				m.logger.Printf("apn: rollback status unavailable")
			}
			if rollbackErr := m.rollback(cleanupCtx, ctx, path, before, state, changed, detached); rollbackErr != nil {
				err = errors.Join(err, fmt.Errorf("rollback: %w", rollbackErr))
			} else {
				m.pending = nil
			}
		}
	}()
	// Block NM's own profile retry while the modem is deregistered.
	if err = m.nm.SetAutoconnect(ctx, m.connection, false); err != nil {
		return fmt.Errorf("NM suspend: %w", err)
	}
	detached = true
	if err = m.nm.Disconnect(ctx, m.connection); err != nil {
		return fmt.Errorf("NM disconnect: %w", err)
	}
	if _, err = m.mm.SendCommandContext(ctx, path, "AT+COPS=2", atDeregisterTime); err != nil {
		return fmt.Errorf("deregister: %w", err)
	}
	changed = true
	if err = m.nm.SetGSM(ctx, m.connection, cfg); err != nil {
		return fmt.Errorf("NM write: %w", err)
	}
	if mmErr := m.setMM(ctx, path, encodeMMSettings(cfg)); mmErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// CID1 is authoritative when the plugin cannot update its attach settings.
		m.logger.Printf("apn: MM attach settings unavailable; using CID1 fallback")
	}
	if err = m.writeCID(ctx, path, desiredContext(cfg)); err != nil {
		return err
	}
	if err = m.writeAuth(ctx, path, cfg); err != nil {
		return err
	}
	actual, err := m.readCID(ctx, path)
	if err != nil {
		return err
	}
	if actual != desiredContext(cfg) {
		return errors.New("CID1 readback mismatch")
	}
	nmCfg, err := m.nm.GetGSM(ctx, m.connection)
	if err != nil {
		return fmt.Errorf("NM readback: %w", err)
	}
	if !nmCfg.EqualNM(cfg) {
		return errors.New("NM readback mismatch")
	}
	if _, err = m.mm.SendCommandContext(ctx, path, "AT+COPS=0", atAutoRegisterMax); err != nil {
		return fmt.Errorf("reattach: %w", err)
	}
	if err = m.nm.Connect(ctx, m.connection); err != nil {
		return fmt.Errorf("NM connect: %w", err)
	}
	// NM activation can rewrite the modem's contexts too.
	actual, err = m.readCID(ctx, path)
	if err != nil {
		return err
	}
	if !cfg.IsEmpty() && actual != desiredContext(cfg) {
		return errors.New("CID1 changed during NM activation")
	}
	if err = m.nm.Commit(ctx, m.connection, state.Autoconnect); err != nil {
		return fmt.Errorf("commit NM settings: %w", err)
	}
	m.lastAppliedCID = actual
	m.pending = nil
	return nil
}

func (m *Manager) rollback(ctx, operationCtx context.Context, path dbus.ObjectPath, before snapshot, state ConnectionState, changed, detached bool) error {
	var failures []error
	add := func(err error) {
		if err != nil {
			failures = append(failures, err)
		}
	}
	if changed {
		// Activation or a partially completed commit may have resumed autoconnect.
		err := m.nm.SetAutoconnect(ctx, m.connection, false)
		add(err)
		if err == nil {
			err = m.nm.Disconnect(ctx, m.connection)
			add(err)
		}
		if err == nil {
			_, err = m.mm.SendCommandContext(ctx, path, "AT+COPS=2", atDeregisterTime)
			add(err)
		}
		add(m.nm.SetGSM(ctx, m.connection, before.nm))
		if err == nil {
			if err := m.setMM(ctx, path, before.mm); err != nil {
				m.logger.Printf("apn: MM rollback unavailable; restoring CID1 directly")
			}
			add(m.writeAuth(ctx, path, parseMMSettings(before.mm)))
			add(m.writeCID(ctx, path, before.cid))
			actual, readErr := m.readCID(ctx, path)
			add(readErr)
			if readErr == nil && actual != before.cid {
				add(errors.New("rollback CID1 readback mismatch"))
			}
		}
	}
	if changed {
		actual, err := m.nm.GetGSM(ctx, m.connection)
		add(err)
		if err == nil && !actual.EqualNM(before.nm) {
			add(errors.New("rollback NM readback mismatch"))
		}
	}
	if len(failures) != 0 {
		// Leave only the in-memory profile suspended; retain the original snapshot for retry.
		return errors.Join(failures...)
	}
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), nmcliApplyTimeout)
	defer cancel()
	if err := m.nm.Commit(commitCtx, m.connection, state.Autoconnect); err != nil {
		return err
	}
	// Cancellation can mean a power-off request: never explicitly reattach in that case.
	if detached && operationCtx.Err() == nil {
		deadline, _ := ctx.Deadline()
		reconnectCtx, stop := context.WithDeadline(operationCtx, deadline)
		defer stop()
		_, err := m.mm.SendCommandContext(reconnectCtx, path, "AT+COPS=0", atAutoRegisterMax)
		add(err)
		if err == nil && state.Active {
			add(m.nm.Connect(reconnectCtx, m.connection))
		}
	}
	return errors.Join(failures...)
}

func (m *Manager) writeCID(ctx context.Context, path dbus.ObjectPath, cid attachContext) error {
	command := "AT+CGDCONT=1"
	if cid.Present {
		command = fmt.Sprintf(`AT+CGDCONT=1,"%s","%s"`, cid.PDPType, cid.APN)
	}
	_, err := m.mm.SendCommandContext(ctx, path, command, atQuickTimeout)
	return err
}

func (m *Manager) writeAuth(ctx context.Context, path dbus.ObjectPath, cfg Config) error {
	if err := validateConfig(cfg); err != nil {
		return err
	}
	auth := 0
	switch cfg.Auth {
	case "pap":
		auth = 1
	case "chap":
		auth = 2
	}
	command := "AT+CGAUTH=1,0"
	if auth != 0 && (cfg.Username != "" || cfg.Password != "") {
		command = fmt.Sprintf(`AT+CGAUTH=1,%d,"%s","%s"`, auth, cfg.Username, cfg.Password)
	}
	_, err := m.mm.SendCommandContext(ctx, path, command, atQuickTimeout)
	return err
}

// Reattach shares the APN operation lock; service callers also hold the lifecycle lock.
func (m *Manager) Reattach(ctx context.Context, path dbus.ObjectPath) error {
	if path == "" {
		return errors.New("empty modem path")
	}
	if !m.mu.TryLock() {
		return errors.New("APN transaction in progress")
	}
	defer m.mu.Unlock()
	if _, err := m.mm.SendCommandContext(ctx, path, "AT+COPS=2", atDeregisterTime); err != nil {
		return err
	}
	_, err := m.mm.SendCommandContext(ctx, path, "AT+COPS=0", atAutoRegisterMax)
	return err
}

func validateATValue(s string) error {
	if strings.ContainsAny(s, "\"\r\n,\x00") {
		return errors.New("contains AT command delimiters")
	}
	return nil
}

func validateConfig(c Config) error {
	for _, value := range []string{c.APN, c.Username, c.Password} {
		if err := validateATValue(value); err != nil {
			return err
		}
	}
	switch c.Auth {
	case "", "none", "pap", "chap":
		return nil
	}
	return errors.New("unsupported authentication type")
}

func parseMMSettings(s map[string]dbus.Variant) Config {
	var c Config
	if v, ok := s["apn"]; ok {
		c.APN, _ = v.Value().(string)
	}
	if v, ok := s["user"]; ok {
		c.Username, _ = v.Value().(string)
	}
	if v, ok := s["password"]; ok {
		c.Password, _ = v.Value().(string)
	}
	if v, ok := s["allowed-auth"]; ok {
		if u, ok := v.Value().(uint32); ok {
			c.Auth = decodeAuth(u)
		}
	}
	return c
}

func encodeMMSettings(c Config) map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"apn": dbus.MakeVariant(c.APN), "user": dbus.MakeVariant(c.Username), "password": dbus.MakeVariant(c.Password),
		"allowed-auth": dbus.MakeVariant(encodeAuth(c.Auth)), "ip-type": dbus.MakeVariant(mm.BearerIPv4),
	}
}

func encodeAuth(s string) uint32 {
	switch s {
	case "pap":
		return mm.BearerAuthPAP
	case "chap":
		return mm.BearerAuthCHAP
	}
	return mm.BearerAuthNone
}

func decodeAuth(u uint32) string {
	switch {
	case u&mm.BearerAuthPAP != 0:
		return "pap"
	case u&mm.BearerAuthCHAP != 0:
		return "chap"
	}
	return "none"
}

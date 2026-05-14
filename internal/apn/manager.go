// Package apn reconciles the LTE attach APN and data-bearer credentials
// (cellular.apn, cellular.username, cellular.password, cellular.auth) with
// two backends:
//
//   - ModemManager initial-EPS-bearer settings, which persist in the modem
//     and govern the LTE attach context (AT+CGDCONT in firmware terms). A
//     wrong attach APN can prevent registration entirely on some networks,
//     even if NetworkManager's data APN is correct.
//   - The NetworkManager "wwan" GSM connection profile, which sets the data
//     bearer APN/user/password used after attach.
//
// Both must be reconciled — NM-only doesn't fix attach problems, MM-only
// doesn't authenticate the data session.
//
// SIM swap handling: the manager tracks the ICCID we last applied settings
// for. If the modem reports a different ICCID, NM and MM are cleared to
// defaults (the new SIM's operator decides) and the user has to set new
// values in cellular.* settings to re-arm reconciliation.
package apn

import (
	"fmt"
	"log"
	"sync"

	"github.com/godbus/dbus/v5"

	"modem-service/internal/mm"
)

// Outcome describes the result of one Reconcile call. Published verbatim to
// the modem.apn-action Redis field.
type Outcome string

const (
	OutcomeNoSIM             Outcome = "no-sim"
	OutcomeUnconfigured      Outcome = "unconfigured"
	OutcomeOK                Outcome = "ok"
	OutcomeApplied           Outcome = "applied"
	OutcomeICCIDChangedClear Outcome = "iccid-changed-cleared"
	OutcomeError             Outcome = "error"
)

// Config is the APN configuration on one side (settings, NM, or MM). Empty
// strings and AuthNone are the "unset" representation.
type Config struct {
	APN      string
	Username string
	Password string
	Auth     string // "none", "pap", "chap"; empty == "none"
}

// IsEmpty reports whether the config carries no meaningful APN values.
func (c Config) IsEmpty() bool {
	return c.APN == "" && c.Username == "" && c.Password == "" && (c.Auth == "" || c.Auth == "none")
}

// Equal compares two configs treating empty Auth as "none".
func (c Config) Equal(o Config) bool {
	a := c.Auth
	if a == "" {
		a = "none"
	}
	b := o.Auth
	if b == "" {
		b = "none"
	}
	return c.APN == o.APN && c.Username == o.Username && c.Password == o.Password && a == b
}

// Input is the per-cycle snapshot passed to Reconcile.
type Input struct {
	ICCID     string          // current SIM ICCID; empty if no SIM
	ModemPath dbus.ObjectPath // for MM SetInitialEpsBearerSettings; "" skips MM side
	Desired   Config          // from cellular.* settings (empty fields == cleared)
}

// MMDBus is the narrow ModemManager surface the manager needs.
type MMDBus interface {
	GetInitialEpsBearerSettings(modemPath dbus.ObjectPath) (map[string]dbus.Variant, error)
	SetInitialEpsBearerSettings(modemPath dbus.ObjectPath, settings map[string]dbus.Variant) error
}

// NM is the narrow NetworkManager surface the manager needs. Concrete impl
// shells out to nmcli; tests substitute a recorder.
type NM interface {
	GetGSM(connection string) (Config, error)
	SetGSM(connection string, cfg Config) error
	Reapply(connection string) error
}

// Manager owns the reconcile loop. Single connection name, single modem.
type Manager struct {
	mm         MMDBus
	nm         NM
	logger     *log.Logger
	connection string

	mu               sync.Mutex
	lastAppliedICCID string
}

// New returns a manager bound to the given backends. connection is the NM
// connection name (typically "wwan"). logger may be nil.
func New(mmClient MMDBus, nm NM, connection string, logger *log.Logger) *Manager {
	if logger == nil {
		logger = log.Default()
	}
	return &Manager{
		mm:         mmClient,
		nm:         nm,
		logger:     logger,
		connection: connection,
	}
}

// Reconcile decides what (if anything) to do this cycle. At most one set of
// SetGSM + SetInitialEpsBearerSettings calls per invocation.
func (m *Manager) Reconcile(in Input) Outcome {
	if in.ICCID == "" {
		return OutcomeNoSIM
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// SIM swap detection. If the ICCID changed since we last applied (and
	// we ever did), clear both backends to defaults. We don't try to
	// re-apply settings to the new SIM — those may have been right for the
	// old SIM. User must explicitly set values for the new SIM, which
	// triggers reapply via the settings watcher → next Reconcile path.
	if m.lastAppliedICCID != "" && m.lastAppliedICCID != in.ICCID {
		m.logger.Printf("apn: ICCID changed (%s -> %s), clearing NM + MM APN config",
			m.lastAppliedICCID, in.ICCID)
		empty := Config{}
		if err := m.applyBoth(in.ModemPath, empty); err != nil {
			m.logger.Printf("apn: clear after ICCID change failed: %v", err)
			return OutcomeError
		}
		m.lastAppliedICCID = ""
		return OutcomeICCIDChangedClear
	}

	if in.Desired.IsEmpty() {
		// No user config. Make sure NM/MM are also empty so the SIM's
		// operator defaults are used. Skip work if already empty.
		nmCur, mmCur, err := m.readCurrent(in.ModemPath)
		if err != nil {
			m.logger.Printf("apn: read current failed: %v", err)
			return OutcomeError
		}
		if nmCur.IsEmpty() && mmCur.IsEmpty() {
			m.lastAppliedICCID = in.ICCID
			return OutcomeUnconfigured
		}
		m.logger.Printf("apn: no settings configured but NM/MM hold values, clearing")
		if err := m.applyBoth(in.ModemPath, Config{}); err != nil {
			m.logger.Printf("apn: clear failed: %v", err)
			return OutcomeError
		}
		m.lastAppliedICCID = in.ICCID
		return OutcomeApplied
	}

	nmCur, mmCur, err := m.readCurrent(in.ModemPath)
	if err != nil {
		m.logger.Printf("apn: read current failed: %v", err)
		return OutcomeError
	}
	if nmCur.Equal(in.Desired) && mmCur.Equal(in.Desired) && m.lastAppliedICCID == in.ICCID {
		return OutcomeOK
	}

	m.logger.Printf("apn: applying apn=%q user=%q auth=%q (nm-change=%v mm-change=%v)",
		in.Desired.APN, in.Desired.Username, in.Desired.Auth,
		!nmCur.Equal(in.Desired), !mmCur.Equal(in.Desired))

	if err := m.applyBoth(in.ModemPath, in.Desired); err != nil {
		m.logger.Printf("apn: apply failed: %v", err)
		return OutcomeError
	}
	m.lastAppliedICCID = in.ICCID
	return OutcomeApplied
}

func (m *Manager) readCurrent(modemPath dbus.ObjectPath) (nm, mmCfg Config, err error) {
	nm, err = m.nm.GetGSM(m.connection)
	if err != nil {
		return Config{}, Config{}, fmt.Errorf("nm get: %w", err)
	}
	if modemPath == "" {
		return nm, Config{}, nil
	}
	settings, err := m.mm.GetInitialEpsBearerSettings(modemPath)
	if err != nil {
		// Treat as empty rather than failing the whole cycle — some MM
		// states (early enable, SIM locked) don't expose this property.
		m.logger.Printf("apn: MM GetInitialEpsBearerSettings: %v (treating as empty)", err)
		return nm, Config{}, nil
	}
	return nm, parseMMSettings(settings), nil
}

func (m *Manager) applyBoth(modemPath dbus.ObjectPath, cfg Config) error {
	if err := m.nm.SetGSM(m.connection, cfg); err != nil {
		return fmt.Errorf("nm set: %w", err)
	}
	if err := m.nm.Reapply(m.connection); err != nil {
		// Non-fatal: NM will use the new config on the next activation
		// attempt anyway, and the modem-recovery loop will eventually
		// re-establish.
		m.logger.Printf("apn: nm reapply failed (will retry on next activation): %v", err)
	}
	if modemPath == "" {
		return nil
	}
	if err := m.mm.SetInitialEpsBearerSettings(modemPath, encodeMMSettings(cfg)); err != nil {
		return fmt.Errorf("mm set: %w", err)
	}
	return nil
}

func parseMMSettings(s map[string]dbus.Variant) Config {
	var c Config
	if v, ok := s["apn"]; ok {
		if str, ok := v.Value().(string); ok {
			c.APN = str
		}
	}
	if v, ok := s["user"]; ok {
		if str, ok := v.Value().(string); ok {
			c.Username = str
		}
	}
	if v, ok := s["password"]; ok {
		if str, ok := v.Value().(string); ok {
			c.Password = str
		}
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
		"apn":          dbus.MakeVariant(c.APN),
		"user":         dbus.MakeVariant(c.Username),
		"password":     dbus.MakeVariant(c.Password),
		"allowed-auth": dbus.MakeVariant(encodeAuth(c.Auth)),
		"ip-type":      dbus.MakeVariant(mm.BearerIPv4),
	}
}

func encodeAuth(s string) uint32 {
	switch s {
	case "pap":
		return mm.BearerAuthPAP
	case "chap":
		return mm.BearerAuthCHAP
	default:
		return mm.BearerAuthNone
	}
}

func decodeAuth(u uint32) string {
	switch {
	case u&mm.BearerAuthPAP != 0:
		return "pap"
	case u&mm.BearerAuthCHAP != 0:
		return "chap"
	default:
		return "none"
	}
}

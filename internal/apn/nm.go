package apn

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// nmcli timeouts. nmcli can block for a while when MM is busy; bound it so
// reconcile doesn't stall the monitor loop.
const (
	nmcliQueryTimeout = 5 * time.Second
	nmcliApplyTimeout = 15 * time.Second
)

// NMCli is an NM implementation that shells out to `nmcli`. The password
// flows through argv, which is briefly visible in /proc; this matches how
// existing keyfile-based configs already expose it on disk.
type NMCli struct{}

// NewNMCli returns an NMCli runner.
func NewNMCli() *NMCli { return &NMCli{} }

// GetGSM reads gsm.apn / gsm.username / gsm.password / gsm.password-flags
// from the named NM connection. Returns a zero Config (no error) if the
// connection has no gsm section yet.
func (NMCli) GetGSM(connection string) (Config, error) {
	ctx, cancel := context.WithTimeout(context.Background(), nmcliQueryTimeout)
	defer cancel()
	// -t terse, -e no-escape, -f fields, -g would suppress headers but
	// strips empty cells too — terse mode keeps colons in place.
	out, err := exec.CommandContext(ctx, "nmcli", "-t", "-e", "no",
		"-f", "gsm.apn,gsm.username,gsm.password",
		"connection", "show", connection).Output()
	if err != nil {
		// "no such connection" → empty config, no error. Anything else
		// (nmcli missing, NM down) is a real error.
		if strings.Contains(strings.ToLower(string(stderrOf(err))), "unknown connection") {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("nmcli show %s: %w", connection, err)
	}
	cfg := Config{}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if val == "--" {
			val = ""
		}
		switch key {
		case "gsm.apn":
			cfg.APN = val
		case "gsm.username":
			cfg.Username = val
		case "gsm.password":
			cfg.Password = val
		}
	}
	// nmcli doesn't expose gsm.password-auth directly; auth is negotiated.
	// Treat as "none" for diffing — only MM round-trips auth precisely.
	return cfg, nil
}

// SetGSM writes the four GSM fields on the named connection. Empty strings
// clear the field. Sets password-flags=0 (system-stored) so NM uses the
// password we provide.
func (NMCli) SetGSM(connection string, cfg Config) error {
	ctx, cancel := context.WithTimeout(context.Background(), nmcliApplyTimeout)
	defer cancel()
	args := []string{
		"connection", "modify", connection,
		"gsm.apn", cfg.APN,
		"gsm.username", cfg.Username,
		"gsm.password", cfg.Password,
		"gsm.password-flags", "0",
	}
	cmd := exec.CommandContext(ctx, "nmcli", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nmcli modify %s: %w: %s", connection, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Reapply forces NM to pick up the new settings on the active connection by
// bouncing it. If the connection is currently inactive, `down` may report
// "not an active connection" — that's not an error.
func (NMCli) Reapply(connection string) error {
	ctx, cancel := context.WithTimeout(context.Background(), nmcliApplyTimeout)
	defer cancel()
	// Best-effort down; ignore errors (connection may already be down).
	_ = exec.CommandContext(ctx, "nmcli", "connection", "down", connection).Run()
	upCtx, upCancel := context.WithTimeout(context.Background(), nmcliApplyTimeout)
	defer upCancel()
	out, err := exec.CommandContext(upCtx, "nmcli", "connection", "up", connection).CombinedOutput()
	if err != nil {
		return fmt.Errorf("nmcli up %s: %w: %s", connection, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// stderrOf returns the stderr from an *exec.ExitError, or nil.
func stderrOf(err error) []byte {
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.Stderr
	}
	return nil
}

package apn

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	nmcliQueryTimeout   = 5 * time.Second
	nmcliApplyTimeout   = 15 * time.Second
	nmcliConnectTimeout = 60 * time.Second
)

type ConnectionState struct {
	Autoconnect bool
	Active      bool
}

// NMCli keeps command output private: nmcli errors can echo GSM credentials.
type NMCli struct {
	run func(context.Context, ...string) ([]byte, error)
}

func NewNMCli() *NMCli { return &NMCli{} }

func (n NMCli) command(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	run := n.run
	if run == nil {
		run = func(ctx context.Context, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, "nmcli", args...)
			cmd.Env = append(os.Environ(), "LC_ALL=C")
			return cmd.CombinedOutput()
		}
	}
	out, err := run(ctx, args...)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return out, err
}

func nmError(operation string, err error) error {
	if err == context.Canceled || err == context.DeadlineExceeded {
		return fmt.Errorf("nmcli %s: %w", operation, err)
	}
	return fmt.Errorf("nmcli %s failed", operation)
}

func (n NMCli) GetGSM(ctx context.Context, connection string) (Config, error) {
	out, err := n.command(ctx, nmcliQueryTimeout, "--show-secrets", "-t", "-e", "no",
		"-f", "gsm.apn,gsm.username,gsm.password", "connection", "show", connection)
	if err != nil {
		return Config{}, nmError("show GSM settings", err)
	}
	var cfg Config
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
	return cfg, nil
}

func (n NMCli) SetGSM(ctx context.Context, connection string, cfg Config) error {
	_, err := n.command(ctx, nmcliApplyTimeout, "connection", "modify", "--temporary", connection,
		"gsm.apn", cfg.APN, "gsm.username", cfg.Username, "gsm.password", cfg.Password, "gsm.password-flags", "0")
	if err != nil {
		return nmError("modify GSM settings", err)
	}
	return nil
}

func (n NMCli) State(ctx context.Context, connection string) (ConnectionState, error) {
	var state ConnectionState
	out, err := n.command(ctx, nmcliQueryTimeout, "-g", "connection.autoconnect", "connection", "show", connection)
	if err != nil {
		return state, nmError("read autoconnect", err)
	}
	switch strings.TrimSpace(string(out)) {
	case "yes":
		state.Autoconnect = true
	case "no":
	default:
		return state, fmt.Errorf("invalid NM autoconnect state")
	}
	out, err = n.command(ctx, nmcliQueryTimeout, "-g", "GENERAL.STATE", "connection", "show", connection)
	if err != nil {
		return state, nmError("read connection state", err)
	}
	switch strings.TrimSpace(string(out)) {
	case "activated", "activating", "deactivating":
		state.Active = true
	case "", "--", "deactivated", "unknown":
	default:
		return state, fmt.Errorf("invalid NM connection state")
	}
	return state, nil
}

func (n NMCli) SetAutoconnect(ctx context.Context, connection string, enabled bool) error {
	value := "no"
	if enabled {
		value = "yes"
	}
	_, err := n.command(ctx, nmcliApplyTimeout, "connection", "modify", "--temporary", connection, "connection.autoconnect", value)
	if err != nil {
		return nmError("set autoconnect", err)
	}
	return nil
}

// Commit persists the staged GSM settings together with the original autoconnect policy.
func (n NMCli) Commit(ctx context.Context, connection string, autoconnect bool) error {
	value := "no"
	if autoconnect {
		value = "yes"
	}
	_, err := n.command(ctx, nmcliApplyTimeout, "connection", "modify", connection, "connection.autoconnect", value)
	if err != nil {
		return nmError("commit connection", err)
	}
	return nil
}

func (n NMCli) Disconnect(ctx context.Context, connection string) error {
	out, err := n.command(ctx, nmcliApplyTimeout, "connection", "down", connection)
	if err != nil {
		if ctx.Err() == nil && strings.Contains(string(out), "is not an active connection") {
			return nil
		}
		return nmError("disconnect", err)
	}
	return nil
}

func (n NMCli) Connect(ctx context.Context, connection string) error {
	_, err := n.command(ctx, nmcliConnectTimeout, "--wait", "55", "connection", "up", connection)
	if err != nil {
		return nmError("connect", err)
	}
	return nil
}

func (n NMCli) Reapply(ctx context.Context, connection string) error {
	if err := n.Disconnect(ctx, connection); err != nil {
		return err
	}
	return n.Connect(ctx, connection)
}

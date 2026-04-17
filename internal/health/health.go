package health

import (
	"context"
	"fmt"
	"net"
	"syscall"
	"time"
)

// Constants for health states
const (
	MaxRecoveryAttempts = 5
	RecoveryWaitTime    = 60 * time.Second

	StateNormal             = "normal"
	StateRecovering         = "recovering"
	StateRecoveryFailedWait = "recovery-failed-waiting-reboot"
	StatePermanentFailure   = "permanent-failure-needs-replacement"
)

// Health represents the health status of the modem
type Health struct {
	RecoveryAttempts int
	LastRecoveryTime time.Time
	State            string
}

// New creates a new Health instance
func New() *Health {
	return &Health{
		State: StateNormal,
	}
}

// StartRecovery marks the health as recovering
func (h *Health) StartRecovery() {
	h.State = StateRecovering
	h.RecoveryAttempts++
	h.LastRecoveryTime = time.Now()
}

// MarkNormal marks the health as normal and zeros the recovery counter.
// The counter tracks sequential failed recovery rounds, so any success — a
// strategy that actually worked, a healthy probe, or the terminal-wait
// cooldown expiring — clears it. Otherwise a service that survives 5
// unrelated recovery events over its lifetime would drift into the
// recovery-failed-wait terminal state even though every one of them fixed
// itself on strategy 1.
func (h *Health) MarkNormal() {
	h.State = StateNormal
	h.RecoveryAttempts = 0
}

// MarkRecoveryFailed marks the health as recovery failed
func (h *Health) MarkRecoveryFailed() {
	if h.RecoveryAttempts >= MaxRecoveryAttempts {
		h.State = StateRecoveryFailedWait
	} else {
		h.State = StatePermanentFailure
	}
}

// IsRecovering returns true if the health is recovering
func (h *Health) IsRecovering() bool {
	return h.State == StateRecovering
}

// IsTerminal returns true if the health is in a terminal state
func (h *Health) IsTerminal() bool {
	return h.State == StateRecoveryFailedWait || h.State == StatePermanentFailure
}

// CanRecover returns true if recovery can be attempted
func (h *Health) CanRecover() bool {
	return h.RecoveryAttempts < MaxRecoveryAttempts
}

// String returns a string representation of the health
func (h *Health) String() string {
	return fmt.Sprintf("Health{State: %s, RecoveryAttempts: %d}", h.State, h.RecoveryAttempts)
}

// connectivityTargets are tried in order by CheckInternetConnectivity. A
// single target can be filtered or deprioritised on a given carrier, so we
// spread the check across multiple providers — any one success means we
// have a data path to the public internet. TCP port 53 is almost universally
// open, even on networks that block ICMP or filter HTTPS.
var connectivityTargets = []string{
	"8.8.8.8:53",        // Google Public DNS
	"1.1.1.1:53",        // Cloudflare
	"9.9.9.9:53",        // Quad9
	"208.67.222.222:53", // OpenDNS
}

const connectivityDialTimeout = 2 * time.Second

// CheckInternetConnectivity attempts TCP:53 connections to a short list of
// public DNS resolvers via the given modem interface. Returns true if any
// target is reachable. Returns false with the accumulated errors if all of
// them fail, so the caller can log what was tried.
func CheckInternetConnectivity(ctx context.Context, interfaceName string) (bool, error) {
	dialer := &net.Dialer{
		Timeout: connectivityDialTimeout,
		Control: func(network, address string, c syscall.RawConn) error {
			var sockErr error
			err := c.Control(func(fd uintptr) {
				// Bind socket to interface using SO_BINDTODEVICE so the
				// probe traffic never escapes via the wifi/wired path that
				// the MDB might also have.
				sockErr = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, interfaceName)
			})
			if err != nil {
				return err
			}
			return sockErr
		},
	}

	var errs []string
	for _, target := range connectivityTargets {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		conn, err := dialer.DialContext(ctx, "tcp", target)
		if err == nil {
			conn.Close()
			return true, nil
		}
		errs = append(errs, fmt.Sprintf("%s: %v", target, err))
	}
	return false, fmt.Errorf("all connectivity targets unreachable: %s", errs)
}

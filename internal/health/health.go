package health

import (
	"context"
	"fmt"
	"os/exec"
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

// MarkNormal marks the health as normal
func (h *Health) MarkNormal() {
	h.State = StateNormal
}

// MarkRecoveryFailed marks the health as recovery failed
func (h *Health) MarkRecoveryFailed() {
	if h.RecoveryAttempts == MaxRecoveryAttempts {
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

// CheckInternetConnectivity attempts to ping an external host via the specified interface.
func CheckInternetConnectivity(ctx context.Context, interfaceName string) (bool, error) {
	// Use context with timeout for the ping command
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second) // 2-second timeout for ping
	defer cancel()

	// Command: ping -c 1 (one packet) -W 1 (1-second wait) -I interface 8.8.8.8 (Google DNS)
	cmd := exec.CommandContext(pingCtx, "ping", "-c", "1", "-W", "1", "-I", interfaceName, "8.8.8.8")

	err := cmd.Run()

	if err != nil {
		// Check if the error is due to context deadline exceeded (timeout)
		if ctxErr := pingCtx.Err(); ctxErr == context.DeadlineExceeded {
			return false, fmt.Errorf("ping timed out: %w", err)
		}
		// Other errors (e.g., ping command not found, interface doesn't exist, network unreachable)
		return false, fmt.Errorf("ping command failed: %w", err)
	}

	// If cmd.Run() returns nil, the ping was successful
	return true, nil
}

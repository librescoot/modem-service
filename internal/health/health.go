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

// CheckInternetConnectivity attempts to connect to an external host via the specified interface.
// Uses TCP connect to Google DNS (8.8.8.8:53) with interface binding.
func CheckInternetConnectivity(ctx context.Context, interfaceName string) (bool, error) {
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	dialer := &net.Dialer{
		Timeout: 2 * time.Second,
		Control: func(network, address string, c syscall.RawConn) error {
			var sockErr error
			err := c.Control(func(fd uintptr) {
				// Bind socket to interface using SO_BINDTODEVICE
				sockErr = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, interfaceName)
			})
			if err != nil {
				return err
			}
			return sockErr
		},
	}

	// Try to establish TCP connection to Google DNS
	conn, err := dialer.DialContext(checkCtx, "tcp", "8.8.8.8:53")
	if err != nil {
		if checkCtx.Err() == context.DeadlineExceeded {
			return false, fmt.Errorf("connection timed out: %w", err)
		}
		return false, fmt.Errorf("connection failed: %w", err)
	}
	conn.Close()

	return true, nil
}

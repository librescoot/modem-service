package health

import (
	"fmt"
	"time"
)

const (
	MaxRecoveryAttempts = 5
	RecoveryWaitTime    = 60 * time.Second

	StateNormal             = "normal"
	StateRecovering         = "recovering"
	StateRecoveryFailedWait = "recovery-failed-waiting-reboot"
	StatePermanentFailure   = "permanent-failure-needs-replacement"
)

type Health struct {
	RecoveryAttempts int
	LastRecoveryTime time.Time
	State            string
}

func New() *Health {
	return &Health{
		State: StateNormal,
	}
}

func (h *Health) StartRecovery() {
	h.State = StateRecovering
	h.RecoveryAttempts++
	h.LastRecoveryTime = time.Now()
}

// MarkNormal clears sequential recovery failures after any successful round.
func (h *Health) MarkNormal() {
	h.State = StateNormal
	h.RecoveryAttempts = 0
}

func (h *Health) FinishRecoveryAttempt() bool {
	if h.State != StateRecovering {
		return false
	}
	h.State = StateNormal
	return true
}

func (h *Health) MarkRecoveryFailed() {
	if h.RecoveryAttempts >= MaxRecoveryAttempts {
		h.State = StateRecoveryFailedWait
	} else {
		h.State = StatePermanentFailure
	}
}

func (h *Health) IsRecovering() bool {
	return h.State == StateRecovering
}

func (h *Health) IsTerminal() bool {
	return h.State == StateRecoveryFailedWait || h.State == StatePermanentFailure
}

func (h *Health) CanRecover() bool {
	return h.RecoveryAttempts < MaxRecoveryAttempts
}

func (h *Health) String() string {
	return fmt.Sprintf("Health{State: %s, RecoveryAttempts: %d}", h.State, h.RecoveryAttempts)
}

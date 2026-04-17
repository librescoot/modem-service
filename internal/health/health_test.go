package health

import "testing"

func TestMarkNormalResetsRecoveryAttempts(t *testing.T) {
	h := New()

	h.StartRecovery()
	h.StartRecovery()
	if h.RecoveryAttempts != 2 {
		t.Fatalf("expected 2 attempts after two StartRecovery, got %d", h.RecoveryAttempts)
	}

	h.MarkNormal()
	if h.State != StateNormal {
		t.Errorf("expected state %q, got %q", StateNormal, h.State)
	}
	if h.RecoveryAttempts != 0 {
		t.Errorf("expected RecoveryAttempts=0 after MarkNormal, got %d", h.RecoveryAttempts)
	}

	// Subsequent successes after real recovery should keep giving us the
	// full 5-attempt budget, not drift toward the terminal state.
	for range 4 {
		h.StartRecovery()
		h.MarkNormal()
	}
	if !h.CanRecover() {
		t.Errorf("CanRecover should be true after repeated MarkNormal; RecoveryAttempts=%d", h.RecoveryAttempts)
	}
}

func TestCanRecoverBoundary(t *testing.T) {
	h := New()
	for range MaxRecoveryAttempts - 1 {
		h.StartRecovery()
	}
	if !h.CanRecover() {
		t.Errorf("expected CanRecover=true at RecoveryAttempts=%d", h.RecoveryAttempts)
	}
	h.StartRecovery()
	if h.CanRecover() {
		t.Errorf("expected CanRecover=false at RecoveryAttempts=%d", h.RecoveryAttempts)
	}
}

func TestMarkRecoveryFailedAtOrAboveMax(t *testing.T) {
	h := New()
	h.RecoveryAttempts = MaxRecoveryAttempts
	h.MarkRecoveryFailed()
	if h.State != StateRecoveryFailedWait {
		t.Errorf("at max attempts, expected %q, got %q", StateRecoveryFailedWait, h.State)
	}

	h2 := New()
	h2.RecoveryAttempts = MaxRecoveryAttempts + 3
	h2.MarkRecoveryFailed()
	if h2.State != StateRecoveryFailedWait {
		t.Errorf("above max attempts, expected %q, got %q", StateRecoveryFailedWait, h2.State)
	}
}

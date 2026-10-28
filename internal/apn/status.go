package apn

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"
)

type OperationPhase string

const (
	PhaseChecking          OperationPhase = "checking"
	PhaseApplying          OperationPhase = "applying"
	PhaseRollingBack       OperationPhase = "rolling-back"
	PhaseComplete          OperationPhase = "complete"
	PhaseRecoveryRequired  OperationPhase = "recovery-required"
	operationStatusTimeout                = 2 * time.Second
)

// OperationStatus deliberately excludes authentication values and modem commands.
type OperationStatus struct {
	ID              string         `json:"operation_id"`
	Phase           OperationPhase `json:"phase"`
	ICCID           string         `json:"iccid"`
	ModemPath       string         `json:"modem_path"`
	RequestedAPN    string         `json:"requested_apn"`
	EffectiveAPN    string         `json:"effective_apn"`
	Authentication  string         `json:"authentication"`
	Outcome         Outcome        `json:"outcome"`
	PendingRollback bool           `json:"pending_rollback"`
}

// OperationObserver must honor the supplied deadline. Publications are synchronous
// and serialized with native APN operations, including compensating rollback.
type OperationObserver func(context.Context, OperationStatus) error

func (m *Manager) SetOperationObserver(observer OperationObserver) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.observer = observer
}

func (m *Manager) beginOperation(ctx context.Context, in Input, effective Config) error {
	if m.observer == nil {
		return nil
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	auth := "none"
	if effective.Username != "" || effective.Password != "" || (effective.Auth != "" && effective.Auth != "none") {
		auth = "custom"
	}
	m.operation = &OperationStatus{ID: hex.EncodeToString(id[:]), ICCID: in.ICCID, ModemPath: string(in.ModemPath), RequestedAPN: in.Desired.APN, EffectiveAPN: effective.APN, Authentication: auth}
	if err := m.observeOperation(ctx, PhaseChecking); err != nil {
		m.operation = nil
		return err
	}
	return nil
}

func (m *Manager) observeOperation(ctx context.Context, phase OperationPhase) error {
	if m.operation == nil {
		return nil
	}
	m.operation.Phase = phase
	m.operation.PendingRollback = m.pending != nil
	bounded, cancel := context.WithTimeout(ctx, operationStatusTimeout)
	defer cancel()
	return m.observer(bounded, *m.operation)
}

func (m *Manager) finishOperation(ctx context.Context, outcome Outcome) error {
	if m.operation == nil {
		return nil
	}
	defer func() { m.operation = nil }()
	m.operation.Outcome = outcome
	phase := PhaseComplete
	if m.pending != nil {
		phase = PhaseRecoveryRequired
	}
	return m.observeOperation(context.WithoutCancel(ctx), phase)
}

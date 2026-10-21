package service

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"modem-service/internal/apn"
)

func TestAPNTransactionExcludesRecoveryAndCancelsOnDisable(t *testing.T) {
	entered := make(chan struct{})
	s := &Service{Logger: log.New(io.Discard, "", 0), raiseFaultFn: func(int, string) {},
		reconcileAPNFn: func(ctx context.Context, _ apn.Input) apn.Outcome {
			close(entered)
			<-ctx.Done()
			return apn.OutcomeError
		}}
	s.modemEnabled.Store(true)
	done := make(chan apn.Outcome, 1)
	go func() { done <- s.reconcileAPN(context.Background(), apn.Input{}) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("APN operation not entered")
	}
	if err := s.handleModemFailure(context.Background(), "test"); err == nil || !strings.Contains(err.Error(), "in progress") {
		t.Fatalf("recovery overlapped APN: %v", err)
	}
	if got := s.reconcileAPN(context.Background(), apn.Input{}); got != apn.OutcomeError {
		t.Fatal("second transaction accepted")
	}
	if err := s.handleModemCommand("disable"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disable did not cancel APN operation")
	}
	if !s.recoveryRunMu.TryLock() {
		t.Fatal("lifecycle lock retained")
	}
	s.recoveryRunMu.Unlock()
	s.modemOpCancelMu.Lock()
	defer s.modemOpCancelMu.Unlock()
	if s.modemOpCancel != nil {
		t.Fatal("APN cancellation handle retained")
	}
}

func TestAPNDoesNotReplaceRecoveryCancellation(t *testing.T) {
	s := &Service{}
	s.modemEnabled.Store(true)
	ctx, finish := s.startModemOperation(context.Background())
	defer finish()
	s.recoveryRunMu.Lock()
	defer s.recoveryRunMu.Unlock()
	if got := s.reconcileAPN(context.Background(), apn.Input{}); got != apn.OutcomeError {
		t.Fatal(got)
	}
	s.cancelModemOperation()
	if ctx.Err() != context.Canceled {
		t.Fatal("lost recovery cancellation handle")
	}
}

func TestAPNSkipsDisabledOrCancelledOperation(t *testing.T) {
	s := &Service{reconcileAPNFn: func(context.Context, apn.Input) apn.Outcome {
		t.Fatal("reconciled disabled/cancelled modem")
		return apn.OutcomeApplied
	}}
	if got := s.reconcileAPN(context.Background(), apn.Input{}); got != apn.OutcomeError {
		t.Fatal(got)
	}
	s.modemEnabled.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := s.reconcileAPN(ctx, apn.Input{}); got != apn.OutcomeError {
		t.Fatal(got)
	}
}

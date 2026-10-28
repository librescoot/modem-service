package apn

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOperationStatusCoversMutationAndExcludesCredentials(t *testing.T) {
	m, f, _ := fixture()
	var statuses []OperationStatus
	m.SetOperationObserver(func(ctx context.Context, s OperationStatus) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > operationStatusTimeout {
			t.Fatal("unbounded status publication")
		}
		statuses = append(statuses, s)
		return nil
	})
	f.hook = func(_ context.Context, name string, _ int) error {
		if name == "nm auto false" && statuses[len(statuses)-1].Phase != PhaseApplying {
			t.Fatal("mutation preceded active status")
		}
		return nil
	}
	if got := m.Reconcile(context.Background(), input(newConfig)); got != OutcomeApplied {
		t.Fatalf("outcome=%s", got)
	}
	var phases []OperationPhase
	for _, s := range statuses {
		phases = append(phases, s.Phase)
		if len(s.ID) != 32 || s.ID != statuses[0].ID || s.ICCID != "sim-a" || s.EffectiveAPN != newConfig.APN {
			t.Fatal("operation binding changed")
		}
	}
	if !reflect.DeepEqual(phases, []OperationPhase{PhaseChecking, PhaseApplying, PhaseComplete}) {
		t.Fatalf("phases=%v", phases)
	}
	final := statuses[len(statuses)-1]
	if final.Outcome != OutcomeApplied || final.PendingRollback || f.counts["nm commit true"] != 1 {
		t.Fatal("completion did not follow native commit")
	}
	encoded, _ := json.Marshal(statuses)
	for _, secret := range []string{newConfig.Username, newConfig.Password, oldConfig.Username, oldConfig.Password} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("status disclosed an authentication value")
		}
	}
	oldID := final.ID
	statuses = nil
	if got := m.Reconcile(context.Background(), input(newConfig)); got != OutcomeOK {
		t.Fatalf("outcome=%s", got)
	}
	if len(statuses) != 2 || statuses[0].ID == oldID || statuses[1].Phase != PhaseComplete {
		t.Fatal("readback reused stale completion")
	}
}

func TestOperationStatusFailurePreventsMutation(t *testing.T) {
	for _, phase := range []OperationPhase{PhaseChecking, PhaseApplying} {
		t.Run(string(phase), func(t *testing.T) {
			m, f, logs := fixture()
			m.SetOperationObserver(func(_ context.Context, s OperationStatus) error {
				if s.Phase == phase {
					return errors.New("PRIVATE-TRANSPORT-DETAIL")
				}
				return nil
			})
			if got := m.Reconcile(context.Background(), input(newConfig)); got != OutcomeError {
				t.Fatalf("outcome=%s", got)
			}
			if f.counts["nm auto false"] != 0 || f.counts["nm set"] != 0 {
				t.Fatal("mutated without observable active status")
			}
			if strings.Contains(logs.String(), "PRIVATE-TRANSPORT-DETAIL") {
				t.Fatal("transport error disclosed")
			}
		})
	}
}

func TestOperationCompletionFailureDoesNotReportSuccess(t *testing.T) {
	m, f, _ := fixture()
	m.SetOperationObserver(func(_ context.Context, s OperationStatus) error {
		if s.Phase == PhaseComplete {
			return errors.New("offline")
		}
		return nil
	})
	if got := m.Reconcile(context.Background(), input(newConfig)); got != OutcomeError {
		t.Fatalf("outcome=%s", got)
	}
	if !f.cfg.Equal(newConfig) || m.operation != nil {
		t.Fatal("ambiguous completed operation was not retained correctly")
	}
}

func TestRollbackStatusFailureDoesNotPreventCompensation(t *testing.T) {
	m, f, _ := fixture()
	var statuses []OperationStatus
	m.SetOperationObserver(func(_ context.Context, s OperationStatus) error {
		statuses = append(statuses, s)
		if s.Phase == PhaseRollingBack {
			return errors.New("offline")
		}
		return nil
	})
	f.hook = func(_ context.Context, name string, n int) error {
		if name == "nm up" && n == 1 {
			return errors.New("activation failed")
		}
		return nil
	}
	if got := m.Reconcile(context.Background(), input(newConfig)); got != OutcomeError {
		t.Fatalf("outcome=%s", got)
	}
	if !f.cfg.Equal(oldConfig) || m.pending != nil {
		t.Fatal("compensation was skipped")
	}
	if len(statuses) != 4 || statuses[2].Phase != PhaseRollingBack || statuses[3].Phase != PhaseComplete || statuses[3].PendingRollback {
		t.Fatal("incorrect rollback lifecycle")
	}
}

func TestUnresolvedRollbackNeverPublishesComplete(t *testing.T) {
	m, f, _ := fixture()
	var last OperationStatus
	m.SetOperationObserver(func(_ context.Context, s OperationStatus) error { last = s; return nil })
	f.hook = func(_ context.Context, name string, n int) error {
		if name == "nm up" && n == 1 || name == "nm set" && n == 2 {
			return errors.New("failed")
		}
		return nil
	}
	if got := m.Reconcile(context.Background(), input(newConfig)); got != OutcomeError {
		t.Fatalf("outcome=%s", got)
	}
	if last.Phase != PhaseRecoveryRequired || !last.PendingRollback || last.Outcome != OutcomeError {
		t.Fatalf("status=%+v", last)
	}
}

func TestBusyReconcileCannotOverwriteActiveStatus(t *testing.T) {
	m, f, _ := fixture()
	var mu sync.Mutex
	var statuses []OperationStatus
	m.SetOperationObserver(func(_ context.Context, s OperationStatus) error {
		mu.Lock()
		defer mu.Unlock()
		statuses = append(statuses, s)
		return nil
	})
	entered, release := make(chan struct{}), make(chan struct{})
	f.hook = func(_ context.Context, name string, n int) error {
		if name == "nm auto false" && n == 1 {
			close(entered)
			<-release
		}
		return nil
	}
	done := make(chan Outcome, 1)
	go func() { done <- m.Reconcile(context.Background(), input(newConfig)) }()
	<-entered
	if got := m.Reconcile(context.Background(), input(newConfig)); got != OutcomeError {
		t.Fatalf("busy outcome=%s", got)
	}
	mu.Lock()
	count := len(statuses)
	phase := statuses[count-1].Phase
	mu.Unlock()
	close(release)
	if got := <-done; got != OutcomeApplied {
		t.Fatalf("outcome=%s", got)
	}
	if count != 2 || phase != PhaseApplying {
		t.Fatal("busy caller replaced active status")
	}
}

func TestCancellationStillPublishesFinalRollbackState(t *testing.T) {
	m, f, _ := fixture()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var last OperationStatus
	m.SetOperationObserver(func(ctx context.Context, s OperationStatus) error {
		if ctx.Err() != nil {
			t.Fatal("cleanup status inherited canceled operation context")
		}
		last = s
		return nil
	})
	f.hook = func(_ context.Context, name string, n int) error {
		if name == "nm set" && n == 1 {
			cancel()
			return context.Canceled
		}
		return nil
	}
	if got := m.Reconcile(ctx, input(newConfig)); got != OutcomeError {
		t.Fatalf("outcome=%s", got)
	}
	if last.Phase != PhaseComplete || last.PendingRollback || last.Outcome != OutcomeError {
		t.Fatalf("status=%+v", last)
	}
}

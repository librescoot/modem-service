package apn

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestNMCliStagesChangesBeforeCommit(t *testing.T) {
	var calls [][]string
	n := NMCli{run: func(_ context.Context, args ...string) ([]byte, error) { calls = append(calls, args); return nil, nil }}
	ctx := context.Background()
	if err := n.SetAutoconnect(ctx, "wwan", false); err != nil {
		t.Fatal(err)
	}
	if err := n.SetGSM(ctx, "wwan", newConfig); err != nil {
		t.Fatal(err)
	}
	if err := n.Commit(ctx, "wwan", true); err != nil {
		t.Fatal(err)
	}
	for _, call := range calls[:2] {
		if !strings.Contains(strings.Join(call, " "), "modify --temporary wwan") {
			t.Fatalf("persisted intermediate state: %q", call)
		}
	}
	want := []string{"connection", "modify", "wwan", "connection.autoconnect", "yes"}
	if !reflect.DeepEqual(calls[2], want) {
		t.Fatalf("commit=%q", calls[2])
	}
}

func TestNMCliReadsUnmaskedCredentials(t *testing.T) {
	n := NMCli{run: func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] != "--show-secrets" {
			t.Fatal("password would be masked")
		}
		return []byte("gsm.apn:carrier.example\ngsm.username:user\ngsm.password:pass:word\n"), nil
	}}
	cfg, err := n.GetGSM(context.Background(), "wwan")
	if err != nil || cfg.Password != "pass:word" || cfg.APN != "carrier.example" || cfg.Username != "user" {
		t.Fatalf("invalid GSM settings, error=%v", err)
	}
}

func TestNMCliErrorDoesNotEchoCredentials(t *testing.T) {
	n := NMCli{run: func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte("secret-password"), errors.New("secret-password")
	}}
	err := n.SetGSM(context.Background(), "wwan", Config{Password: "secret-password"})
	if err == nil || strings.Contains(err.Error(), "secret-password") {
		t.Fatalf("unsafe error: %v", err)
	}
	if _, err := n.GetGSM(context.Background(), "missing"); err == nil {
		t.Fatal("missing profile treated as empty")
	}
}

func TestNMCliInactiveDisconnectAndCancellation(t *testing.T) {
	n := NMCli{run: func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte("Error: 'wwan' is not an active connection."), errors.New("exit status 10")
	}}
	if err := n.Disconnect(context.Background(), "wwan"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := n.Disconnect(ctx, "wwan"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestNMCliState(t *testing.T) {
	for _, active := range []string{"activated", "activating", "", "--"} {
		n := NMCli{run: func(_ context.Context, args ...string) ([]byte, error) {
			if args[1] == "connection.autoconnect" {
				return []byte("yes\n"), nil
			}
			return []byte(active + "\n"), nil
		}}
		state, err := n.State(context.Background(), "wwan")
		if err != nil || !state.Autoconnect || state.Active != (active == "activated" || active == "activating") {
			t.Fatalf("state=%v, error=%v", state, err)
		}
	}
}

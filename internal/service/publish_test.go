package service

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"

	"modem-service/internal/health"
	"modem-service/internal/modem"
	"modem-service/internal/modem/connectivity"
)

func TestPublishModemStateClearsUnavailableIdentityFields(t *testing.T) {
	current := modem.NewState()
	last := *current
	last.IMEI = "old-imei"
	last.IMSI = "old-imsi"
	last.ICCID = "old-iccid"
	last.OperatorName = "old-operator"
	last.OperatorCode = "00101"

	classifier := connectivity.New()
	classifier.Force(connectivity.Disconnected)
	s := &Service{
		Logger: log.New(io.Discard, "", 0), Health: health.New(),
		LastState: &last, connClassifier: classifier,
		lastPubConn: connectivity.Disconnected,
	}
	s.modemEnabled.Store(true)
	published := make(map[string]string)
	s.publishFn = func(key, value string) error {
		published["internet."+key] = value
		return nil
	}
	s.publishModemFn = func(key, value string) error {
		published["modem."+key] = value
		return nil
	}

	if err := s.publishModemState(context.Background(), current, current.Status); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"internet.sim-imei", "internet.sim-imsi", "internet.sim-iccid",
		"modem.operator-name", "modem.operator-code",
	} {
		value, ok := published[key]
		if !ok || value != "" {
			t.Errorf("%s = %q, present=%v; want an explicit empty value", key, value, ok)
		}
	}
}

func TestPublishModemStateRetriesFailedFields(t *testing.T) {
	for _, field := range []string{"modem-state", "registration", "error-state", "connectivity"} {
		t.Run(field, func(t *testing.T) {
			current := modem.NewState()
			current.Status = "disconnected"
			current.LastRawModemStatus = current.Status
			current.Registration = modem.RegistrationHome
			current.ErrorState = "ok"
			last := *current
			classifier := connectivity.New()
			classifier.Force(connectivity.Disconnected)
			s := &Service{
				Logger: log.New(io.Discard, "", 0), Health: health.New(),
				LastState: &last, connClassifier: classifier,
				lastPubConn: connectivity.Disconnected,
			}
			s.modemEnabled.Store(true)
			var cached func() string
			var want string
			switch field {
			case "modem-state":
				last.LastRawModemStatus = "old"
				cached = func() string { return last.LastRawModemStatus }
				want = current.Status
			case "registration":
				last.Registration = "old"
				cached = func() string { return last.Registration }
				want = current.Registration
			case "error-state":
				last.ErrorState = "old"
				cached = func() string { return last.ErrorState }
				want = current.ErrorState
			case "connectivity":
				s.lastPubConn = "old"
				cached = func() string { return string(s.lastPubConn) }
				want = string(connectivity.Disconnected)
			}

			failure := errors.New("redis unavailable")
			calls := 0
			publish := func(key, value string) error {
				if key != field || value != want {
					t.Errorf("unexpected write %s=%s, want %s=%s", key, value, field, want)
				}
				calls++
				if calls == 1 {
					return failure
				}
				return nil
			}
			s.publishFn, s.publishModemFn = publish, publish
			// No GPS mode work should be launched during shutdown.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := s.publishModemState(ctx, current, current.Status); !errors.Is(err, failure) {
				t.Fatalf("first publish error = %v", err)
			}
			if cached() != "old" {
				t.Fatalf("failed write advanced cache to %q", cached())
			}
			if err := s.publishModemState(ctx, current, current.Status); err != nil {
				t.Fatal(err)
			}
			if cached() != want {
				t.Fatalf("successful write left cache at %q", cached())
			}
			if err := s.publishModemState(ctx, current, current.Status); err != nil {
				t.Fatal(err)
			}
			if calls != 2 {
				t.Fatalf("writes = %d, want failure + retry only", calls)
			}
		})
	}
}

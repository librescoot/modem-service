package service

import (
	"context"
	"errors"
	"io"
	"log"
	"reflect"
	"testing"

	"modem-service/internal/health"
	"modem-service/internal/location"
	"modem-service/internal/modem"
	"modem-service/internal/modem/connectivity"
)

func TestPublishGPSMode(t *testing.T) {
	for _, tc := range []struct {
		name  string
		modes []location.GPSMode
		want  []string
	}{
		{"initial standalone and duplicate", []location.GPSMode{location.ModeStandalone, location.ModeStandalone}, []string{"standalone"}},
		{"initial nonzero and duplicate", []location.GPSMode{location.ModeUEBased, location.ModeUEBased}, []string{"ue-based"}},
		{"transitions", []location.GPSMode{location.ModeStandalone, location.ModeUEBased, location.ModeStandalone, location.ModeStandalone}, []string{"standalone", "ue-based", "standalone"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var writes []string
			s := &Service{Logger: log.New(io.Discard, "", 0), Location: &location.Service{}}
			s.publishGPSModeFn = func(mode string) error {
				writes = append(writes, mode)
				return nil
			}
			for i, mode := range tc.modes {
				if i == 0 && mode == location.ModeStandalone {
					s.publishGPSMode()
				} else {
					s.publishGPSModeValue(mode)
				}
			}
			if !reflect.DeepEqual(writes, tc.want) {
				t.Errorf("writes = %v, want %v", writes, tc.want)
			}
		})
	}
}

func TestPublishGPSModeRetriesFailedWrite(t *testing.T) {
	for _, initial := range []bool{true, false} {
		t.Run(map[bool]string{true: "initial", false: "transition"}[initial], func(t *testing.T) {
			var writes []string
			fail := false
			s := &Service{Logger: log.New(io.Discard, "", 0), Location: &location.Service{}}
			s.publishGPSModeFn = func(mode string) error {
				writes = append(writes, mode)
				if fail {
					fail = false
					return errors.New("redis unavailable")
				}
				return nil
			}
			if !initial {
				s.publishGPSModeValue(location.ModeUEBased)
			}
			fail = true
			s.publishGPSMode()
			s.publishGPSMode()
			s.publishGPSMode()
			want := []string{"standalone", "standalone"}
			if !initial {
				want = append([]string{"ue-based"}, want...)
			}
			if !reflect.DeepEqual(writes, want) {
				t.Errorf("writes = %v, want %v", writes, want)
			}
		})
	}
}

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

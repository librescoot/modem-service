package service

import (
	"modem-service/internal/location"
	"testing"
	"time"
)

func TestGPSProgressionRejectsReplayAndSuspend(t *testing.T) {
	base := location.Location{TimeFromGPS: true, GPSSession: 1, Timestamp: time.Unix(1800000000, 0), ReceiptBootNS: int64(10 * time.Second), ReceiptMonoNS: int64(10 * time.Second)}
	for _, tc := range []struct {
		name           string
		dt, boot, mono time.Duration
		valid          bool
	}{
		{"normal", time.Second, time.Second, time.Second, true},
		{"frozen", 0, time.Second, time.Second, false},
		{"replay", -time.Second, time.Second, time.Second, false},
		{"delayed", time.Second, 10 * time.Second, 10 * time.Second, false},
		{"suspend", time.Second, time.Second, 100 * time.Millisecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := base
			b.Timestamp = b.Timestamp.Add(tc.dt)
			b.ReceiptBootNS += int64(tc.boot)
			b.ReceiptMonoNS += int64(tc.mono)
			if got := gpsProgresses(base, b); got != tc.valid {
				t.Fatalf("got %v", got)
			}
		})
	}
}
func TestGPSConfirmationRequiresSelectedConvergedGPS(t *testing.T) {
	q := clockQuality{Source: "gps", External: true, Outcome: "completed"}
	if !gpsClockConfirmed(q) {
		t.Fatal("GPS rejected")
	}
	for _, mutate := range []func(*clockQuality){func(q *clockQuality) { q.Source = "ntp" }, func(q *clockQuality) { q.External = false }, func(q *clockQuality) { q.AgeNS = int64(31 * time.Second) }, func(q *clockQuality) { q.Residual = 6 }, func(q *clockQuality) { q.Outcome = "unknown" }} {
		p := q
		mutate(&p)
		if gpsClockConfirmed(p) {
			t.Fatalf("accepted %+v", p)
		}
	}
}

func TestGPSProgressionRequiresSameSession(t *testing.T) {
	a := location.Location{TimeFromGPS: true, GPSSession: 1, Timestamp: time.Unix(1800000000, 0), ReceiptBootNS: int64(time.Second), ReceiptMonoNS: int64(time.Second)}
	b := a
	b.GPSSession = 2
	b.Timestamp = b.Timestamp.Add(time.Second)
	b.ReceiptBootNS += int64(time.Second)
	b.ReceiptMonoNS += int64(time.Second)
	if gpsProgresses(a, b) {
		t.Fatal("accepted cross-session pair")
	}
}

func TestGPSReceiptFreshRejectsQueuedAndSuspendCrossing(t *testing.T) {
	loc := location.Location{ReceiptBootNS: int64(10 * time.Second), ReceiptMonoNS: int64(10 * time.Second)}
	for _, tc := range []struct {
		name       string
		boot, mono time.Duration
		valid      bool
	}{
		{"immediate", time.Second, time.Second, true}, {"queued", 3 * time.Second, 3 * time.Second, false}, {"suspend", time.Second, 0, false}, {"future", -time.Second, -time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if gpsReceiptFresh(loc, loc.ReceiptBootNS+int64(tc.boot), loc.ReceiptMonoNS+int64(tc.mono)) != tc.valid {
				t.Fatal("incorrect freshness")
			}
		})
	}
}

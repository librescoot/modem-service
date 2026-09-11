package service

import (
	"errors"
	"testing"
	"time"

	"modem-service/internal/modem/link"
)

// Remote silence must never trigger recovery when every local layer is healthy.
func TestUnreachableProbeNeverTriggersRemedy(t *testing.T) {
	healthy := link.Assessment{Healthy: true}
	s := &Service{remedyCooldown: map[link.Remedy]time.Time{}}

	var applied []link.Remedy
	s.applyRemedyFn = func(r link.Remedy) { applied = append(applied, r) }

	for range 500 {
		s.handleAssessment(healthy)
	}
	if len(applied) != 0 {
		t.Errorf("applied %v remedies over 500 unreachable ticks, want none", applied)
	}
}

func TestRemedyAppliedOnLayerFailure(t *testing.T) {
	tests := []struct {
		name string
		in   link.Assessment
		want link.Remedy
	}{
		{
			name: "bearer",
			in:   link.Assessment{FailedLayer: link.LayerBearer, Remedy: link.RemedyBearerBounce},
			want: link.RemedyBearerBounce,
		},
		{
			name: "packet-service",
			in:   link.Assessment{FailedLayer: link.LayerPacketService, Remedy: link.RemedyReattach},
			want: link.RemedyReattach,
		},
		{
			name: "hardware",
			in:   link.Assessment{FailedLayer: link.LayerHardware, Remedy: link.RemedyModemReset},
			want: link.RemedyModemReset,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{remedyCooldown: map[link.Remedy]time.Time{}}
			var applied []link.Remedy
			s.applyRemedyFn = func(r link.Remedy) { applied = append(applied, r) }

			for range remedyConfirmations {
				s.handleAssessment(tc.in)
			}
			if len(applied) != 1 || applied[0] != tc.want {
				t.Errorf("applied = %v, want [%v]", applied, tc.want)
			}
		})
	}
}

func TestRemedyNoneIsNeverApplied(t *testing.T) {
	// A locked SIM or a modem that simply is not registered are real layer
	// failures, but no modem action can fix either, so nothing must fire.
	for _, layer := range []link.Layer{link.LayerSIM, link.LayerRegistration} {
		s := &Service{remedyCooldown: map[link.Remedy]time.Time{}}
		var applied []link.Remedy
		s.applyRemedyFn = func(r link.Remedy) { applied = append(applied, r) }

		for range remedyConfirmations + 1 {
			s.handleAssessment(link.Assessment{FailedLayer: layer, Remedy: link.RemedyNone})
		}
		if len(applied) != 0 {
			t.Errorf("layer %v: applied = %v for RemedyNone, want none", layer, applied)
		}
	}
}

func TestRemedyCooldownSuppressesRepeats(t *testing.T) {
	now := time.Unix(0, 0)
	s := &Service{
		remedyCooldown: map[link.Remedy]time.Time{},
		now:            func() time.Time { return now },
	}
	var applied []link.Remedy
	s.applyRemedyFn = func(r link.Remedy) { applied = append(applied, r) }

	fail := link.Assessment{FailedLayer: link.LayerBearer, Remedy: link.RemedyBearerBounce}

	for range remedyConfirmations {
		s.handleAssessment(fail)
	}
	now = now.Add(time.Minute)
	s.handleAssessment(fail)
	now = now.Add(time.Minute)
	s.handleAssessment(fail)
	if len(applied) != 1 {
		t.Fatalf("applied %d times inside the cooldown, want 1", len(applied))
	}

	now = now.Add(bearerBounceCooldown)
	s.handleAssessment(fail)
	if len(applied) != 2 {
		t.Errorf("applied %d times after the cooldown expired, want 2", len(applied))
	}
}

func TestRemedyCooldownsAreIndependent(t *testing.T) {
	// A bearer bounce on cooldown must not suppress a modem reset: they are
	// different remedies with different windows.
	now := time.Unix(0, 0)
	s := &Service{
		remedyCooldown: map[link.Remedy]time.Time{},
		now:            func() time.Time { return now },
	}
	var applied []link.Remedy
	s.applyRemedyFn = func(r link.Remedy) { applied = append(applied, r) }

	for range remedyConfirmations {
		s.handleAssessment(link.Assessment{FailedLayer: link.LayerBearer, Remedy: link.RemedyBearerBounce})
	}
	for range remedyConfirmations {
		s.handleAssessment(link.Assessment{FailedLayer: link.LayerLiveness, Remedy: link.RemedyModemReset})
	}

	if len(applied) != 2 {
		t.Fatalf("applied = %v, want both remedies to fire", applied)
	}
	if applied[0] != link.RemedyBearerBounce || applied[1] != link.RemedyModemReset {
		t.Errorf("applied = %v, want [bearer-bounce modem-reset]", applied)
	}
}

func TestModemResetCooldownIsTheLongest(t *testing.T) {
	// A reset drops the data session and the GPS fix, so it must be rate
	// limited harder than the cheap remedies.
	if remedyCooldownFor(link.RemedyModemReset) <= remedyCooldownFor(link.RemedyBearerBounce) {
		t.Errorf("modem reset cooldown %v is not longer than bearer bounce %v",
			remedyCooldownFor(link.RemedyModemReset), remedyCooldownFor(link.RemedyBearerBounce))
	}
	if remedyCooldownFor(link.RemedyNone) != 0 {
		t.Errorf("RemedyNone cooldown = %v, want 0", remedyCooldownFor(link.RemedyNone))
	}
}

func TestProbeBackoffDoublesOnSuccess(t *testing.T) {
	const base = 30 * time.Second
	const maxInterval = 5 * time.Minute

	cur := base
	want := []time.Duration{
		time.Minute,
		2 * time.Minute,
		4 * time.Minute,
		maxInterval,
		maxInterval,
	}
	for i, w := range want {
		cur = nextProbeInterval(cur, base, maxInterval)
		if cur != w {
			t.Errorf("interval after %d probes = %v, want %v", i+1, cur, w)
		}
	}
}

// Probe failure also backs off because remote silence is not a local fault.
func TestProbeBackoffDoesNotResetOnFailure(t *testing.T) {
	const base = 30 * time.Second
	const maxInterval = 5 * time.Minute
	if got := nextProbeInterval(4*time.Minute, base, maxInterval); got <= 4*time.Minute {
		t.Errorf("nextProbeInterval() = %v, want it to keep growing regardless of outcome", got)
	}
}

func TestProbeBackoffNeverBelowBase(t *testing.T) {
	// A zero starting interval must not collapse the backoff to zero and spin
	// the probe every tick.
	const base = 30 * time.Second
	const maxInterval = 5 * time.Minute
	if got := nextProbeInterval(0, base, maxInterval); got < base {
		t.Errorf("nextProbeInterval(0, ...) = %v, want at least %v", got, base)
	}
}

func TestReachabilityField(t *testing.T) {
	tests := []struct {
		name       string
		reachable  bool
		assessment link.Assessment
		want       string
	}{
		{"reachable", true, link.Assessment{Healthy: true}, "ok"},
		{
			// The restricted-APN case: nothing answers, but the modem is fine.
			// This must not read as a fault, or we are back to the original bug.
			name: "unreachable-but-alive", reachable: false,
			assessment: link.Assessment{Healthy: true}, want: "unreachable",
		},
		{
			name: "no-path-when-liveness-fails", reachable: false,
			assessment: link.Assessment{FailedLayer: link.LayerLiveness}, want: "no-path",
		},
		{
			name: "no-path-when-a-lower-layer-fails", reachable: false,
			assessment: link.Assessment{FailedLayer: link.LayerBearer}, want: "no-path",
		},
		{
			// Reachable wins: if something answered, the path demonstrably
			// works whatever else the assessment says.
			name: "reachable-overrides-unhealthy", reachable: true,
			assessment: link.Assessment{FailedLayer: link.LayerBearer}, want: "ok",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := reachabilityField(tc.reachable, tc.assessment); got != tc.want {
				t.Errorf("reachabilityField() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLinkLayerField(t *testing.T) {
	tests := []struct {
		name string
		in   link.Assessment
		want string
	}{
		{"healthy", link.Assessment{Healthy: true}, "ok"},
		{
			name: "failure-includes-reason",
			in:   link.Assessment{FailedLayer: link.LayerBearer, Reason: "bearer not connected"},
			want: "bearer: bearer not connected",
		},
		{
			name: "failure-without-reason",
			in:   link.Assessment{FailedLayer: link.LayerNetdev},
			want: "netdev",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := linkLayerField(tc.in); got != tc.want {
				t.Errorf("linkLayerField() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPublishIfChangedOnlyWritesOnChange(t *testing.T) {
	var writes []string
	s := &Service{publishFn: func(field, value string) error {
		writes = append(writes, field+"="+value)
		return nil
	}}

	var last string
	for range 10 {
		s.publishIfChanged("reachability", "ok", &last)
	}
	if len(writes) != 1 {
		t.Fatalf("writes = %v, want exactly one for an unchanged value", writes)
	}

	s.publishIfChanged("reachability", "unreachable", &last)
	if len(writes) != 2 || writes[1] != "reachability=unreachable" {
		t.Errorf("writes = %v, want a second write on change", writes)
	}
}

func TestPublishIfChangedRetriesAfterFailure(t *testing.T) {
	// A failed write must not record the value as published, or a transient
	// Redis error would suppress the field until it happened to change again.
	fail := true
	writes := 0
	s := &Service{publishFn: func(field, value string) error {
		writes++
		if fail {
			return errors.New("redis down")
		}
		return nil
	}}

	var last string
	s.publishIfChanged("link-layer", "ok", &last)
	if last != "" {
		t.Fatalf("last = %q after a failed write, want it left unset for retry", last)
	}
	fail = false
	s.publishIfChanged("link-layer", "ok", &last)
	if writes != 2 || last != "ok" {
		t.Errorf("writes = %d, last = %q; want the value retried and then recorded", writes, last)
	}
}

// Exercise the real assessor and service debounce together.
func TestPersistentFlapEventuallyRemediates(t *testing.T) {
	now := time.Unix(0, 0)
	a := link.New()
	s := &Service{
		remedyCooldown: map[link.Remedy]time.Time{},
		now:            func() time.Time { return now },
		link:           a,
	}
	var applied []link.Remedy
	s.applyRemedyFn = func(r link.Remedy) { applied = append(applied, r) }

	snap := link.Snapshot{
		ModemPresent: true, PrimaryPortKnown: true, PrimaryPortOK: true, PowerState: "on",
		Registration: "roaming", PacketService: "attached",
		BearerKnown: true, BearerConnected: true, BearerConnectedKnown: true,
		BearerSuspendedKnown: true, BearerInterface: "wwan0", BearerIPKnown: true, BearerIP: "10.64.13.241",
		CarrierKnown: true, Carrier: true, NetdevIP: "10.64.13.241",
		DefaultRouteKnown: true, HasDefaultRoute: true,
		BearerSessionKnown: true, BearerAttempts: 1, BearerDuration: 1200,
	}
	a.Assess(snap)

	for range 300 {
		snap.BearerAttempts++
		snap.BearerDuration = 5
		s.handleAssessment(a.Assess(snap))
		now = now.Add(30 * time.Second)
	}
	if len(applied) == 0 {
		t.Fatal("a permanently flapping session produced zero remedies over 300 ticks")
	}
	t.Logf("applied %d remedies over 300 ticks: %v", len(applied), applied[:min(4, len(applied))])
}

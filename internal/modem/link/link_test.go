package link

import "testing"

// healthy returns a snapshot of a fully working modem, matching what deep-blue
// reports. Tests override single fields to isolate one layer at a time.
func healthy() Snapshot {
	return Snapshot{
		ModemPresent:         true,
		PrimaryPortKnown:     true,
		PrimaryPortOK:        true,
		PowerState:           "on",
		SIMLock:              "",
		Registration:         "home",
		PacketService:        "attached",
		BearerKnown:          true,
		BearerConnected:      true,
		BearerConnectedKnown: true,
		BearerSuspended:      false,
		BearerSuspendedKnown: true,
		BearerInterface:      "wwan0",
		BearerIPKnown:        true,
		BearerIP:             "10.64.13.241",
		BearerStatsKnown:     true,
		BearerAttempts:       1,
		BearerDuration:       46290,
		CarrierKnown:         true,
		Carrier:              true,
		NetdevIP:             "10.64.13.241",
		DefaultRouteKnown:    true,
		HasDefaultRoute:      true,
	}
}

func TestHealthySnapshotIsHealthy(t *testing.T) {
	a := New()
	if got := a.Assess(healthy()); !got.Healthy {
		t.Errorf("Assess(healthy) = %+v, want Healthy", got)
	}
}

func TestRoamingIsHealthy(t *testing.T) {
	// The affected fleet registers as roaming. Treating roaming as unhealthy
	// would recreate the exact false-positive class this package removes.
	s := healthy()
	s.Registration = "roaming"
	if got := New().Assess(s); !got.Healthy {
		t.Errorf("Assess(roaming) = %+v, want Healthy", got)
	}
}

func TestPin2IsNotALock(t *testing.T) {
	// PIN2 gates fixed dialling, not attach or data. Every scooter in the
	// fleet reports it.
	s := healthy()
	s.SIMLock = "sim-pin2"
	if got := New().Assess(s); !got.Healthy {
		t.Errorf("Assess(sim-pin2) = %+v, want Healthy", got)
	}
}

func TestLayerFailures(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*Snapshot)
		wantLayer  Layer
		wantRemedy Remedy
	}{
		{"modem-absent", func(s *Snapshot) { s.ModemPresent = false }, LayerHardware, RemedyModemReset},
		{"primary-port-down", func(s *Snapshot) { s.PrimaryPortOK = false }, LayerHardware, RemedyModemReset},
		{"powered-off", func(s *Snapshot) { s.PowerState = "off" }, LayerHardware, RemedyModemReset},
		{"sim-pin-locked", func(s *Snapshot) { s.SIMLock = "sim-pin" }, LayerSIM, RemedyNone},
		{"sim-puk-locked", func(s *Snapshot) { s.SIMLock = "sim-puk" }, LayerSIM, RemedyNone},
		{"not-registered", func(s *Snapshot) { s.Registration = "searching" }, LayerRegistration, RemedyNone},
		{"registration-denied", func(s *Snapshot) { s.Registration = "denied" }, LayerRegistration, RemedyNone},
		{"packet-detached", func(s *Snapshot) { s.PacketService = "detached" }, LayerPacketService, RemedyReattach},
		{"bearer-disconnected", func(s *Snapshot) { s.BearerConnected = false }, LayerBearer, RemedyBearerBounce},
		{"bearer-suspended", func(s *Snapshot) { s.BearerSuspended = true }, LayerBearer, RemedyBearerBounce},
		{"bearer-no-ip", func(s *Snapshot) { s.BearerIP = "" }, LayerBearer, RemedyBearerBounce},
		{"no-carrier", func(s *Snapshot) { s.Carrier = false }, LayerNetdev, RemedyBearerBounce},
		{"no-default-route", func(s *Snapshot) { s.HasDefaultRoute = false }, LayerNetdev, RemedyBearerBounce},
		{"netdev-ip-mismatch", func(s *Snapshot) { s.NetdevIP = "10.0.0.9" }, LayerNetdev, RemedyBearerBounce},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := healthy()
			tc.mutate(&s)
			got := New().Assess(s)
			if got.Healthy {
				t.Fatalf("Assess() = Healthy, want failure at %v", tc.wantLayer)
			}
			if got.FailedLayer != tc.wantLayer {
				t.Errorf("FailedLayer = %v, want %v", got.FailedLayer, tc.wantLayer)
			}
			if got.Remedy != tc.wantRemedy {
				t.Errorf("Remedy = %v, want %v", got.Remedy, tc.wantRemedy)
			}
			if got.Reason == "" {
				t.Error("Reason is empty, want a description")
			}
		})
	}
}

func TestLowestFailingLayerWins(t *testing.T) {
	s := healthy()
	s.ModemPresent = false    // layer 0
	s.BearerConnected = false // layer 5
	if got := New().Assess(s); got.FailedLayer != LayerHardware {
		t.Errorf("FailedLayer = %v, want %v", got.FailedLayer, LayerHardware)
	}
}

func TestUnknownFieldsDoNotFail(t *testing.T) {
	// A failed D-Bus read must never manufacture a remedy.
	s := healthy()
	s.PacketService = ""
	s.Registration = ""
	s.PowerState = ""
	s.PrimaryPortKnown = false
	s.BearerConnectedKnown = false
	s.BearerSuspendedKnown = false
	s.BearerIPKnown = false
	s.BearerIP = ""
	s.CarrierKnown = false
	s.DefaultRouteKnown = false
	if got := New().Assess(s); !got.Healthy {
		t.Errorf("Assess(unknowns) = %+v, want Healthy", got)
	}
}

func TestWantATCheckSetOnSuspicion(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Snapshot)
		want   bool
	}{
		{"healthy", func(*Snapshot) {}, false},
		{"bearer-disconnected", func(s *Snapshot) { s.BearerConnected = false }, true},
		{"bearer-suspended", func(s *Snapshot) { s.BearerSuspended = true }, true},
		{"packet-detached", func(s *Snapshot) { s.PacketService = "detached" }, true},
		{"netdev-ip-mismatch", func(s *Snapshot) { s.NetdevIP = "10.0.0.9" }, true},
		{"sim-locked", func(s *Snapshot) { s.SIMLock = "sim-pin" }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := healthy()
			tc.mutate(&s)
			if got := New().Assess(s); got.WantATCheck != tc.want {
				t.Errorf("WantATCheck = %v, want %v", got.WantATCheck, tc.want)
			}
		})
	}
}

func TestATCrossCheckConfirmsWedge(t *testing.T) {
	s := healthy()
	s.ATChecked = true
	s.CGACTActive = false // modem says the context is down, MM says it is up
	s.CGPADDR = ""
	got := New().Assess(s)
	if got.Healthy {
		t.Fatalf("Assess() = Healthy, want a bearer failure")
	}
	if got.FailedLayer != LayerBearer || got.Remedy != RemedyBearerBounce {
		t.Errorf("got layer %v remedy %v, want %v / %v",
			got.FailedLayer, got.Remedy, LayerBearer, RemedyBearerBounce)
	}
}

func TestATCrossCheckAddressMismatchIsAWedge(t *testing.T) {
	s := healthy()
	s.ATChecked = true
	s.CGACTActive = true
	s.CGPADDR = "10.0.0.9" // disagrees with the bearer and the netdev
	if got := New().Assess(s); got.Healthy {
		t.Error("Assess() = Healthy, want a bearer failure on address mismatch")
	}
}

func TestATCrossCheckClearsSuspicion(t *testing.T) {
	s := healthy()
	s.ATChecked = true
	s.CGACTActive = true
	s.CGPADDR = "10.64.13.241"
	if got := New().Assess(s); !got.Healthy {
		t.Errorf("Assess() = %+v, want Healthy: AT agrees with MM", got)
	}
}

// TestProbeSizedTxDoesNotFailLiveness is the regression test for the way this
// change could have reintroduced the bug it exists to fix.
//

// TestLivenessLadderNeverReachesModemReset pins the decision that layer 7 may
// not power-cycle a modem until the behaviour has been soaked on affected

// stableSession returns a healthy snapshot whose data session has been up long
// enough to count as stable.
func stableSession() Snapshot {
	s := healthy()
	s.BearerAttempts = 1
	s.BearerDuration = stableSessionSeconds * 10
	return s
}

// TestSilentNetworkNeverFailsLiveness is the regression test for the way this
// package could have reintroduced the bug it exists to fix.
//
// Layer 7 used to compare transmitted bytes against received bytes. The
// connectivity probe transmits on the same interface, so on a destination that
// never answers the service would read its own probe traffic as tx-with-no-rx,
// call it a wedge, and escalate to remedies on a perfectly healthy modem.
// Liveness must depend on nothing that a silent far end can influence.
func TestSilentNetworkNeverFailsLiveness(t *testing.T) {
	a := New()
	s := stableSession()
	for range 50 {
		// Session stays up and stable; nothing answers on the network, which
		// this layer cannot and must not observe.
		s.BearerDuration += 30
		if got := a.Assess(s); !got.Healthy {
			t.Fatalf("Assess() = %+v, want Healthy: a silent network is not a wedge", got)
		}
	}
}

func TestSessionFlapped(t *testing.T) {
	tests := []struct {
		name string
		prev Snapshot
		cur  Snapshot
		want bool
	}{
		{
			name: "reconnect-into-short-session-is-a-flap",
			prev: Snapshot{BearerAttempts: 1, BearerDuration: 4000},
			cur:  Snapshot{BearerAttempts: 2, BearerDuration: 5},
			want: true,
		},
		{
			name: "no-reconnect-is-not-a-flap",
			prev: Snapshot{BearerAttempts: 1, BearerDuration: 100},
			cur:  Snapshot{BearerAttempts: 1, BearerDuration: 130},
			want: false,
		},
		{
			name: "long-uptime-without-reconnect-is-not-a-flap",
			prev: Snapshot{BearerAttempts: 3, BearerDuration: 50},
			cur:  Snapshot{BearerAttempts: 3, BearerDuration: 80},
			want: false,
		},
		{
			name: "reconnect-into-already-stable-session-is-a-clean-handover",
			prev: Snapshot{BearerAttempts: 1, BearerDuration: 900},
			cur:  Snapshot{BearerAttempts: 2, BearerDuration: stableSessionSeconds + 1},
			want: false,
		},
		{
			name: "attempts-going-backwards-is-not-a-flap",
			prev: Snapshot{BearerAttempts: 5, BearerDuration: 900},
			cur:  Snapshot{BearerAttempts: 1, BearerDuration: 3},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.prev.BearerStatsKnown = true
			tc.cur.BearerStatsKnown = true
			if got := sessionFlapped(tc.prev, tc.cur); got != tc.want {
				t.Errorf("sessionFlapped() = %v, want %v", got, tc.want)
			}
		})
	}
}

// flap advances s by one reconnect-into-a-short-session and returns the verdict.
func flap(a *Assessor, s *Snapshot) Assessment {
	s.BearerAttempts++
	s.BearerDuration = 5
	return a.Assess(*s)
}

func TestFlappingNeedsRepetition(t *testing.T) {
	a := New()
	s := stableSession()
	a.Assess(s)

	for i := 1; i < flapsBeforeFailing; i++ {
		if got := flap(a, &s); !got.Healthy {
			t.Fatalf("flap %d: Assess() = %+v, want Healthy, one reconnect is not a fault", i, got)
		}
	}
	got := flap(a, &s)
	if got.Healthy || got.FailedLayer != LayerLiveness {
		t.Errorf("Assess() = %+v, want a liveness failure after %d flaps", got, flapsBeforeFailing)
	}
}

func TestStableSessionClearsFlapsAndLadder(t *testing.T) {
	a := New()
	s := stableSession()
	a.Assess(s)

	for range flapsBeforeFailing {
		flap(a, &s)
	}
	a.NoteRemedyApplied(LayerLiveness, livenessLadder[0])

	// The session comes back and stays up.
	s.BearerDuration = stableSessionSeconds * 5
	if got := a.Assess(s); !got.Healthy {
		t.Fatalf("Assess() = %+v, want Healthy once the session is stable", got)
	}

	// Both the flap count and the ladder position must have reset: the next
	// episode starts from the cheapest remedy again.
	a.Assess(s)
	for i := 1; i < flapsBeforeFailing; i++ {
		if got := flap(a, &s); !got.Healthy {
			t.Fatalf("flap %d after recovery: Assess() = %+v, want Healthy", i, got)
		}
	}
	got := flap(a, &s)
	if got.Remedy != livenessLadder[0] {
		t.Errorf("remedy after a stable period = %v, want the ladder to restart at %v",
			got.Remedy, livenessLadder[0])
	}
}

func TestLadderDoesNotPromoteWithoutApplication(t *testing.T) {
	// A remedy the caller declined to run, typically because it is still in
	// cooldown, must not buy promotion to a costlier one.
	a := New()
	s := stableSession()
	a.Assess(s)

	drive := func() Assessment {
		var got Assessment
		for range flapsBeforeFailing {
			got = flap(a, &s)
		}
		return got
	}

	first := drive()
	second := drive()
	if first.Remedy != second.Remedy {
		t.Errorf("promoted without NoteRemedyApplied: %v then %v", first.Remedy, second.Remedy)
	}
	if first.Remedy != livenessLadder[0] {
		t.Errorf("first remedy = %v, want %v", first.Remedy, livenessLadder[0])
	}
}

func TestLadderPromotesOnceAppliedAndClamps(t *testing.T) {
	a := New()
	s := stableSession()
	a.Assess(s)

	drive := func() Assessment {
		var got Assessment
		for range flapsBeforeFailing {
			got = flap(a, &s)
		}
		return got
	}

	var seen []Remedy
	for range len(livenessLadder) + 3 {
		got := drive()
		seen = append(seen, got.Remedy)
		a.NoteRemedyApplied(LayerLiveness, got.Remedy)
	}

	for i, r := range seen {
		want := livenessLadder[min(i, len(livenessLadder)-1)]
		if r != want {
			t.Errorf("remedy[%d] = %v, want %v", i, r, want)
		}
	}
}

// TestLivenessNeverReachesModemReset pins the decision that layer 7 may not
// power-cycle a modem until the behaviour has been soaked on affected
// hardware. A genuinely dead modem still reaches the reset ladder via layer 0.
func TestLivenessNeverReachesModemReset(t *testing.T) {
	for i, r := range livenessLadder {
		if r == RemedyModemReset {
			t.Fatalf("livenessLadder[%d] is RemedyModemReset; layer 7 must not reach a reset yet", i)
		}
	}
	a := New()
	s := stableSession()
	a.Assess(s)
	for range 60 {
		got := flap(a, &s)
		if got.Remedy == RemedyModemReset {
			t.Fatalf("liveness escalated to a modem reset: %+v", got)
		}
		a.NoteRemedyApplied(LayerLiveness, got.Remedy)
	}
}

// TestPersistentFlapReportsEveryTick is the regression test for a critical
// self-inflicted defect: Assess used to clear the flap count the moment it
// reported a failure, so the next tick read healthy. The caller debounces by
// requiring the same failing layer on consecutive assessments, so that reset
// meant a permanently flapping session could never be confirmed twice in a row
// and no liveness remedy could ever be applied. Measured before the fix: 100
// failures reported, 0 remedies applied, across 300 ticks.
func TestPersistentFlapReportsEveryTick(t *testing.T) {
	a := New()
	s := stableSession()
	a.Assess(s)

	for range flapsBeforeFailing {
		flap(a, &s)
	}

	// Every subsequent flapping tick must keep reporting, so a caller that
	// needs consecutive confirmations can get them.
	for i := range 5 {
		got := flap(a, &s)
		if got.Healthy || got.FailedLayer != LayerLiveness {
			t.Fatalf("tick %d after threshold: Assess() = %+v, want a sustained liveness failure", i, got)
		}
	}
}

func TestOtherLayersDoNotPromoteTheLivenessLadder(t *testing.T) {
	// A bearer bounce applied for a lower-layer fault must not start the next
	// liveness episode part-way up the ladder.
	a := New()
	s := stableSession()
	a.Assess(s)

	a.NoteRemedyApplied(LayerBearer, RemedyBearerBounce)
	a.NoteRemedyApplied(LayerNetdev, RemedyBearerBounce)
	a.NoteRemedyApplied(LayerPacketService, RemedyReattach)

	var got Assessment
	for range flapsBeforeFailing {
		got = flap(a, &s)
	}
	if got.Remedy != livenessLadder[0] {
		t.Errorf("first liveness remedy = %v, want %v: other layers contaminated the ladder",
			got.Remedy, livenessLadder[0])
	}
}

// TestStableSessionClearsFlapCount pins the half of the reset that the
// combined test could not see: with the flap count no longer cleared on
// report, only the stable-session branch clears it.
func TestStableSessionClearsFlapCount(t *testing.T) {
	a := New()
	s := stableSession()
	a.Assess(s)
	for range flapsBeforeFailing {
		flap(a, &s)
	}

	s.BearerDuration = stableSessionSeconds * 5
	if got := a.Assess(s); !got.Healthy {
		t.Fatalf("Assess() = %+v, want Healthy once the session is stable", got)
	}

	// One flap after recovery must not immediately re-trip: the count must
	// have gone back to zero, not merely stopped growing.
	if got := flap(a, &s); !got.Healthy {
		t.Errorf("Assess() = %+v, want Healthy: flap count should have reset", got)
	}
}

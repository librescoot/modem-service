package modem

import (
	"os"
	"path/filepath"
	"testing"

	"modem-service/internal/mm"
)

func TestBearerUsagePrefersAcrossReconnectTotals(t *testing.T) {
	info := mm.BearerInfo{
		Path:       "/org/freedesktop/ModemManager1/Bearer/1",
		StatsKnown: true,
		Stats: mm.BearerStats{
			RxBytes: 13434742, TxBytes: 3903371,
			TotalRxBytes: 74062451, TotalTxBytes: 7986025, HaveTotals: true,
		},
	}
	got := bearerUsage(info)
	want := BearerUsage{
		Valid:   true,
		Path:    "/org/freedesktop/ModemManager1/Bearer/1",
		RxBytes: 74062451,
		TxBytes: 7986025,
	}
	if got != want {
		t.Errorf("bearerUsage() = %+v, want %+v", got, want)
	}
}

func TestBearerUsageRequiresObservedStats(t *testing.T) {
	got := bearerUsage(mm.BearerInfo{Path: "/bearer/1"})
	if got.Valid {
		t.Fatalf("bearerUsage() = %+v, want invalid", got)
	}
}

func TestBearerUsageFallsBackToPerAttemptCounters(t *testing.T) {
	info := mm.BearerInfo{
		Path:       "/org/freedesktop/ModemManager1/Bearer/1",
		StatsKnown: true,
		Stats:      mm.BearerStats{RxBytes: 100, TxBytes: 200},
	}
	got := bearerUsage(info)
	if got.RxBytes != 100 || got.TxBytes != 200 || !got.Valid {
		t.Errorf("bearerUsage() = %+v, want the per-attempt counters", got)
	}
}

func TestParseCGACT(t *testing.T) {
	// parsed matters as much as active: an unreadable reply must not be
	// recorded as "no context active", or a busy AT port manufactures a
	// bearer bounce.
	tests := []struct {
		name       string
		in         string
		want       bool
		wantParsed bool
	}{
		{"active-context-1", "+CGACT: 1,1", true, true},
		{"inactive-context-1", "+CGACT: 1,0", false, true},
		{"multi-context-first-active", "+CGACT: 1,1\r\n+CGACT: 2,0", true, true},
		{"multi-context-second-active", "+CGACT: 1,0\r\n+CGACT: 2,1", true, true},
		{"with-ok-suffix", "+CGACT: 1,1\r\n\r\nOK", true, true},
		{"spaces", "+CGACT:  1 , 1 ", true, true},
		{"error-reply-is-unparseable", "ERROR", false, false},
		{"empty-is-unparseable", "", false, false},
		{"truncated-line-is-unparseable", "+CGACT: 1", false, false},
		{"noise-is-unparseable", "RING\r\nNO CARRIER", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, parsed := ParseCGACT(tc.in)
			if got != tc.want || parsed != tc.wantParsed {
				t.Errorf("ParseCGACT(%q) = (%v, %v), want (%v, %v)",
					tc.in, got, parsed, tc.want, tc.wantParsed)
			}
		})
	}
}

func TestParseCGPADDR(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"single", "+CGPADDR: 1,10.64.13.241", "10.64.13.241"},
		{"quoted", `+CGPADDR: 1,"10.64.13.241"`, "10.64.13.241"},
		{"with-ok", "+CGPADDR: 1,10.154.207.248\r\n\r\nOK", "10.154.207.248"},
		{"unassigned", "+CGPADDR: 1,0.0.0.0", ""},
		{"empty-address", "+CGPADDR: 1,", ""},
		{"garbage", "ERROR", ""},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseCGPADDR(tc.in); got != tc.want {
				t.Errorf("ParseCGPADDR(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// normalizeAddr guards a real trap: State.IfIPAddr defaults to the literal
// "UNKNOWN" (see NewState), not the empty string. Passing that straight into a
// link.Snapshot would make the netdev layer compare "UNKNOWN" against the
// bearer address, report a mismatch, and request a remedy on a healthy modem.
func TestNormalizeAddr(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"unknown-sentinel-becomes-empty", "UNKNOWN", ""},
		{"lowercase-unknown-too", "unknown", ""},
		{"unassigned-becomes-empty", "0.0.0.0", ""},
		{"whitespace-trimmed", "  10.64.13.241  ", "10.64.13.241"},
		{"real-address-kept", "10.64.13.241", "10.64.13.241"},
		{"empty-stays-empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeAddr(tc.in); got != tc.want {
				t.Errorf("normalizeAddr(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseDefaultRoute(t *testing.T) {
	// Real /proc/net/route shape: destination 00000000 marks the default
	// route. Columns are Iface, Destination, Gateway, then flags and metrics.
	const routes = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
wwan0	00000000	F9CF9A0A	0003	0	0	700	00000000	0	0	0
wwan0	F8CF9A0A	00000000	0001	0	0	700	FCFFFFFF	0	0	0
wg0	0007070A	00000000	0001	0	0	50	0000FFFF	0	0	0
`
	tests := []struct {
		name  string
		data  string
		iface string
		want  bool
	}{
		{"default-route-present", routes, "wwan0", true},
		{"iface-present-but-no-default", routes, "wg0", false},
		{"iface-absent", routes, "eth0", false},
		{"header-only", "Iface\tDestination\tGateway\n", "wwan0", false},
		{"empty", "", "wwan0", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseDefaultRoute([]byte(tc.data), tc.iface); got != tc.want {
				t.Errorf("parseDefaultRoute(%q) = %v, want %v", tc.iface, got, tc.want)
			}
		})
	}
}

func TestReadCarrier(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "wwan0"), 0o755); err != nil {
		t.Fatal(err)
	}
	// operstate is deliberately not consulted: the modem netdev is
	// POINTOPOINT/NOARP and reports "unknown" even when fully up, so carrier
	// is the only trustworthy signal on this hardware.
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"carrier-up", "1\n", true},
		{"carrier-down", "0\n", false},
		{"garbage", "banana\n", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(root, "wwan0", "carrier")
			if err := os.WriteFile(p, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := readCarrier(root, "wwan0"); got != tc.want {
				t.Errorf("readCarrier() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReadCarrierMissingInterface(t *testing.T) {
	root := t.TempDir()
	if readCarrier(root, "nope0") {
		t.Error("readCarrier() = true for a missing interface, want false")
	}
	if _, known := readCarrierObservation(root, "nope0"); known {
		t.Fatal("missing carrier file marked as a known down link")
	}
}

func TestDefaultRouteReadFailureIsUnknown(t *testing.T) {
	old := procNetRoute
	procNetRoute = filepath.Join(t.TempDir(), "missing-route")
	t.Cleanup(func() { procNetRoute = old })
	if _, known := defaultRouteObservation("wwan0"); known {
		t.Fatal("missing route table marked as a known absent route")
	}
}

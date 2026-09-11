package mm

import (
	"testing"

	"github.com/godbus/dbus/v5"
)

type testBearerProperties struct {
	values map[string]dbus.Variant
	failed map[string]bool
}

func (p *testBearerProperties) Get(_ string, property string) (dbus.Variant, *dbus.Error) {
	if p.failed[property] {
		return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.Failed", []interface{}{"unavailable"})
	}
	return p.values[property], nil
}

func TestGetBearerInfoTracksPropertyValidity(t *testing.T) {
	client, server, _ := privateBus(t)
	const path = dbus.ObjectPath("/Bearer/1")
	props := &testBearerProperties{
		values: map[string]dbus.Variant{
			"Suspended": dbus.MakeVariant(false),
			"Interface": dbus.MakeVariant("wwan0"),
			"Ip4Config": dbus.MakeVariant("wrong-type"),
			"Stats": mapVariant(map[string]dbus.Variant{
				"rx-bytes": dbus.MakeVariant(uint64(10)),
				"attempts": dbus.MakeVariant(uint32(2)),
				"duration": dbus.MakeVariant(uint64(30)),
			}),
		},
		failed: map[string]bool{"Connected": true},
	}
	if err := server.Export(props, path, DBusPropertiesInterface); err != nil {
		t.Fatal(err)
	}
	info, err := client.GetBearerInfo(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.ConnectedKnown || info.IP4Known || info.StatsKnown {
		t.Fatalf("failed/incomplete observations marked known: %+v", info)
	}
	if !info.SuspendedKnown || !info.InterfaceKnown || !info.SessionStatsKnown {
		t.Fatalf("successful observations marked unknown: %+v", info)
	}
}

func mapVariant(m map[string]dbus.Variant) dbus.Variant { return dbus.MakeVariant(m) }

func TestParseIP4Config(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]dbus.Variant
		want IP4Config
	}{
		{
			name: "full",
			in: map[string]dbus.Variant{
				"address": dbus.MakeVariant("10.64.13.241"),
				"prefix":  dbus.MakeVariant(uint32(30)),
				"gateway": dbus.MakeVariant("10.64.13.242"),
				"dns1":    dbus.MakeVariant("172.16.64.2"),
				"dns2":    dbus.MakeVariant("172.16.64.3"),
				"mtu":     dbus.MakeVariant(uint32(1500)),
			},
			want: IP4Config{
				Address: "10.64.13.241",
				Prefix:  30,
				Gateway: "10.64.13.242",
				DNS:     []string{"172.16.64.2", "172.16.64.3"},
				MTU:     1500,
			},
		},
		{
			name: "single-dns",
			in: map[string]dbus.Variant{
				"address": dbus.MakeVariant("10.0.0.1"),
				"dns1":    dbus.MakeVariant("8.8.8.8"),
			},
			want: IP4Config{Address: "10.0.0.1", DNS: []string{"8.8.8.8"}},
		},
		{
			name: "empty-dns-entries-dropped",
			in: map[string]dbus.Variant{
				"dns1": dbus.MakeVariant(""),
				"dns2": dbus.MakeVariant("1.1.1.1"),
			},
			want: IP4Config{DNS: []string{"1.1.1.1"}},
		},
		{"empty", map[string]dbus.Variant{}, IP4Config{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseIP4Config(tc.in)
			if got.Address != tc.want.Address || got.Prefix != tc.want.Prefix ||
				got.Gateway != tc.want.Gateway || got.MTU != tc.want.MTU {
				t.Errorf("parseIP4Config() scalars = %+v, want %+v", got, tc.want)
			}
			if len(got.DNS) != len(tc.want.DNS) {
				t.Fatalf("parseIP4Config() DNS = %v, want %v", got.DNS, tc.want.DNS)
			}
			for i := range got.DNS {
				if got.DNS[i] != tc.want.DNS[i] {
					t.Errorf("parseIP4Config() DNS[%d] = %q, want %q", i, got.DNS[i], tc.want.DNS[i])
				}
			}
		})
	}
}

func TestBearerStatsKnownRequiresCompleteCounterPair(t *testing.T) {
	tests := []struct {
		name  string
		stats map[string]dbus.Variant
		want  bool
	}{
		{"per-attempt", map[string]dbus.Variant{"rx-bytes": dbus.MakeVariant(uint64(0)), "tx-bytes": dbus.MakeVariant(uint64(0))}, true},
		{"totals", map[string]dbus.Variant{"total-rx-bytes": dbus.MakeVariant(uint64(0)), "total-tx-bytes": dbus.MakeVariant(uint64(0))}, true},
		{"partial", map[string]dbus.Variant{"rx-bytes": dbus.MakeVariant(uint64(1))}, false},
		{"wrong-type", map[string]dbus.Variant{"rx-bytes": dbus.MakeVariant("1"), "tx-bytes": dbus.MakeVariant(uint64(1))}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := bearerStatsKnown(tc.stats); got != tc.want {
				t.Fatalf("bearerStatsKnown() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseBearerStats(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]dbus.Variant
		want BearerStats
	}{
		{
			name: "uint64-bytes-uint32-attempts",
			in: map[string]dbus.Variant{
				"rx-bytes": dbus.MakeVariant(uint64(511510)),
				"tx-bytes": dbus.MakeVariant(uint64(1611281)),
				"duration": dbus.MakeVariant(uint64(46290)),
				"attempts": dbus.MakeVariant(uint32(1)),
			},
			want: BearerStats{RxBytes: 511510, TxBytes: 1611281, Duration: 46290, Attempts: 1},
		},
		{
			// MM has shipped these as uint32 on some plugins; accept both
			// widths rather than silently reading zero.
			name: "uint32-bytes",
			in: map[string]dbus.Variant{
				"rx-bytes": dbus.MakeVariant(uint32(100)),
				"tx-bytes": dbus.MakeVariant(uint32(200)),
			},
			want: BearerStats{RxBytes: 100, TxBytes: 200},
		},
		{
			name: "with-totals",
			in: map[string]dbus.Variant{
				"rx-bytes":       dbus.MakeVariant(uint64(13434742)),
				"tx-bytes":       dbus.MakeVariant(uint64(3903371)),
				"total-rx-bytes": dbus.MakeVariant(uint64(74062451)),
				"total-tx-bytes": dbus.MakeVariant(uint64(7986025)),
				"duration":       dbus.MakeVariant(uint32(64770)),
				"attempts":       dbus.MakeVariant(uint32(2)),
			},
			want: BearerStats{
				RxBytes: 13434742, TxBytes: 3903371,
				TotalRxBytes: 74062451, TotalTxBytes: 7986025, HaveTotals: true,
				Duration: 64770, Attempts: 2,
			},
		},
		{
			// Pre-1.20 MM omits the totals entirely. Half a pair is not a pair:
			// accounting must fall back rather than mix the two sources.
			name: "partial-totals-ignored",
			in: map[string]dbus.Variant{
				"rx-bytes":       dbus.MakeVariant(uint64(10)),
				"tx-bytes":       dbus.MakeVariant(uint64(20)),
				"total-rx-bytes": dbus.MakeVariant(uint64(999)),
			},
			want: BearerStats{RxBytes: 10, TxBytes: 20},
		},
		{"empty", map[string]dbus.Variant{}, BearerStats{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseBearerStats(tc.in); got != tc.want {
				t.Errorf("parseBearerStats() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestSelectDataBearer(t *testing.T) {
	// Reset-generated object paths make list index unusable as identity.
	attach := BearerInfo{Path: "/org/freedesktop/ModemManager1/Bearer/0", Connected: true}
	data := BearerInfo{
		Path: "/org/freedesktop/ModemManager1/Bearer/312", Connected: true,
		Interface: "wwan0", IP4: IP4Config{Address: "10.64.13.241"},
	}
	dataConnectedFailed := BearerInfo{
		Path: "/org/freedesktop/ModemManager1/Bearer/312", Connected: false,
		Interface: "wwan0", IP4: IP4Config{Address: "10.64.13.241"},
	}
	tests := []struct {
		name string
		in   []BearerInfo
		want dbus.ObjectPath
		ok   bool
	}{
		{"picks-the-one-with-an-interface", []BearerInfo{attach, data}, data.Path, true},
		{"order-independent", []BearerInfo{data, attach}, data.Path, true},
		{"connected-read-failure-degraded", []BearerInfo{attach, dataConnectedFailed}, dataConnectedFailed.Path, true},
		{"none-usable", []BearerInfo{attach}, "", false},
		{"empty", nil, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := selectDataBearer(tc.in)
			if ok != tc.ok {
				t.Fatalf("selectDataBearer() ok = %v, want %v", ok, tc.ok)
			}
			if ok && got.Path != tc.want {
				t.Errorf("selectDataBearer() = %q, want %q", got.Path, tc.want)
			}
		})
	}
}

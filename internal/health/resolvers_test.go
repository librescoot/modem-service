package health

import (
	"strings"
	"testing"
)

func TestParseResolvConf(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "systemd-resolved-stub-with-comments",
			in: `# This is /run/systemd/resolve/resolv.conf managed by man:systemd-resolved(8).
# Do not edit.
nameserver 172.16.64.2
nameserver 172.16.64.3
search example.com
`,
			want: []string{"172.16.64.2", "172.16.64.3"},
		},
		{
			name: "tabs-and-extra-spaces",
			in:   "nameserver\t8.8.8.8\nnameserver   1.1.1.1\n",
			want: []string{"8.8.8.8", "1.1.1.1"},
		},
		{
			name: "ipv6-kept",
			in:   "nameserver 2001:4860:4860::8888\n",
			want: []string{"2001:4860:4860::8888"},
		},
		{"garbage-ignored", "nameserver not-an-ip\nnameserver\n", nil},
		{"empty", "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertStrings(t, "ParseResolvConf", ParseResolvConf([]byte(tc.in)), tc.want)
		})
	}
}

func TestParseResolvectl(t *testing.T) {
	// Verbatim shape of `resolvectl status wwan0` on deep-blue.
	in := `Link 6 (wwan0)
    Current Scopes: DNS LLMNR/IPv4 mDNS/IPv4
         Protocols: +DefaultRoute +LLMNR +mDNS -DNSOverTLS
                    DNSSEC=allow-downgrade/supported
       DNS Servers: 62.109.121.17 62.109.121.18
     Default Route: yes
`
	assertStrings(t, "ParseResolvectl", ParseResolvectl([]byte(in)),
		[]string{"62.109.121.17", "62.109.121.18"})
}

func TestParseResolvectlNoServers(t *testing.T) {
	in := "Link 6 (wwan0)\n     Default Route: yes\n"
	assertStrings(t, "ParseResolvectl", ParseResolvectl([]byte(in)), nil)
}

func TestResolverSourcesUnion(t *testing.T) {
	src := func(vals ...string) func() []string {
		return func() []string { return vals }
	}
	tests := []struct {
		name string
		in   ResolverSources
		want []string
	}{
		{
			name: "priority-order-preserved",
			in:   ResolverSources{Sources: []func() []string{src("1.1.1.1"), src("8.8.8.8")}},
			want: []string{"1.1.1.1", "8.8.8.8"},
		},
		{
			name: "deduplicated-across-sources",
			in: ResolverSources{Sources: []func() []string{
				src("172.16.64.2", "172.16.64.3"),
				src("172.16.64.2"),
			}},
			want: []string{"172.16.64.2", "172.16.64.3"},
		},
		{
			name: "empty-source-skipped-not-fatal",
			in:   ResolverSources{Sources: []func() []string{src(), src("172.16.64.2")}},
			want: []string{"172.16.64.2"},
		},
		{
			name: "nil-source-func-tolerated",
			in:   ResolverSources{Sources: []func() []string{nil, src("1.1.1.1")}},
			want: []string{"1.1.1.1"},
		},
		{"all-empty", ResolverSources{}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertStrings(t, "Resolvers", tc.in.Resolvers(), tc.want)
		})
	}
}

func assertStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s() = [%s], want [%s]", what, strings.Join(got, " "), strings.Join(want, " "))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s()[%d] = %q, want %q", what, i, got[i], want[i])
		}
	}
}

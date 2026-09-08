package health

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// udpResponder answers every UDP packet with respBuilder(request). Returns the
// listening address. Closed via t.Cleanup.
func udpResponder(t *testing.T, respBuilder func([]byte) []byte) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if resp := respBuilder(buf[:n]); resp != nil {
				conn.WriteTo(resp, addr)
			}
		}
	}()
	return conn.LocalAddr().String()
}

// echoHeader returns a minimal DNS response: same transaction ID, QR bit set,
// and the supplied rcode in the low nibble of the second flags byte.
func echoHeader(rcode byte) func([]byte) []byte {
	return func(req []byte) []byte {
		if len(req) < 12 {
			return nil
		}
		resp := make([]byte, 12)
		copy(resp, req[:12])
		resp[2] = 0x81                  // QR=1, RD=1
		resp[3] = 0x80 | (rcode & 0x0f) // RA=1 plus rcode
		return resp
	}
}

func txtResponse(value string) func([]byte) []byte {
	return func(req []byte) []byte {
		if len(req) < 12 || len(value) > 255 {
			return nil
		}
		resp := append([]byte(nil), req...)
		resp[2] = 0x81
		resp[3] = 0x80
		binary.BigEndian.PutUint16(resp[6:8], 1) // ANCOUNT
		resp = append(resp, 0xc0, 0x0c)          // owner name -> question name
		resp = binary.BigEndian.AppendUint16(resp, 16)
		resp = binary.BigEndian.AppendUint16(resp, 1)
		resp = binary.BigEndian.AppendUint32(resp, 30)
		resp = binary.BigEndian.AppendUint16(resp, uint16(len(value)+1))
		resp = append(resp, byte(len(value)))
		resp = append(resp, value...)
		return resp
	}
}

func testProber(t *testing.T) *Prober {
	t.Helper()
	p := NewProber("", nil)
	// Empty interface name means no SO_BINDTODEVICE, which lets these tests
	// run on darwin as well as linux.
	p.DNSTimeout = 500 * time.Millisecond
	p.DialTimeout = 500 * time.Millisecond
	return p
}

// assertReachableViaDNS asserts the result is not just Reachable, but that it
// was the DNS leg that produced it. Without the Targets override and the
// Detail-prefix check, a regression that turns dnsQuery into rejecting a
// non-NOERROR rcode would make Probe fall through to the TCP targets loop;
// against DefaultConnectivityTargets (real public resolvers) that TCP dial
// succeeds on any networked host, and the test would still pass despite
// exercising none of what it claims to.
func assertReachableViaDNS(t *testing.T, got Result) {
	t.Helper()
	if !got.Reachable {
		t.Errorf("Probe() Reachable = false, want true (detail: %s)", got.Detail)
	}
	if !strings.HasPrefix(got.Detail, "dns ") {
		t.Errorf("Probe() Detail = %q, want a \"dns \" prefix (i.e. verdict came from the DNS leg, not a TCP fallback)", got.Detail)
	}
}

func TestProbeAssignedResolverNOERROR(t *testing.T) {
	addr := udpResponder(t, echoHeader(0))
	p := testProber(t)
	p.Targets = []string{"127.0.0.1:1"} // unreachable: a TCP fallback must not be able to rescue this
	got := p.Probe(context.Background(), []string{addr})
	assertReachableViaDNS(t, got)
}

func TestProbeVerifiedTXT(t *testing.T) {
	addr := udpResponder(t, txtResponse("librescoot-online-v1"))
	p := testProber(t)
	p.Targets = nil
	p.VerificationName = "reachability.example.test"
	p.VerificationValue = "librescoot-online-v1"
	got := p.Probe(context.Background(), []string{addr})
	if !got.Reachable {
		t.Fatalf("Probe() Reachable = false, want true (detail: %s)", got.Detail)
	}
	if !strings.HasPrefix(got.Detail, "verified dns ") {
		t.Errorf("Probe() Detail = %q, want verified DNS result", got.Detail)
	}
}

func TestProbeVerifiedTXTRejectsCaptiveResolverForgery(t *testing.T) {
	// A captive resolver can synthesize a well-formed answer, but it cannot
	// guess the deployment-controlled TXT token.
	addr := udpResponder(t, txtResponse("captive-portal"))
	p := testProber(t)
	p.Targets = nil
	p.VerificationName = "reachability.example.test"
	p.VerificationValue = "librescoot-online-v1"
	got := p.Probe(context.Background(), []string{addr})
	if got.Reachable {
		t.Fatalf("Probe() Reachable = true for forged TXT content (detail: %s)", got.Detail)
	}
}

func TestProbeAssignedResolverNXDOMAIN(t *testing.T) {
	// NXDOMAIN is a response. Bytes came back over the bearer, so the path
	// works. Treating it as failure is the bug this whole change exists to
	// avoid.
	addr := udpResponder(t, echoHeader(3))
	p := testProber(t)
	p.Targets = []string{"127.0.0.1:1"}
	got := p.Probe(context.Background(), []string{addr})
	assertReachableViaDNS(t, got)
}

func TestProbeAssignedResolverSERVFAIL(t *testing.T) {
	addr := udpResponder(t, echoHeader(2))
	p := testProber(t)
	p.Targets = []string{"127.0.0.1:1"}
	got := p.Probe(context.Background(), []string{addr})
	assertReachableViaDNS(t, got)
}

func TestProbeIgnoresMismatchedTransactionID(t *testing.T) {
	addr := udpResponder(t, func(req []byte) []byte {
		resp := echoHeader(0)(req)
		if resp != nil {
			resp[0] ^= 0xff // corrupt the ID
		}
		return resp
	})
	p := testProber(t)
	// Without this, a mismatched ID falls through to the default fallback
	// targets, which are real public resolvers. On a host with outbound
	// internet the TCP dial to one of those would succeed for real, making
	// this test pass or fail depending on host network access rather than on
	// the transaction-ID check it's meant to exercise.
	p.Targets = []string{"127.0.0.1:1"}
	got := p.Probe(context.Background(), []string{addr})
	if got.Reachable {
		t.Error("Probe() Reachable = true for a mismatched transaction ID, want false")
	}
}

func TestProbeSilentResolverFallsThroughToTargets(t *testing.T) {
	silent := udpResponder(t, func([]byte) []byte { return nil })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	p := testProber(t)
	p.Targets = []string{ln.Addr().String()}
	got := p.Probe(context.Background(), []string{silent})
	if !got.Reachable {
		t.Errorf("Probe() Reachable = false, want true via TCP target (detail: %s)", got.Detail)
	}
}

func TestProbeAllSilent(t *testing.T) {
	silent := udpResponder(t, func([]byte) []byte { return nil })
	p := testProber(t)
	// 127.0.0.1:1 has nothing listening; connect fails fast rather than
	// hanging, which is fine: we only assert the verdict.
	p.Targets = []string{"127.0.0.1:1"}
	got := p.Probe(context.Background(), []string{silent})
	if got.Reachable {
		t.Errorf("Probe() Reachable = true, want false (detail: %s)", got.Detail)
	}
	if got.Detail == "" {
		t.Error("Probe() Detail is empty on failure, want a description of what was tried")
	}
}

func TestProbeNoResolversUsesTargetsOnly(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	p := testProber(t)
	p.Targets = []string{ln.Addr().String()}
	got := p.Probe(context.Background(), nil)
	if !got.Reachable {
		t.Errorf("Probe() Reachable = false with no assigned resolvers, want true")
	}
}

func TestProbeRespectsCancelledContext(t *testing.T) {
	silent := udpResponder(t, func([]byte) []byte { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := testProber(t)
	p.Targets = []string{"127.0.0.1:1"}
	if got := p.Probe(ctx, []string{silent}); got.Reachable {
		t.Error("Probe() Reachable = true with a cancelled context, want false")
	}
}

func TestWithDefaultPort(t *testing.T) {
	// Resolver entries arrive as bare addresses from the network, but the
	// tests above hand the prober an ephemeral host:port. Both must work.
	tests := []struct {
		in   string
		want string
	}{
		{"172.16.64.2", "172.16.64.2:53"},
		{"172.16.64.2:53", "172.16.64.2:53"},
		{"127.0.0.1:54321", "127.0.0.1:54321"},
		{"2001:4860:4860::8888", "[2001:4860:4860::8888]:53"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := withDefaultPort(tc.in, "53"); got != tc.want {
				t.Errorf("withDefaultPort(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// decodeQName decodes a sequence of length-prefixed labels terminated by a
// zero-length root label, returning the dotted name and the number of bytes
// consumed (including the terminator).
func decodeQName(b []byte) (string, int, error) {
	var labels []string
	i := 0
	for {
		if i >= len(b) {
			return "", 0, fmt.Errorf("truncated qname")
		}
		n := int(b[i])
		i++
		if n == 0 {
			return strings.Join(labels, "."), i, nil
		}
		if i+n > len(b) {
			return "", 0, fmt.Errorf("label length %d exceeds remaining bytes", n)
		}
		labels = append(labels, string(b[i:i+n]))
		i += n
	}
}

func TestBuildDNSQuery(t *testing.T) {
	// Nothing else in this file inspects the question section: echoHeader
	// only ever echoes the 12-byte header. Without this test, a malformed
	// QNAME/QTYPE/QCLASS encoding would pass every other test in the package
	// while being rejected or ignored by a real resolver on target.
	tests := []string{
		probeName,
		"example.com",
		"a.b.c.d",
	}
	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			msg, id, err := buildDNSQuery(name)
			if err != nil {
				t.Fatalf("buildDNSQuery(%q) error: %v", name, err)
			}
			if len(msg) < 12 {
				t.Fatalf("message too short: %d bytes", len(msg))
			}
			if got := binary.BigEndian.Uint16(msg[0:2]); got != id {
				t.Errorf("header ID = %d, want %d (the returned id)", got, id)
			}
			if msg[2]&0x01 == 0 {
				t.Error("RD bit not set in flags")
			}
			if qdcount := binary.BigEndian.Uint16(msg[4:6]); qdcount != 1 {
				t.Errorf("QDCOUNT = %d, want 1", qdcount)
			}

			gotName, consumed, err := decodeQName(msg[12:])
			if err != nil {
				t.Fatalf("decodeQName: %v", err)
			}
			if gotName != name {
				t.Errorf("QNAME decoded = %q, want %q", gotName, name)
			}

			rest := msg[12+consumed:]
			if len(rest) != 4 {
				t.Fatalf("expected 4 trailing bytes (QTYPE+QCLASS) after QNAME, got %d", len(rest))
			}
			if qtype := binary.BigEndian.Uint16(rest[0:2]); qtype != 1 {
				t.Errorf("QTYPE = %d, want 1 (A)", qtype)
			}
			if qclass := binary.BigEndian.Uint16(rest[2:4]); qclass != 1 {
				t.Errorf("QCLASS = %d, want 1 (IN)", qclass)
			}
		})
	}
}

func TestBuildDNSQueryTransactionIDsVary(t *testing.T) {
	_, id1, err := buildDNSQuery(probeName)
	if err != nil {
		t.Fatalf("buildDNSQuery: %v", err)
	}
	_, id2, err := buildDNSQuery(probeName)
	if err != nil {
		t.Fatalf("buildDNSQuery: %v", err)
	}
	// Two random 16-bit IDs colliding is a ~1/65536 event; astronomically
	// unlikely to flake in practice, and a stuck/non-random source would show
	// up as a hard failure here rather than silently reusing IDs on target.
	if id1 == id2 {
		t.Errorf("two successive buildDNSQuery calls both returned id %d, want independent random IDs", id1)
	}
}

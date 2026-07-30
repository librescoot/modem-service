package health

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"time"
)

// DefaultConnectivityTargets is the fallback target list, used only when no
// network-assigned resolver answers. Public resolvers are wrong for a private
// APN, which is why -connectivity-targets exists.
var DefaultConnectivityTargets = []string{
	"8.8.8.8:53",
	"1.1.1.1:53",
	"9.9.9.9:53",
	"208.67.222.222:53",
}

// probeName is the name we ask for. The answer is never inspected: some
// networks run hijacking resolvers, so a returned A record proves only that
// the path works, which is exactly what we are measuring.
const probeName = "connectivity-probe.invalid"

// Result is the layer-8 verdict. It deliberately has no notion of path
// liveness: that belongs to internal/modem/link, and duplicating it here would
// give two sources of truth for one fact.
type Result struct {
	Reachable bool
	Detail    string
}

// Prober answers one question: did any target respond.
type Prober struct {
	Interface   string   // SO_BINDTODEVICE target; empty disables binding
	Targets     []string // host:port fallbacks
	DNSTimeout  time.Duration
	DialTimeout time.Duration
}

// NewProber returns a Prober with the default timeouts.
func NewProber(iface string, targets []string) *Prober {
	if len(targets) == 0 {
		targets = DefaultConnectivityTargets
	}
	return &Prober{
		Interface:   iface,
		Targets:     targets,
		DNSTimeout:  2 * time.Second,
		DialTimeout: 2 * time.Second,
	}
}

// Probe tries the network-assigned resolvers first, then the configured
// targets. Any DNS response, whatever the rcode, counts: NXDOMAIN, SERVFAIL and
// REFUSED all prove bytes made the round trip.
func (p *Prober) Probe(ctx context.Context, assignedDNS []string) Result {
	var tried []string

	for _, resolver := range assignedDNS {
		if err := ctx.Err(); err != nil {
			return Result{Detail: fmt.Sprintf("cancelled after %v", tried)}
		}
		addr := withDefaultPort(resolver, "53")
		if err := p.dnsQuery(ctx, addr); err == nil {
			return Result{Reachable: true, Detail: "dns " + addr + " answered"}
		} else {
			tried = append(tried, fmt.Sprintf("dns %s: %v", addr, err))
		}
	}

	for _, target := range p.Targets {
		if err := ctx.Err(); err != nil {
			return Result{Detail: fmt.Sprintf("cancelled after %v", tried)}
		}
		if err := p.tcpDial(ctx, target); err == nil {
			return Result{Reachable: true, Detail: "tcp " + target + " open"}
		} else {
			tried = append(tried, fmt.Sprintf("tcp %s: %v", target, err))
		}
	}

	return Result{Detail: "no target answered: " + strings.Join(tried, "; ")}
}

// dialer builds a dialer bound to the modem interface, so probe traffic cannot
// escape via the wifi or wired path the MDB might also have. bindToDevice
// (Task 1b) returns nil for a blank interface, which keeps the prober testable
// off-target.
func (p *Prober) dialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, Control: bindToDevice(p.Interface)}
}

func (p *Prober) tcpDial(ctx context.Context, target string) error {
	dialer := p.dialer(p.DialTimeout)
	conn, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		return err
	}
	return conn.Close()
}

// dnsQuery sends one A query and returns nil if any well-formed response with a
// matching transaction ID comes back.
func (p *Prober) dnsQuery(ctx context.Context, addr string) error {
	dialer := p.dialer(p.DialTimeout)
	conn, err := dialer.DialContext(ctx, "udp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	query, id, err := buildDNSQuery(probeName)
	if err != nil {
		return err
	}

	deadline := time.Now().Add(p.DNSTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	if _, err := conn.Write(query); err != nil {
		return err
	}

	buf := make([]byte, 512)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return err
		}
		if n < 12 {
			continue
		}
		if binary.BigEndian.Uint16(buf[0:2]) != id {
			continue // stale or spoofed, keep waiting until the deadline
		}
		if buf[2]&0x80 == 0 {
			continue // not a response
		}
		return nil // any rcode counts
	}
}

// buildDNSQuery assembles a standard recursive A query.
func buildDNSQuery(name string) ([]byte, uint16, error) {
	var idBytes [2]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return nil, 0, err
	}
	id := binary.BigEndian.Uint16(idBytes[:])

	msg := make([]byte, 0, 32+len(name))
	msg = binary.BigEndian.AppendUint16(msg, id)
	msg = binary.BigEndian.AppendUint16(msg, 0x0100) // RD
	msg = binary.BigEndian.AppendUint16(msg, 1)      // QDCOUNT
	msg = binary.BigEndian.AppendUint16(msg, 0)      // ANCOUNT
	msg = binary.BigEndian.AppendUint16(msg, 0)      // NSCOUNT
	msg = binary.BigEndian.AppendUint16(msg, 0)      // ARCOUNT

	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 {
			return nil, 0, fmt.Errorf("invalid dns label %q", label)
		}
		msg = append(msg, byte(len(label)))
		msg = append(msg, label...)
	}
	msg = append(msg, 0)                        // root
	msg = binary.BigEndian.AppendUint16(msg, 1) // QTYPE A
	msg = binary.BigEndian.AppendUint16(msg, 1) // QCLASS IN
	return msg, id, nil
}

// withDefaultPort appends a port when the entry does not already carry one.
func withDefaultPort(addr, port string) string {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return net.JoinHostPort(addr, port)
}

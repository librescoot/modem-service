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

// probeName is used by the legacy permissive probe. A deployment can instead
// configure a TXT record and expected value to prove end-to-end reachability.
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

	VerificationName  string // DNS TXT name; empty keeps the permissive probe
	VerificationValue string // exact TXT value expected from VerificationName
}

// NewProber returns a permissive Prober with the default timeouts.
func NewProber(iface string, targets []string) *Prober {
	return NewProberWithVerification(iface, targets, "", "")
}

// NewProberWithVerification returns a Prober that requires an exact TXT value
// when both verification arguments are non-empty. The empty default preserves
// the pre-verification behaviour for existing deployments.
func NewProberWithVerification(iface string, targets []string, name, value string) *Prober {
	if len(targets) == 0 {
		targets = DefaultConnectivityTargets
	}
	return &Prober{
		Interface:         iface,
		Targets:           targets,
		DNSTimeout:        2 * time.Second,
		DialTimeout:       2 * time.Second,
		VerificationName:  strings.TrimSuffix(strings.TrimSpace(name), "."),
		VerificationValue: value,
	}
}

// Probe tries the network-assigned resolvers first, then the configured
// targets. When TXT verification is configured, only the expected content
// counts. Otherwise the legacy behaviour remains: any DNS response, whatever
// the rcode, or an open fallback TCP port proves bytes made the round trip.
func (p *Prober) Probe(ctx context.Context, assignedDNS []string) Result {
	if p.VerificationName != "" || p.VerificationValue != "" {
		if p.VerificationName == "" || p.VerificationValue == "" {
			return Result{Detail: "connectivity verification requires both name and value"}
		}
		return p.probeVerifiedTXT(ctx, assignedDNS)
	}

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

func (p *Prober) probeVerifiedTXT(ctx context.Context, assignedDNS []string) Result {
	resolvers := make([]string, 0, len(assignedDNS)+len(p.Targets))
	resolvers = append(resolvers, assignedDNS...)
	resolvers = append(resolvers, p.Targets...)

	var tried []string
	for _, resolver := range resolvers {
		if err := ctx.Err(); err != nil {
			return Result{Detail: fmt.Sprintf("cancelled after %v", tried)}
		}
		addr := withDefaultPort(resolver, "53")
		if err := p.dnsTXTQuery(ctx, addr, p.VerificationName, p.VerificationValue); err == nil {
			return Result{Reachable: true, Detail: "verified dns " + addr}
		} else {
			tried = append(tried, fmt.Sprintf("verified dns %s: %v", addr, err))
		}
	}
	return Result{Detail: "no verified target answered: " + strings.Join(tried, "; ")}
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

// dnsTXTQuery requires a NOERROR response containing an exact TXT value.
func (p *Prober) dnsTXTQuery(ctx context.Context, addr, name, expected string) error {
	dialer := p.dialer(p.DialTimeout)
	conn, err := dialer.DialContext(ctx, "udp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	query, id, err := buildDNSQueryType(name, 16)
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

	buf := make([]byte, 1500)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return err
		}
		if n < 12 || binary.BigEndian.Uint16(buf[0:2]) != id || buf[2]&0x80 == 0 {
			continue
		}
		if rcode := buf[3] & 0x0f; rcode != 0 {
			return fmt.Errorf("rcode %d", rcode)
		}
		values, err := parseTXTAnswers(buf[:n])
		if err != nil {
			return err
		}
		for _, value := range values {
			if value == expected {
				return nil
			}
		}
		return fmt.Errorf("expected TXT value not found")
	}
}

// parseTXTAnswers returns TXT strings from a DNS response. It supports both
// literal and compressed owner names and concatenates multi-string TXT RDATA.
func parseTXTAnswers(msg []byte) ([]string, error) {
	if len(msg) < 12 {
		return nil, fmt.Errorf("short dns response")
	}
	off := 12
	qdcount := int(binary.BigEndian.Uint16(msg[4:6]))
	ancount := int(binary.BigEndian.Uint16(msg[6:8]))
	for i := 0; i < qdcount; i++ {
		var err error
		off, err = skipDNSName(msg, off)
		if err != nil {
			return nil, err
		}
		if off+4 > len(msg) {
			return nil, fmt.Errorf("truncated dns question")
		}
		off += 4
	}

	var values []string
	for i := 0; i < ancount; i++ {
		var err error
		off, err = skipDNSName(msg, off)
		if err != nil {
			return nil, err
		}
		if off+10 > len(msg) {
			return nil, fmt.Errorf("truncated dns answer")
		}
		typ := binary.BigEndian.Uint16(msg[off : off+2])
		class := binary.BigEndian.Uint16(msg[off+2 : off+4])
		rdlen := int(binary.BigEndian.Uint16(msg[off+8 : off+10]))
		off += 10
		if off+rdlen > len(msg) {
			return nil, fmt.Errorf("truncated dns rdata")
		}
		if typ == 16 && class == 1 {
			end := off + rdlen
			var value strings.Builder
			for pos := off; pos < end; {
				length := int(msg[pos])
				pos++
				if pos+length > end {
					return nil, fmt.Errorf("truncated TXT string")
				}
				value.Write(msg[pos : pos+length])
				pos += length
			}
			values = append(values, value.String())
		}
		off += rdlen
	}
	return values, nil
}

func skipDNSName(msg []byte, off int) (int, error) {
	for {
		if off >= len(msg) {
			return 0, fmt.Errorf("truncated dns name")
		}
		n := int(msg[off])
		switch {
		case n == 0:
			return off + 1, nil
		case n&0xc0 == 0xc0:
			if off+1 >= len(msg) {
				return 0, fmt.Errorf("truncated dns compression pointer")
			}
			return off + 2, nil
		case n&0xc0 != 0:
			return 0, fmt.Errorf("invalid dns label")
		default:
			off++
			if off+n > len(msg) {
				return 0, fmt.Errorf("truncated dns label")
			}
			off += n
		}
	}
}

// buildDNSQuery assembles a standard recursive A query.
func buildDNSQuery(name string) ([]byte, uint16, error) {
	return buildDNSQueryType(name, 1)
}

func buildDNSQueryType(name string, qtype uint16) ([]byte, uint16, error) {
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
	msg = append(msg, 0)                            // root
	msg = binary.BigEndian.AppendUint16(msg, qtype) // QTYPE
	msg = binary.BigEndian.AppendUint16(msg, 1)     // QCLASS IN
	return msg, id, nil
}

// withDefaultPort appends a port when the entry does not already carry one.
func withDefaultPort(addr, port string) string {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return net.JoinHostPort(addr, port)
}

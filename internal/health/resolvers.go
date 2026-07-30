package health

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

// resolvectlTimeout bounds the resolvectl call so a wedged systemd-resolved
// cannot stall the monitor loop.
const resolvectlTimeout = 3 * time.Second

// ResolverSources gathers the network-assigned resolvers from several places.
// No single source is reliable: ModemManager's bearer property can be
// unreadable while /etc/resolv.conf has the resolvers, and resolv.conf is
// global so it can carry entries belonging to another interface. Probes are
// bound to the modem interface, so a resolver from the wrong link simply fails
// to answer: a wasted probe, never a wrong verdict.
type ResolverSources struct {
	// Sources are consulted in order; results are concatenated and
	// deduplicated. A nil entry is skipped.
	Sources []func() []string
}

// Resolvers returns the deduplicated union, in source priority order.
func (r ResolverSources) Resolvers() []string {
	var out []string
	seen := make(map[string]bool)
	for _, src := range r.Sources {
		if src == nil {
			continue
		}
		for _, addr := range src() {
			if addr == "" || seen[addr] {
				continue
			}
			seen[addr] = true
			out = append(out, addr)
		}
	}
	return out
}

// ParseResolvConf extracts nameserver addresses from resolv.conf syntax.
func ParseResolvConf(data []byte) []string {
	var out []string
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		if net.ParseIP(fields[1]) != nil {
			out = append(out, fields[1])
		}
	}
	return out
}

// ParseResolvectl extracts the per-link resolvers from `resolvectl status
// <iface>` output.
func ParseResolvectl(out []byte) []string {
	var res []string
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		rest, ok := strings.CutPrefix(line, "DNS Servers:")
		if !ok {
			continue
		}
		for _, f := range strings.Fields(rest) {
			if net.ParseIP(f) != nil {
				res = append(res, f)
			}
		}
	}
	return res
}

// ResolvConfSource reads /etc/resolv.conf. Last resort: global, so it may
// carry resolvers from another link.
func ResolvConfSource() []string {
	data, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil
	}
	return ParseResolvConf(data)
}

// ResolvectlSource reads the resolvers systemd-resolved has for one link.
// Per-link, so resolvers belonging to wifi or the management VPN cannot leak
// in.
func ResolvectlSource(iface string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), resolvectlTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "resolvectl", "status", iface).Output()
	if err != nil {
		return nil
	}
	return ParseResolvectl(out)
}

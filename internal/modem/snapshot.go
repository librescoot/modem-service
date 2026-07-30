package modem

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"

	"modem-service/internal/mm"
	"modem-service/internal/modem/link"
)

// sysfsNetRoot is where the kernel exposes per-interface link state. A var so
// tests can point it elsewhere.
var sysfsNetRoot = "/sys/class/net"

// procNetRoute is the kernel's routing table in text form. Parsed directly
// rather than shelling out to `ip`.
var procNetRoute = "/proc/net/route"

// atCommandTimeout bounds one AT round trip. The AT ports are shared with GPS
// and ModemManager serialises access to them, so these run only when the
// assessor asks for them.
const atCommandTimeout = 5 * time.Second

// LinkSnapshot gathers connectivity layers 0 through 7. Every read degrades to
// a zero value rather than failing: link.Assess treats unknown as passing, so a
// flaky D-Bus read can never manufacture a modem reset.
//
// state is the modem state the caller already read on this tick; passing it in
// avoids a redundant round of D-Bus reads. A nil state leaves those fields
// empty, which link.Assess treats as passing.
//
// withAT controls the AT cross-check, which the previous assessment requests
// via Assessment.WantATCheck. In steady state it is false and no AT commands
// are issued.
func (m *Manager) LinkSnapshot(state *State, iface string, withAT bool) link.Snapshot {
	snap := link.Snapshot{}

	modemPath, err := m.FindModem()
	if err != nil {
		// ModemPresent stays false: a layer 0 failure, which is the truth.
		return snap
	}
	snap.ModemPresent = true
	snap.PrimaryPortOK = m.CheckPrimaryPort() == nil
	if err := m.CheckPowerState(); err == nil {
		snap.PowerState = PowerStateOn
	}

	if state != nil {
		snap.SIMLock = state.SIMLockStatus
		snap.Registration = state.Registration
		// Normalise: IfIPAddr carries the literal "UNKNOWN" sentinel when
		// unread, and comparing that against the bearer address would report a
		// netdev mismatch on a healthy modem.
		snap.NetdevIP = normalizeAddr(state.IfIPAddr)
	}

	if v, err := m.client.GetProperty(modemPath, mm.Modem3gppInterface, "PacketServiceState"); err == nil {
		snap.PacketService = packetServiceString(v)
	}

	if bearer, err := m.client.DataBearer(modemPath); err == nil {
		snap.BearerConnected = bearer.Connected
		snap.BearerSuspended = bearer.Suspended
		snap.BearerInterface = bearer.Interface
		snap.BearerIP = normalizeAddr(bearer.IP4.Address)
		snap.BearerAttempts = bearer.Stats.Attempts
		snap.BearerDuration = bearer.Stats.Duration
	}

	if withAT {
		m.readATCrossCheck(modemPath, &snap)
	}

	snap.Carrier = readCarrier(sysfsNetRoot, iface)
	snap.HasDefaultRoute = hasDefaultRoute(iface)

	return snap
}

// readATCrossCheck asks the modem directly what it thinks its PDP context and
// address are, so they can be compared against what ModemManager reports.
// Disagreement is a wedge that no reachability probe could distinguish from a
// filtered destination.
//
// ATChecked is left false if either command fails: an unanswered AT port must
// not read as "context inactive", which would request a remedy on no evidence.
func (m *Manager) readATCrossCheck(modemPath dbus.ObjectPath, snap *link.Snapshot) {
	out, err := m.client.SendCommand(modemPath, "AT+CGACT?", atCommandTimeout)
	if err != nil {
		return
	}
	active, parsed := ParseCGACT(out)
	if !parsed {
		// The modem answered something we cannot read (ERROR, empty, a
		// truncated line). Recording that as "context inactive" would fail
		// layer 5 because the AT port was busy.
		return
	}

	addrOut, err := m.client.SendCommand(modemPath, "AT+CGPADDR", atCommandTimeout)
	if err != nil {
		return
	}

	snap.ATChecked = true
	snap.CGACTActive = active
	snap.CGPADDR = ParseCGPADDR(addrOut)
}

// ParseCGACT reports whether any PDP context is active in an AT+CGACT? reply,
// and whether the reply could be parsed at all.
//
// The second return value matters: SendCommand returns a nil error for any
// successful D-Bus round trip, including a modem that answers "ERROR", an
// empty string, or a truncated line. Collapsing those into "no context is
// active" would fail layer 5 and request a bearer bounce because the AT port
// was momentarily busy, which is exactly what the treat-unknown-as-passing
// rule exists to prevent.
func ParseCGACT(out string) (active, parsed bool) {
	for _, line := range strings.Split(out, "\n") {
		_, rest, ok := strings.Cut(line, "+CGACT:")
		if !ok {
			continue
		}
		_, state, ok := strings.Cut(rest, ",")
		if !ok {
			continue
		}
		parsed = true
		if strings.TrimSpace(state) == "1" {
			return true, true
		}
	}
	return false, parsed
}

// ParseCGPADDR extracts the address from an AT+CGPADDR reply. An unassigned
// context reports 0.0.0.0, which is normalised to the empty string here rather
// than at the call site, so no caller can forget and end up comparing the
// placeholder against a real bearer address.
func ParseCGPADDR(out string) string {
	for _, line := range strings.Split(out, "\n") {
		_, rest, ok := strings.Cut(line, "+CGPADDR:")
		if !ok {
			continue
		}
		_, addr, ok := strings.Cut(rest, ",")
		if !ok {
			continue
		}
		return normalizeAddr(strings.Trim(strings.TrimSpace(addr), `"`))
	}
	return ""
}

// normalizeAddr maps the several ways this codebase spells "no address" onto
// the empty string, which is what link.Assess treats as unknown-and-passing.
// State.IfIPAddr in particular defaults to the literal "UNKNOWN".
func normalizeAddr(s string) string {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return ""
	case strings.EqualFold(s, "UNKNOWN"):
		return ""
	case s == "0.0.0.0":
		return ""
	}
	return s
}

// readCarrier reads the link state. operstate is useless on this hardware: the
// modem netdev is POINTOPOINT/NOARP and reports "unknown" even when fully up.
func readCarrier(root, iface string) bool {
	data, err := os.ReadFile(filepath.Join(root, iface, "carrier"))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == "1"
}

// hasDefaultRoute reports whether a default route exists via the interface.
func hasDefaultRoute(iface string) bool {
	data, err := os.ReadFile(procNetRoute)
	if err != nil {
		return false
	}
	return parseDefaultRoute(data, iface)
}

// parseDefaultRoute scans /proc/net/route content. Destination 00000000 marks
// the default route.
func parseDefaultRoute(data []byte, iface string) bool {
	lines := strings.Split(string(data), "\n")
	if len(lines) < 2 {
		return false
	}
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[0] == iface && fields[1] == "00000000" {
			return true
		}
	}
	return false
}

// packetServiceString maps MMModem3gppPacketServiceState onto the strings
// link.Assess expects. 0 unknown, 1 detached, 2 attached. An unrecognised
// width or value yields "", which Assess treats as unknown-and-passing.
func packetServiceString(v dbus.Variant) string {
	var n uint32
	switch t := v.Value().(type) {
	case uint32:
		n = t
	case int32:
		if t < 0 {
			return ""
		}
		n = uint32(t)
	default:
		return ""
	}
	switch n {
	case 1:
		return "detached"
	case 2:
		return "attached"
	}
	return ""
}

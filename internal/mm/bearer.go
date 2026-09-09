package mm

import (
	"github.com/godbus/dbus/v5"
	"github.com/pkg/errors"
)

// BearerInterface is the D-Bus interface name for a ModemManager bearer.
const BearerInterface = "org.freedesktop.ModemManager1.Bearer"

var (
	ErrNoDataBearer                 = errors.New("no data bearer with an interface")
	ErrBearerObservationUnavailable = errors.New("bearer interface unavailable")
)

// IP4Config is the subset of Bearer.Ip4Config we act on. DNS carries the
// resolvers the network delivered via PCO during bearer activation; there is
// no DHCP on a mobile bearer, so this is the only in-band source.
type IP4Config struct {
	Address string
	Prefix  uint32
	Gateway string
	DNS     []string
	MTU     uint32
}

// BearerStats mirrors Bearer.Stats. Note that ModemManager refreshes these on
// roughly a 30 second cadence, so byte deltas are useless for sub-30s liveness
// checks. Attempts and Duration are the useful fields for health: Attempts
// increments on each bearer reconnect and Duration resets with it, which
// together identify a session that is flapping.
//
// RxBytes/TxBytes cover the current connection attempt and zero on every
// reconnect. TotalRxBytes/TotalTxBytes cover the bearer object's whole life and
// do not, which makes them the better input for byte accounting: a reconnect
// between two polls otherwise loses whatever moved in the tail of the old
// session. They arrived in ModemManager 1.20, hence HaveTotals — MM omits the
// keys entirely on older versions rather than reporting zero.
type BearerStats struct {
	RxBytes      uint64
	TxBytes      uint64
	TotalRxBytes uint64
	TotalTxBytes uint64
	HaveTotals   bool
	Duration     uint64
	Attempts     uint32
}

// BearerInfo is one bearer's state.
type BearerInfo struct {
	Path           dbus.ObjectPath
	Connected      bool
	ConnectedKnown bool
	Suspended      bool
	SuspendedKnown bool
	Interface      string
	InterfaceKnown bool
	IP4            IP4Config
	IP4Known       bool
	Stats          BearerStats
	StatsKnown     bool
}

// ListBearers returns the modem's bearer object paths.
func (c *Client) ListBearers(modemPath dbus.ObjectPath) ([]dbus.ObjectPath, error) {
	variant, err := c.GetProperty(modemPath, ModemInterface, "Bearers")
	if err != nil {
		return nil, err
	}
	paths, ok := variant.Value().([]dbus.ObjectPath)
	if !ok {
		return nil, errors.New("invalid Bearers type")
	}
	return paths, nil
}

// GetBearerInfo reads one bearer's properties. All property failures degrade
// to zero values rather than failing the whole read: a bearer that is
// mid-teardown can drop properties independently. This allows robust bearer
// discovery even when individual properties are transiently unreadable.
func (c *Client) GetBearerInfo(bearerPath dbus.ObjectPath) (BearerInfo, error) {
	info := BearerInfo{Path: bearerPath}

	if v, err := c.GetProperty(bearerPath, BearerInterface, "Connected"); err == nil {
		info.Connected, info.ConnectedKnown = v.Value().(bool)
	}
	if v, err := c.GetProperty(bearerPath, BearerInterface, "Suspended"); err == nil {
		info.Suspended, info.SuspendedKnown = v.Value().(bool)
	}
	if v, err := c.GetProperty(bearerPath, BearerInterface, "Interface"); err == nil {
		info.Interface, info.InterfaceKnown = v.Value().(string)
	}
	if v, err := c.GetProperty(bearerPath, BearerInterface, "Ip4Config"); err == nil {
		if m, ok := v.Value().(map[string]dbus.Variant); ok {
			info.IP4 = parseIP4Config(m)
			info.IP4Known = true
		}
	}
	if v, err := c.GetProperty(bearerPath, BearerInterface, "Stats"); err == nil {
		if m, ok := v.Value().(map[string]dbus.Variant); ok {
			info.Stats = parseBearerStats(m)
			info.StatsKnown = bearerStatsKnown(m)
		}
	}
	return info, nil
}

// DataBearer enumerates the modem's bearers and returns the one carrying the
// data session. Never select by index: every modem reset creates fresh bearer
// objects and ModemManager hands out incrementing object paths, so on a modem
// that has been reset repeatedly the live bearer sits at an arbitrary index.
func (c *Client) DataBearer(modemPath dbus.ObjectPath) (BearerInfo, error) {
	paths, err := c.ListBearers(modemPath)
	if err != nil {
		return BearerInfo{}, err
	}
	infos := make([]BearerInfo, 0, len(paths))
	observationComplete := true
	for _, p := range paths {
		info, err := c.GetBearerInfo(p)
		if err != nil {
			observationComplete = false
			continue
		}
		if !info.InterfaceKnown {
			observationComplete = false
		}
		infos = append(infos, info)
	}
	got, ok := selectDataBearer(infos)
	if !ok {
		if !observationComplete {
			return BearerInfo{}, ErrBearerObservationUnavailable
		}
		return BearerInfo{}, ErrNoDataBearer
	}
	return got, nil
}

// selectDataBearer picks the data bearer out of a bearer list. The attach
// bearer (MM type default-attach) carries no interface and no IP config, which
// is the discriminator we use: it needs no MM version-specific BearerType.
func selectDataBearer(infos []BearerInfo) (BearerInfo, bool) {
	for _, info := range infos {
		if info.Interface != "" {
			return info, true
		}
	}
	return BearerInfo{}, false
}

func parseIP4Config(m map[string]dbus.Variant) IP4Config {
	cfg := IP4Config{}
	if v, ok := m["address"]; ok {
		cfg.Address, _ = v.Value().(string)
	}
	if v, ok := m["gateway"]; ok {
		cfg.Gateway, _ = v.Value().(string)
	}
	if v, ok := m["prefix"]; ok {
		cfg.Prefix = uint32(variantUint(v))
	}
	if v, ok := m["mtu"]; ok {
		cfg.MTU = uint32(variantUint(v))
	}
	for _, key := range []string{"dns1", "dns2", "dns3"} {
		v, ok := m[key]
		if !ok {
			continue
		}
		s, _ := v.Value().(string)
		if s != "" {
			cfg.DNS = append(cfg.DNS, s)
		}
	}
	return cfg
}

func bearerStatsKnown(m map[string]dbus.Variant) bool {
	_, rxOK := variantUintOK(m["rx-bytes"])
	_, txOK := variantUintOK(m["tx-bytes"])
	_, totalRxOK := variantUintOK(m["total-rx-bytes"])
	_, totalTxOK := variantUintOK(m["total-tx-bytes"])
	return (rxOK && txOK) || (totalRxOK && totalTxOK)
}

func parseBearerStats(m map[string]dbus.Variant) BearerStats {
	s := BearerStats{}
	if v, ok := m["rx-bytes"]; ok {
		s.RxBytes = variantUint(v)
	}
	if v, ok := m["tx-bytes"]; ok {
		s.TxBytes = variantUint(v)
	}
	// mmcli displays these as total-bytes-rx/total-bytes-tx; the dict keys are
	// the other way around.
	rxTotal, haveRx := m["total-rx-bytes"]
	txTotal, haveTx := m["total-tx-bytes"]
	if haveRx && haveTx {
		s.TotalRxBytes = variantUint(rxTotal)
		s.TotalTxBytes = variantUint(txTotal)
		s.HaveTotals = true
	}
	if v, ok := m["duration"]; ok {
		s.Duration = variantUint(v)
	}
	if v, ok := m["attempts"]; ok {
		s.Attempts = uint32(variantUint(v))
	}
	return s
}

// variantUint reads an unsigned integer out of a variant regardless of the
// width ModemManager chose for it.
func variantUint(v dbus.Variant) uint64 {
	n, _ := variantUintOK(v)
	return n
}

func variantUintOK(v dbus.Variant) (uint64, bool) {
	switch n := v.Value().(type) {
	case uint64:
		return n, true
	case uint32:
		return uint64(n), true
	case uint16:
		return uint64(n), true
	case int64:
		if n >= 0 {
			return uint64(n), true
		}
	case int32:
		if n >= 0 {
			return uint64(n), true
		}
	}
	return 0, false
}

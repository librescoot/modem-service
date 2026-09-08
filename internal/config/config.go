package config

import (
	"flag"
	"strings"
	"time"
)

type Config struct {
	RedisURL          string
	InternetCheckTime time.Duration

	// InternetCheckMaxInterval bounds the probe's exponential backoff while
	// healthy. Local layer checks are unaffected and still run every tick.
	InternetCheckMaxInterval time.Duration

	connectivityTargetsRaw string

	// ConnectivityVerificationName and ConnectivityVerificationValue enable a
	// content-verified DNS TXT probe. Both are empty by default, preserving the
	// legacy permissive probe until a deployment configures its own record.
	ConnectivityVerificationName  string
	ConnectivityVerificationValue string

	Interface    string
	GpsdServer   string
	SuplServer   string
	SMSKeepalive bool
	Debug        bool

	// DataUsageFile is where the cellular byte totals are persisted. Empty
	// keeps the counters in memory only, which is what a dev box wants.
	DataUsageFile string
}

func New() *Config {
	cfg := &Config{}

	flag.StringVar(&cfg.RedisURL, "redis-url", "redis://127.0.0.1:6379", "Redis URL")
	flag.DurationVar(&cfg.InternetCheckTime, "internet-check-time", 30*time.Second, "Internet check interval")
	// Only the network probe backs off; the local layer checks still run every
	// InternetCheckTime, so a real fault is still caught within one tick.
	flag.DurationVar(&cfg.InternetCheckMaxInterval, "internet-check-max-interval",
		5*time.Minute, "Upper bound for connectivity probe backoff")
	// Fallback probe targets, used only when no network-assigned resolver
	// answers. Fleets on a private APN point this at something reachable;
	// public resolvers are wrong there and time out rather than refusing.
	flag.StringVar(&cfg.connectivityTargetsRaw, "connectivity-targets",
		"8.8.8.8:53,1.1.1.1:53,9.9.9.9:53,208.67.222.222:53",
		"Comma-separated host:port fallback targets for the connectivity probe")
	flag.StringVar(&cfg.ConnectivityVerificationName, "connectivity-verification-name", "",
		"DNS name whose TXT record verifies internet reachability (empty disables verification)")
	flag.StringVar(&cfg.ConnectivityVerificationValue, "connectivity-verification-value", "",
		"Expected TXT value for the connectivity verification name (empty disables verification)")
	flag.StringVar(&cfg.Interface, "interface", "wwan0", "Network interface to monitor")
	flag.StringVar(&cfg.GpsdServer, "gpsd-server", "localhost:2947", "GPSD server address")
	// Port 7276 is the plain-TCP SUPL port; 7275 is TLS-only and requires
	// a cert that we don't ship (and CGPSSSL=0). Forum-validated config.
	flag.StringVar(&cfg.SuplServer, "supl-server", "supl.google.com:7276", "SUPL server for A-GPS")
	// Off by default: the keepalive works around one operator's short CS
	// implicit-detach timer, and its self-call trick is only free when the
	// SIM's mailbox doesn't pick up busy calls. Enable per fleet/SIM setup.
	flag.BoolVar(&cfg.SMSKeepalive, "sms-keepalive", false, "Keep the CS (SGs) registration alive for SMS delivery via periodic self-calls")
	flag.BoolVar(&cfg.Debug, "debug", false, "Enable debug logging")
	// Written at power transitions and shutdown, not on a timer: see
	// internal/datausage. /data is the only writable partition that survives
	// an OTA, and it is flat by convention (/data/trips.db, /data/profiles.db).
	// A /data/modem-service/ directory would also collide with the staged
	// binary of the same name that the deploy instructions leave lying around.
	flag.StringVar(&cfg.DataUsageFile, "data-usage-file", "/data/internet-usage.json",
		"Where to persist cellular byte totals; empty keeps them in memory only")

	return cfg
}

// ConnectivityTargets splits the -connectivity-targets flag. Call after
// flag.Parse().
func (c *Config) ConnectivityTargets() []string {
	var out []string
	for _, t := range strings.Split(c.connectivityTargetsRaw, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

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

	Interface    string
	GpsdServer   string
	SuplServer   string
	SMSKeepalive bool
	Debug        bool
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

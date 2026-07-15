package config

import (
	"flag"
	"time"
)

type Config struct {
	RedisURL          string
	InternetCheckTime time.Duration
	Interface         string
	GpsdServer        string
	SuplServer        string
	SMSKeepalive      bool
	Debug             bool
}

func New() *Config {
	cfg := &Config{}

	flag.StringVar(&cfg.RedisURL, "redis-url", "redis://127.0.0.1:6379", "Redis URL")
	flag.DurationVar(&cfg.InternetCheckTime, "internet-check-time", 30*time.Second, "Internet check interval")
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

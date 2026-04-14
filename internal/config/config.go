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
	Debug             bool
}

func New() *Config {
	cfg := &Config{}

	flag.StringVar(&cfg.RedisURL, "redis-url", "redis://127.0.0.1:6379", "Redis URL")
	flag.DurationVar(&cfg.InternetCheckTime, "internet-check-time", 30*time.Second, "Internet check interval")
	flag.StringVar(&cfg.Interface, "interface", "wwu1i5", "Network interface to monitor")
	flag.StringVar(&cfg.GpsdServer, "gpsd-server", "localhost:2947", "GPSD server address")
	// Port 7276 is the plain-TCP SUPL port; 7275 is TLS-only and requires
	// a cert that we don't ship (and CGPSSSL=0). Forum-validated config.
	flag.StringVar(&cfg.SuplServer, "supl-server", "supl.google.com:7276", "SUPL server for A-GPS")
	flag.BoolVar(&cfg.Debug, "debug", false, "Enable debug logging")

	return cfg
}

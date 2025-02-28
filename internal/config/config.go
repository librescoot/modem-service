package config

import (
	"flag"
	"time"
)

type Config struct {
	RedisURL          string
	PollingTime       time.Duration
	InternetCheckTime time.Duration
	Interface         string
	GpsdServer        string
}

func New() *Config {
	cfg := &Config{}

	flag.StringVar(&cfg.RedisURL, "redis-url", "redis://127.0.0.1:6379", "Redis URL")
	flag.DurationVar(&cfg.PollingTime, "polling-time", 5*time.Second, "Polling interval")
	flag.DurationVar(&cfg.InternetCheckTime, "internet-check-time", 30*time.Second, "Internet check interval")
	flag.StringVar(&cfg.Interface, "interface", "wwan0", "Network interface to monitor")
	flag.StringVar(&cfg.GpsdServer, "gpsd-server", "localhost:2947", "GPSD server address")

	return cfg
}

func (c *Config) Parse() {
	flag.Parse()
}

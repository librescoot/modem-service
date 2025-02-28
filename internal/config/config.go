package config

import (
	"flag"
	"time"
)

// Config holds the application configuration
type Config struct {
	RedisURL          string
	PollingTime       time.Duration
	InternetCheckTime time.Duration
	Interface         string
}

// New creates a new Config with values from command line flags
func New() *Config {
	cfg := &Config{}

	flag.StringVar(&cfg.RedisURL, "redis-url", "redis://127.0.0.1:6379", "Redis URL")
	flag.DurationVar(&cfg.PollingTime, "polling-time", 5*time.Second, "Polling interval")
	flag.DurationVar(&cfg.InternetCheckTime, "internet-check-time", 30*time.Second, "Internet check interval")
	flag.StringVar(&cfg.Interface, "interface", "wwan0", "Network interface to monitor")

	return cfg
}

// Parse parses the command line flags
func (c *Config) Parse() {
	flag.Parse()
}

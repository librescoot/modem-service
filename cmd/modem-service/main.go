package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"modem-service/internal/config"
	"modem-service/internal/service"
)

var version = "dev" // Default version, can be overridden during build

func main() {
	// Create config first to register all flags
	cfg := config.New()

	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("modem-service %s\n", version)
		return
	}

	// Create logger - skip timestamps if running under systemd/journald
	var logger *log.Logger
	if os.Getenv("JOURNAL_STREAM") != "" {
		logger = log.New(os.Stdout, "", 0)
	} else {
		logger = log.New(os.Stdout, "modem-service: ", log.LstdFlags|log.Lmsgprefix)
	}

	// Create context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create service
	svc, err := service.New(cfg, logger, version)
	if err != nil {
		logger.Fatalf("Failed to create service: %v", err)
	}

	// Handle signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		cancel()
	}()

	// Run service
	if err := svc.Run(ctx); err != nil {
		logger.Fatalf("Service failed: %v", err)
	}
}

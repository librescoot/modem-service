package service

import (
	"errors"
	"log"
	"os"
	"testing"

	"modem-service/internal/datausage"
)

// newUsageService builds the smallest Service that can publish data usage, with
// the Redis write captured instead of performed.
func newUsageService(t *testing.T) (*Service, *[]map[string]interface{}, *error) {
	t.Helper()
	var writes []map[string]interface{}
	var writeErr error
	s := &Service{
		Logger: log.New(os.Stdout, "TEST: ", 0),
		// Empty path: in-memory only, nothing touches the disk in tests.
		Usage: datausage.New("", nil),
		publishUsageFn: func(fields map[string]interface{}) error {
			if writeErr != nil {
				return writeErr
			}
			writes = append(writes, fields)
			return nil
		},
	}
	return s, &writes, &writeErr
}

func TestPublishDataUsageWritesOnlyOnChange(t *testing.T) {
	s, writes, _ := newUsageService(t)

	s.publishDataUsage()
	if len(*writes) != 1 {
		t.Fatalf("first publish wrote %d times, want 1", len(*writes))
	}
	if got := (*writes)[0]["rx-bytes"]; got != "0" {
		t.Fatalf("rx-bytes = %v, want the zero baseline as a string", got)
	}

	// An idle modem must not re-publish the same totals every tick.
	s.publishDataUsage()
	s.publishDataUsage()
	if len(*writes) != 1 {
		t.Fatalf("idle ticks wrote %d times, want 1", len(*writes))
	}

	s.Usage.Observe(datausage.Sample{BearerPath: "/b/1", RxBytes: 2048, TxBytes: 512})
	s.publishDataUsage()
	if len(*writes) != 2 {
		t.Fatalf("wrote %d times after traffic, want 2", len(*writes))
	}
	last := (*writes)[1]
	if last["rx-bytes"] != "2048" || last["tx-bytes"] != "512" {
		t.Fatalf("published %v, want rx 2048 tx 512", last)
	}
	if last["since"] != s.Usage.Totals().Since {
		t.Fatalf("since = %v, want %q", last["since"], s.Usage.Totals().Since)
	}
}

func TestPublishDataUsageRetriesAfterFailure(t *testing.T) {
	s, writes, writeErr := newUsageService(t)

	*writeErr = errors.New("redis is down")
	s.Usage.Observe(datausage.Sample{BearerPath: "/b/1", RxBytes: 10, TxBytes: 5})
	s.publishDataUsage()
	if len(*writes) != 0 {
		t.Fatalf("failed write recorded %d entries", len(*writes))
	}

	// The gate must not have advanced on a failed write, or the totals would
	// sit unpublished until the next byte moves.
	*writeErr = nil
	s.publishDataUsage()
	if len(*writes) != 1 {
		t.Fatalf("wrote %d times after recovery, want the retry", len(*writes))
	}
	if got := (*writes)[0]["rx-bytes"]; got != "10" {
		t.Fatalf("rx-bytes = %v, want 10", got)
	}
}

func TestPublishDataUsageCarriesRoamingSplit(t *testing.T) {
	s, writes, _ := newUsageService(t)

	s.Usage.Observe(datausage.Sample{BearerPath: "/b/1", RxBytes: 100, TxBytes: 20})
	s.Usage.Observe(datausage.Sample{BearerPath: "/b/1", RxBytes: 400, TxBytes: 60, Roaming: true})
	s.publishDataUsage()

	last := (*writes)[len(*writes)-1]
	if last["rx-bytes"] != "400" || last["rx-bytes-roaming"] != "300" {
		t.Fatalf("published %v, want rx 400 with 300 roaming", last)
	}
	if last["tx-bytes"] != "60" || last["tx-bytes-roaming"] != "40" {
		t.Fatalf("published %v, want tx 60 with 40 roaming", last)
	}
}

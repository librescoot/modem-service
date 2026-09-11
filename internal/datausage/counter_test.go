package datausage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newTestCounter builds a Counter with a controllable clock. The clock pointer
// lets a test advance time to exercise the backstop.
func newTestCounter(t *testing.T, path string, clock *time.Time) *Counter {
	t.Helper()
	c := &Counter{path: path, logf: func(string, ...any) {}, now: func() time.Time { return *clock }}
	c.load()
	return c
}

func TestObserveAccumulatesDeltas(t *testing.T) {
	clock := time.Unix(1700000000, 0).UTC()
	c := newTestCounter(t, "", &clock)

	c.Observe(Sample{BearerPath: "/b/1", RxBytes: 100, TxBytes: 20})
	c.Observe(Sample{BearerPath: "/b/1", RxBytes: 250, TxBytes: 45})

	got := c.Totals()
	if got.RxBytes != 250 || got.TxBytes != 45 {
		t.Fatalf("totals = rx %d tx %d, want rx 250 tx 45", got.RxBytes, got.TxBytes)
	}
}

func TestObserveTreatsDecreaseAsReset(t *testing.T) {
	clock := time.Unix(1700000000, 0).UTC()
	c := newTestCounter(t, "", &clock)

	c.Observe(Sample{BearerPath: "/b/1", RxBytes: 1000, TxBytes: 500})
	c.Observe(Sample{BearerPath: "/b/1", RxBytes: 30, TxBytes: 10})
	c.Observe(Sample{BearerPath: "/b/1", RxBytes: 80, TxBytes: 15})

	got := c.Totals()
	if got.RxBytes != 1080 || got.TxBytes != 515 {
		t.Fatalf("totals = rx %d tx %d, want rx 1080 tx 515", got.RxBytes, got.TxBytes)
	}
}

// A reset that races the poll can show one counter up and the other down. Both
// must be treated as restarted, otherwise the rising one is credited a delta
// against a baseline that no longer exists.
func TestObserveTreatsMixedDecreaseAsReset(t *testing.T) {
	clock := time.Unix(1700000000, 0).UTC()
	c := newTestCounter(t, "", &clock)

	c.Observe(Sample{BearerPath: "/b/1", RxBytes: 1000, TxBytes: 500})
	c.Observe(Sample{BearerPath: "/b/1", RxBytes: 1200, TxBytes: 40})

	got := c.Totals()
	if got.RxBytes != 2200 || got.TxBytes != 540 {
		t.Fatalf("totals = rx %d tx %d, want rx 2200 tx 540", got.RxBytes, got.TxBytes)
	}
}

// ModemManager hands out a fresh bearer path on every reset, so a new path is a
// new session even when its counters read higher than the old ones.
func TestObserveTreatsNewBearerAsReset(t *testing.T) {
	clock := time.Unix(1700000000, 0).UTC()
	c := newTestCounter(t, "", &clock)

	c.Observe(Sample{BearerPath: "/b/1", RxBytes: 100, TxBytes: 50})
	c.Observe(Sample{BearerPath: "/b/2", RxBytes: 400, TxBytes: 60})

	got := c.Totals()
	if got.RxBytes != 500 || got.TxBytes != 110 {
		t.Fatalf("totals = rx %d tx %d, want rx 500 tx 110", got.RxBytes, got.TxBytes)
	}
}

func TestObserveSplitsRoaming(t *testing.T) {
	clock := time.Unix(1700000000, 0).UTC()
	c := newTestCounter(t, "", &clock)

	c.Observe(Sample{BearerPath: "/b/1", RxBytes: 100, TxBytes: 10})
	c.Observe(Sample{BearerPath: "/b/1", RxBytes: 300, TxBytes: 40, Roaming: true})
	c.Observe(Sample{BearerPath: "/b/1", RxBytes: 350, TxBytes: 45})

	got := c.Totals()
	if got.RxBytes != 350 || got.TxBytes != 45 {
		t.Fatalf("totals = rx %d tx %d, want rx 350 tx 45", got.RxBytes, got.TxBytes)
	}
	// Only the delta observed while roaming counts toward the roaming subset.
	if got.RxBytesRoaming != 200 || got.TxBytesRoaming != 30 {
		t.Fatalf("roaming = rx %d tx %d, want rx 200 tx 30", got.RxBytesRoaming, got.TxBytesRoaming)
	}
}

func TestFlushRoundTrip(t *testing.T) {
	clock := time.Unix(1700000000, 0).UTC()
	path := filepath.Join(t.TempDir(), "nested", "data-usage.json")

	c := newTestCounter(t, path, &clock)
	c.Observe(Sample{BearerPath: "/b/1", RxBytes: 4096, TxBytes: 1024, Roaming: true})
	if err := c.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	reloaded := newTestCounter(t, path, &clock)
	got := reloaded.Totals()
	if got.RxBytes != 4096 || got.TxBytes != 1024 || got.RxBytesRoaming != 4096 {
		t.Fatalf("reloaded totals = %+v", got)
	}
	if got.Since != c.Totals().Since {
		t.Fatalf("Since = %q, want it carried over as %q", got.Since, c.Totals().Since)
	}

	reloaded.Observe(Sample{BearerPath: "/b/9", RxBytes: 500, TxBytes: 100})
	if rx := reloaded.Totals().RxBytes; rx != 4596 {
		t.Fatalf("rx after reload = %d, want 4596", rx)
	}
}

// ModemManager keeps running across a modem-service restart, so the restarted
// service usually meets the same bearer still connected and still counting. Its
// running total is already in the stored total and must not be added again.
func TestRestartContinuesFromStoredReading(t *testing.T) {
	clock := time.Unix(1700000000, 0).UTC()
	path := filepath.Join(t.TempDir(), "data-usage.json")

	c := newTestCounter(t, path, &clock)
	c.Observe(Sample{BearerPath: "/b/1", RxBytes: 50 << 20, TxBytes: 10 << 20})
	if err := c.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	restarted := newTestCounter(t, path, &clock)
	restarted.Observe(Sample{BearerPath: "/b/1", RxBytes: 60 << 20, TxBytes: 12 << 20})

	got := restarted.Totals()
	if got.RxBytes != 60<<20 || got.TxBytes != 12<<20 {
		t.Fatalf("totals = rx %d tx %d, want rx %d tx %d (the reading was counted twice)",
			got.RxBytes, got.TxBytes, 60<<20, 12<<20)
	}
}

// A file written before the last reading was stored has no baseline, and the
// same is true of a first run. Counting the next reading in full is the right
// answer there: it is the only figure available, and it is closer to the truth
// than dropping the session so far.
func TestRestartWithoutStoredReadingCountsInFull(t *testing.T) {
	clock := time.Unix(1700000000, 0).UTC()
	path := filepath.Join(t.TempDir(), "data-usage.json")
	if err := os.WriteFile(path, []byte(`{"rx-bytes":100,"tx-bytes":50,"since":"2026-01-01T00:00:00Z"}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	c := newTestCounter(t, path, &clock)
	c.Observe(Sample{BearerPath: "/b/1", RxBytes: 400, TxBytes: 200})

	got := c.Totals()
	if got.RxBytes != 500 || got.TxBytes != 250 {
		t.Fatalf("totals = rx %d tx %d, want rx 500 tx 250", got.RxBytes, got.TxBytes)
	}
	if got.Since != "2026-01-01T00:00:00Z" {
		t.Fatalf("Since = %q, want the stored baseline preserved", got.Since)
	}
}

func TestFlushIsAtomicAndReadable(t *testing.T) {
	clock := time.Unix(1700000000, 0).UTC()
	dir := t.TempDir()
	path := filepath.Join(dir, "data-usage.json")

	c := newTestCounter(t, path, &clock)
	c.Observe(Sample{BearerPath: "/b/1", RxBytes: 7, TxBytes: 3})
	if err := c.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("dir holds %d entries, want just the totals file: %v", len(entries), entries)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Fatalf("mode = %v, want 0644", perm)
	}

	// The field names are the Redis field names; a rename here is a consumer
	// break, so pin them.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("stored file is not JSON: %v", err)
	}
	for _, field := range []string{
		"rx-bytes", "tx-bytes", "rx-bytes-roaming", "tx-bytes-roaming", "since",
		"last-bearer", "last-rx-bytes", "last-tx-bytes",
	} {
		if _, ok := raw[field]; !ok {
			t.Errorf("stored file has no %q field: %s", field, data)
		}
	}
}

func TestFlushSkipsWhenNothingChanged(t *testing.T) {
	clock := time.Unix(1700000000, 0).UTC()
	path := filepath.Join(t.TempDir(), "data-usage.json")

	c := newTestCounter(t, path, &clock)
	if err := c.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Flush wrote a file with nothing to record (err = %v)", err)
	}

	c.Observe(Sample{BearerPath: "/b/1", RxBytes: 10, TxBytes: 5})
	if err := c.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	first, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	// A second flush with no new traffic must not touch the file: this is the
	// whole point of the write policy.
	clock = clock.Add(time.Hour)
	if err := c.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	second, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !first.ModTime().Equal(second.ModTime()) {
		t.Fatal("Flush rewrote the file with nothing new to record")
	}
}

func TestBackstopOnlyWritesAfterInterval(t *testing.T) {
	clock := time.Unix(1700000000, 0).UTC()
	path := filepath.Join(t.TempDir(), "data-usage.json")

	c := newTestCounter(t, path, &clock)
	c.Observe(Sample{BearerPath: "/b/1", RxBytes: 1 << 30, TxBytes: 1 << 30})

	clock = clock.Add(backstopInterval - time.Minute)
	c.Backstop()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("Backstop wrote before the interval elapsed; bytes must not trigger a write")
	}

	clock = clock.Add(2 * time.Minute)
	c.Backstop()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Backstop did not write after the interval elapsed: %v", err)
	}
}

func TestLoadOfCorruptFileCountsFromZero(t *testing.T) {
	clock := time.Unix(1700000000, 0).UTC()
	path := filepath.Join(t.TempDir(), "data-usage.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	c := newTestCounter(t, path, &clock)
	got := c.Totals()
	if got.RxBytes != 0 || got.TxBytes != 0 {
		t.Fatalf("totals = %+v, want zero", got)
	}
	// A fresh Since is how a consumer differencing totals learns its baseline
	// is gone.
	if got.Since != clock.Format(time.RFC3339) {
		t.Fatalf("Since = %q, want the current time %q", got.Since, clock.Format(time.RFC3339))
	}
}

func TestEmptyPathDisablesPersistence(t *testing.T) {
	clock := time.Unix(1700000000, 0).UTC()
	c := newTestCounter(t, "", &clock)

	c.Observe(Sample{BearerPath: "/b/1", RxBytes: 10, TxBytes: 5})
	if err := c.Flush(); err != nil {
		t.Fatalf("Flush with no path: %v", err)
	}
	if got := c.Totals().RxBytes; got != 10 {
		t.Fatalf("rx = %d, want the counter still accumulating in memory", got)
	}
}

func TestObservePersistsZeroCounterReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	c := New(path, nil)
	c.Observe(Sample{BearerPath: "/b/1", RxBytes: 100, TxBytes: 50})
	if err := c.Flush(); err != nil {
		t.Fatal(err)
	}
	c.Observe(Sample{BearerPath: "/b/1"})
	if err := c.Flush(); err != nil {
		t.Fatal(err)
	}

	resumed := New(path, nil)
	resumed.Observe(Sample{BearerPath: "/b/1", RxBytes: 150, TxBytes: 75})
	got := resumed.Totals()
	if got.RxBytes != 250 || got.TxBytes != 125 {
		t.Fatalf("totals after zero reset and restart = %+v, want rx=250 tx=125", got)
	}
	if c.dirty {
		t.Fatal("successful baseline flush left counter dirty")
	}
	c.Observe(Sample{BearerPath: "/b/1"})
	if c.dirty {
		t.Fatal("unchanged zero sample dirtied persisted baseline")
	}
}

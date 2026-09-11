// Package datausage persists monotonic totals from per-bearer byte counters.
package datausage

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// backstopInterval limits loss on a hard power cut without writing eMMC on
// every poll; normal persistence happens at power transitions and shutdown.
const backstopInterval = 6 * time.Hour

// Totals is the persisted state: bytes since Since, never decreasing.
//
// The roaming figures are a subset of RxBytes/TxBytes, not a separate pot:
// roaming traffic is counted in both. Consumers wanting home-only traffic
// subtract.
type Totals struct {
	RxBytes        uint64 `json:"rx-bytes"`
	TxBytes        uint64 `json:"tx-bytes"`
	RxBytesRoaming uint64 `json:"rx-bytes-roaming"`
	TxBytesRoaming uint64 `json:"tx-bytes-roaming"`

	// Since is when counting started, RFC 3339. It changes only when the
	// stored file is missing or unreadable, which is how a consumer that
	// differences totals across time can tell its baseline was thrown away.
	Since string `json:"since"`
}

// persisted includes the live baseline so restarting against the same bearer
// does not count its running total twice. Totals and baseline are atomic.
type persisted struct {
	Totals
	LastBearer string `json:"last-bearer"`
	LastRx     uint64 `json:"last-rx-bytes"`
	LastTx     uint64 `json:"last-tx-bytes"`
}

// Sample is one reading of the live bearer's counters.
//
// BearerPath identifies the session: ModemManager hands out a fresh bearer
// object path on every reset, so a change means the counters restarted even if
// they happen to read higher than the last ones.
type Sample struct {
	BearerPath string
	RxBytes    uint64
	TxBytes    uint64
	Roaming    bool
}

// Counter accumulates samples. Safe for concurrent use.
type Counter struct {
	mu   sync.Mutex
	path string
	logf func(string, ...any)
	now  func() time.Time

	totals Totals

	haveLast bool
	lastPath string
	lastRx   uint64
	lastTx   uint64

	dirty     bool
	lastFlush time.Time
}

// New returns a Counter primed from path. A missing or unreadable file is not
// an error: counting starts from zero with a fresh Since, because a total that
// refuses to run without its history is worse than one that admits it lost it.
// An empty path disables persistence entirely.
func New(path string, logger *log.Logger) *Counter {
	c := &Counter{path: path, now: time.Now, logf: func(string, ...any) {}}
	if logger != nil {
		c.logf = logger.Printf
	}
	c.load()
	return c
}

func (c *Counter) load() {
	c.lastFlush = c.now()
	fresh := func() {
		c.totals = Totals{Since: c.now().UTC().Format(time.RFC3339)}
	}

	if c.path == "" {
		fresh()
		return
	}
	data, err := os.ReadFile(c.path)
	if err != nil {
		if !os.IsNotExist(err) {
			c.logf("data usage: cannot read %s, counting from zero: %v", c.path, err)
		}
		fresh()
		return
	}
	var p persisted
	if err := json.Unmarshal(data, &p); err != nil {
		c.logf("data usage: %s is unreadable, counting from zero: %v", c.path, err)
		fresh()
		return
	}
	if p.Since == "" {
		p.Since = c.now().UTC().Format(time.RFC3339)
	}
	c.totals = p.Totals
	// Without a stored baseline, the next reading must be counted in full.
	if p.LastBearer != "" {
		c.haveLast, c.lastPath, c.lastRx, c.lastTx = true, p.LastBearer, p.LastRx, p.LastTx
	}
	c.logf("data usage: resuming at rx=%d tx=%d since %s", p.RxBytes, p.TxBytes, p.Since)
}

// Observe folds one bearer reading into the totals.
//
// The delta is the difference from the previous reading, except when the
// session restarted — a different bearer, or either counter going backwards —
// in which case the whole reading is new traffic. Both counters are treated as
// restarted when either one is: a reset zeroes them together, so a mixed
// reading is a reset that raced the poll, not a genuine rx-only decrease.
//
// Call this only with a reading that was actually taken. A failed bearer read
// passed in as zeroes would look like a reset and double-count the session so
// far on the next successful poll.
func (c *Counter) Observe(s Sample) {
	c.mu.Lock()
	defer c.mu.Unlock()

	rx, tx := s.RxBytes, s.TxBytes
	continuing := c.haveLast && s.BearerPath == c.lastPath && rx >= c.lastRx && tx >= c.lastTx
	if continuing {
		rx -= c.lastRx
		tx -= c.lastTx
	}
	// A zero-byte reset still changes the baseline needed after a restart.
	if !c.haveLast || c.lastPath != s.BearerPath || c.lastRx != s.RxBytes || c.lastTx != s.TxBytes {
		c.dirty = true
	}
	c.haveLast, c.lastPath, c.lastRx, c.lastTx = true, s.BearerPath, s.RxBytes, s.TxBytes

	if rx == 0 && tx == 0 {
		return
	}
	c.totals.RxBytes += rx
	c.totals.TxBytes += tx
	if s.Roaming {
		c.totals.RxBytesRoaming += rx
		c.totals.TxBytesRoaming += tx
	}
}

// Totals returns the current totals.
func (c *Counter) Totals() Totals {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.totals
}

// Backstop persists dirty totals after backstopInterval.
func (c *Counter) Backstop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.dirty || c.now().Sub(c.lastFlush) < backstopInterval {
		return
	}
	if err := c.flushLocked(); err != nil {
		c.logf("data usage: %v", err)
	}
}

// Flush persists unconditionally, if there is anything new to write. Call it at
// power transitions and on shutdown; those are the intended write points.
func (c *Counter) Flush() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.flushLocked()
}

func (c *Counter) flushLocked() error {
	if c.path == "" || !c.dirty {
		return nil
	}
	if err := c.writeAtomic(); err != nil {
		return err
	}
	c.dirty, c.lastFlush = false, c.now()
	return nil
}

// writeAtomic writes the totals via temp file and rename, so a power cut
// mid-write leaves either the old file or the new one, never a half-written
// total that load() would then reject and reset to zero.
func (c *Counter) writeAtomic() error {
	data, err := json.Marshal(persisted{
		Totals:     c.totals,
		LastBearer: c.lastPath,
		LastRx:     c.lastRx,
		LastTx:     c.lastTx,
	})
	if err != nil {
		return fmt.Errorf("encode totals: %w", err)
	}
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".data-usage-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	name := tmp.Name()
	// No-op once the rename below has succeeded; cleans up every failure path.
	defer os.Remove(name)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	// CreateTemp makes the file 0600; the totals are not a secret and other
	// tooling on the vehicle should be able to read them.
	if err := os.Chmod(name, 0o644); err != nil {
		return fmt.Errorf("chmod %s: %w", name, err)
	}
	if err := os.Rename(name, c.path); err != nil {
		return fmt.Errorf("rename %s to %s: %w", name, c.path, err)
	}
	c.syncDir(dir)
	return nil
}

// syncDir makes the rename itself durable. Best effort and deliberately not
// returned: the rename has already happened and readers see the new file, so a
// failure here is worth a log line but not a retry.
func (c *Counter) syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		c.logf("data usage: cannot open %s to sync: %v", dir, err)
		return
	}
	if err := d.Sync(); err != nil {
		c.logf("data usage: cannot sync %s: %v", dir, err)
	}
	d.Close()
}

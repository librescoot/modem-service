# SIM PIN unlock/enable support — design

- Bean: librescoot-t0jf
- Date: 2026-05-05
- Status: design (pre-implementation)

## Summary

Add support for PIN-locked SIMs in modem-service. The user configures `cellular.sim-pin` via settings-service. modem-service reads it and either unlocks the SIM (when PIN-locked) or enables PIN lock (when PIN-disabled), gated by retry counts so the service can never brick a SIM by retrying a wrong PIN.

## Goals

- A PIN-locked SIM unlocks automatically on boot if `cellular.sim-pin` matches.
- A SIM with PIN-lock disabled gets PIN-lock enabled if `cellular.sim-pin` is set and matches.
- A wrong PIN consumes at most one retry per process lifetime, never drops `unlock-retries["sim-pin"]` below 2.
- Lock state and last action outcome visible to other services via the existing `modem` Redis hash.
- No PIN value ever logged to journal.

## Non-goals

- PUK handling. If the SIM enters PUK state, modem-service publishes `pin-action=puk-required` and stops; recovery is manual (`mmcli --send-puk`).
- PIN2. The Deep-Blue test SIM runs in `unlock-required: sim-pin2` and is harmless for data; we never touch PIN2.
- Multi-SIM. SIM7100E has one slot.
- Changing the PIN. `cellular.sim-pin` is "the current PIN", not "the new PIN."
- A scootui UI for entering the PIN. The setting flows through `/data/settings.toml` and the mobile app, both of which read settings regardless of the `user-visible` flag.

## Trust boundary note

`cellular.sim-pin` is plain text in `/data/settings.toml` and the `settings` Redis hash. settings-service has no "secret" type today — `wireguard.private-key` and similar credentials use the same path. This is acceptable for the threat model (the disk is encrypted at rest, the Redis socket is local-only). Worth flagging if/when a "secret" tier is added to settings-service.

## Components

### 1. settings-service schema

One new entry in `settings-service/settings.schema.json`:

```json
"cellular.sim-pin": {
  "type": "string",
  "description": "PIN for SIM unlock and lock-enable (4–8 digits, empty = leave SIM as-is)",
  "label": "SIM PIN",
  "user-visible": false,
  "service": "modem-service",
  "default": "",
  "example": "1234"
}
```

`user-visible: false` because the DBC has no text-entry UI. settings.toml and the mobile app read all keys regardless.

### 2. mm package additions (`internal/mm/client.go`)

```go
const SimInterface = "org.freedesktop.ModemManager1.Sim"

// SendPin calls org.freedesktop.ModemManager1.Sim.SendPin(pin) on simPath.
// Returns the raw D-Bus error on failure (caller maps to outcome).
func (c *Client) SendPin(simPath dbus.ObjectPath, pin string) error

// EnablePin calls org.freedesktop.ModemManager1.Sim.EnablePin(pin, enabled).
func (c *Client) EnablePin(simPath dbus.ObjectPath, pin string, enabled bool) error

// GetUnlockRetries reads modem.UnlockRetries (a{uu} → map[uint32]uint32),
// keyed by MMLock* constants.
func (c *Client) GetUnlockRetries(modemPath dbus.ObjectPath) (map[uint32]uint32, error)

// GetEnabledFacilities reads modem.EnabledFacilityLocks (u, MM3gppFacility flags),
// or modem.EnabledLocks if the modem exposes that. Used to detect whether sim-pin
// lock is currently enabled or disabled on the SIM.
func (c *Client) GetEnabledFacilities(modemPath dbus.ObjectPath) (uint32, error)
```

PIN string is never logged in mm. A short comment at each call site enforces it.

### 3. modem.State extension

`internal/modem/modem.go` — `State` already carries `SIMLockStatus`. Add:

```go
SIMPath              dbus.ObjectPath  // the SIM object path, used for SendPin/EnablePin
SIMPinLockEnabled    bool             // sim-pin appears in EnabledFacilityLocks
UnlockRetriesPin     uint32           // remaining attempts; 0 if unknown
```

These are populated alongside the existing SIM property reads.

### 4. New package `internal/sim`

One file, `internal/sim/manager.go`, ~150 lines.

```go
package sim

type Outcome string

const (
    OutcomeUnconfigured  Outcome = "unconfigured"
    OutcomeOK            Outcome = "ok"
    OutcomeUnlocked      Outcome = "unlocked"
    OutcomeLockEnabled   Outcome = "lock-enabled"
    OutcomeWrongPin      Outcome = "wrong-pin"
    OutcomeLowRetriesBail Outcome = "low-retries-bail"
    OutcomePukRequired   Outcome = "puk-required"
    OutcomeError         Outcome = "error"
)

type Manager struct {
    mm     SimDBus    // narrow interface, mockable
    logger *log.Logger
    triedThisRun bool
}

type SimDBus interface {
    SendPin(simPath dbus.ObjectPath, pin string) error
    EnablePin(simPath dbus.ObjectPath, pin string, enabled bool) error
}

// Reconcile is called once per modem-status cycle after GetModemInfo.
// It reads the current state and the configured PIN and decides whether to act.
//
// The caller is responsible for only invoking Reconcile when the modem is in a
// state where SIM properties have been read (i.e. not during recovery).
func (m *Manager) Reconcile(in Input) Outcome
```

`Input` carries `SIMPath`, `LockStatus`, `PinLockEnabled`, `UnlockRetriesPin`, `ConfiguredPIN`. Pure-data input keeps the manager testable without D-Bus.

### 5. Decision matrix

`Reconcile` implements this table:

| Configured PIN | `unlock-required` | `enabled-locks` has `sim-pin`? | Action |
|---|---|---|---|
| empty | * | * | `unconfigured`, no-op |
| set | `sim-pin` | (locked, can't read) | If `retries[pin] >= 3` and `!triedThisRun`: `SendPin`. On success → `unlocked`. On bad-pin error → `wrong-pin`, `triedThisRun=true`. On other error → `error`. If `retries[pin] < 3`: `low-retries-bail`. If `triedThisRun`: stay `wrong-pin`. |
| set | `sim-pin2` | yes | `ok` (PIN already enabled, PIN2 harmless) |
| set | `sim-pin2` | no | `ok` (we leave PIN2 alone; SIM is otherwise fine) |
| set | `sim-puk` or `sim-puk2` | * | `puk-required`, no-op |
| set | `none` | yes | `ok` |
| set | `none` | no | If `retries[pin] >= 3` and `!triedThisRun`: `EnablePin(pin, true)`. On success → `lock-enabled`. On bad-pin error → `wrong-pin`, `triedThisRun=true`. Else same gating as the unlock row. |

`triedThisRun`:
- Flips to true only on a *failed* attempt that consumed a retry.
- A successful action does *not* flip it — a follow-up settings change in the same run can try again.
- Resets on process restart, but the retry-count gate still protects: once retries drop to 2, no automatic attempt happens until something brings it back to 3 (which only a successful unlock can do, performed manually via `mmcli --send-pin`).

### 6. Wiring in `service/service.go`

Inside `monitorStatus`, after `modemMgr.GetModemInfo` returns `currentState`:

1. Read latest `cellular.sim-pin` from the cached settings (existing `HashWatcher` keeps it fresh).
2. Build `sim.Input` from `currentState` + the configured PIN.
3. Call `simMgr.Reconcile(input)`.
4. Add `currentState.PinAction` to the change-detection block; publish to `modem.pin-action` when it differs from `LastState.PinAction`.

The existing `HashWatcher.OnField("cellular.sim-pin", ...)` callback stores the value in a struct field guarded by `sync.Mutex`. The reconcile cycle reads that field. No tighter coupling than that.

### 7. Redis output

New field on the existing `modem` hash: `pin-action`, values from the `Outcome` enum above. Separate from `error-state` because `pin-action` is the result of an action attempt, not a steady-state error description. `error-state="sim-locked"` and `pin-action="wrong-pin"` are both useful and orthogonal.

### 8. Logging

- One INFO line per state transition: `pin-action: unconfigured -> unlocked` (no PIN value).
- One WARN line on `wrong-pin`: includes remaining retries from the modem property.
- One WARN line on `low-retries-bail`: explains why we won't try.
- D-Bus errors logged at ERROR with the raw error string (which never contains the PIN — ModemManager error strings reference the operation, not arguments).

### 9. Tests

`internal/sim/manager_test.go` — table-driven coverage of every row in the decision matrix using a mock `SimDBus` that records the call and returns a configurable error. Test PIN values are obviously fake (`"0000"`).

Two integration touch points are not unit-tested (covered manually on Deep Blue):

- The actual D-Bus call to ModemManager — exercised by stopping `librescoot-modem`, locking the SIM via `mmcli --enable-pin --pin=...`, configuring `cellular.sim-pin`, and restarting the service.
- The `HashWatcher` plumbing — already exercised by other settings in the service.

## Failure modes and recovery

| Situation | What happens |
|---|---|
| User configures wrong PIN | First boot: one attempt, retries 3→2, `pin-action=wrong-pin`. Subsequent boots: `low-retries-bail`. User edits settings, fixes PIN. Service still bails because retries=2. User manually unlocks once via `mmcli --send-pin=CORRECT` (resetting retries to 3 on success); next service cycle sees retries=3 and acts normally. |
| User changes PIN on SIM via another device | Same as wrong-PIN flow above. |
| modem-service restarts mid-day for unrelated reasons | `triedThisRun` resets but retries are still 3 if untouched. Service re-reads state, sees `unlock-required=none` and `sim-pin` enabled → `ok`. No-op. |
| Modem hard-reset (existing recovery path) | After modem reappears, GetModemInfo populates fresh state; reconcile runs again. Same gating applies. |
| SIM enters PUK state (3 wrong PINs from any source) | `pin-action=puk-required`. Service does nothing. Manual recovery required. |

## File touch list

- `settings-service/settings.schema.json` — add `cellular.sim-pin`
- `modem-service/internal/mm/client.go` — add `SendPin`, `EnablePin`, `GetUnlockRetries`, `GetEnabledFacilities`, `SimInterface` const
- `modem-service/internal/modem/modem.go` — extend `State`, populate new fields in `GetModemInfo`
- `modem-service/internal/sim/manager.go` — new file
- `modem-service/internal/sim/manager_test.go` — new file
- `modem-service/internal/service/service.go` — wire `simMgr.Reconcile` into `monitorStatus`, add `pin-action` change detection, register a `HashWatcher` field for `cellular.sim-pin`
- `modem-service/internal/redis/redis.go` — minor: ensure the existing `HashWatcher` plumbing covers the new field (likely no change, just a registration call from `service.go`)
- `modem-service/README.md` — document `pin-action` field

## Open questions

None at design time. If implementation surfaces an MM idiom mismatch (e.g. the `EnabledFacilityLocks` property name varies across MM versions), the implementer falls back to parsing `mmcli --output-json` if needed and notes the deviation in the implementation plan.

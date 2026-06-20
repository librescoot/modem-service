# SMS send/receive support — design

- Date: 2026-06-15
- Status: implemented

## Summary

Add SMS send and receive to modem-service. ModemManager already exposes a full
SMS API (`org.freedesktop.ModemManager1.Modem.Messaging` for list/create/delete
and `org.freedesktop.ModemManager1.Sms` for the per-message object); it was
previously unused. Outbound messages are requested by other services pushing a
JSON payload onto the `scooter:sms` Redis list. Inbound messages are picked up
from a ModemManager `Added` signal, published to the `sms` Redis hash, and
deleted from modem storage immediately. The feature mirrors the `internal/sim`
package: a narrow, mockable D-Bus interface and a thin manager, with all Redis
and lifecycle wiring kept in `internal/service`.

## Goals

- Send an SMS on request: `LPUSH scooter:sms '{"to":"+49...","text":"..."}'`.
- Receive SMS with low latency, driven by ModemManager's `Added` signal.
- Never let the modem's small SMS store (a few dozen slots on SIMCom hardware)
  fill up: every handled message — inbound or our own outbound — is deleted.
- Survive modem resets: the receive watch re-arms on the new D-Bus path after
  recovery, and a startup drain recovers anything received while offline.
- Expose state via a dedicated `sms` Redis hash, following the librescoot
  hash+channel convention (field name published on the hash's channel).

## Non-goals

- Multipart message reassembly logic of our own. ModemManager assembles
  concatenated SMS; we only act on a message once its `State` is `received`.
- Persisting an inbox. We are a relay: read → publish → delete. Consumers that
  need history keep their own.
- Delivery reports / read receipts. Not requested.
- A command/keyword protocol over SMS. We publish the raw text; interpreting it
  is another service's job.
- CDMA. These are GSM/LTE modems; only 3GPP PDU types are handled.

## Components

### 1. mm package additions (`internal/mm/client.go`, `types.go`)

```go
const (
    ModemMessagingInterface = "org.freedesktop.ModemManager1.Modem.Messaging"
    SmsInterface            = "org.freedesktop.ModemManager1.Sms"
)

// SMSProperties is the subset of Sms properties we read. ModemManager has no
// "direction" property; PduType distinguishes inbound (DELIVER) from outbound
// (SUBMIT).
type SMSProperties struct {
    Number, Text, Timestamp string
    State, PduType          uint32
}

func (c *Client) ListSMS(modemPath dbus.ObjectPath) ([]dbus.ObjectPath, error)
func (c *Client) CreateSMS(modemPath dbus.ObjectPath, number, text string) (dbus.ObjectPath, error)
func (c *Client) DeleteSMS(modemPath, smsPath dbus.ObjectPath) error
func (c *Client) SendSMS(smsPath dbus.ObjectPath) error
func (c *Client) GetSMSProperties(smsPath dbus.ObjectPath) (SMSProperties, error)

// WatchSMSAdded subscribes to Messaging.Added on modemPath until ctx is
// cancelled (removing its match rule on exit so re-arms don't accumulate
// rules); re-armable after a reset rebinds the path. Mirrors
// WatchPropertyChanges.
func (c *Client) WatchSMSAdded(ctx context.Context, modemPath dbus.ObjectPath, onAdded func(dbus.ObjectPath, bool)) error
```

`Messaging.Create` takes an `a{sv}` dict — `CreateSMS` builds `{"number","text"}`.
`Send` is a method on the Sms object, so `SendSMS` is called on the SMS path,
not the modem path. `types.go` gains `MMSmsState*` and `MMSmsPduType*` constants
plus `SmsStateToString` and `SmsPduTypeIsIncoming`.

### 2. New package `internal/sms`

`internal/sms/manager.go`, ~190 lines.

```go
type MessagingDBus interface { // satisfied by mm.Client; mocked in tests
    ListSMS(modemPath dbus.ObjectPath) ([]dbus.ObjectPath, error)
    CreateSMS(modemPath dbus.ObjectPath, number, text string) (dbus.ObjectPath, error)
    DeleteSMS(modemPath, smsPath dbus.ObjectPath) error
    SendSMS(smsPath dbus.ObjectPath) error
    GetSMSProperties(smsPath dbus.ObjectPath) (mm.SMSProperties, error)
}

type SendRequest struct { To, Text string } // JSON from scooter:sms
type Message struct { Number, Text string; Timestamp time.Time } // parsed inbound
type Outcome string // ok | error | no-modem

func (m *Manager) Send(modemPath dbus.ObjectPath, req SendRequest) (Outcome, error)
func (m *Manager) DrainReceived(modemPath dbus.ObjectPath) ([]*Message, error)
```

- **Send**: `CreateSMS` → `SendSMS` → `DeleteSMS` (deferred, so a created object
  is cleaned up even when the send fails).
- **processOne** (shared by the two entry points below) classifies one SMS *by
  elimination*: assembling (`receiving`) → leave; status report → delete;
  outbound (PduType SUBMIT, or state sending/sent) → delete as a stale leftover;
  everything else → treat as inbound (delete, return). Classifying by elimination
  matters: some modems report received SMS as state `stored` with no PduType,
  which an allow-list of "DELIVER or received" would silently drop. Deleting
  stale outbound cleans up a previous send whose delete didn't take.
- **HandleAdded(modemPath, smsPath)**: runs processOne on the exact object a
  Messaging.Added signal carried. Processing the signalled path directly (rather
  than only re-listing) is essential — some modems deliver a received message as
  a transient object that never appears in Messaging.List().
- **DrainReceived**: lists storage and runs processOne on each — the startup and
  periodic-poll path, catching anything the signal missed and cleaning up stale
  outbound. A mutex serialises Send with both paths, so any outbound object a
  drain sees is guaranteed not to be a live in-flight send and is safe to delete.

### 3. redis package additions (`internal/redis/redis.go`)

```go
func (c *Client) PublishSMSState(field, value string) error          // Hash("sms").Set(..., Sync())
func (c *Client) PublishSMSFields(fields map[string]string, notifyField string) error // SetManyPublishOne
func (c *Client) StartSMSCommandHandler(handler SMSCommandHandler) error // HandleRequests on scooter:sms
```

`PublishSMSState` mirrors `PublishModemState` (one field, one notification).
`PublishSMSFields` mirrors `PublishLocationState`'s `SetManyPublishOne` (batch
write, single notification) for the multi-field incoming/sent updates.
`scooter:sms` is the first queue carrying a JSON payload rather than a bare
string — justified because a send needs two structured fields.

### 4. Wiring in `service/service.go`

- New fields: `SMS *sms.Manager`, `smsWatchCancel context.CancelFunc`,
  `unreadSMS atomic.Int64`.
- `Run`: start `StartSMSCommandHandler(s.handleSMSCommand)`; after
  `ensureModemEnabled`, call `startSMSWatch(ctx)`.
- `recoverySucceeded`: call `startSMSWatch(ctx)` to re-arm on the (possibly new)
  modem path.
- Shutdown: cancel the watch (it also stops via the ctx it derives from).
- `handleSMSCommand`: unmarshal JSON, resolve modem path, publish
  `state=sending`, `SMS.Send`, then publish `state=idle`+`last-sent-*` or
  `state=error`.
- `startSMSWatch` arms the `Added` watch and *then* drains storage (arm-first, so
  a message arriving during setup isn't missed). `drainSMS` / `publishIncomingSMS`
  publish each inbound message to the `sms` hash and bump `unread-count`.
- `monitorStatus` also drains on its periodic tick (every `InternetCheckTime`) as
  a reliable fallback, since some modems never emit `Added` for SIM-stored
  messages — a signal-only design would silently miss them.
- On each arm, `configureAndDiagnoseSMS` sends `AT+CNMI=2,1,0,0,0` (via MM's
  command interface, the same path the APN code uses) so the modem actually
  indicates inbound SMS to ModemManager, and logs the modem's SMS storage/config
  plus an `AT+CMGL` dump. Reception failing while sending works is almost always
  the modem not raising a new-message indication that MM can see.

## Receive flow

Reception has two independent paths into the same `processOne` classifier
(serialised by the manager mutex):

1. **Signal** (low latency): on `Messaging.Added(path, received=true)` the
   service logs the event and calls `HandleAdded(path)`, processing that exact
   object — including transient ones that never reach storage. `received=false`
   (an object we created) is ignored.
2. **Poll / startup** (reliable fallback for modems that don't emit `Added` for
   stored messages): `DrainReceived` lists storage and processes each object.

An inbound message is only returned once it has been deleted — if the delete
fails we skip delivery and leave it for a later drain, so we never re-publish the
same message. `publishIncomingSMS` then writes `last-received-from`,
`last-received-text`, `last-received-at`, `unread-count` in one batch with a
single `last-received-at` notification.

## Redis output

### `sms` hash

| Field | Values |
|---|---|
| `state` | `idle` \| `sending` \| `error` |
| `last-sent-to` | recipient number |
| `last-sent-at` | RFC 3339 timestamp |
| `last-received-from` | sender number |
| `last-received-text` | message body |
| `last-received-at` | RFC 3339 timestamp |
| `unread-count` | inbound messages since service start |

Per librescoot convention, a field change publishes the field name on the `sms`
channel; consumers `HGET` the value. No separate `sms:received` channel is
added — the standard hash+channel pattern covers it (the `gps:tpv` JSON channel
is a GPS-only optimisation for its 1 Hz update rate, which SMS does not have).

### `scooter:sms` command queue

`LPUSH scooter:sms '{"to":"+4915112345678","text":"Hello"}'`

## Logging

- One INFO line per sent message (recipient + length) and per received message
  (sender + length). Message bodies are not logged in full at INFO.
- The `scooter:sms` handler logs "Received SMS send command" without the payload
  (it contains the recipient and body).
- D-Bus failures (create/send/delete/list/properties) logged with the raw error.

## Tests

`internal/sms/manager_test.go` — table-style coverage with a mock `MessagingDBus`
recorder: send happy path, no-modem, empty recipient, create failure, send
failure (still deletes), and drain cases (inbound received, inbound in `stored`
state, inbound with unknown PduType, status-report deleted, outbound left,
incomplete left, delete-failure defers delivery, mixed batch). Plus
`parseTimestamp`. `internal/redis/redis_test.go` gains `TestPublishSMSState` and
`TestPublishSMSFields` (skipped when no Redis is reachable, like the others).

Two integration touch points are covered manually on hardware:

- The actual D-Bus calls to ModemManager — `LPUSH scooter:sms ...` then
  `HGETALL sms`; and sending an SMS to the modem then watching `sms`.
- Re-arming the watch after a modem reset.

## Failure modes and recovery

| Situation | What happens |
|---|---|
| Send while no modem present | `handleSMSCommand` publishes `state=error`, returns. |
| Send fails at the modem | Created object is deleted anyway; `state=error`. |
| Delete of a received message fails | Not delivered this round; left in storage; retried on the next drain (avoids re-publishing duplicates). |
| Status-report (delivery report) object | Deleted on drain, never published, so it can't fill the store. |
| Multipart SMS still assembling | `state == receiving` → left; a later `Added`/poll picks it up once complete. |
| Modem never emits `Added` for stored SMS | The `monitorStatus` periodic poll drains them within one tick. |
| Modem delivers inbound as a transient object (not in `List`) | `HandleAdded` processes the `Added` signal's path directly, so it's still delivered. |
| Sent message left in storage (its delete didn't take) | Reclassified as stale outbound and deleted on the next drain. |
| Modem hard-reset (existing recovery path) | `recoverySucceeded` re-arms `startSMSWatch` on the new path and drains anything queued meanwhile. |
| Messages received while service was down | `startSMSWatch` drains storage on startup (after arming the watch). |

## File touch list

- `modem-service/internal/mm/client.go` — Messaging/Sms consts, `SMSProperties`, `ListSMS`/`CreateSMS`/`DeleteSMS`/`SendSMS`/`GetSMSProperties`, `WatchSMSAdded`
- `modem-service/internal/mm/types.go` — `MMSmsState*`, `MMSmsPduType*`, `SmsStateToString`, `SmsPduTypeIsIncoming`
- `modem-service/internal/sms/manager.go` — new file
- `modem-service/internal/sms/manager_test.go` — new file
- `modem-service/internal/redis/redis.go` — `PublishSMSState`, `PublishSMSFields`, `StartSMSCommandHandler`, `smsHandler` field + Close
- `modem-service/internal/redis/redis_test.go` — `TestPublishSMSState`, `TestPublishSMSFields`
- `modem-service/internal/service/service.go` — SMS manager + watch wiring, `handleSMSCommand`, `startSMSWatch`, `drainSMS`, `publishIncomingSMS`
- `modem-service/README.md` — document the `sms` hash and `scooter:sms` queue

## Open questions

None at implementation time. If a particular modem emits `Added` only once for a
multipart message and then only updates `State` via `PropertiesChanged` (rather
than re-emitting `Added`), a still-assembling message is picked up by the next
periodic poll once complete, so the wait is bounded by `InternetCheckTime`. If
even lower latency is needed, add a `PropertiesChanged` watch on receiving SMS
objects.

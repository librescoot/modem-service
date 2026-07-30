# Librescoot Modem Service

Part of the [Librescoot](https://librescoot.org/) open-source platform.

## Overview

The Librescoot Modem Service is a Go-based monitoring tool designed to track and manage cellular modem connectivity using ModemManager, with state synchronization through Redis.

It is a free and open replacement for the `unu-modem` service on unu's Scooter Pro.

## Features

- Real-time modem state monitoring
- Retrieval of modem connectivity information
- Public and interface IP address tracking
- Signal quality and access technology reporting
- SMS send and receive via Redis
- Redis-based state synchronization and pub/sub notifications

## Dependencies

- ModemManager (`mmcli`)
- Redis
- gpsd

## Building / Installing

Assuming your mdb is connected via USB Ethernet as 192.168.7.1 and aliased as `mdb`;
```bash
make dist
scp modem-service-arm-dist root@mdb:/usr/bin/modem-service
```

## Configuration

The service supports the following command-line flags:

| Flag               | Default Value | Description                             |
|--------------------|---------------|-----------------------------------------|
| `-redis-url`       | `redis://127.0.0.1:6379` | Redis URL                    |
| `-gpsd-server`     | `localhost:2947` | GPSD server address                  |
| `-polling-time`    | `5s`          | Polling interval for modem checks       |
| `-internet-check-time` | `30s`     | Interval for local layer checks, and the base interval for the reachability probe |
| `-internet-check-max-interval` | `5m` | Upper bound for probe backoff while healthy. Local layer checks are unaffected and still run every tick |
| `-connectivity-targets` | `8.8.8.8:53,1.1.1.1:53,9.9.9.9:53,208.67.222.222:53` | Fallback probe targets, used only when no network-assigned resolver answers. Fleets on a private APN should point this at something actually reachable |
| `-interface`       | `wwan0`       | Network interface to monitor            |
| `-sms-keepalive`   | `false`       | Keep the CS (SGs) registration alive for SMS delivery via periodic self-calls |

## Modem State Tracking

The service monitors and publishes the following modem state attributes:

- Connection status
- Public IP address
- Interface IP address
- Access technology
- Signal quality
- IMEI
- ICCID

## GPS Location Tracking

The service configures the modem location ports to unmanaged mode for gpsd use.
Position, altitude, speed and course information is forwarded from gpsd to
Redis for consumption by other components.

## Redis Integration

The service uses Redis to:
- Store current modem state
- Publish state change notifications on the `internet` channel
- Publish GPS information
- Register a power inhibitor in `power:inhibits` while the modem is powered

### Redis Hash Structure

The service maintains various Redis hashes:

#### `internet` hash
- `modem-health` (`normal`, `recovering`, `recovery-failed-waiting-reboot`, `permanent-failure-needs-replacement`)
- `reachability` — why the probe reports what it does
     - `ok` — a target answered
     - `unreachable` — nothing answered, but the local stack is healthy. Normal and permanent on a fleet whose APN only routes to a private range; never a reason to touch the modem
     - `no-path` — a local layer is failing, which is a real fault
- `link-layer` — `ok`, or the lowest failing layer and the reason (e.g. `bearer: bearer not connected`)
- `modem-state` (`off`, `disconnected`, `connected`, or `UNKNOWN`)
- `ip-address` (external IPv4 address or `UNKNOWN`)
- `access-tech` (access tech, depending on modem & SIM support)
     - `"UNKNOWN"`
     - `"2G"` / `"GSM"`
     - `"3G"` / `"UMTS"`
     - `"4G"` / `"LTE"`
     - `"5G"`
- `signal-quality` (0-100, in %, or 255 if UNKNOWN)
- `sim-imei` IMEI (unique hardware identifier) (this actually identifies the modem, but the name is kept for backward compatibility)
- `sim-imsi` IMSI (unique subscriber identity)
- `sim-iccid` ICCID (unique SIM card identifier)

#### `modem` hash
- `power-state` (on/off/low-power)
- `sim-state` (active/locked/missing/unknown)
- `sim-lock` (enabled/disabled/unknown)
- `pin-action` outcome of the last SIM PIN reconcile cycle:
  - `unconfigured` - no `cellular.sim-pin` setting
  - `ok` - SIM is in the desired state
  - `unlocked` - service just sent the PIN to unlock
  - `lock-enabled` - service just enabled PIN lock with the configured PIN
  - `wrong-pin` - last attempt was rejected; service will not retry until restart
  - `low-retries-bail` - retries < 3, refusing to attempt to avoid PUK lock
  - `puk-required` - SIM is in PUK state, manual recovery needed
  - `error` - D-Bus call failed (see journal)
- `operator-name` (current network operator name)
- `operator-code` (current network operator code)
- `is-roaming` (true/false)
- `registration-fail` (reason if registration failed)

When `cellular.sim-pin` is set in the `settings` Redis hash (typically via
`/data/settings.toml` or the mobile app), modem-service will:

- send the PIN to unlock the SIM if it's PIN-locked, or
- enable PIN lock with the configured PIN if the SIM has the lock disabled.

A wrong PIN attempt consumes one retry. To prevent the SIM from ever entering
PUK state through the service, modem-service refuses to attempt a PIN unless
`unlock-retries["sim-pin"]` is at the maximum (3) and the service has not
already failed an attempt this run. To recover after a wrong-PIN configuration,
fix the value, then unlock the SIM once manually (e.g.
`mmcli -i 0 --send-pin=CORRECT`) — that restores the retry counter and
modem-service will resume normally.

#### `gps` hash

Location information is tracked in a Redis hash `gps` with the keys:
- `latitude`, `longitude` Current GPS lat/lon with 6 decimals
- `altitude` Current GPS altitude
- `speed` Current GPS speed in km/h
- `course` Current GPS course (heading) in degrees
- `timestamp` GPS timestamp
- `state` GPS state with the following possible values:
  - `off` GPS is disabled (initial state)
  - `searching` Actively searching for GPS signal
  - `fix-established` Valid GPS fix obtained (2D or 3D)
  - `error` GPS configuration or connection failed

GPS state transitions occur when:
1. Enabling GPS: `off` → `searching`
2. Getting first fix: `searching` → `fix-established`
3. Losing fix: `fix-established` → `searching`
4. Configuration failure: `searching` → `error`
5. Disabling GPS: any state → `off`

#### `sms` hash

Latest-value convenience state only. Per-message data lives on the
`sms:received` / `sms:sent` streams (see below); don't build anything that
needs every message on this hash.

- `state` send state of the last outbound message, published as a `state`
  notification on the `sms` channel:
  - `idle` - no send in progress (also the post-success state)
  - `sending` - an outbound message is being transmitted
  - `error` - the last send failed (see the `sms:sent` stream for the reason)
- `last-sent-to` recipient number of the last successfully sent SMS
- `last-sent-at` timestamp (RFC 3339) the last send completed
- `last-received-from` sender number of the last inbound SMS
- `last-received-text` body of the last inbound SMS
- `last-received-at` timestamp (RFC 3339) the last inbound SMS arrived
- `unread-count` number of inbound messages received since service start

The `last-received-*` fields are refreshed silently; the per-message
notification is the dedicated `sms:received` channel.

### SMS streams and channels

Per-message events use Redis streams (capped at 100 entries each) plus a
dedicated pub/sub channel per direction. The channel payload is the stream
entry as JSON, including its stream `id`, so a subscriber can act on it
directly; a consumer that wants catch-up semantics instead XREADs from its
last-seen ID. Redis is not persistent on librescoot, so the streams are a
live window, not an archive - both are empty after a reboot.

#### `sms:received` (stream + channel)

One entry per inbound message: `from`, `text`, `timestamp` (RFC 3339).
Channel payload: `{"id":"<stream-id>","from":...,"text":...,"timestamp":...}`.

The XADD is the delivery commit point: a message is only deleted from modem
storage after it has landed on the stream. If Redis is unreachable, the
message stays in the modem and the periodic drain retries, so nothing is lost
to a Redis hiccup. Messages that arrived while the service was offline are
drained on startup.

#### `sms:sent` (stream + channel)

One entry per terminal send outcome: `request-id` (the caller's `id` token
from the `scooter:sms` payload, empty if none), `to`, `text`, `outcome`
(`sent`/`error`), `error` (empty on success), `timestamp`. Channel payload is
the same entry as JSON including the stream `id`.

### SGs keepalive (`-sms-keepalive`, off by default)

Some operators (seen on O2/Lebara DE) run a ~15-minute implicit-detach timer
on the CS registration that SMS delivery over LTE depends on; once it expires,
inbound SMS silently stops until the next combined attach. With
`-sms-keepalive` enabled, the service places a brief call to the scooter's own
number whenever no CS activity has happened for ~13 minutes, which resets the
operator's timer. The call never connects and costs nothing as long as the
SIM's mailbox is not set to pick up busy calls; check that before enabling.
Each self-call briefly falls back to 2G, and SIMs without a stored MSISDN
can't use the keepalive at all (the service logs this once and disables it).

### Sending an SMS

Push a JSON payload onto the `scooter:sms` Redis list:

```bash
redis-cli LPUSH scooter:sms '{"to":"+4915112345678","text":"Hello from the scooter"}'

# With a correlation token, echoed back as request-id on sms:sent:
redis-cli LPUSH scooter:sms '{"id":"trip-42","to":"+4915112345678","text":"Hello"}'
```

The service creates, transmits, and deletes the message, then records the
outcome on the `sms:sent` stream/channel and updates the `sms` hash (`state`,
`last-sent-to`, `last-sent-at`). This is the only command queue that takes a
JSON payload rather than a bare string, because a send needs both a recipient
and a body.

### Power inhibitor

While the modem is powered, modem-service registers a `block` inhibitor in the
`power:inhibits` hash (`who=librescoot-modem`). This holds pm-service off from
suspending the MDB until the modem has been told to shut down and confirmed off:
on a `disable` command modem-service powers the modem down and only then removes
the inhibitor. The inhibitor is also dropped on clean shutdown, so a restart
cannot leave a stale entry blocking suspend.

## Usage

```bash
modem-service \
  -redis-url redis://redis.example.com:6379 \
  -interface wwan0 \
  -internet-check-time 45s \
  -gpsd-server localhost:2947
```

## Error Handling

The service logs errors and attempts to maintain the most recent known state. If modem information cannot be retrieved, internet connectivity is lost, or GPS connection fails, the service will update the state accordingly and automatically attempt to recover.

## License

This project is dual-licensed. The source code is available under the
[GNU Affero General Public License v3.0][agpl-3.0].
The maintainers reserve the right to grant separate licenses for commercial distribution; please contact the maintainers to discuss commercial licensing.

[![AGPL v3][agpl-image]][agpl-3.0]

[agpl-3.0]: https://www.gnu.org/licenses/agpl-3.0.en.html
[agpl-image]: https://www.gnu.org/graphics/agplv3-88x31.png

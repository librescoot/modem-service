# Librescoot Modem Service

## Overview

The Librescoot Modem Service is a Go-based monitoring tool designed to track and manage cellular modem connectivity using ModemManager, with state synchronization through Redis.

## Features

- Real-time modem state monitoring
- Retrieval of modem connectivity information
- Public and interface IP address tracking
- Signal quality and access technology reporting
- Redis-based state synchronization and pub/sub notifications

## Dependencies

- ModemManager (`mmcli`)
- Redis
- gpsd

## Building / Installing

Assuming your mdb is connected via USB Ethernet as 192.168.7.1 and aliased as `mdb`;
```bash
make dist
scp rescoot-modem-arm-dist root@mdb:/usr/bin/rescoot-modem
```

## Configuration

The service supports the following command-line flags:

| Flag               | Default Value | Description                             |
|--------------------|---------------|-----------------------------------------|
| `-redis-url`       | `redis://127.0.0.1:6379` | Redis URL                    |
| `-gpsd-server`     | `localhost:2947` | GPSD server address                  |
| `-polling-time`    | `5s`          | Polling interval for modem checks       |
| `-internet-check-time` | `30s`     | Interval for internet connectivity checks |
| `-interface`       | `wwan0`       | Network interface to monitor            |

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

### Redis Hash Structure

The service maintains various Redis hashes:

#### `internet` hash
- `modem-health` (`normal`, `recovering`, `recovery-failed-waiting-reboot`, `permanent-failure-needs-replacement`)
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
- `operator-name` (current network operator name)
- `operator-code` (current network operator code)
- `is-roaming` (true/false)
- `registration-fail` (reason if registration failed)

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

## Usage

```bash
rescoot-modem \
  -redis-url redis://redis.example.com:6379 \
  -interface wwan0 \
  -internet-check-time 45s \
  -gpsd-server localhost:2947
```

## Error Handling

The service logs errors and attempts to maintain the most recent known state. If modem information cannot be retrieved, internet connectivity is lost, or GPS connection fails, the service will update the state accordingly and automatically attempt to recover.

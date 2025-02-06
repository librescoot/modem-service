# Rescoot Modem Service

## Overview

The Rescoot Modem Service is a Go-based monitoring tool designed to track and manage cellular modem connectivity using ModemManager, with state synchronization through Redis.

It is a free and open replacement for the `unu-modem` service on unu's Scooter Pro.

## Features

- Real-time modem state monitoring
- Retrieval of modem connectivity information
- Public and interface IP address tracking
- Signal quality and access technology reporting
- Redis-based state synchronization and pub/sub notifications

## Dependencies

- ModemManager (`mmcli`)
- Redis

## Configuration

The service supports the following command-line flags:

| Flag               | Default Value | Description                             |
|--------------------|---------------|-----------------------------------------|
| `--redis-host`     | `localhost`   | Redis server hostname                   |
| `--redis-port`     | `6379`        | Redis server port                       |
| `--polling-time`   | `5s`          | Polling interval for modem checks       |
| `--internet-check-time` | `30s`    | Interval for internet connectivity checks |
| `--interface`      | `wwan0`       | Network interface to monitor            |

## Modem State Tracking

The service monitors and publishes the following modem state attributes:

- Connection status
- Public IP address
- Interface IP address
- Access technology
- Signal quality
- IMEI
- ICCID

## Redis Integration

The service uses Redis to:
- Store current modem state
- Publish state change notifications on the `internet` channel

### Redis Hash Structure

The service maintains a Redis hash `internet` with the following keys:
- `modem-state` (`off`, `disconnected`, `connected`, or `UNKNOWN`)
- `ip-address` (external IPv4 address or `UNKNOWN`)
- `if-ip-address` (interface assigned IPv4 address, e.g. carrier NAT)
- `access-tech` (access tech, depending on modem & SIM support)
     - `"UNKNOWN"`
     - `"2G"` / `"GSM"`
     - `"3G"` / `"UMTS"`
     - `"4G"` / `"LTE"`
     - `"5G"`
- `signal-quality` (0-100, in %, or 255 if UNKNOWN)
- `sim-imei` IMEI (unique hardware identifier)
- `sim-iccid` ICCID (unique SIM card identifier)

## Usage

```bash
rescoot-modem \
  --redis-host=redis.example.com \
  --redis-port=6379 \
  --interface=wwan0 \
  --internet-check-time=45s
```

## Error Handling

The service logs errors and attempts to maintain the most recent known state. If modem information cannot be retrieved or internet connectivity is lost, the service will update the state accordingly.

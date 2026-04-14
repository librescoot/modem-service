# Offline scooter reference — unu-mdb-f21c47ea

Captured 2026-04-14. Deactivated SIM on E-Plus / Telefónica Germany,
operator code 26203. Scooter is searching for service but cannot
register. GPS still fixes (standalone mode, cached ephemeris).

## Key signals that identify "deactivated SIM"

- `3gpp.registration-state`: `searching` (not `denied` or `failed`)
- `3gpp.packet-service-state`: `detached`
- `3gpp.network-rejection-error`: `no-cells-in-location-area`
- `3gpp.network-rejection-access-technology`: `lte`
- `generic.state`: `searching`
- `generic.bearers`: `[]` (no data bearer)
- `generic.signal-quality.value`: 33 (recent: yes — signal exists, just can't register)
- `generic.unlock-required`: `sim-pin2` (PIN2 lock, harmless for data)

## GPS state

- Fix established (3D, 22.8m eph, HDOP 1.2) despite offline cellular
- `GPS Time: 2006-08-29 16:51:07 UTC` — GPS week-number rollover
  (our correctGPSWeekRollover handles it in location.go)
- System clock: `2006-08-29 18:51:07 CEST` — the Linux system clock
  is ALSO wrong; modem-service probably hasn't had a chance to sync
  yet because chrony needs network

## MM Location capabilities enabled

`enabled: 3gpp-lac-ci, gps-unmanaged, agps-msb`

Note `agps-msb` is still enabled here because this scooter predates
the Phase 1 cleanup that dropped it from required sources.

## Why "searching" instead of "denied"?

The network's rejection isn't a clean "your IMSI is barred" (which
would produce `denied`). Instead the network says "no cells in this
location area will accept you" (`no-cells-in-location-area`), and MM
keeps the high-level state at `searching` while the modem retries.
Our connectivity classifier's 3-minute offline debounce eventually
lands on `offline` regardless.

## Full mmcli -m any -J

See accompanying JSON file.

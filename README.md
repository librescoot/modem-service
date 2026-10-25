# Librescoot Modem Service

Part of the [Librescoot](https://librescoot.org/) open-source platform.

`modem-service` manages cellular connectivity, modem-backed location, SMS, and
cellular usage reporting for Librescoot vehicles. It uses ModemManager over
D-Bus and coordinates with other services through Redis.

## Capabilities

- Monitors modem, SIM, registration, bearer, signal, and connectivity state.
- Powers and recovers the modem when the hardware and platform support it.
- Publishes GPS and optional cell-location state.
- Sends and receives SMS through Redis command and event surfaces.
- Reconciles supported SIM PIN and APN settings from Redis.
- Persists cellular data-usage totals when a data-usage file is configured.

## Operation and Redis interface

The service publishes current connectivity in `internet`, modem/SIM data in
`modem`, GPS state in `gps`, cellular usage in `internet-usage`, and cell
location in `cell-location`. Hash updates follow the Librescoot field-change
notification convention. Full GPS TPV snapshots are additionally published as
JSON on `gps:tpv`.

It consumes `enable` and `disable` commands from `scooter:modem` and watches
`vehicle.state`. It also reads these `settings` fields at startup and when they
change: `modem.gps`, `modem.cell-location`, `cellular.sim-pin`,
`cellular.apn`, `cellular.username`, `cellular.password`, and `cellular.auth`.

Outbound SMS requests are JSON values on `scooter:sms` with `to` and `text`,
and may include an `id` correlation value. Terminal results are appended to the
capped `sms:sent` stream and published as JSON on the `sms:sent` channel.
Inbound messages use the corresponding `sms:received` stream and channel. The
`sms` hash carries latest-value state such as send state and the most recent
message metadata; it is not a message archive.

While the modem is powered, the service creates the `modem-active` block
inhibitor in `power:inhibits` and removes it after power-down or clean shutdown.

## Configuration

Run `bin/modem-service -help` after building for the authoritative flag list.
The principal options configure the Redis URL, network interface, GPSD and SUPL
servers, connectivity-check timing and fallback targets, debug logging, SMS
keepalive, and the data-usage file. The default data-usage file is
`/data/internet-usage.json`; an empty value keeps totals in memory only.

SIM PIN and APN credentials are sensitive. Do not expose Redis to untrusted
networks or users, and avoid enabling debug logging where SMS or modem
information would be inappropriate for the journal.

### APN reconciliation

The service checks the actual CID1 context with `AT+CGDCONT?`, as well as the
NetworkManager profile and ModemManager attach settings. Modem `ERROR`,
`+CME ERROR` and `+CMS ERROR` responses fail the command even when D-Bus succeeds.
Authentication commands, replies and settings errors do not log credentials.

An APN change temporarily suspends profile autoconnect, disconnects the bearer
and deregisters before writing settings. CID1 and the NM profile are read back
before reattachment; CID1 is checked again after NM activation. NM changes are
staged in memory and saved with the original autoconnect policy only after
validation. Modem recovery and APN changes share the same operation lock.

Failures attempt a bounded rollback to the prior profile and actual CID1.
An incomplete rollback keeps the profile temporarily suspended and retains the
original snapshot for retry. Disabling the modem cancels the transaction;
rollback does not explicitly reattach after cancellation. `modem.apn-action`
reports `error` until reconciliation succeeds. There are at most three apply
attempts per settings/SIM/modem-path combination, with 30- and 60-second retry
delays. A settings or identity change, successful modem recovery, or service
restart permits a fresh attempt sequence.

## Build and test

```bash
make build        # Linux ARMv7 binary: bin/modem-service
make build-host   # local-development binary: bin/modem-service
make test
make lint         # requires golangci-lint
```

## Deployment and operations

The Yocto layer ships `librescoot-modem.service`. It starts
`/usr/bin/modem-service -interface wwan0`, requires Valkey, orders itself after
Valkey, D-Bus, and ModemManager, and restarts the process on failure. Install
and enable it according to the target distribution's systemd policy.

The runtime requires Redis, ModemManager and its D-Bus service, a configured
cellular network interface, and access to the modem control hardware. GPS
operation additionally requires GPSD. The process handles `SIGINT` and
`SIGTERM`, flushes configured usage totals, and releases its power inhibitor on
shutdown.

## License

Original Librescoot code in this repository is available, at your option, under
[AGPL-3.0-only](LICENSE) or [CC-BY-NC-SA-4.0](LICENSE.CC-BY-NC-SA-4.0).
Third-party code retains its existing license.

Made with ❤️ by the Librescoot community

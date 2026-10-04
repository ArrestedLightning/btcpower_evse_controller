# evse-controller

Replacement controller for BTC Power dual-port J1772 EVSEs. It is a single static Go binary that:

- talks directly to the charge control boards and power meter boards over serial,
- serves a status page for the built-in kiosk display and for network browsers,
- provides a JSON API,
- does optional load sharing,
- needs no internet access, database or OCPP server.

It supports 1–4 chargers and 0–2 meter boards.

## Build

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=0.1.0" -o evsectl ./cmd/evsectl
go test ./...
```

All web assets are embedded in the binary. The binary runs on the EVSE's Ubuntu 16.04.

## Try it without hardware

```sh
./evsectl -simulate -config deploy/config.example.yaml -data-dir ./data
```

Open http://localhost:8080/. In simulate mode, vehicles come and go on their own. To script them instead:

```sh
curl -XPOST localhost:8080/api/v1/sim/ports/1/manual    # stop automatic behaviour
curl -XPOST localhost:8080/api/v1/sim/ports/1/plug      # also: unplug, request, stop, fault, clear, auto
```

## Confirm the protocol on real hardware first

The serial protocols were reverse engineered from the vendor's C sources (`linuxPOS/serialChargerInterface`). A few details could not be confirmed from the code. Check them with `probe`, with the service stopped:

```sh
evsectl probe -port /dev/ttyS1                      # status frames and firmware version
evsectl probe -port /dev/ttyS1 -duty 30             # board should then report duty 30 (18 A)
evsectl probe -port /dev/ttyS1 -enable -keepalive=false -count 20   # does enable lapse without the resend? (watchdog)
evsectl probe -port /dev/ttyS2 -type meter          # meter frame header bytes and scaling
```

Unconfirmed items:

- **Duty command:** that the `0x33` command takes the duty cycle in percent.
- **Meter reply header:** the header bytes of the meter reply. The parser only relies on the leading `R` and trailing `E`.
- **Meter scaling:** the power factor and frequency scaling, which are reported raw.
- **State codes 21–26:** their meaning. They are grouped the same way the vendor UI grouped them.

## Install on the EVSE

Copy `evsectl` and `deploy/` to the EVSE, then run `sudo deploy/install.sh`. The script:

1. Backs up root's crontab and disables the original software: cron jobs, serial daemons, apache2 and mysql. Nothing is deleted.
2. Installs `/usr/local/bin/evsectl`, `/etc/evse-controller/config.yaml` and a systemd unit running as user `evse`.
3. Points the kiosk Chrome at `http://127.0.0.1:8080/`.

Set `admin_password` in the config before using the settings page. `deploy/restore-original.sh` undoes the install.

## Web UI and API

| Path | |
|---|---|
| `/` | Status page: one card per port. Sized for the 800×480 kiosk; responsive elsewhere. |
| `/settings` | Settings, port enable/disable, logo upload, recent sessions. Needs HTTP Basic auth with `admin_password` (any user name). |
| `GET /api/v1/status` | Full status: ports, meters, load sharing. |
| `GET /api/v1/ports/{id}` | One port. |
| `POST /api/v1/ports/{id}/enable`, `/disable` | Admin. Persisted across restarts. |
| `GET /api/v1/sessions?limit=N` | Finished sessions, newest first (also stored in `data_dir/sessions.jsonl`). |
| `GET`/`PUT /api/v1/config` | Admin. Saving validates, rewrites the YAML and restarts device communication. |
| `PUT`/`DELETE /api/v1/logo` | Admin. Accepts PNG, JPEG, WebP or SVG up to 1 MB. Served at `/logo`. |

Port states are `idle`, `connected`, `charging`, `fault`, `offline` and `unknown`. The raw board state code and the full board status are included as well.

## Load sharing

**Port maximum.** Each port's `max_amps` is always enforced through the J1772 pilot duty cycle. The boards accept 20–94 % duty, so the usable range is 12–75 A.

**With `load_sharing.enabled`:**

- `system_max_amps` is divided among enabled ports with a vehicle.
- Charging vehicles share it equally. A port capped below its share passes the remainder on to the others.
- A vehicle that is connected but not requesting charge has `min_amps` reserved for it.
- Vehicles that can't be given `min_amps` are paused (charging disabled) until capacity frees up. The newest session waits first.
- An idle port stays enabled only if spare capacity covers `min_amps`. Otherwise a newly plugged vehicle is admitted only after the other ports have reduced.
- Reductions are sent immediately. Increases wait 6 s after any reduction, so vehicles have time to respond (J1772 allows 5 s) and the total offered stays within the budget.
- An offline board is assumed to keep drawing its last limit.

**`dynamic: true`** additionally needs meter channels:

- A port that draws less than its limit minus `throttle_margin_amps` for `throttle_delay_sec` is capped at its measured current plus `throttle_headroom_amps`. The freed current goes to the other ports.
- The port gets its full share back when its draw approaches the cap, and every `probe_interval_sec`.
- Ports without fresh meter data are never throttled.

## Notes

- Restarting the service, or saving settings, ends sessions in progress in the log. Charging itself continues and a new session record starts.
- The `-data-dir` and `-listen` flags override the config file. If you save settings while using them, the overridden values are written to the file.

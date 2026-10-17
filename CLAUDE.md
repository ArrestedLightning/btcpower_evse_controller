# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Replacement controller for a BTC Power dual-port J1772 EVSE: one static Go binary (`evsectl`) that drives the charge boards and power meter boards over serial, serves an embedded status page and JSON API, and does optional load sharing. It replaces the vendor stack (C serial daemons + MySQL + PHP + OCPP). The README is the user-facing reference for config, API, load-sharing behaviour and deployment.

## Commands

Toolchain is Go 1.22; always set `GOTOOLCHAIN=local` so `go get`/builds don't silently upgrade the module to a newer Go.

```sh
GOTOOLCHAIN=local go test ./...                                   # all tests (add -race for station/device)
GOTOOLCHAIN=local go test ./internal/loadmgr -run TestRandomizedBudgetInvariant -v
GOTOOLCHAIN=local go vet ./...

# Release build for the EVSE (must be static, it runs on Ubuntu 16.04)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOTOOLCHAIN=local go build -trimpath -ldflags "-s -w -X main.version=X.Y.Z" -o evsectl ./cmd/evsectl

# Run without hardware (simulated boards; vehicles come and go on their own)
go run ./cmd/evsectl -simulate -config deploy/config.example.yaml -data-dir ./data
curl -XPOST localhost:8080/api/v1/sim/ports/1/manual   # then plug|unplug|request|stop|fault|clear|auto

# Talk to a real board directly (service stopped)
evsectl probe -port /dev/ttyS1 [-duty 30] [-type meter]
```

To kill a background simulator, don't use `pkill -f evsectl`/`pgrep -f` (they match the invoking shell); use `ps -eo pid,args | awk '$2 ~ /evsectl$/'`.

## Architecture

Data flows one way per poll cycle: device drivers → `station` → `loadmgr` → back to device drivers; `web` only reads snapshots from `station` and calls a few mutators.

- `internal/proto` — pure wire formats (no I/O). Charger: 38400 baud, TX `02 CMD [DATA] 03 XOR`, 29-byte `STA…END` status frame, `F…R` version reply. Meter: 9600 baud, ASCII `MC8`, 89-byte `R…E` reply. Also amps↔duty conversion and the `ChargeState` grouping of raw board state codes. Scanners resync on garbage and partial frames. The golden test frame comes from a real capture.
- `internal/device` — `Charger`/`Meter` interfaces with serial and simulated implementations. Each serial driver owns its port in a goroutine: version handshake, then per cycle send enable/disable (acts as keep-alive), duty (on change, on state change, or every 10 s), poll. Readings carry a timestamp; stale after 5 s, port reopened after 30 s of silence.
- `internal/loadmgr` — pure, deterministic allocator: `Manager.Step(now, []PortInput) []PortOutput`. Safety invariant: the sum of currents advertised to vehicles never exceeds `system_max_amps` at any instant, accounting for J1772's 5 s vehicle response time. Hence reductions apply immediately but increases wait `IncreaseHoldoff` (6 s) after any reduction; idle ports are only armed from spare capacity; offline boards are reserved at their last advertised current; the newest session is paused when over capacity. `TestRandomizedBudgetInvariant` checks this — keep it passing when touching allocation.
- `internal/station` — the central loop. Each cycle reads devices, runs `loadmgr`, commands devices, tracks sessions (a session ends only after the vehicle has been absent `session_end_delay_sec`; boards blip "disconnected" when charging finishes), and builds the `Status` snapshot served by the API. `Store` persists per-port enable flags (`state.json`) and finished sessions (`sessions.jsonl`, size-rotated). Tests use `newWithDevices` with fake devices.
- `internal/app` — owns config + station lifecycle. `ApplyConfig` validates, saves YAML, and restarts the station in-process (keeping the running listen address and data dir). `Config()` returns a deep clone; `config.Clone` must deep-copy every pointer field (e.g. `*bool` display options).
- `internal/config` — YAML with `KnownFields` (unknown keys are errors), defaults, validation (1–4 chargers, 0–2 meters, 1-based meter refs, 12–75 A). Every field has both `yaml` and `json` tags because the settings page edits config via `GET`/`PUT /api/v1/config`. Optional booleans that default to true are `*bool` with an accessor (e.g. `ClockVisible()`).
- `internal/web` — Go 1.22 `ServeMux` patterns; admin endpoints use Basic auth against `admin_password` (403 if unset). Assets in `internal/web/assets` are embedded with `embed.FS`; the uploaded logo lives in `data_dir/logo.*`.
- `internal/rotlog` — size-based rotating log writer used by `-log-file`.
- `deploy/` — systemd unit, kiosk autostart, `install.sh`/`restore-original.sh`, example config.

## Constraints that aren't obvious from the code

- **Kiosk browser is Chromium 62 (also Firefox 57).** Front-end JS must be ES2016 with no `fetch` abort/`AbortController` (polling uses `XMLHttpRequest`); CSS must not use flexbox `gap` (use margins) or grid `gap` (use `grid-gap`). Primary display is 800×480, not touch — status first, controls belong on `/settings`.
- **Target OS is Ubuntu 16.04 / systemd 229.** Keep the unit file free of newer directives (`StateDirectory`, `ProtectSystem=strict`, `ReadWritePaths`); use `ReadWriteDirectories`. The binary must stay `CGO_ENABLED=0`.
- **Duty range is 20–94 % (12–75 A).** J1772 allows 10 %, but firmware 5.09C boards report anything below 20 % back as 20 %.
- Must work fully offline: no CDN or external fetches from the UI.
- Dependencies are deliberately minimal (`go.bug.st/serial`, `yaml.v3`); `go.bug.st/serial` and `golang.org/x/sys` are pinned to versions compatible with Go 1.22.
- Protocol details came from the vendor's C sources and a few remain unconfirmed (meter header bytes, PF/frequency scaling, meaning of state codes 21–26); see README.

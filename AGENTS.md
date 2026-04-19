# AGENTS.md

You are a Go backend engineer working on `marstek-prometheus-controller`: a
single-binary daemon that reads grid power from Prometheus and writes
timed-discharge commands to a Marstek B2500 battery over MQTT to keep grid
import/export near zero.

This is a **hardware-adjacent control loop**. Bad commands can cause the
battery to export to the grid (wasted energy), wear flash memory, or leave the
inverter in a bad state. Correctness and safety matter more than cleverness.

## Tech stack

- **Language:** Go 1.26.1 (see `go.mod` — don't downgrade)
- **Runtime:** single static binary, distroless Docker image (nonroot)
- **Key deps:**
  - `github.com/eclipse/paho.mqtt.golang` v1.5.1 — MQTT client
  - `github.com/prometheus/client_golang` v1.23.2 — metrics + HTTP scrape
- **Std lib only** for HTTP, JSON, logging (`log/slog`), config, tests. Don't
pull in new frameworks (no `cobra`, `viper`, `testify`, `zap`, etc.) without
explicit approval.
- **Config:** environment variables only, parsed in `internal/config`.
- **Logging:** `log/slog` with lowercase level names (for Loki).
- **CI:** GitHub Actions — `go test`, `golangci-lint` v2.11.4, multi-arch
Docker build pushed to `ghcr.io`.

## Commands

Use these exact commands — they match CI. Run them from the repo root.

```bash
make fmt                 # gofmt ./...
make lint                # go vet ./...  (CI also runs golangci-lint v2.11.4)
make test                # go test ./...
go test -race ./...      # run before finishing any concurrency-touching change
go test -run TestFoo ./internal/controller   # target one package/test
make build               # builds ./bin/marstek-controller
make docker-build        # local docker build (linux/amd64)
make run                 # go run ./cmd/marstek-controller (needs env vars)
```

Before committing, run at minimum:

```bash
make fmt && make lint && make test
```

If you change anything in `internal/controller`, `internal/mqttclient`, or
`cmd/`, also run `go test -race ./...`.

## Project structure

```
cmd/marstek-controller/   # main() — wiring only, no logic
internal/
  config/                 # env-var parsing + validation (the ONLY config source)
  controller/             # the control loop (Step, ramps, deadband, fallback)
  httpserver/             # /metrics /healthz /readyz
  marstek/                # Marstek wire protocol: topics, cd=20/cd=7 payload encoding
  metrics/                # all Prometheus metric definitions (one Registry)
  mqttclient/             # paho wrapper: connect, publish, subscribe, reconnect
  promclient/             # Prometheus HTTP query_range-free client (instant query)
  promquery/              # (helpers)
tools/marstek-probe/      # Python scripts for manual protocol debugging — NOT shipped
.github/workflows/ci.yaml # CI pipeline
Dockerfile                # multi-stage distroless build
Makefile                  # canonical task runner
README.md                 # user-facing docs (env vars, deployment, metrics)
```

**Hard rules for structure:**

- Nothing under `internal/` may import `cmd/`.
- `internal/controller` must not import `internal/mqttclient` directly — it
depends on the `Publisher` / `StatusSource` interfaces in
`internal/controller/interfaces.go`. Keep it mockable.
- All Prometheus metrics live in `internal/metrics`. Don't register metrics
ad-hoc elsewhere.
- All env vars are read in `internal/config`. Don't call `os.Getenv` anywhere
else.
- `cmd/marstek-controller/main.go` should stay ~thin wiring only.

## Code style

### Conventions

- `gofmt` + `go vet` must pass. CI enforces `golangci-lint` v2.11.4.
- Package comments on every package (`// Package foo ...`).
- Errors: wrap with `fmt.Errorf("context: %w", err)`. Never `panic` in
request/control paths. `os.Exit(1)` only from `main`.
- Logging: structured `slog` with key-value pairs, never formatted strings.
- Units in variable names and log keys: `Watts`, `Seconds`, `Duration`,
`watts`, `age_seconds`. Ambiguous `power` or `time` is not acceptable.
- Times: `time.Duration` in Go, `time.ParseDuration` strings in env
(e.g. `30s`, `2m`, `5m`). Never int-seconds.
- Public identifiers are `PascalCase`, private `camelCase`, env vars
`SCREAMING_SNAKE_CASE` (matching the README table).
- No blank-import tricks. No `init()` functions except for metric registration.

### Good style example

```go
// ✅ Good — wrapped error, structured log, units in names, context honored
func (c *Controller) readGrid(ctx context.Context) (float64, error) {
    sample, err := c.prom.Query(ctx)
    if err != nil {
        c.m.PrometheusErrorsTotal.WithLabelValues("other").Inc()
        return 0, fmt.Errorf("prometheus query: %w", err)
    }
    if age := time.Since(sample.SampleTime); age > c.cfg.PrometheusStaleAfter {
        c.m.PrometheusErrorsTotal.WithLabelValues("stale").Inc()
        slog.Warn("prometheus sample stale", "age_seconds", age.Seconds())
        return 0, errStale
    }
    return sample.Watts, nil
}
```

```go
// ❌ Bad — swallowed error, printf log, no units, no context
func readGrid() float64 {
    s, err := prom.Query()
    if err != nil {
        log.Printf("err: %v", err)
        return 0
    }
    return s.Watts
}
```

### Tests

- Standard library `testing` only. Table-driven where it fits.
- Use fakes (plain structs implementing the interface) over mocks — see
`internal/controller/controller_test.go` for the pattern (`fakeProm`,
`fakePublisher`). Do **not** introduce `testify`, `gomock`, or `mockery`.
- Every new control-loop behavior (ramp, deadband, fallback, bias) needs a
dedicated table-driven test case.
- Concurrency changes must have `go test -race` passing.

## Git workflow

- Branch off `main`. Small, focused PRs.
- Commit messages: short imperative subject line (e.g.
`controller: bypass ramp-down on active export`). Body optional but
welcomed for non-trivial changes.
- CI must be green (`test`, `lint`, `build`) before merge.
- **Never** force-push `main`. Never commit binaries — `marstek-controller`
and `bin/` are gitignored; don't re-add them.
- Release = push a `v*.*.`* tag on `main`. The `docker` job publishes the
multi-arch image to `ghcr.io/lucavb/marstek-prometheus-controller`.

## Safety boundaries — read carefully

This daemon talks to real hardware. The following rules exist because breaking
them has real-world cost (wasted energy, flash wear, or a grid-exporting
battery).

### Always do

- ✅ Keep `cd=20` (volatile) as the default write kind. `cd=7` writes to
  flash and is wear-limited.
- ✅ Preserve all five slots on every write — only the controlled slot's
  power changes. See `internal/controller` and `internal/marstek`.
- ✅ Clamp commanded power to `[0, MAX_OUTPUT_WATTS]` with 800 W as the hard
  ceiling (device limit).
- ✅ Drop to 0 W immediately when grid is exporting (the export fast-path
  that bypasses `RAMP_DOWN_WATTS_PER_CYCLE`).
- ✅ Fall back to zero discharge on any of: Prometheus stale, MQTT
  disconnected, status silent beyond `MQTT_STATUS_HARD_FAIL_AFTER`.
- ✅ Treat `MIN_OUTPUT_WATTS=80` as a floor on non-zero commands (device
  silently clamps 1–79 → 80). Only exact 0 disables the slot.
- ✅ **Don't fight the BMS.** When SoC is below the soft floor derived from
  `devStatus.DoDPercent`, go explicitly idle (disable the slot) rather than
  publishing commands the BMS will silently gate. Derive the floor from
  `(100 − DoDPercent) + BATTERY_SOC_FLOOR_MARGIN_PERCENT` so a DoD change
  in the Marstek app flows through automatically without a redeploy. Read
  SoC from `devStatus` (MQTT status), never from Prometheus — the exporter
  is a separate concern.

### Ask first

- ⚠️ Changing the control law, EMA smoothing default, import bias default,
or ramp defaults. These are tuned.
- ⚠️ Adding new runtime dependencies to `go.mod`.
- ⚠️ Changing the Prometheus metric names or labels under
`marstek_controller_*` — dashboards and alerts depend on them.
- ⚠️ Changing env var names in `internal/config` — they're documented in
`README.md` and in users' deployments.
- ⚠️ Changing MQTT topic structure in `internal/marstek` — it must match
what the device and the companion exporter expect.

### Never do

- 🚫 Never remove or weaken the `PERSIST_TO_FLASH` + `ALLOW_FLASH_WRITES`
foot-gun guard. Flash is finite; this gate is intentional.
- 🚫 Never raise `MaxOutputWatts` above 800. The device caps at 800 W; higher
values do nothing except confuse the operator.
- 🚫 Never make the controller modify charging mode (`cs=`) automatically.
Log a warning if `cs=1` is detected; let the user fix it in the app.
- 🚫 Never commit secrets, `.env` files, MQTT credentials, or device IDs.
`MARSTEK_DEVICE_ID` in examples must stay as the placeholder
`60323bd14b6e`.
- 🚫 Never commit the root `marstek-controller` binary, `bin/`, `tmp/`, or
`dist/`.
- 🚫 Never edit generated or fetched content under `tmp/` or `dist/`.
- 🚫 Never take a dependency on `internal/...` packages from outside this
module — they're internal by Go's rules, keep them that way.
- 🚫 Never introduce `time.Sleep` in the control loop for synchronization.
Use `time.Ticker`, `context`, or the existing `Clock` interface so tests
stay deterministic.
- 🚫 Never replace `log/slog` with another logger. Loki pipelines rely on the
current JSON shape and lowercase levels.

## Quick orientation for new tasks

If the task is…

- **"Add a new metric"** → define it in `internal/metrics/metrics.go`, emit
from `internal/controller`, document it in the `README.md` metrics table.
- **"Change a default"** → update `internal/config/config.go`, the README
env-var table, and add/extend a test in `internal/controller`.
- **"New protocol field"** → add it to `internal/marstek/protocol.go` with a
round-trip test in `protocol_test.go`.
- **"Debug the real device"** → use `tools/marstek-probe/mqtt_control.py`;
don't write new Go throwaway code for this.
- **"Reproduce an incident"** → write a table-driven test against
  `controller.Step` with fake `PromReader` / `Publisher` / `StatusSource`.
- **"Change SoC floor behavior"** → tweak `BATTERY_SOC_FLOOR_MARGIN_PERCENT`
  or `BATTERY_SOC_HYSTERESIS_PERCENT` in `internal/config/config.go`, then
  extend the `TestStep_SoCFloor_*` table tests in
  `internal/controller/controller_test.go`. Do not read SoC from Prometheus.


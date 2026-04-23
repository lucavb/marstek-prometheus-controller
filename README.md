# marstek-prometheus-controller

A Go daemon that keeps grid power near zero by adjusting the power of one
Marstek B2500 timed-discharge slot over MQTT.

It reads `electricity_power_watts` (configurable) from Prometheus, subscribes
to the device status topic for live battery state, and publishes `cd=20`
timed-discharge writes whenever the smoothed grid-power signal deviates outside
the configured deadband.

Works with any `hame_energy`-protocol Marstek device (HMJ-2 and siblings).

## How it works

1. **Grid power** is read from Prometheus each control cycle.
2. **Battery state** is received from the MQTT status topic in real time (the
  same broadcast your existing
   [prometheus-marstek-mqtt-exporter](https://github.com/lucavb/prometheus-marstek-mqtt-exporter)
   subscribes to — no conflict, no extra polling load).
3. **Control law**: `next_slot_power = EMA(grid_watts) − IMPORT_BIAS_WATTS`
   clamped to `[0, MAX_OUTPUT_WATTS]`, with ramp limits and a minimum hold time
   to avoid command chatter.
  - Grid importing (positive watts) → increase slot power (offset by bias).
  - Grid exporting (negative watts) → reduce slot power to zero immediately
    (ramp-down limit is bypassed when export is detected, see [Control bias](#control-bias)).
4. The full 5-slot timed-discharge schedule is published on every write
  (`cd=20`, volatile — no flash wear), with only the controlled slot's power
   changed. Other slots are preserved exactly as the device reported them.
5. On stale Prometheus data, MQTT disconnection, or prolonged status silence,
  the controller falls back to zero discharge and keeps retrying.

## Control bias

The controller is intentionally asymmetric:

**Import bias (`IMPORT_BIAS_WATTS`, default 50 W)**

The raw discharge target is `EMA(grid_watts) − IMPORT_BIAS_WATTS`. This means
the battery always tries to leave a small deliberate import rather than driving
the grid meter to exact zero. For example, with the default 50 W bias:

| Grid reading | Target slot power |
|---|---|
| 300 W import | 250 W discharge |
| 50 W import | 0 W (floor) |
| 0 W | 0 W |
| −50 W export | 0 W (floor) |

The reasoning is practical: any energy the battery discharges that ends up
exported to the grid is permanently lost. Over-importing by 50 W costs at most
a few cents per day; accidentally exporting burns battery cycles for zero gain.
Set `IMPORT_BIAS_WATTS=0` to disable the offset.

**Export fast-path**

Ramp-down limits (`RAMP_DOWN_WATTS_PER_CYCLE`) exist to prevent rapid swings
during normal load fluctuation. However, when the smoothed grid signal goes
negative (the house is actively exporting), every watt still being discharged
makes it worse. The controller therefore bypasses the ramp-down limit entirely
and drops directly to the computed target (0 W) in a single step when export is
detected. The ramp-down limit still applies when reducing during positive-grid
operation.

The same fast-path logic applies to `MIN_HOLD_TIME` and the min-delta gate: both use
`MIN_COMMAND_DELTA_WATTS_EXPORTING` (default `5` W) rather than the non-export
`MIN_COMMAND_DELTA_WATTS` (default `25` W) when the smoothed grid is negative,
so small export-driven reductions are never swallowed while the battery is
giving energy away.

## Full-battery override

In `ct_t=7` mode (externally-controlled setpoint) the device uses the commanded
slot-power as a **hard ceiling** on its AC output. When the battery is full
(SoC=100 %) and the house load is below the panels' production, this ceiling
can be too low to give firmware a legal place to put the excess solar energy —
so the device disables MPPT entirely. Panels go dark while the battery slowly
discharges serving the house load; MPPT only re-enables once SoC ticks off 100 %.

The full-battery override prevents this by raising the commanded ceiling to
`MAX_OUTPUT_WATTS` (800 W) whenever the battery is full and solar is producing.
With the ceiling lifted, firmware can route panels to house load and — provided
surplus feed-in is enabled in the Marstek app (`tc_dis=0`) — export the
remainder to the grid.

**Entry:** `FULL_BATTERY_SOC_ENTER_PERCENT` (default 100 %) for
`FULL_BATTERY_ENTER_CONSECUTIVE_SAMPLES` (default 2) consecutive control cycles
**and** `w1+w2 > 0 W` (sun is up).

**Exit:** SoC drops to or below `FULL_BATTERY_SOC_EXIT_PERCENT` (default 98 %)
**or** solar drops to 0 W. Normal zero-import control then resumes.

**Surplus feed-in requirement:** the "panels-to-grid" outcome requires surplus
feed-in to be enabled in the Marstek app. The controller logs a warning on first
status if it detects `tc_dis=1` with the override enabled. If feed-in is off,
raising the ceiling still prevents the overshoot trap but the device will serve
house load from solar rather than exporting the difference.

**Panel arrays above 800 W:** the override raises the ceiling to
`MAX_OUTPUT_WATTS=800` W (device hard limit — cannot be raised higher). Arrays
that can produce more than 800 W at peak will see a small amount of DC-side
curtailment near solar noon while the battery is full. This is still a strict
improvement over the pre-override state where MPPT was fully inhibited.

To disable the override entirely set `FULL_BATTERY_OVERRIDE_ENABLED=false`.

## Prerequisites

1. Your B2500 is configured to connect to a local MQTT broker — see the
  [exporter README](https://github.com/lucavb/prometheus-marstek-mqtt-exporter#readme).
2. A timed-discharge slot is already configured in the Marstek app to run
  all day (e.g. `00:00–23:59`). The controller only overwrites its **power**
   value; start/end times come from `SCHEDULE_START`/`SCHEDULE_END`.
3. The device clock is correct — timed-discharge slots silently won't fire if
  the device time is wrong. Run once after setup:
4. Charging mode is **simultaneous** (`cs=0`). The controller logs a warning
  if it detects `cs=1` but does not change it automatically.

## Configuration

All settings are environment variables:


| Variable                      | Default                    | Description                                                         |
| ----------------------------- | -------------------------- | ------------------------------------------------------------------- |
| `PROMETHEUS_BASE_URL`         | *required*                 | Prometheus base URL, e.g. `http://prometheus:9090`                  |
| `PROMETHEUS_GRID_POWER_QUERY` | `electricity_power_watts`  | PromQL expression returning grid power in watts                     |
| `PROMETHEUS_TIMEOUT`          | `5s`                       | HTTP timeout for Prometheus queries                                 |
| `PROMETHEUS_STALE_AFTER`      | `60s`                      | Reject samples older than this                                      |
| `MQTT_BROKER_URL`             | *required*                 | MQTT broker URL, e.g. `tcp://10.1.1.5:1883`                         |
| `MQTT_USERNAME`               | ``                         | Optional broker username                                            |
| `MQTT_PASSWORD`               | ``                         | Optional broker password                                            |
| `MQTT_CLIENT_ID`              | `marstek-controller-<pid>` | MQTT client ID                                                      |
| `MARSTEK_DEVICE_TYPE`         | `HMJ-2`                    | Device type segment in MQTT topics                                  |
| `MARSTEK_DEVICE_ID`           | *required*                 | Device ID segment in MQTT topics                                    |
| `MQTT_STATUS_STALE_AFTER`     | `2m`                       | Self-poll if no status received in this long                        |
| `MQTT_STATUS_POLL_TIMEOUT`    | `5s`                       | Timeout for the self-poll response                                  |
| `MQTT_STATUS_HARD_FAIL_AFTER` | `5m`                       | Fall back to zero discharge after this much silence                 |
| `CONTROL_INTERVAL`            | `15s`                      | Control loop cadence                                                |
| `SMOOTHING_ALPHA`             | `0.5`                      | EMA factor for the grid-power signal (0–1, higher = less smoothing) |
| `DEADBAND_WATTS`              | `25`                       | Suppress commands when smoothed power is within this band           |
| `IMPORT_BIAS_WATTS`           | `50`                       | Deliberate grid-import headroom; subtracted from the raw target so the battery always leaves this much import rather than driving to exact zero (see [Control bias](#control-bias)) |
| `RAMP_UP_WATTS_PER_CYCLE`     | `150`                      | Maximum discharge increase per loop iteration; `0` = unlimited      |
| `RAMP_DOWN_WATTS_PER_CYCLE`   | `300`                      | Maximum discharge decrease per loop iteration; `0` = unlimited. Bypassed on active export — see [Control bias](#control-bias). Bypassed on active export also skips `MIN_HOLD_TIME` for that cycle. |
| `MIN_COMMAND_DELTA_WATTS`     | `25`                       | Suppress writes where the change vs. the last command is smaller than this value (applies when smoothed grid >= 0, i.e. importing or idle). |
| `MIN_COMMAND_DELTA_WATTS_EXPORTING` | `5`               | Same idea but applied when the smoothed grid is negative (exporting). Defaults to `5` so 1–4 W meter noise around zero does not republish the same schedule, while still responding aggressively to real export events. Set to `0` to never filter during export. |
| `MIN_HOLD_TIME`               | `30s`                      | Minimum time between published commands                             |
| `MIN_OUTPUT_WATTS`            | `80`                       | Lower clamp on non-zero slot power. The B2500 silently clamps `v=0..79` to 80 W on an enabled slot; any computed target in that range is snapped up to this value. A target of exactly 0 W disables the slot (`a<N>=0`) — the only real way to stop discharge. |
| `MAX_OUTPUT_WATTS`            | `800`                      | Hard cap on slot power (device max is 800 W)                        |
| `SCHEDULE_SLOT`               | `1`                        | Which timed-discharge slot to drive (1–5)                           |
| `SCHEDULE_START`              | `00:00`                    | Slot start time written to the device                               |
| `SCHEDULE_END`                | `23:59`                    | Slot end time written to the device                                 |
| `HTTP_LISTEN_ADDR`            | `:8080`                    | HTTP bind address                                                   |
| `LOG_LEVEL`                   | `info`                     | `debug`, `info`, `warn`, `error`                                    |
| `LOG_FORMAT`                  | `text`                     | `text` or `json`                                                    |
| `PERSIST_TO_FLASH`            | `false`                    | Write to persistent flash (`cd=7`) instead of volatile (`cd=20`)    |
| `ALLOW_FLASH_WRITES`          | `false`                    | Must be `true` to enable `PERSIST_TO_FLASH` (foot-gun guard)        |
| `BATTERY_SOC_FLOOR_MARGIN_PERCENT` | `2`               | Added to `(100 − device DoD%)` to derive the controller SoC soft floor. When SoC falls at or below this floor, discharge is suppressed until SoC recovers by `BATTERY_SOC_HYSTERESIS_PERCENT`. |
| `BATTERY_SOC_HYSTERESIS_PERCENT`   | `5`               | Hysteresis band above the soft floor; discharge only resumes once SoC ≥ `(soft_floor + hysteresis)`. Prevents rapid on/off cycling near the floor. |
| `BATTERY_SOC_FLOOR_FALLBACK_PERCENT` | `15`            | Absolute SoC floor used when the device status does not report a DoD value (`do=0`). |
| `FULL_BATTERY_OVERRIDE_ENABLED`    | `true`            | Enable the full-battery override (see [Full-battery override](#full-battery-override)). Set to `false` to disable entirely. |
| `FULL_BATTERY_SOC_ENTER_PERCENT`   | `100`             | SoC threshold to enter the override. Must be 1–100. |
| `FULL_BATTERY_SOC_EXIT_PERCENT`    | `98`              | SoC threshold to exit the override. Must be 0–99 and less than `FULL_BATTERY_SOC_ENTER_PERCENT`. |
| `FULL_BATTERY_ENTER_CONSECUTIVE_SAMPLES` | `2`       | Number of consecutive control cycles at or above `FULL_BATTERY_SOC_ENTER_PERCENT` (with solar > 0) required before the override activates. Prevents false activation on rapid SoC jumps near the top. |


## Deployment

### Docker Compose

```yaml
services:
  marstek-controller:
    image: ghcr.io/lucavb/marstek-prometheus-controller:latest
    environment:
      - PROMETHEUS_BASE_URL=http://prometheus:9090
      - MQTT_BROKER_URL=tcp://10.1.1.5:1883
      - MARSTEK_DEVICE_ID=60323bd14b6e
    ports:
      - "8080:8080"
    restart: unless-stopped
```

### Binary

```bash
make build
PROMETHEUS_BASE_URL=http://prometheus:9090 \
  MQTT_BROKER_URL=tcp://10.1.1.5:1883 \
  MARSTEK_DEVICE_ID=60323bd14b6e \
  ./bin/marstek-controller
```

## HTTP Endpoints


| Path           | Description                                                                                      |
| -------------- | ------------------------------------------------------------------------------------------------ |
| `GET /metrics` | Prometheus scrape endpoint (controller's own metrics)                                            |
| `GET /healthz` | Liveness: always `200 ok` while the process is up                                                |
| `GET /readyz`  | Readiness: `200 ok` once the controller has completed at least one full control step that successfully read Prometheus and observed a live device status over MQTT; `503` otherwise |


## Prometheus Integration

Scrape the controller as a separate target alongside the exporter:

```yaml
scrape_configs:
  - job_name: marstek-exporter
    static_configs:
      - targets: ["marstek-exporter:9734"]

  - job_name: marstek-controller
    static_configs:
      - targets: ["marstek-controller:8080"]
```

### Exported Metrics

All metrics are prefixed `marstek_controller_` and carry a constant label
`device_id=<MARSTEK_DEVICE_ID>`.

**Controller state**


| Metric                       | Type  | Description                                                    |
| ---------------------------- | ----- | -------------------------------------------------------------- |
| `grid_power_watts`           | Gauge | Last Prometheus sample (W)                                     |
| `smoothed_grid_power_watts`  | Gauge | EMA-smoothed signal driving control (W)                        |
| `target_slot_power_watts`    | Gauge | Computed target before ramp/hold limits (W)                    |
| `commanded_slot_power_watts` | Gauge | Last value published to the device (W)                         |
| `slot_index`                 | Gauge | Slot being driven (1–5)                                        |
| `min_output_watts`           | Gauge | Lower clamp on non-zero commanded slot power (W)               |
| `max_output_watts`           | Gauge | Effective upper clamp (W)                                      |
| `state`                      | Gauge | 0=starting, 1=idle, 2=discharging, 3=holding, 4=fallback       |
| `info`                       | Gauge | Always 1; labels carry version, device_type, device_id, broker |
| `battery_soc_percent`        | Gauge | Device-reported battery SoC (%) as seen by the controller      |
| `battery_soc_soft_floor_percent` | Gauge | Derived SoC soft floor: `(100−DoD)+margin`. Discharge is suppressed below this value. |
| `battery_temp_min_celsius`   | Gauge | Device-reported minimum cell temperature (°C); observability only |
| `battery_temp_max_celsius`   | Gauge | Device-reported maximum cell temperature (°C); observability only |
| `full_battery_override_active` | Gauge | 1 while the full-battery override is active (SoC at ceiling, solar producing); 0 otherwise |
| `surplus_feed_in_enabled`    | Gauge | 1 when the device has surplus feed-in enabled (`tc_dis=0`); 0 when disabled |


**Dependency health**


| Metric                                      | Type  | Description                                      |
| ------------------------------------------- | ----- | ------------------------------------------------ |
| `mqtt_connected`                            | Gauge | 1 connected, 0 disconnected                      |
| `prometheus_up`                             | Gauge | 1 if last query was fresh, 0 if stale or errored |
| `last_prometheus_success_timestamp_seconds` | Gauge | Unix timestamp of last successful query          |
| `last_mqtt_publish_timestamp_seconds`       | Gauge | Unix timestamp of last successful publish        |
| `device_last_status_seconds`                | Gauge | Seconds since the last device status message     |
| `last_status_age_seconds`                   | Gauge | Seconds since last device status message         |


**Activity**


| Metric                          | Type      | Labels   | Description                                                                               |
| ------------------------------- | --------- | -------- | ----------------------------------------------------------------------------------------- |
| `prometheus_queries_total`      | Counter   |          | Total Prometheus queries                                                                  |
| `prometheus_errors_total`       | Counter   | `reason` | Query errors (stale, timeout, parse, empty, other)                                        |
| `mqtt_publishes_total`          | Counter   | `kind`   | Publishes by kind: `write`, `self_poll`                                                   |
| `mqtt_publish_errors_total`     | Counter   | `reason` | Publish failures (disconnected, timeout, other)                                           |
| `mqtt_reconnects_total`         | Counter   |          | Times the MQTT client reconnected                                                         |
| `mqtt_status_messages_total`    | Counter   |          | Total device status messages received                                                     |
| `self_polls_total`              | Counter   |          | Times the controller self-polled (status was stale)                                       |
| `control_cycles_total`          | Counter   |          | Total control loop iterations                                                             |
| `command_suppressed_total`      | Counter   | `reason` | Suppressed commands (deadband, delta, hold_time, disconnected, status_stale, soc_floor, transient_zero_output) |
| `fallback_total`                | Counter   | `reason` | Fallback events (prometheus_error, prometheus_stale, mqtt_status_stale, mqtt_write_error) |
| `full_battery_override_entered_total` | Counter | | Times the full-battery override has been activated (rising edge) |
| `full_battery_override_exited_total`  | Counter | | Times the full-battery override has been deactivated (falling edge) |
| `control_loop_duration_seconds` | Histogram |          | Wall time per control cycle                                                               |


### Suggested Alert Rules

```yaml
groups:
  - name: marstek_controller
    rules:
      - alert: MarsitekControllerMQTTDisconnected
        expr: marstek_controller_mqtt_connected == 0
        for: 5m
        annotations:
          summary: "Marstek controller MQTT disconnected"

      - alert: MarsitekControllerPrometheusStale
        expr: time() - marstek_controller_last_prometheus_success_timestamp_seconds > 120
        annotations:
          summary: "Marstek controller has not received fresh grid-power data"

      - alert: MarsitekControllerFallback
        expr: rate(marstek_controller_fallback_total[15m]) > 0
        annotations:
          summary: "Marstek controller is in fallback mode"

      - alert: MarsitekControllerDeviceStatusSilent
        expr: marstek_controller_device_last_status_seconds > 300
        annotations:
          summary: "Marstek controller has not received device status for 5 minutes"

      - alert: MarsitekControllerAtCap
        expr: marstek_controller_commanded_slot_power_watts >= marstek_controller_max_output_watts
        for: 30m
        annotations:
          summary: "Marstek controller is permanently at max output; battery may be undersized"
```

## Battery Safety Notes

- **No flash wear**: all control-loop writes use `cd=20` (volatile). Settings
reset on reboot — this is intentional. Use the Marstek app for persistent
configuration.
- **DoD enforcement is on the device**: when SOC reaches the DoD floor the
device simply stops outputting, regardless of what we command. The controller
does not need to track this.
- **Slot preservation**: every write sends all 5 slots with their current
values. The controlled slot's power is the only thing that changes.
- **Propagation latency**: writes take 5–15 s to take effect. `MIN_HOLD_TIME`
(default 30 s) ensures commands don't stack.

## Troubleshooting

### Device disappears from Wi-Fi and stops responding

One failure mode for the Marstek battery is a broken WPA2 4-way handshake loop
inside the device firmware. On the AP this shows up as repeated
`AP-STA-POSSIBLE-PSK-MISMATCH` lines for the battery MAC and **no**
corresponding `EAPOL-4WAY-HS-COMPLETED` — the device re-authenticates and
re-associates every ~7 s but never completes the key handshake.

This matches known ESP-IDF Wi-Fi bugs:

- [espressif/esp-idf#6920](https://github.com/espressif/esp-idf/issues/6920)
- [espressif/esp-idf#7286](https://github.com/espressif/esp-idf/issues/7286)
- [raspberrypi/linux#6975](https://github.com/raspberrypi/linux/issues/6975)

Observed characteristics (firmware `110.9` on the `HMJ-2` / B2500-D):

- The battery is **not** fully dead during the outage; it is repeatedly
  authenticating and associating, but failing the WPA2 key handshake.
- RF is fine. In the investigated case the AP saw about `-53 dBm`, which rules
  out poor signal as the primary cause.
- The device does **not** self-recover within any reasonable window. Measured:
  **405** `AP-STA-POSSIBLE-PSK-MISMATCH` attempts, **0** successful handshakes,
  and 7 deauthentications over 60 min on a dedicated SSID.
- The PSK is correct; the same PSK works on other devices on the same SSID,
  and works for this device immediately after either recovery step below.

AP-side mitigations that were tried and do **not** prevent the lockup (they
were kept for hygiene but the device still enters the loop regardless):

- Put the battery on its own dedicated 2.4 GHz SSID
- Keep it on the existing IoT VLAN/network
- Use `psk2` with `wpa_group_rekey = 86400`, `wpa_disable_eapol_key_retries = true`,
  and `ieee80211w = "0"`

The point of the dedicated SSID is to scope these more permissive settings to
the single misbehaving client instead of weakening the shared IoT SSID for
everyone else.

Conclusion: this is a pure **device firmware** bug in the ESP-IDF WPA
supplicant that ships inside the battery. No hostapd tuning will fix it.

### Recovery

Two paths, both of which immediately re-associate the device cleanly:

1. **Re-send `CMD_SET_WIFI` (0x05) over BLE.** This is the scripted path used
   in this repo, equivalent to entering WiFi credentials in the Marstek app:

   ```bash
   uv run tools/marstek-probe/ble_probe.py set-wifi \
       --ssid <your-ssid>
   # prompts for password on a tty, or reads MARSTEK_WIFI_PASSWORD env var,
   # or accepts --password 'xxx'
   ```

   Must be run within ~10 m of the battery. See
   [`tools/marstek-probe/README.md`](tools/marstek-probe/README.md#set-wifi-destructive)
   for the full flag surface.

2. **Cold power cycle** the battery (a smart plug works). Slightly slower,
   but usable without BLE range. A future iteration may automate this via a
   controller metric + smart-plug watchdog; not yet implemented.

### What to watch

- `marstek_controller_device_last_status_seconds` should normally stay low and
  grow only briefly during transient MQTT or Wi-Fi gaps.
- A sustained climb past half of `MQTT_STATUS_HARD_FAIL_AFTER` triggers a
  throttled warning log. Past the full `MQTT_STATUS_HARD_FAIL_AFTER` threshold,
  the controller falls back to zero discharge.
- Cheap IoT Wi-Fi stacks often drop or delay ICMP even when application traffic
  is fine, so packet loss alone is not enough evidence of RF trouble.

### Reporting to the vendor

If you hit this bug, a firmware-level fix can only come from Marstek. A
reproducible bug report including the firmware version (from the BLE
`DEVICE_INFO (0x04)` exchange — run `uv run tools/marstek-probe/ble_probe.py`)
and the `hostapd` log pattern above is the most useful form of pressure.

## Logging / Loki

The Docker image defaults to `LOG_FORMAT=json`. Every log line is a JSON object on stdout with the fields:

| Field | Example | Description |
|---|---|---|
| `time` | `2026-04-18T11:30:00.123Z` | RFC 3339 timestamp |
| `level` | `info` | Lowercase level: `debug`, `info`, `warn`, `error` |
| `msg` | `schedule updated` | Log message |
| `slot` | `1` | Structured key–value pairs added per call site |

Example Alloy / Promtail pipeline (no transformation stage needed — levels are already lowercase):

```logql
{app="marstek-controller"} | json | level="warn"
```

Example LogQL query to watch for fallbacks:

```logql
{app="marstek-controller"} | json | msg="fallback: commanded zero discharge"
```

For local development use `LOG_FORMAT=text` to get human-readable output.

## Development

```bash
make fmt        # gofmt
make lint       # go vet
make test       # go test ./...
make build      # bin/marstek-controller
make docker-build
```

For manual protocol debugging see `tools/marstek-probe/mqtt_control.py`.
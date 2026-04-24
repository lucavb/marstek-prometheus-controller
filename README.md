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
| ------------ | ----------------- |
| 300 W import | 250 W discharge   |
| 50 W import  | 0 W (floor)       |
| 0 W          | 0 W               |
| −50 W export | 0 W (floor)       |


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

## Near-full idle

When the battery is near full there is nothing useful for the controller to do:
any discharge it commands is just dumping stored energy that the panels are
already replacing. The controller therefore enters an explicit **idle regime**
in the top SoC band — it disables the controlled discharge slot entirely and
lets the firmware handle excess PV via its own surplus feed-in path
(`tc_dis=0`).

Near-full idle is SoC-driven and grid-gated on entry. While idle is active the
controller publishes a schedule with the slot disabled (`a<N>=0`, `v<N>=0`) and
otherwise gets out of the way:

- The battery does **not** discharge in the top band — even if the meter shows
grid import.
- Excess PV is routed to the grid by the device's firmware (provided surplus
feed-in is enabled in the app), not by the controller commanding a discharge
setpoint.
- The SoC soft floor still wins: if it's active, near-full idle does not
engage. (In practice the two ranges don't overlap, but the precedence is
explicit.)

**Entry (debounced):** `SoC ≥ NEAR_FULL_IDLE_ENTER_PERCENT` (default `98`)
**and** smoothed grid ≤ 0 (balanced or exporting) for
`NEAR_FULL_IDLE_CONSECUTIVE_SAMPLES` (default `2`) consecutive control cycles.
The grid-surplus gate is what prevents the post-`grid_import`-exit flap: on
the LFP 100% plateau SoC does not drop for many minutes after a secondary
exit, so a SoC-only entry counter would re-arm in two cycles and put idle
back on while the grid was still importing. Requiring smoothed ≤ 0 means
"we actually have surplus to feed back"; any positive smoothed reading
resets the entry counter.

**Exit (debounced):** `SoC < NEAR_FULL_IDLE_EXIT_PERCENT` (default `95`) for
`NEAR_FULL_IDLE_CONSECUTIVE_SAMPLES` consecutive control cycles. Normal
grid-meter-driven control then resumes on the next cycle. The 3-point
hysteresis band rides through LFP top-end SoC flicker without holding idle long
after the battery has actually come off full.

**Secondary exit on sustained grid import:** SoC alone is not enough to break
out of idle when solar drops below house load. With the controlled slot
disabled no discharge happens, so SoC stays pinned at 100% and the SoC exit
path never fires. Idle exits when the smoothed grid reading shows sustained
import above `NEAR_FULL_IDLE_GRID_IMPORT_EXIT_WATTS` (default `50`) for
`NEAR_FULL_IDLE_GRID_IMPORT_EXIT_SAMPLES` (default `8` ≈ 2 min at the 15 s
control interval) consecutive cycles, but only when the firmware is not already
in pass-through. When pass-through is active (`p1`/`p2` bit 1 set), the B2500
can ignore timed-discharge slots entirely, so publishing another slot would be
futile chatter.

**Pass-through stall recovery:** at full SoC with surplus feed-in enabled, the
B2500 firmware may enter solar pass-through and acknowledge timed-discharge
slots while still reporting `g1=g2=0`. If solar is below house load, the grid
imports even though the controller is trying to command discharge. The
controller detects this as a pass-through stall. By default it only logs and
exports metrics. To have it actually force discharge, set both
`PASSTHROUGH_AUTO_RECOVERY=true` and `ALLOW_FLASH_WRITES=true`. Recovery writes
the device's flash-only surplus-feed-in setting (`cd=31,touchuan_disa=1`) once
to break pass-through, lets normal timed-discharge control run, then restores
surplus feed-in (`cd=31,touchuan_disa=0`) after the battery leaves the full
plateau or the restore delay expires. Expect two flash writes per recovery
event.

**Hard dependency on surplus feed-in (`tc_dis=0`):** near-full idle will only
engage when the device reports `SurplusFeedIn = true`. With surplus feed-in
disabled, the firmware curtails MPPT at full SoC if there is no export path,
which would strand PV. To avoid that trap, the controller skips idle and keeps
running normal grid-meter-driven control whenever surplus feed-in is off, and
logs a warning on startup so the misconfiguration is visible. If surplus
feed-in flips off **while** idle is active, idle exits immediately (no
debounce) and normal control resumes.

**Kill switch:** set `NEAR_FULL_IDLE_ENABLED=false` to disable the regime
entirely. The controller then runs its normal grid-meter-driven control at all
SoC levels.

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


| Variable                             | Default                    | Description                                                                                                                                                                                                                                                       |
| ------------------------------------ | -------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `PROMETHEUS_BASE_URL`                | *required*                 | Prometheus base URL, e.g. `http://prometheus:9090`                                                                                                                                                                                                                |
| `PROMETHEUS_GRID_POWER_QUERY`        | `electricity_power_watts`  | PromQL expression returning grid power in watts                                                                                                                                                                                                                   |
| `PROMETHEUS_TIMEOUT`                 | `5s`                       | HTTP timeout for Prometheus queries                                                                                                                                                                                                                               |
| `PROMETHEUS_STALE_AFTER`             | `60s`                      | Reject samples older than this                                                                                                                                                                                                                                    |
| `MQTT_BROKER_URL`                    | *required*                 | MQTT broker URL, e.g. `tcp://10.1.1.5:1883`                                                                                                                                                                                                                       |
| `MQTT_USERNAME`                      | ``                         | Optional broker username                                                                                                                                                                                                                                          |
| `MQTT_PASSWORD`                      | ``                         | Optional broker password                                                                                                                                                                                                                                          |
| `MQTT_CLIENT_ID`                     | `marstek-controller-<pid>` | MQTT client ID                                                                                                                                                                                                                                                    |
| `MARSTEK_DEVICE_TYPE`                | `HMJ-2`                    | Device type segment in MQTT topics                                                                                                                                                                                                                                |
| `MARSTEK_DEVICE_ID`                  | *required*                 | Device ID segment in MQTT topics                                                                                                                                                                                                                                  |
| `MQTT_STATUS_STALE_AFTER`            | `2m`                       | Self-poll if no status received in this long                                                                                                                                                                                                                      |
| `MQTT_STATUS_POLL_TIMEOUT`           | `5s`                       | Timeout for the self-poll response                                                                                                                                                                                                                                |
| `MQTT_STATUS_HARD_FAIL_AFTER`        | `5m`                       | Fall back to zero discharge after this much silence                                                                                                                                                                                                               |
| `CONTROL_INTERVAL`                   | `15s`                      | Control loop cadence                                                                                                                                                                                                                                              |
| `SMOOTHING_ALPHA`                    | `0.5`                      | EMA factor for the grid-power signal (0–1, higher = less smoothing)                                                                                                                                                                                               |
| `DEADBAND_WATTS`                     | `25`                       | Suppress commands when smoothed power is within this band                                                                                                                                                                                                         |
| `IMPORT_BIAS_WATTS`                  | `50`                       | Deliberate grid-import headroom; subtracted from the raw target so the battery always leaves this much import rather than driving to exact zero (see [Control bias](#control-bias))                                                                               |
| `RAMP_UP_WATTS_PER_CYCLE`            | `150`                      | Maximum discharge increase per loop iteration; `0` = unlimited                                                                                                                                                                                                    |
| `RAMP_DOWN_WATTS_PER_CYCLE`          | `300`                      | Maximum discharge decrease per loop iteration; `0` = unlimited. Bypassed on active export — see [Control bias](#control-bias). Bypassed on active export also skips `MIN_HOLD_TIME` for that cycle.                                                               |
| `MIN_COMMAND_DELTA_WATTS`            | `25`                       | Suppress writes where the change vs. the last command is smaller than this value (applies when smoothed grid >= 0, i.e. importing or idle).                                                                                                                       |
| `MIN_COMMAND_DELTA_WATTS_EXPORTING`  | `5`                        | Same idea but applied when the smoothed grid is negative (exporting). Defaults to `5` so 1–4 W meter noise around zero does not republish the same schedule, while still responding aggressively to real export events. Set to `0` to never filter during export. |
| `MIN_HOLD_TIME`                      | `30s`                      | Minimum time between published commands                                                                                                                                                                                                                           |
| `MIN_OUTPUT_WATTS`                   | `80`                       | Lower clamp on non-zero slot power. The B2500 silently clamps `v=0..79` to 80 W on an enabled slot; any computed target in that range is snapped up to this value. A target of exactly 0 W disables the slot (`a<N>=0`) — the only real way to stop discharge.    |
| `MAX_OUTPUT_WATTS`                   | `800`                      | Hard cap on slot power (device max is 800 W)                                                                                                                                                                                                                      |
| `SCHEDULE_SLOT`                      | `1`                        | Which timed-discharge slot to drive (1–5)                                                                                                                                                                                                                         |
| `SCHEDULE_START`                     | `00:00`                    | Slot start time written to the device                                                                                                                                                                                                                             |
| `SCHEDULE_END`                       | `23:59`                    | Slot end time written to the device                                                                                                                                                                                                                               |
| `HTTP_LISTEN_ADDR`                   | `:8080`                    | HTTP bind address                                                                                                                                                                                                                                                 |
| `LOG_LEVEL`                          | `info`                     | `debug`, `info`, `warn`, `error`                                                                                                                                                                                                                                  |
| `LOG_FORMAT`                         | `text`                     | `text` or `json`                                                                                                                                                                                                                                                  |
| `PERSIST_TO_FLASH`                   | `false`                    | Write to persistent flash (`cd=7`) instead of volatile (`cd=20`)                                                                                                                                                                                                  |
| `ALLOW_FLASH_WRITES`                 | `false`                    | Must be `true` to enable `PERSIST_TO_FLASH` (foot-gun guard)                                                                                                                                                                                                      |
| `BATTERY_SOC_FLOOR_MARGIN_PERCENT`   | `2`                        | Added to `(100 − device DoD%)` to derive the controller SoC soft floor. When SoC falls at or below this floor, discharge is suppressed until SoC recovers by `BATTERY_SOC_HYSTERESIS_PERCENT`.                                                                    |
| `BATTERY_SOC_HYSTERESIS_PERCENT`     | `5`                        | Hysteresis band above the soft floor; discharge only resumes once SoC ≥ `(soft_floor + hysteresis)`. Prevents rapid on/off cycling near the floor.                                                                                                                |
| `BATTERY_SOC_FLOOR_FALLBACK_PERCENT` | `15`                       | Absolute SoC floor used when the device status does not report a DoD value (`do=0`).                                                                                                                                                                              |
| `NEAR_FULL_IDLE_ENABLED`             | `true`                     | Enable the near-full idle regime (see [Near-full idle](#near-full-idle)). Set to `false` to disable entirely; the controller will run normal grid-meter-driven control at all SoC levels.                                                                         |
| `NEAR_FULL_IDLE_ENTER_PERCENT`       | `98`                       | SoC threshold to enter idle after `NEAR_FULL_IDLE_CONSECUTIVE_SAMPLES` consecutive samples at or above it. Must satisfy `0 ≤ EXIT_PERCENT < ENTER_PERCENT ≤ 100`.                                                                                                 |
| `NEAR_FULL_IDLE_EXIT_PERCENT`        | `95`                       | SoC threshold to exit idle after `NEAR_FULL_IDLE_CONSECUTIVE_SAMPLES` consecutive samples strictly below it. The 3-point hysteresis band rides through LFP top-end SoC flicker.                                                                                   |
| `NEAR_FULL_IDLE_CONSECUTIVE_SAMPLES` | `2`                        | Debounce length (in control cycles) for both idle entry and exit. Must be ≥ 1. Surplus-feed-in flipping off bypasses this debounce and exits immediately.                                                                                                         |
| `NEAR_FULL_IDLE_GRID_IMPORT_EXIT_WATTS` | `50`                    | Smoothed-grid import threshold (W) above which an "import sample" is counted while idle is active. Should mirror `IMPORT_BIAS_WATTS` — setting it lower can cause flapping because normal control would not command a discharge on exit. Must be ≥ 0.              |
| `NEAR_FULL_IDLE_GRID_IMPORT_EXIT_SAMPLES` | `8`                  | Consecutive high-import samples required to exit idle via the grid-import path (≈ 2 min at the 15 s control interval). Set to `0` to disable this exit path entirely (SoC-only behaviour). Must be ≥ 0.                                                            |
| `PASSTHROUGH_STALL_DETECT_CYCLES`    | `5`                        | Consecutive cycles of full-SOC firmware pass-through, sustained grid import, commanded non-zero discharge, and `g1+g2=0` before a pass-through stall is recorded. Set to `0` to disable detection.                                                                |
| `PASSTHROUGH_STALL_MIN_COMMAND_WATTS` | `MIN_OUTPUT_WATTS`         | Minimum commanded or computed discharge target required to arm pass-through stall detection.                                                                                                                                                                      |
| `PASSTHROUGH_AUTO_RECOVERY`          | `false`                    | Opt-in recovery for full-SOC pass-through stalls. Requires `ALLOW_FLASH_WRITES=true` before any flash-only surplus-feed-in toggle is sent.                                                                                                                        |
| `PASSTHROUGH_AUTO_RECOVERY_MIN_INTERVAL` | `1h`                    | Minimum time between automatic pass-through recovery starts. Prevents repeated flash writes if the device keeps re-entering the same state.                                                                                                                       |
| `PASSTHROUGH_AUTO_RECOVERY_RESTORE_DELAY` | `5m`                  | Maximum time to leave surplus feed-in disabled after auto-recovery starts before restoring it, unless SoC drops below the near-full exit threshold first.                                                                                                          |
| `DEVICE_RESTART_SCHEDULE`            | `""` (disabled)            | **Opt-in.** 5-field UTC cron spec (e.g. `0 4` * * * for 04:00 daily). When empty the scheduler is not started and the device is never restarted by the controller. See [Scheduled device restart](#scheduled-device-restart).                                     |
| `DEVICE_RESTART_TIMEZONE`            | `UTC`                      | IANA timezone name for `DEVICE_RESTART_SCHEDULE` (e.g. `Europe/Berlin`). Ignored when `DEVICE_RESTART_SCHEDULE` is empty.                                                                                                                                         |


## Scheduled device restart

> **Disabled by default. Opt-in workaround while a device-hang root cause is being investigated. Remove once the root cause is resolved.**

Setting `DEVICE_RESTART_SCHEDULE` causes the controller to publish a `cd=10` (SOFTWARE_RESTART) command to the device at each scheduled time. The device goes offline for approximately 30 s. The controller's existing status-hard-fail fallback drops output to 0 W automatically during that window, so no extra coordination is needed. After reconnecting the device resumes normal operation and the controller republishes the discharge slot on the next control cycle.

### Configuration

```
DEVICE_RESTART_SCHEDULE=0 4 * * *
DEVICE_RESTART_TIMEZONE=Europe/Berlin
```

The schedule uses standard 5-field cron syntax: `minute hour day-of-month month day-of-week`. The timezone defaults to UTC and accepts any IANA zone name (the IANA database is embedded in the binary — no OS timezone package required).

An invalid spec or timezone name is a **hard startup error**; the daemon will refuse to start rather than silently running without the restart you asked for.

### DST edge cases

For zones with daylight saving time, avoid scheduling during the `02:00–03:00` local window. During spring-forward that hour does not exist and the occurrence is skipped; during fall-back Go resolves ambiguous wall-clock times to standard time (the later UTC occurrence). Any time outside that window is unaffected by DST transitions.

### Metrics


| Metric                                                     | Description                                                                    |
| ---------------------------------------------------------- | ------------------------------------------------------------------------------ |
| `marstek_controller_device_restart_info{spec, timezone}`   | Value 1 while the scheduler is active; not emitted when disabled.              |
| `marstek_controller_device_restarts_total{outcome}`        | Restart commands by outcome: `sent`, `skipped_not_connected`, `publish_error`. |
| `marstek_controller_last_device_restart_timestamp_seconds` | Unix timestamp of the last successful restart command.                         |
| `marstek_controller_next_device_restart_timestamp_seconds` | Unix timestamp of the next scheduled fire.                                     |


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


| Path           | Description                                                                                                                                                                         |
| -------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `GET /metrics` | Prometheus scrape endpoint (controller's own metrics)                                                                                                                               |
| `GET /healthz` | Liveness: always `200 ok` while the process is up                                                                                                                                   |
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


| Metric                           | Type  | Description                                                                           |
| -------------------------------- | ----- | ------------------------------------------------------------------------------------- |
| `grid_power_watts`               | Gauge | Last Prometheus sample (W)                                                            |
| `smoothed_grid_power_watts`      | Gauge | EMA-smoothed signal driving control (W)                                               |
| `target_slot_power_watts`        | Gauge | Computed target before ramp/hold limits (W)                                           |
| `commanded_slot_power_watts`     | Gauge | Last value published to the device (W)                                                |
| `slot_index`                     | Gauge | Slot being driven (1–5)                                                               |
| `min_output_watts`               | Gauge | Lower clamp on non-zero commanded slot power (W)                                      |
| `max_output_watts`               | Gauge | Effective upper clamp (W)                                                             |
| `state`                          | Gauge | 0=starting, 1=idle, 2=discharging, 3=holding, 4=fallback                              |
| `info`                           | Gauge | Always 1; labels carry version, device_type, device_id, broker                        |
| `battery_soc_percent`            | Gauge | Device-reported battery SoC (%) as seen by the controller                             |
| `battery_soc_soft_floor_percent` | Gauge | Derived SoC soft floor: `(100−DoD)+margin`. Discharge is suppressed below this value. |
| `battery_temp_min_celsius`       | Gauge | Device-reported minimum cell temperature (°C); observability only                     |
| `battery_temp_max_celsius`       | Gauge | Device-reported maximum cell temperature (°C); observability only                     |
| `near_full_idle_active`          | Gauge | 1 while the controller is in the near-full idle regime (slot disabled); 0 otherwise   |
| `surplus_feed_in_enabled`        | Gauge | 1 when the device has surplus feed-in enabled (`tc_dis=0`); 0 when disabled           |
| `passthrough_active`             | Gauge | 1 when device status `p1`/`p2` indicate firmware solar pass-through mode              |


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


| Metric                             | Type      | Labels   | Description                                                                                                                                    |
| ---------------------------------- | --------- | -------- | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| `prometheus_queries_total`         | Counter   |          | Total Prometheus queries                                                                                                                       |
| `prometheus_errors_total`          | Counter   | `reason` | Query errors (stale, timeout, parse, empty, other)                                                                                             |
| `mqtt_publishes_total`             | Counter   | `kind`   | Publishes by kind: `write`, `self_poll`                                                                                                        |
| `mqtt_publish_errors_total`        | Counter   | `reason` | Publish failures (disconnected, timeout, other)                                                                                                |
| `mqtt_reconnects_total`            | Counter   |          | Times the MQTT client reconnected                                                                                                              |
| `mqtt_status_messages_total`       | Counter   |          | Total device status messages received                                                                                                          |
| `self_polls_total`                 | Counter   |          | Times the controller self-polled (status was stale)                                                                                            |
| `control_cycles_total`             | Counter   |          | Total control loop iterations                                                                                                                  |
| `command_suppressed_total`         | Counter   | `reason` | Suppressed commands (`deadband`, `delta`, `hold_time`, `disconnected`, `status_stale`, `soc_floor`, `transient_zero_output`, `near_full_idle`) |
| `fallback_total`                   | Counter   | `reason` | Fallback events (prometheus_error, prometheus_stale, mqtt_status_stale, mqtt_write_error)                                                      |
| `near_full_idle_entered_total`     | Counter   |          | Times the near-full idle regime has been activated (rising edge)                                                                               |
| `near_full_idle_exited_total`      | Counter   |          | Times the near-full idle regime has been deactivated (falling edge)                                                                            |
| `near_full_idle_exit_reason_total` | Counter   | `reason` | Reason-specific exits from near-full idle (`soc_exit`, `grid_import`, `fallback`, `surplus_feed_in_disabled`, `disabled`)                      |
| `passthrough_stall_detected_total` | Counter   |          | Full-SOC pass-through stalls detected on the rising edge                                                                                        |
| `passthrough_recovery_total`       | Counter   | `outcome` | Pass-through recovery outcomes (`started`, `blocked_flash_guard`, `rate_limited`, `publish_error`, `restored`)                                |
| `surplus_feedin_toggled_total`     | Counter   | `direction` | Automatic surplus-feed-in toggles (`disable`, `restore`)                                                                                     |
| `control_loop_duration_seconds`    | Histogram |          | Wall time per control cycle                                                                                                                    |


### Migration: full-battery override → near-full idle

The previous `full-battery override` / "top-band passthrough" mechanism has
been removed entirely. It commanded `MAX_OUTPUT_WATTS` in the top SoC band,
which the firmware interpreted as a discharge setpoint and actively drained
the battery while PV was still producing. The new [Near-full idle](#near-full-idle)
regime instead **disables the slot** in the top SoC band and lets the
firmware's own surplus-feed-in path carry excess PV to the grid.

If you have dashboards or alerts referencing the old surface, replace them as
follows:


| Removed (no longer exposed)                                  | Replacement                                                                                                                     |
| ------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------- |
| `FULL_BATTERY_OVERRIDE_ENABLED`                              | `NEAR_FULL_IDLE_ENABLED`                                                                                                        |
| `FULL_BATTERY_SOC_ENTER_PERCENT`                             | `NEAR_FULL_IDLE_ENTER_PERCENT`                                                                                                  |
| `FULL_BATTERY_SOC_EXIT_PERCENT`                              | `NEAR_FULL_IDLE_EXIT_PERCENT`                                                                                                   |
| `FULL_BATTERY_ENTER_CONSECUTIVE_SAMPLES`                     | `NEAR_FULL_IDLE_CONSECUTIVE_SAMPLES`                                                                                            |
| `marstek_controller_full_battery_override_active`            | `marstek_controller_near_full_idle_active`                                                                                      |
| `marstek_controller_full_battery_override_entered_total`     | `marstek_controller_near_full_idle_entered_total`                                                                               |
| `marstek_controller_full_battery_override_exited_total`      | `marstek_controller_near_full_idle_exited_total`                                                                                |
| `marstek_controller_full_battery_override_exit_reason_total` | `marstek_controller_near_full_idle_exit_reason_total` (reasons: `soc_exit`, `grid_import`, `fallback`, `surplus_feed_in_disabled`, `disabled`) |


Old environment variables no longer have any effect — they will be ignored on
startup. Old metrics simply disappear from `/metrics`; recording rules and
alerts referencing them must be updated to the new names.

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
   Must be run within ~10 m of the battery. See
   `[tools/marstek-probe/README.md](tools/marstek-probe/README.md#set-wifi-destructive)`
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


| Field   | Example                    | Description                                       |
| ------- | -------------------------- | ------------------------------------------------- |
| `time`  | `2026-04-18T11:30:00.123Z` | RFC 3339 timestamp                                |
| `level` | `info`                     | Lowercase level: `debug`, `info`, `warn`, `error` |
| `msg`   | `schedule updated`         | Log message                                       |
| `slot`  | `1`                        | Structured key–value pairs added per call site    |


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
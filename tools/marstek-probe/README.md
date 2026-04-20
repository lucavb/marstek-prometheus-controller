# marstek-probe

Single-file Python diagnostic tool that probes a Marstek battery across every
plausible local interface and prints a report of what it actually exposes.

This was built because the repo's Go controller was originally scoped to the
Venus C/D/E UDP JSON-RPC Local API, and we needed empirical evidence of
whether a B2500-D (or any other model) speaks the same protocol before
committing to a transport for the controller.

## Prerequisites

- `uv` installed (<https://github.com/astral-sh/uv>)
- The battery on the same network, reachable by hostname or IP
- The battery actually online (B2500 units sleep their Wi-Fi stack when SOC is
  too low, typically below ~5-10%)

No Python install, virtualenv, or `pip install` required. Dependencies are
declared inline via PEP 723 and resolved by `uv run` automatically.

## Run

```bash
uv run tools/marstek-probe/probe.py --host 172.16.0.66
```

Or by DNS name:

```bash
uv run tools/marstek-probe/probe.py --host marstek-battery.iot
```

Flags:

- `--host`           target hostname or IP (default: `marstek-battery.iot`)
- `--port`           UDP port for the Marstek Local API (default: `30000`)
- `--timeout`        per-probe timeout in seconds (default: `2.0`)
- `--output PATH`    JSON report path (default: `tools/marstek-probe/reports/probe-<host>-<ts>.json`)
- `--verbose`        also dump per-probe response payloads to stdout

## What it probes

1. **DNS resolution** of `--host`.
2. **TCP port scan** of a small curated list: `22, 80, 443, 502, 1883, 8080, 8123, 8883, 8888`.
3. **UDP JSON-RPC** on port 30000 with the Venus-family method set:
   - `Marstek.GetDevice`
   - `Bat.GetStatus`
   - `ES.GetStatus`, `ES.GetMode`
   - `PV.GetStatus`
   - `Wifi.GetStatus`, `BLE.GetStatus`
   - `EM.GetStatus`
4. **HTTP GET** on any of `80 / 8080 / 443` that came back open, against
   `/`, `/status`, `/info`, `/api`, `/api/status`, `/api/info`, `/device`,
   `/metrics`. Response bodies are truncated to 4 KB in the report.
5. **Modbus TCP** on port 502 (only if open): FC=3 read holding register 0
   for unit IDs 1..5 until one answers or the sweep ends.
6. **mDNS / Zeroconf** browse for 3 s across common service types
   (`_http._tcp`, `_mqtt._tcp`, `_marstek._tcp`, `_hame._tcp`, `_esphomelib._tcp`, ...).
   Entries advertised by the target IP are highlighted.

All probes are read-only. No writes, no mode changes, no commands are sent.

## Interpreting the output

You'll see a table per probe, green `OK` or red `--`, plus a one-line
summary. The most important rows for the B2500 question are the
`UDP 30000 Marstek.GetDevice` and `UDP 30000 Bat.GetStatus` lines:

- If **both** respond cleanly: the device speaks the Venus-family Local API and
  the existing Go controller is a realistic starting point.
- If only `Marstek.GetDevice` answers: partial support. Worth reading the raw
  response (use `--verbose`) to see which firmware/model string it returns,
  then consulting the `MarstekDeviceOpenApi.pdf` for per-model scope.
- If the UDP probes all time out: the device almost certainly needs to be
  pointed at an MQTT broker (via BLE + `hmjs` or the Marstek app) before it
  will talk to anything locally. Move to setting up Mosquitto + `hm2mqtt`.

The full JSON report under `reports/` contains raw response bodies so you can
paste them back into planning conversations.

## BLE probe and config (`ble_probe.py`)

If the network probe finds nothing useful (the B2500 series will show exactly that result without a local MQTT broker configured), or if the battery has fallen off WiFi and needs to be re-provisioned without opening the Marstek app, use the Bluetooth tool:

```bash
uv run tools/marstek-probe/ble_probe.py
```

Run within ~10 m of the battery. It scans for devices advertising the Marstek Hame GATT service (`0000ff00-0000-1000-8000-00805f9b34fb`) or names matching `HM_*`, `B2500*`, `Marstek*`, `BluePalm*`, `MST*`, and picks the strongest candidate.

### `probe` (default, read-only)

Sends three read-only commands and dumps the parsed responses:

- `0x04` DEVICE_INFO -> model, device id, MAC, Wi-Fi SSID
- `0x03` RUNTIME_INFO -> SOC, input/output power, wifi/mqtt state, temperatures, daily totals
- `0x0F` CELL_INFO -> per-cell data

Output mirrors the UDP probe (rich summary table plus a JSON report under `reports/ble-probe-<ts>.json`).

### `set-wifi` (destructive)

Equivalent to re-entering WiFi credentials in the Marstek app. Useful when the battery is stuck in a WPA2 MIC-failure reassociation loop and no longer reachable over IP — see the "Troubleshooting" section of the repo README.

```bash
# explicit password (shows up in shell history — fine on a trusted machine)
uv run tools/marstek-probe/ble_probe.py set-wifi \
    --ssid my-iot-ssid --password 'hunter2'

# via env var (keeps it out of shell history)
MARSTEK_WIFI_PASSWORD='hunter2' \
    uv run tools/marstek-probe/ble_probe.py set-wifi --ssid my-iot-ssid

# omit --password entirely: prompted interactively on a tty
uv run tools/marstek-probe/ble_probe.py set-wifi --ssid my-iot-ssid
```

After the write, the device re-associates on the new SSID within ~30 s. The tool reads back `RUNTIME_INFO` once as a sanity check; `wifi_connected` may briefly still be `false` in the readback because reassociation hasn't completed yet. Rerun `probe` a minute later to confirm WiFi + MQTT are back.

If the SSID or password is wrong, the battery will drop off WiFi permanently until you rerun this command with correct values — BLE stays available regardless.

### `set-mqtt` / `reset-mqtt`

Point the battery at a custom MQTT broker (or reset it back to the Marstek cloud). Used during initial provisioning of `hm2mqtt`. See `--help` for flags.

### Common flags

- `--scan-timeout` BLE scan duration (default 10 s)
- `--cmd-timeout`  per-command response timeout (default 3 s)
- `--address`      skip discovery and connect to this BLE MAC / CoreBluetooth UUID directly
- `--output PATH`  custom report path (probe only)

On macOS, the first run will trigger a Bluetooth permission prompt for the terminal.

## Tuning

- Run with `--timeout 5.0` if probes are flaky on Wi-Fi.
- If your network has multiple Marstek devices and you want to discover all
  of them rather than probe one, use the Go CLI's broadcast discover
  (`go run ./cmd/marstek-cli discover --host 255.255.255.255`) first and
  then run this probe against each IP individually.

#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = [
#     "rich>=13",
#     "paho-mqtt>=1.6",
# ]
# ///
"""Marstek B2500-D MQTT control CLI.

Publishes commands to the local MQTT broker and optionally polls the
device's status response. No BLE required after initial SET_MQTT config.

Topic map:
  device → broker:      hame_energy/<type>/device/<id>/ctrl
  controller → device:  hame_energy/<type>/App/<id>/ctrl

Usage (from repo root):
  uv run tools/marstek-probe/mqtt_control.py status
  uv run tools/marstek-probe/mqtt_control.py set-dod 80
  uv run tools/marstek-probe/mqtt_control.py set-dod 80 --flash
  uv run tools/marstek-probe/mqtt_control.py set-mode simultaneous
  uv run tools/marstek-probe/mqtt_control.py set-outputs on off
  uv run tools/marstek-probe/mqtt_control.py set-threshold 200
  uv run tools/marstek-probe/mqtt_control.py set-surplus-feed-in on
  uv run tools/marstek-probe/mqtt_control.py sync-time
  uv run tools/marstek-probe/mqtt_control.py restart
  uv run tools/marstek-probe/mqtt_control.py raw 'cd=1'

Flash vs no-flash:
  By default, set commands use the no-flash cd codes (17-20) which are
  temporary and reset on reboot — safer for automation. Pass --flash to
  write to persistent storage (uses the base cd codes 3-5,7).

Protocol reference:
  hm2mqtt b2500V2: https://github.com/tomquist/hm2mqtt/blob/main/src/device/b2500V2.ts
"""

from __future__ import annotations

import argparse
import sys
import threading
import time
import uuid
import warnings
from datetime import datetime, timezone
from typing import Any

warnings.filterwarnings("ignore", category=DeprecationWarning, module="paho")
import paho.mqtt.client as mqtt
from rich.console import Console
from rich.table import Table

# ── defaults ──────────────────────────────────────────────────────────────────

DEFAULT_HOST = "10.1.1.5"
DEFAULT_PORT = 1883
DEFAULT_DEVICE_TYPE = "HMJ-2"
DEFAULT_DEVICE_ID = "60323bd14b6e"
DEFAULT_TIMEOUT = 8.0

# ── topic helpers ─────────────────────────────────────────────────────────────


def ctrl_topic(device_type: str, device_id: str) -> str:
    """Topic we publish commands to (App → device)."""
    return f"hame_energy/{device_type}/App/{device_id}/ctrl"


def status_topic(device_type: str, device_id: str) -> str:
    """Topic the device publishes status on (device → broker)."""
    return f"hame_energy/{device_type}/device/{device_id}/ctrl"


# ── command helpers ───────────────────────────────────────────────────────────

# No-flash cd codes: changes are temporary and reset on reboot.
# Flash cd codes (the enum values themselves) write to persistent storage.
_FLASH_CD = {
    "CHARGING_MODE": 3,
    "DISCHARGE_MODE": 4,
    "DISCHARGE_DEPTH": 5,
    "TIMED_DISCHARGE": 7,
    "SYNC_TIME": 8,
    "TIME_ZONE": 9,
    "RESTART": 10,
    "FACTORY_RESET": 11,
    "OUTPUT_THRESHOLD": 6,
    "SET_PHASE": 22,
    "SURPLUS_FEED_IN": 31,
}
_NO_FLASH_CD = {
    "CHARGING_MODE": 17,
    "DISCHARGE_MODE": 18,
    "DISCHARGE_DEPTH": 19,
    "TIMED_DISCHARGE": 20,
}


def _cd(name: str, flash: bool) -> int:
    if flash or name not in _NO_FLASH_CD:
        return _FLASH_CD[name]
    return _NO_FLASH_CD[name]


def make_cmd(cd: int, **params: Any) -> str:
    """Build a cd=N[,k=v,...] payload string."""
    parts = [f"cd={cd}"]
    for k, v in params.items():
        parts.append(f"{k}={v}")
    return ",".join(parts)


# ── payload parsing ───────────────────────────────────────────────────────────


def parse_payload(payload: str) -> dict[str, str]:
    """Parse 'k=v,k=v,...' device status string.

    Handles time values like e1=0:0 (HH:MM) correctly because we split
    on commas first, then partition on the first '=' only.
    """
    result: dict[str, str] = {}
    for token in payload.split(","):
        if "=" in token:
            k, _, v = token.partition("=")
            result[k.strip()] = v.strip()
    return result


def _int(d: dict[str, str], key: str, default: int = 0) -> int:
    try:
        return int(d.get(key, default))
    except ValueError:
        return default


# ── rich status display ───────────────────────────────────────────────────────


def print_status(console: Console, raw: dict[str, str]) -> None:
    soc = _int(raw, "pe")
    remaining = _int(raw, "kn")
    dod = _int(raw, "do")
    threshold = _int(raw, "lv")
    w1 = _int(raw, "w1")
    w2 = _int(raw, "w2")
    g1 = _int(raw, "g1")
    g2 = _int(raw, "g2")
    o1 = _int(raw, "o1")
    o2 = _int(raw, "o2")
    tl = _int(raw, "tl")
    th = _int(raw, "th")
    vv = raw.get("vv", "?")
    sv = raw.get("sv", "?")
    uv_ver = raw.get("uv", "?")
    cs_raw = _int(raw, "cs")
    cs_label = "Simultaneous" if cs_raw == 0 else "Charge → Discharge"
    adaptive = _int(raw, "md")
    tc_dis = _int(raw, "tc_dis")
    surplus_enabled = tc_dis == 0
    lmo = _int(raw, "lmo")
    lmi = _int(raw, "lmi")
    b1 = _int(raw, "b1")
    b2 = _int(raw, "b2")

    # Solar input status flags: bit0=charging, bit1=pass-through
    p1 = _int(raw, "p1")
    p2 = _int(raw, "p2")

    def _solar_flags(p: int) -> str:
        flags = []
        if p & 1:
            flags.append("charging")
        if p & 2:
            flags.append("pass-through")
        return f"[{', '.join(flags)}]" if flags else "[idle]"

    table = Table(show_header=False, box=None, padding=(0, 2), expand=False)
    table.add_column("Field", style="dim", min_width=22)
    table.add_column("Value")

    # Battery
    soc_color = "green" if soc > 20 else ("yellow" if soc > 10 else "red")
    table.add_row("SOC", f"[{soc_color}]{soc}%[/{soc_color}]")
    table.add_row("Remaining capacity", f"{remaining} Wh")
    table.add_row("Depth of discharge", f"{dod}%  (→ min SOC {100 - dod}%)")
    if b1 or b2:
        packs = [f"Extra {i}" for i, v in enumerate([b1, b2], 1) if v]
        table.add_row("Extra packs", ", ".join(packs))

    table.add_row("", "")

    # Solar inputs
    table.add_row("Solar input 1", f"{w1} W  {_solar_flags(p1)}")
    table.add_row("Solar input 2", f"{w2} W  {_solar_flags(p2)}")

    table.add_row("", "")

    # Outputs
    for n, on, pw in [(1, o1, g1), (2, o2, g2)]:
        col = "green" if on else "dim"
        table.add_row(f"Output {n}", f"[{col}]{'ON' if on else 'OFF'}[/{col}]  ({pw} W)")
    table.add_row("Output threshold", f"{threshold} W")

    table.add_row("", "")

    # Settings
    table.add_row("Charging mode", cs_label)
    table.add_row("Adaptive mode", "ON" if adaptive else "OFF")
    table.add_row(
        "Surplus feed-in",
        "[green]Enabled[/green]" if surplus_enabled else "[dim]Disabled[/dim]",
    )

    table.add_row("", "")

    # Hardware info
    table.add_row("Temperature", f"{tl} °C / {th} °C")
    table.add_row("Rated output", f"{lmo} W")
    table.add_row("Rated input", f"{lmi} W")
    table.add_row("Firmware", f"{vv}.{sv}")
    table.add_row("Bootloader", uv_ver)

    console.rule(f"[bold]HMJ-2  {DEFAULT_DEVICE_ID}")
    console.print(table)

    # Time periods
    has_any = any(_int(raw, f"d{i}") for i in range(1, 6))
    if has_any:
        console.rule("[dim]Timed discharge periods")
        for i in range(1, 6):
            enabled = _int(raw, f"d{i}")
            start = raw.get(f"e{i}", "?")
            end = raw.get(f"f{i}", "?")
            power = _int(raw, f"h{i}")
            if enabled:
                console.print(
                    f"  [green]Period {i}:[/green]  {start}–{end}  {power} W  [bold green][ENABLED][/bold green]"
                )
            else:
                console.print(f"  [dim]Period {i}:  {start}–{end}  {power} W[/dim]")


# ── MQTT session (synchronous request/response) ───────────────────────────────


class MQTTSession:
    """Thread-safe MQTT wrapper for one-shot command + optional response."""

    def __init__(self, host: str, port: int, timeout: float = 8.0) -> None:
        self.host = host
        self.port = port
        self.timeout = timeout
        self._client: mqtt.Client | None = None
        self._status_topic = ""
        self._connected = threading.Event()
        self._response: dict[str, str] | None = None
        self._response_event = threading.Event()

    def _make_client(self) -> mqtt.Client:
        cid = f"marstek-ctrl-{uuid.uuid4().hex[:8]}"
        try:
            return mqtt.Client(
                callback_api_version=mqtt.CallbackAPIVersion.VERSION1,
                client_id=cid,
            )
        except AttributeError:
            return mqtt.Client(client_id=cid)  # paho-mqtt < 2.0

    def connect(self, status_topic: str) -> None:
        self._status_topic = status_topic
        c = self._make_client()

        def on_connect(client: Any, _ud: Any, _flags: Any, rc: int) -> None:
            if rc == 0:
                client.subscribe(status_topic)
                self._connected.set()

        def on_message(_client: Any, _ud: Any, msg: Any) -> None:
            if msg.topic == self._status_topic:
                self._response = parse_payload(msg.payload.decode())
                self._response_event.set()

        c.on_connect = on_connect
        c.on_message = on_message
        c.connect(self.host, self.port, keepalive=30)
        c.loop_start()
        self._client = c

        if not self._connected.wait(timeout=5.0):
            c.loop_stop()
            raise ConnectionError(
                f"Could not connect to MQTT broker at {self.host}:{self.port}"
            )

    def publish(self, topic: str, payload: str) -> None:
        assert self._client is not None
        self._client.publish(topic, payload)

    def reset_response(self) -> None:
        self._response = None
        self._response_event.clear()

    def wait_for_status(self) -> dict[str, str] | None:
        self._response_event.wait(timeout=self.timeout)
        return self._response

    def disconnect(self) -> None:
        if self._client:
            self._client.loop_stop()
            self._client.disconnect()
            self._client = None


# ── shared helpers ────────────────────────────────────────────────────────────


def _poll_status(
    sess: MQTTSession,
    ct: str,
    console: Console,
    timeout: float,
) -> dict[str, str] | None:
    sess.reset_response()
    sess.publish(ct, "cd=1")
    raw = sess.wait_for_status()
    if raw is None:
        console.print(f"[red]no response from device within {timeout}s[/red]")
    return raw


def _set_and_verify(
    args: argparse.Namespace,
    console: Console,
    cmd: str,
    description: str,
) -> int:
    st = status_topic(args.device_type, args.device_id)
    ct = ctrl_topic(args.device_type, args.device_id)
    sess = MQTTSession(args.host, args.port, args.timeout)
    try:
        sess.connect(st)
        console.print(f"[dim]→ {cmd}[/dim]")
        sess.publish(ct, cmd)

        if not args.no_confirm:
            time.sleep(1.0)
            console.print("[dim]readback: cd=1[/dim]")
            raw = _poll_status(sess, ct, console, args.timeout)
            if raw is None:
                console.print("[yellow]command sent but no status readback received[/yellow]")
                return 0
            console.print(f"[green]✓ {description}[/green]")
            print_status(console, raw)
        else:
            console.print(f"[green]✓ {description} sent[/green]")
        return 0
    finally:
        sess.disconnect()


# ── subcommand handlers ───────────────────────────────────────────────────────


def cmd_status(args: argparse.Namespace, console: Console) -> int:
    st = status_topic(args.device_type, args.device_id)
    ct = ctrl_topic(args.device_type, args.device_id)
    sess = MQTTSession(args.host, args.port, args.timeout)
    try:
        console.print(f"[dim]connecting to {args.host}:{args.port}...[/dim]")
        sess.connect(st)
        console.print(f"[dim]polling {ct}...[/dim]")
        raw = _poll_status(sess, ct, console, args.timeout)
        if raw is None:
            return 1
        print_status(console, raw)
        return 0
    finally:
        sess.disconnect()


def cmd_set_dod(args: argparse.Namespace, console: Console) -> int:
    if not 0 <= args.value <= 100:
        console.print("[red]DoD must be 0–100[/red]")
        return 2
    cd = _cd("DISCHARGE_DEPTH", args.flash)
    return _set_and_verify(args, console, make_cmd(cd, md=args.value), f"DoD set to {args.value}%")


def cmd_set_mode(args: argparse.Namespace, console: Console) -> int:
    md = 0 if args.mode == "simultaneous" else 1
    cd = _cd("CHARGING_MODE", args.flash)
    return _set_and_verify(args, console, make_cmd(cd, md=md), f"charging mode → {args.mode}")


def cmd_set_outputs(args: argparse.Namespace, console: Console) -> int:
    def _bool(s: str) -> int:
        return 1 if s.lower() in ("on", "1", "true") else 0

    o1 = _bool(args.out1)
    o2 = _bool(args.out2)
    md = o1 | (o2 << 1)
    cd = _cd("DISCHARGE_MODE", args.flash)
    label = f"out1={'ON' if o1 else 'OFF'} out2={'ON' if o2 else 'OFF'}"
    return _set_and_verify(args, console, make_cmd(cd, md=md), f"outputs → {label}")


def cmd_set_threshold(args: argparse.Namespace, console: Console) -> int:
    if not 0 <= args.watts <= 800:
        console.print("[red]threshold must be 0–800 W[/red]")
        return 2
    cd = _cd("OUTPUT_THRESHOLD", args.flash)
    return _set_and_verify(
        args, console, make_cmd(cd, md=args.watts), f"output threshold → {args.watts} W"
    )


def cmd_set_surplus_feed_in(args: argparse.Namespace, console: Console) -> int:
    enable = args.state.lower() in ("on", "1", "true", "enable", "enabled")
    # tc_dis: 0 = feed-in ENABLED, 1 = feed-in DISABLED
    value = 0 if enable else 1
    cd = _cd("SURPLUS_FEED_IN", args.flash)
    label = "enabled" if enable else "disabled"
    return _set_and_verify(
        args, console, make_cmd(cd, touchuan_disa=value), f"surplus feed-in → {label}"
    )


def cmd_sync_time(args: argparse.Namespace, console: Console) -> int:
    now = datetime.now(timezone.utc)
    # Local timezone offset in minutes (positive = east of UTC)
    if time.daylight and time.localtime().tm_isdst:
        offset_min = -int(time.altzone / 60)
    else:
        offset_min = -int(time.timezone / 60)
    params = dict(
        wy=offset_min,
        yy=now.year - 1900,
        mm=now.month - 1,  # 0-based
        rr=now.day,
        hh=now.hour,
        mn=now.minute,
        ss=now.second,
    )
    cd = _cd("SYNC_TIME", args.flash)
    cmd = make_cmd(cd, **params)
    return _set_and_verify(
        args, console, cmd, f"time synced to {now.strftime('%Y-%m-%dT%H:%M:%SZ')}"
    )


def cmd_restart(args: argparse.Namespace, console: Console) -> int:
    st = status_topic(args.device_type, args.device_id)
    ct = ctrl_topic(args.device_type, args.device_id)
    sess = MQTTSession(args.host, args.port, args.timeout)
    try:
        sess.connect(st)
        console.print(
            "[yellow]sending SOFTWARE_RESTART — device will be offline for ~30 s[/yellow]"
        )
        sess.publish(ct, make_cmd(_cd("RESTART", True)))
        console.print("[green]✓ restart command sent[/green]")
        return 0
    finally:
        sess.disconnect()


def cmd_raw(args: argparse.Namespace, console: Console) -> int:
    return _set_and_verify(args, console, args.payload, f"raw payload: {args.payload}")


# ── argparse ──────────────────────────────────────────────────────────────────


def _add_common(p: argparse.ArgumentParser) -> None:
    p.add_argument("--host", default=DEFAULT_HOST, metavar="HOST")
    p.add_argument("--port", type=int, default=DEFAULT_PORT, metavar="PORT")
    p.add_argument("--device-type", default=DEFAULT_DEVICE_TYPE, metavar="TYPE")
    p.add_argument("--device-id", default=DEFAULT_DEVICE_ID, metavar="ID")
    p.add_argument("--timeout", type=float, default=DEFAULT_TIMEOUT, metavar="SECS")
    p.add_argument(
        "--no-confirm",
        action="store_true",
        help="skip status readback after set commands",
    )


def _add_flash(p: argparse.ArgumentParser) -> None:
    p.add_argument(
        "--flash",
        action="store_true",
        help="write to persistent flash (survives reboot); default is temporary",
    )


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Marstek B2500-D MQTT control CLI",
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    sub = parser.add_subparsers(dest="cmd", required=True)

    p = sub.add_parser("status", help="poll and display current device state")
    _add_common(p)

    p = sub.add_parser("set-dod", help="set depth of discharge (0–100 %%)")
    _add_common(p)
    _add_flash(p)
    p.add_argument("value", type=int, metavar="PERCENT")

    p = sub.add_parser("set-mode", help="set charging mode")
    _add_common(p)
    _add_flash(p)
    p.add_argument("mode", choices=["simultaneous", "charge-then-discharge"])

    p = sub.add_parser("set-outputs", help="enable/disable output ports (out1 out2)")
    _add_common(p)
    _add_flash(p)
    p.add_argument("out1", metavar="OUT1", help="on|off")
    p.add_argument("out2", metavar="OUT2", help="on|off")

    p = sub.add_parser("set-threshold", help="set battery output threshold (W)")
    _add_common(p)
    _add_flash(p)
    p.add_argument("watts", type=int, metavar="WATTS")

    p = sub.add_parser("set-surplus-feed-in", help="enable/disable surplus feed-in")
    _add_common(p)
    _add_flash(p)
    p.add_argument("state", metavar="STATE", help="on|off")

    p = sub.add_parser("sync-time", help="sync device clock to local system time")
    _add_common(p)
    _add_flash(p)

    p = sub.add_parser("restart", help="soft-restart the device (offline ~30 s)")
    _add_common(p)

    p = sub.add_parser("raw", help="send a raw payload string (for testing)")
    _add_common(p)
    p.add_argument("payload", metavar="PAYLOAD", help='e.g. \'cd=1\'')

    args = parser.parse_args()
    console = Console()

    handlers = {
        "status": cmd_status,
        "set-dod": cmd_set_dod,
        "set-mode": cmd_set_mode,
        "set-outputs": cmd_set_outputs,
        "set-threshold": cmd_set_threshold,
        "set-surplus-feed-in": cmd_set_surplus_feed_in,
        "sync-time": cmd_sync_time,
        "restart": cmd_restart,
        "raw": cmd_raw,
    }

    try:
        return handlers[args.cmd](args, console)
    except ConnectionError as exc:
        console.print(f"[red]{exc}[/red]")
        return 1
    except KeyboardInterrupt:
        console.print("[red]aborted[/red]")
        return 130


if __name__ == "__main__":
    sys.exit(main())

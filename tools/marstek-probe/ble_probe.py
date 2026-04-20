#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = [
#     "rich>=13",
#     "bleak>=0.22",
# ]
# ///
"""Bluetooth LE diagnostic + config tool for Marstek B2500 and Hame-protocol siblings.

This is the BLE counterpart to `probe.py`. The B2500-D ignores the Venus UDP
Local API, but it exposes a BLE GATT interface that is well-documented in the
tomquist/hmjs project and is the canonical way to configure these devices
locally.

Subcommands:
    probe       scan + connect + send three read-only commands (DEVICE_INFO,
                RUNTIME_INFO, CELL_INFO). Default if none given.
    set-wifi    point the battery at an SSID/password (equivalent to the
                Marstek app's WiFi setup step). Use this to recover a device
                stuck in a WPA2 MIC-failure loop without opening the app.
    set-mqtt    point the battery at a custom MQTT broker.
    reset-mqtt  reset MQTT config back to the Marstek cloud.

Run it while physically near the battery (~10 m line of sight).

Usage:
    uv run tools/marstek-probe/ble_probe.py
    uv run tools/marstek-probe/ble_probe.py --scan-timeout 15 --address AA:BB:CC:DD:EE:FF
    uv run tools/marstek-probe/ble_probe.py set-wifi --ssid my-iot --password 'hunter2'
    MARSTEK_WIFI_PASSWORD='hunter2' uv run tools/marstek-probe/ble_probe.py set-wifi --ssid my-iot

Protocol reference:
- hmjs GATT UUIDs:      https://github.com/tomquist/hmjs/blob/main/packages/ble/src/BLEDeviceManager.ts
- hmjs command frames:  https://github.com/tomquist/hmjs/blob/main/packages/protocol/src/HMDeviceProtocol.ts

Framing: 0x73 <len> 0x23 <cmd> <payload...> <xor-checksum>
"""

from __future__ import annotations

import argparse
import asyncio
import getpass
import json
import os
import re
import sys
import time
from dataclasses import asdict, dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from bleak import BleakClient, BleakScanner
from bleak.backends.device import BLEDevice
from bleak.backends.scanner import AdvertisementData
from rich.console import Console
from rich.table import Table


SERVICE_UUID = "0000ff00-0000-1000-8000-00805f9b34fb"
COMMAND_CHARACTERISTIC_UUID = "0000ff01-0000-1000-8000-00805f9b34fb"
STATUS_CHARACTERISTIC_UUID = "0000ff02-0000-1000-8000-00805f9b34fb"

START_BYTE = 0x73
IDENTIFIER_BYTE = 0x23

CMD_RUNTIME_INFO = 0x03
CMD_DEVICE_INFO = 0x04
CMD_CELL_INFO = 0x0F
CMD_SET_WIFI = 0x05
CMD_SET_MQTT = 0x20
CMD_RESET_MQTT = 0x21

MQTT_FIELD_SEPARATOR = "<.,.>"

READ_COMMANDS: list[tuple[int, str]] = [
    (CMD_DEVICE_INFO, "DEVICE_INFO (0x04)"),
    (CMD_RUNTIME_INFO, "RUNTIME_INFO (0x03)"),
    (CMD_CELL_INFO, "CELL_INFO (0x0f)"),
]


def build_mqtt_config_payload(
    host: str, port: int, ssl: bool, username: str = "", password: str = ""
) -> bytes:
    """Match hmjs createMqttConfigPayload exactly: ssl<.,.>host<.,.>port<.,.>user<.,.>pass<.,.>"""
    ssl_flag = "1" if ssl else "0"
    sep = MQTT_FIELD_SEPARATOR
    cfg = f"{ssl_flag}{sep}{host}{sep}{port}{sep}{username}{sep}{password}{sep}"
    return cfg.encode("utf-8")


def build_wifi_config_payload(ssid: str, password: str) -> bytes:
    """Match hmjs createWifiConfigPayload exactly: ssid<.,.>password (no trailing separator)."""
    if not ssid or not password:
        raise ValueError("SSID and password are required")
    if MQTT_FIELD_SEPARATOR in ssid or MQTT_FIELD_SEPARATOR in password:
        raise ValueError(f"SSID/password must not contain the separator {MQTT_FIELD_SEPARATOR!r}")
    return f"{ssid}{MQTT_FIELD_SEPARATOR}{password}".encode("utf-8")

NAME_PATTERNS: list[re.Pattern[str]] = [
    re.compile(r"^HM[_\-A-Z0-9]", re.IGNORECASE),
    re.compile(r"B2500", re.IGNORECASE),
    re.compile(r"Marstek", re.IGNORECASE),
    re.compile(r"BluePalm", re.IGNORECASE),
    re.compile(r"MST", re.IGNORECASE),
]


@dataclass
class DiscoveredDevice:
    address: str
    name: str | None
    rssi: int | None
    service_uuids: list[str]
    manufacturer_data: dict[str, str]
    is_candidate: bool
    match_reason: str


@dataclass
class CommandExchange:
    name: str
    command_byte: int
    tx_hex: str
    responses_hex: list[str]
    parsed: Any
    ok: bool
    elapsed_ms: float
    error: str | None = None


@dataclass
class Report:
    started_at: str
    finished_at: str = ""
    scan_timeout_s: float = 0.0
    discovered: list[DiscoveredDevice] = field(default_factory=list)
    target_address: str | None = None
    target_name: str | None = None
    connected: bool = False
    services: list[dict[str, Any]] = field(default_factory=list)
    exchanges: list[CommandExchange] = field(default_factory=list)
    error: str | None = None

    def to_json(self) -> dict[str, Any]:
        return {
            "started_at": self.started_at,
            "finished_at": self.finished_at,
            "scan_timeout_s": self.scan_timeout_s,
            "discovered": [asdict(d) for d in self.discovered],
            "target_address": self.target_address,
            "target_name": self.target_name,
            "connected": self.connected,
            "services": self.services,
            "exchanges": [asdict(e) for e in self.exchanges],
            "error": self.error,
        }


def build_command(command_byte: int, payload: bytes = b"") -> bytes:
    length = 4 + len(payload) + 1
    header = bytes([START_BYTE, length, IDENTIFIER_BYTE, command_byte])
    body = header + payload
    checksum = 0
    for b in body:
        checksum ^= b
    return body + bytes([checksum])


def parse_device_info(data: bytes) -> dict[str, str] | None:
    if len(data) < 6 or data[0] != START_BYTE or data[2] != IDENTIFIER_BYTE:
        return None
    if data[3] != CMD_DEVICE_INFO:
        return None
    try:
        payload = data[4:-1].decode("utf-8", errors="replace")
    except Exception:
        return None
    info: dict[str, str] = {}
    for part in payload.split(","):
        if "=" in part:
            k, v = part.split("=", 1)
            info[k.strip()] = v.strip()
    return info or None


def parse_runtime_info(data: bytes) -> dict[str, Any] | None:
    if len(data) < 39 or data[0] != START_BYTE or data[2] != IDENTIFIER_BYTE:
        return None
    if data[3] != CMD_RUNTIME_INFO:
        return None

    def u16(off: int) -> int:
        return int.from_bytes(data[off : off + 2], "little", signed=False)

    def s16(off: int) -> int:
        return int.from_bytes(data[off : off + 2], "little", signed=True)

    def u32(off: int) -> int:
        return int.from_bytes(data[off : off + 4], "little", signed=False)

    result: dict[str, Any] = {
        "in1_active": bool(data[4] & 0x01),
        "in1_transparent": bool(data[4] & 0x02),
        "in2_active": bool(data[5] & 0x01),
        "in2_transparent": bool(data[5] & 0x02),
        "in1_power_w": u16(6),
        "in2_power_w": u16(8),
        "soc_percent": u16(10),
        "dev_version": data[12],
        "charge_mode_load_first": bool(data[13] & 0x01),
        "out1_enable": bool(data[14] & 0x01),
        "out2_enable": bool(data[14] & 0x02),
        "wifi_connected": bool(data[15] & 0x01),
        "mqtt_connected": bool(data[15] & 0x02),
        "out1_active": data[16],
        "out2_active": data[17],
        "dod": data[18],
        "discharge_threshold": u16(19),
        "device_scene": data[21],
        "remaining_capacity": u16(22),
        "out1_power_w": u16(24),
        "out2_power_w": u16(26),
        "extern1_connected": data[28],
        "extern2_connected": data[29],
        "device_region": data[30],
        "time_hour": data[31],
        "time_minute": data[32],
        "temperature_low": s16(33),
        "temperature_high": s16(35),
    }
    if len(data) >= 41:
        result["device_sub_version"] = data[39]
    if len(data) >= 45:
        result["daily_total_battery_charge"] = u32(40)
    if len(data) >= 49:
        result["daily_total_battery_discharge"] = u32(44)
    if len(data) >= 53:
        result["daily_total_load_charge"] = u32(48)
    if len(data) >= 57:
        result["daily_total_load_discharge"] = u32(52)
    return result


def parse_cell_info(data: bytes) -> dict[str, Any] | None:
    try:
        text = data.decode("utf-8", errors="replace")
    except Exception:
        return None
    tokens = [t for t in re.split(r"[_\x00-\x1f]+", text) if t]
    if len(tokens) < 3:
        return None
    return {"raw_text": text, "tokens": tokens}


def classify(name: str | None, service_uuids: list[str]) -> tuple[bool, str]:
    if any(u.lower() == SERVICE_UUID for u in service_uuids):
        return True, f"advertises service {SERVICE_UUID}"
    if name:
        for pat in NAME_PATTERNS:
            if pat.search(name):
                return True, f"name matches pattern {pat.pattern!r}"
    return False, ""


async def scan(timeout: float, console: Console) -> list[DiscoveredDevice]:
    console.print(f"[dim]Scanning for BLE devices for {timeout:.1f}s...[/dim]")
    seen: dict[str, DiscoveredDevice] = {}

    def detection_callback(device: BLEDevice, adv: AdvertisementData) -> None:
        service_uuids = [u.lower() for u in (adv.service_uuids or [])]
        name = adv.local_name or device.name
        is_candidate, reason = classify(name, service_uuids)
        mfr = {
            str(k): v.hex() if isinstance(v, (bytes, bytearray)) else str(v)
            for k, v in (adv.manufacturer_data or {}).items()
        }
        seen[device.address] = DiscoveredDevice(
            address=device.address,
            name=name,
            rssi=adv.rssi if adv.rssi is not None else None,
            service_uuids=service_uuids,
            manufacturer_data=mfr,
            is_candidate=is_candidate,
            match_reason=reason,
        )

    scanner = BleakScanner(detection_callback=detection_callback)
    await scanner.start()
    try:
        await asyncio.sleep(timeout)
    finally:
        await scanner.stop()
    devices = list(seen.values())
    devices.sort(key=lambda d: (not d.is_candidate, -(d.rssi or -999)))
    return devices


async def exchange(
    client: BleakClient,
    command_byte: int,
    label: str,
    timeout: float,
    notify_bucket: dict[str, Any],
) -> CommandExchange:
    """Send a single command and collect whatever notifications come back.

    Notifications are already running on the STATUS characteristic for the whole
    session (see connect_and_probe). `notify_bucket["responses"]` is the shared
    list all notifications are appended to; we snapshot it before/after.
    """
    t0 = time.perf_counter() * 1000
    tx = build_command(command_byte)
    baseline = len(notify_bucket["responses"])
    error: str | None = None

    try:
        await client.write_gatt_char(COMMAND_CHARACTERISTIC_UUID, tx, response=False)
        await asyncio.sleep(0.05)
        await client.write_gatt_char(COMMAND_CHARACTERISTIC_UUID, tx, response=False)
    except Exception as exc:
        error = str(exc)

    deadline = time.perf_counter() + timeout
    while time.perf_counter() < deadline:
        await asyncio.sleep(0.1)
        new = notify_bucket["responses"][baseline:]
        if not new:
            continue
        joined = b"".join(new)
        looks_framed = (
            len(joined) >= 5
            and joined[0] == START_BYTE
            and joined[2] == IDENTIFIER_BYTE
            and joined[1] <= len(joined)
        )
        looks_cell = b"_" in joined and len(joined) >= 10
        if looks_framed or looks_cell:
            await asyncio.sleep(0.2)
            break
    await asyncio.sleep(0.1)

    responses: list[bytes] = list(notify_bucket["responses"][baseline:])

    if error is not None and not responses:
        return CommandExchange(
            name=label,
            command_byte=command_byte,
            tx_hex=tx.hex(),
            responses_hex=[],
            parsed=None,
            ok=False,
            elapsed_ms=(time.perf_counter() * 1000) - t0,
            error=error,
        )

    parsed: Any = None
    ok = bool(responses)
    if responses:
        joined = responses[0]
        for r in responses[1:]:
            joined += r
        if command_byte == CMD_DEVICE_INFO:
            parsed = parse_device_info(joined)
        elif command_byte == CMD_RUNTIME_INFO:
            parsed = parse_runtime_info(joined)
        elif command_byte == CMD_CELL_INFO:
            parsed = parse_cell_info(joined)

    return CommandExchange(
        name=label,
        command_byte=command_byte,
        tx_hex=tx.hex(),
        responses_hex=[r.hex() for r in responses],
        parsed=parsed,
        ok=ok,
        elapsed_ms=(time.perf_counter() * 1000) - t0,
    )


async def connect_and_probe(
    address: str, cmd_timeout: float, console: Console
) -> tuple[bool, list[dict[str, Any]], list[CommandExchange], str | None]:
    console.print(f"[dim]Connecting to {address}...[/dim]")
    try:
        async with BleakClient(address, timeout=15.0) as client:
            if not client.is_connected:
                return False, [], [], "bleak reports not connected"
            console.print("[green]connected[/green], enumerating GATT...")

            services_info: list[dict[str, Any]] = []
            for svc in client.services:
                services_info.append(
                    {
                        "uuid": str(svc.uuid),
                        "description": svc.description,
                        "characteristics": [
                            {
                                "uuid": str(ch.uuid),
                                "properties": list(ch.properties),
                                "description": ch.description,
                            }
                            for ch in svc.characteristics
                        ],
                    }
                )

            has_service = any(s["uuid"].lower() == SERVICE_UUID for s in services_info)
            if not has_service:
                return True, services_info, [], (
                    f"target service {SERVICE_UUID} not present; exchanges skipped"
                )

            notify_bucket: dict[str, Any] = {"responses": []}

            def on_notify(_: Any, payload: bytearray) -> None:
                notify_bucket["responses"].append(bytes(payload))

            await client.start_notify(STATUS_CHARACTERISTIC_UUID, on_notify)
            try:
                await asyncio.sleep(0.4)

                exchanges: list[CommandExchange] = []
                for cmd, label in READ_COMMANDS:
                    console.print(f"  [dim]-> {label}[/dim]")
                    ex = await exchange(client, cmd, label, cmd_timeout, notify_bucket)
                    exchanges.append(ex)
            finally:
                try:
                    await client.stop_notify(STATUS_CHARACTERISTIC_UUID)
                except Exception:
                    pass

            return True, services_info, exchanges, None
    except Exception as exc:
        return False, [], [], str(exc)


def print_scan_table(console: Console, devices: list[DiscoveredDevice]) -> None:
    table = Table(show_header=True, header_style="bold", title="BLE scan results")
    table.add_column("Candidate")
    table.add_column("Name")
    table.add_column("Address")
    table.add_column("RSSI", justify="right")
    table.add_column("Services / reason")
    for d in devices:
        mark = "[bold green]YES[/bold green]" if d.is_candidate else ""
        reason = d.match_reason
        if not reason and d.service_uuids:
            reason = ", ".join(d.service_uuids[:2])
        table.add_row(
            mark,
            d.name or "[dim]<unknown>[/dim]",
            d.address,
            str(d.rssi) if d.rssi is not None else "?",
            reason or "[dim]-[/dim]",
        )
    console.print(table)


def print_exchange_table(console: Console, exchanges: list[CommandExchange]) -> None:
    table = Table(show_header=True, header_style="bold", title="BLE exchanges")
    table.add_column("Command")
    table.add_column("Result", justify="center")
    table.add_column("ms", justify="right")
    table.add_column("Response (summary)")
    for e in exchanges:
        mark = "[bold green]OK[/bold green]" if e.ok else "[red]--[/red]"
        if e.error:
            summary = f"[red]{e.error}[/red]"
        elif e.parsed is None and e.responses_hex:
            summary = f"{sum(len(r) // 2 for r in e.responses_hex)} bytes raw (unparsed)"
        elif isinstance(e.parsed, dict):
            keys = list(e.parsed.keys())
            summary = f"keys: {', '.join(keys[:5])}{'...' if len(keys) > 5 else ''}"
        else:
            summary = str(e.parsed)
        table.add_row(e.name, mark, f"{e.elapsed_ms:6.1f}", summary)
    console.print(table)


def print_parsed_highlights(console: Console, exchanges: list[CommandExchange]) -> None:
    for e in exchanges:
        if not e.parsed:
            continue
        console.rule(e.name)
        console.print_json(data=e.parsed, indent=2)


def write_report(report: Report, output: Path | None) -> Path:
    if output is None:
        reports_dir = Path(__file__).parent / "reports"
        reports_dir.mkdir(parents=True, exist_ok=True)
        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        output = reports_dir / f"ble-probe-{stamp}.json"
    output.write_text(json.dumps(report.to_json(), indent=2, default=str), encoding="utf-8")
    return output


async def select_target(
    args: argparse.Namespace, console: Console
) -> tuple[DiscoveredDevice | None, list[DiscoveredDevice]]:
    """Scan and pick a target. Honours --address by MAC or CoreBluetooth UUID."""
    discovered = await scan(args.scan_timeout, console)
    print_scan_table(console, discovered)

    if args.address:
        for d in discovered:
            if d.address.lower() == args.address.lower():
                return d, discovered
        return (
            DiscoveredDevice(
                address=args.address,
                name=None,
                rssi=None,
                service_uuids=[],
                manufacturer_data={},
                is_candidate=True,
                match_reason="explicit --address",
            ),
            discovered,
        )

    candidates = [d for d in discovered if d.is_candidate]
    return (candidates[0] if candidates else None), discovered


async def send_write_and_readback(
    address: str,
    command_byte: int,
    payload: bytes,
    write_label: str,
    console: Console,
    cmd_timeout: float,
) -> tuple[bool, dict[str, Any] | None, str | None]:
    """Connect, send a single write command, then read RUNTIME_INFO to verify effect.

    Returns (write_ok, runtime_info_after, error_message).
    """
    tx = build_command(command_byte, payload)
    console.print(
        f"[dim]TX {write_label} ({len(tx)} bytes, "
        f"payload={len(payload)} bytes):[/dim] {tx.hex()}"
    )

    try:
        async with BleakClient(address, timeout=15.0) as client:
            if not client.is_connected:
                return False, None, "bleak reports not connected"

            notify_bucket: dict[str, Any] = {"responses": []}

            def on_notify(_: Any, pkt: bytearray) -> None:
                notify_bucket["responses"].append(bytes(pkt))

            await client.start_notify(STATUS_CHARACTERISTIC_UUID, on_notify)
            try:
                await asyncio.sleep(0.3)
                try:
                    await client.write_gatt_char(COMMAND_CHARACTERISTIC_UUID, tx, response=False)
                    await asyncio.sleep(0.1)
                    await client.write_gatt_char(COMMAND_CHARACTERISTIC_UUID, tx, response=False)
                except Exception as exc:
                    return False, None, f"write failed: {exc}"

                console.print("[green]write dispatched[/green], waiting 1.5 s for device to apply...")
                await asyncio.sleep(1.5)

                console.print("[dim]reading back RUNTIME_INFO...[/dim]")
                readback = await exchange(
                    client, CMD_RUNTIME_INFO, "RUNTIME_INFO (0x03)", cmd_timeout, notify_bucket
                )
                runtime = readback.parsed if isinstance(readback.parsed, dict) else None
                return True, runtime, None
            finally:
                try:
                    await client.stop_notify(STATUS_CHARACTERISTIC_UUID)
                except Exception:
                    pass
    except Exception as exc:
        return False, None, str(exc)


def print_mqtt_state(console: Console, runtime: dict[str, Any] | None, header: str) -> None:
    if not runtime:
        console.print(f"[yellow]{header}: no runtime readback[/yellow]")
        return
    mqtt = runtime.get("mqtt_connected")
    wifi = runtime.get("wifi_connected")
    soc = runtime.get("soc_percent")
    dod = runtime.get("dod")
    out1 = runtime.get("out1_enable")
    out2 = runtime.get("out2_enable")
    console.print(
        f"[bold]{header}[/bold] "
        f"mqtt_connected={mqtt}  wifi_connected={wifi}  "
        f"soc={soc}%  dod={dod}  out1_enable={out1}  out2_enable={out2}"
    )


async def run_set_mqtt(args: argparse.Namespace, console: Console) -> int:
    target, _ = await select_target(args, console)
    if target is None:
        console.print("[red]No candidate BLE device found.[/red]")
        return 1

    console.print(
        f"\n[bold]Target:[/bold] {target.name or '<unknown>'} ({target.address}) rssi={target.rssi}"
    )

    payload = build_mqtt_config_payload(
        host=args.host,
        port=args.port,
        ssl=args.ssl,
        username=args.user or "",
        password=args.password or "",
    )
    console.print(
        f"[dim]Config payload:[/dim] ssl={int(args.ssl)} host={args.host} port={args.port} "
        f"user={args.user or '(none)'} pass={'(set)' if args.password else '(none)'}"
    )

    ok, runtime, err = await send_write_and_readback(
        target.address,
        CMD_SET_MQTT,
        payload,
        "SET_MQTT (0x20)",
        console,
        args.cmd_timeout,
    )
    if not ok:
        console.print(f"[red]SET_MQTT failed: {err}[/red]")
        return 2
    print_mqtt_state(console, runtime, "After SET_MQTT:")
    console.print(
        "[dim]Note: mqtt_connected may take up to ~30 s to flip to true while the "
        "device establishes the session. Rerun `ble_probe.py probe` in a minute to confirm.[/dim]"
    )
    return 0


def resolve_wifi_password(args: argparse.Namespace, console: Console) -> str | None:
    """Pick the WiFi password from --password, MARSTEK_WIFI_PASSWORD env, or tty prompt."""
    if args.password:
        return args.password
    env = os.environ.get("MARSTEK_WIFI_PASSWORD")
    if env:
        return env
    if sys.stdin.isatty():
        try:
            return getpass.getpass("WiFi password: ") or None
        except (EOFError, KeyboardInterrupt):
            console.print("[red]aborted[/red]")
            return None
    console.print(
        "[red]no WiFi password provided (pass --password, set MARSTEK_WIFI_PASSWORD, or run on a tty)[/red]"
    )
    return None


async def run_set_wifi(args: argparse.Namespace, console: Console) -> int:
    password = resolve_wifi_password(args, console)
    if not password:
        return 1

    try:
        payload = build_wifi_config_payload(args.ssid, password)
    except ValueError as exc:
        console.print(f"[red]invalid WiFi config: {exc}[/red]")
        return 1

    target, _ = await select_target(args, console)
    if target is None:
        console.print("[red]No candidate BLE device found.[/red]")
        return 1

    console.print(
        f"\n[bold]Target:[/bold] {target.name or '<unknown>'} ({target.address}) rssi={target.rssi}"
    )
    console.print(
        f"[dim]Config payload:[/dim] ssid={args.ssid!r} password=(set, {len(password)} chars)"
    )
    console.print(
        "[yellow]Warning:[/yellow] an incorrect SSID or password will drop the battery off WiFi "
        "and you'll need to re-run this command (BLE still works) to recover."
    )

    ok, runtime, err = await send_write_and_readback(
        target.address,
        CMD_SET_WIFI,
        payload,
        "SET_WIFI (0x05)",
        console,
        args.cmd_timeout,
    )
    if not ok:
        console.print(f"[red]SET_WIFI failed: {err}[/red]")
        return 2
    print_mqtt_state(console, runtime, "After SET_WIFI:")
    console.print(
        "[dim]Note: wifi_connected may take up to ~30 s to flip to true while the "
        "device re-associates. MQTT will reconnect after that. Rerun "
        "`ble_probe.py probe` in a minute to confirm.[/dim]"
    )
    return 0


async def run_reset_mqtt(args: argparse.Namespace, console: Console) -> int:
    target, _ = await select_target(args, console)
    if target is None:
        console.print("[red]No candidate BLE device found.[/red]")
        return 1

    console.print(
        f"\n[bold]Target:[/bold] {target.name or '<unknown>'} ({target.address}) rssi={target.rssi}"
    )

    ok, runtime, err = await send_write_and_readback(
        target.address,
        CMD_RESET_MQTT,
        b"",
        "RESET_MQTT (0x21)",
        console,
        args.cmd_timeout,
    )
    if not ok:
        console.print(f"[red]RESET_MQTT failed: {err}[/red]")
        return 2
    print_mqtt_state(console, runtime, "After RESET_MQTT:")
    console.print(
        "[dim]The device should drop the custom broker and re-join the Marstek cloud MQTT. "
        "Allow 30-60 s.[/dim]"
    )
    return 0


async def run(args: argparse.Namespace, console: Console) -> Report:
    report = Report(
        started_at=datetime.now(timezone.utc).isoformat(),
        scan_timeout_s=args.scan_timeout,
    )

    try:
        target, discovered = await select_target(args, console)
        report.discovered = discovered

        if target is None:
            report.error = "no candidate Marstek/HM_ BLE device found"
            console.print(
                "[yellow]No candidate device found. "
                "Re-run within ~10 m of the battery, or pass --address explicitly.[/yellow]"
            )
            return report

        report.target_address = target.address
        report.target_name = target.name
        console.print(
            f"\n[bold]Target:[/bold] {target.name or '<unknown>'} ({target.address}) "
            f"rssi={target.rssi}"
        )

        connected, services, exchanges, err = await connect_and_probe(
            target.address, args.cmd_timeout, console
        )
        report.connected = connected
        report.services = services
        report.exchanges = exchanges
        if err:
            report.error = err
            console.print(f"[yellow]note:[/yellow] {err}")

        if exchanges:
            print_exchange_table(console, exchanges)
            print_parsed_highlights(console, exchanges)

    finally:
        report.finished_at = datetime.now(timezone.utc).isoformat()
    return report


def _add_common_ble_args(p: argparse.ArgumentParser) -> None:
    p.add_argument("--scan-timeout", type=float, default=10.0, help="BLE scan duration in seconds")
    p.add_argument("--cmd-timeout", type=float, default=3.0, help="per-command response timeout")
    p.add_argument(
        "--address",
        help="skip discovery and target this BLE address (or CoreBluetooth UUID on macOS) directly",
    )


def main() -> int:
    parser = argparse.ArgumentParser(description="Marstek B2500 BLE diagnostic + config tool")
    sub = parser.add_subparsers(dest="cmd")

    p_probe = sub.add_parser("probe", help="scan, connect, and send read-only commands (default)")
    _add_common_ble_args(p_probe)
    p_probe.add_argument(
        "--output", type=Path, help="report path (default: ./reports/ble-probe-<ts>.json)"
    )

    p_set = sub.add_parser("set-mqtt", help="point the battery at a custom MQTT broker via BLE")
    _add_common_ble_args(p_set)
    p_set.add_argument("--host", required=True, help="MQTT broker host or IP reachable from the battery")
    p_set.add_argument("--port", type=int, default=1883, help="MQTT broker port (default: 1883)")
    p_set.add_argument("--ssl", action="store_true", help="enable MQTT-over-TLS")
    p_set.add_argument("--user", default="", help="MQTT username (optional)")
    p_set.add_argument("--password", default="", help="MQTT password (optional)")

    p_reset = sub.add_parser("reset-mqtt", help="reset the battery's MQTT config back to Marstek cloud")
    _add_common_ble_args(p_reset)

    p_wifi = sub.add_parser(
        "set-wifi",
        help="point the battery at a WiFi SSID/password via BLE (equivalent to the app's WiFi config step)",
    )
    _add_common_ble_args(p_wifi)
    p_wifi.add_argument("--ssid", required=True, help="target WiFi SSID (2.4 GHz, WPA2)")
    p_wifi.add_argument(
        "--password",
        default="",
        help="WiFi password. If omitted, falls back to MARSTEK_WIFI_PASSWORD env var, then a tty prompt.",
    )

    args = parser.parse_args()
    if args.cmd is None:
        args = parser.parse_args(["probe", *sys.argv[1:]])

    console = Console()

    try:
        if args.cmd == "set-mqtt":
            console.rule("Marstek BLE: SET_MQTT")
            return asyncio.run(run_set_mqtt(args, console))
        if args.cmd == "reset-mqtt":
            console.rule("Marstek BLE: RESET_MQTT")
            return asyncio.run(run_reset_mqtt(args, console))
        if args.cmd == "set-wifi":
            console.rule("Marstek BLE: SET_WIFI")
            return asyncio.run(run_set_wifi(args, console))

        console.rule("Marstek BLE probe")
        report = asyncio.run(run(args, console))
    except KeyboardInterrupt:
        console.print("[red]aborted[/red]")
        return 130

    path = write_report(report, args.output)
    console.print(f"\n[dim]Full report written to[/dim] [bold]{path}[/bold]")
    return 0 if report.connected or report.discovered else 1


if __name__ == "__main__":
    sys.exit(main())

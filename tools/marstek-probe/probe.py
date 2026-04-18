#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = [
#     "rich>=13",
#     "zeroconf>=0.130",
#     "httpx>=0.27",
#     "pymodbus>=3.6",
# ]
# ///
"""Marstek B2500 (and friends) local-interface diagnostic probe.

Runs a battery of read-only probes against a single device, captures whatever
it gets back, and emits a human-readable summary plus a JSON report.

Usage:
    uv run tools/marstek-probe/probe.py --host 172.16.0.66
    uv run tools/marstek-probe/probe.py --host marstek-battery.iot --verbose
"""

from __future__ import annotations

import argparse
import asyncio
import json
import socket
import struct
import sys
import time
from dataclasses import asdict, dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import httpx
from rich.console import Console
from rich.table import Table
from zeroconf import ServiceBrowser, ServiceListener, Zeroconf


TCP_PORTS: list[tuple[int, str]] = [
    (22, "ssh"),
    (80, "http"),
    (443, "https"),
    (502, "modbus-tcp"),
    (1883, "mqtt"),
    (8080, "http-alt"),
    (8123, "home-assistant?"),
    (8883, "mqtts"),
    (8888, "http-alt2"),
]

UDP_JSONRPC_METHODS: list[tuple[str, dict[str, Any]]] = [
    ("Marstek.GetDevice", {"ble_mac": "0"}),
    ("Bat.GetStatus", {"id": 0}),
    ("ES.GetStatus", {"id": 0}),
    ("ES.GetMode", {"id": 0}),
    ("PV.GetStatus", {"id": 0}),
    ("Wifi.GetStatus", {"id": 0}),
    ("BLE.GetStatus", {"id": 0}),
    ("EM.GetStatus", {"id": 0}),
]

HTTP_PATHS: list[str] = [
    "/",
    "/status",
    "/info",
    "/api",
    "/api/status",
    "/api/info",
    "/device",
    "/metrics",
]

MDNS_SERVICE_TYPES: list[str] = [
    "_http._tcp.local.",
    "_https._tcp.local.",
    "_mqtt._tcp.local.",
    "_marstek._tcp.local.",
    "_hame._tcp.local.",
    "_esphomelib._tcp.local.",
    "_workstation._tcp.local.",
]


@dataclass
class ProbeResult:
    name: str
    ok: bool
    elapsed_ms: float
    summary: str
    detail: dict[str, Any] = field(default_factory=dict)


@dataclass
class Report:
    host: str
    resolved_ip: str | None
    started_at: str
    finished_at: str
    results: list[ProbeResult] = field(default_factory=list)

    def to_json(self) -> dict[str, Any]:
        return {
            "host": self.host,
            "resolved_ip": self.resolved_ip,
            "started_at": self.started_at,
            "finished_at": self.finished_at,
            "results": [asdict(r) for r in self.results],
        }


def now_ms() -> float:
    return time.perf_counter() * 1000.0


async def probe_dns(host: str) -> tuple[ProbeResult, str | None]:
    t0 = now_ms()
    try:
        loop = asyncio.get_running_loop()
        infos = await loop.getaddrinfo(host, None, type=socket.SOCK_STREAM)
        ips = sorted({info[4][0] for info in infos})
        elapsed = now_ms() - t0
        ip = ips[0] if ips else None
        return (
            ProbeResult(
                name="DNS",
                ok=bool(ips),
                elapsed_ms=elapsed,
                summary=f"{host} -> {', '.join(ips) or 'no addresses'}",
                detail={"addresses": ips},
            ),
            ip,
        )
    except Exception as exc:
        return (
            ProbeResult(
                name="DNS",
                ok=False,
                elapsed_ms=now_ms() - t0,
                summary=f"resolve failed: {exc}",
                detail={"error": str(exc)},
            ),
            None,
        )


async def probe_tcp_port(ip: str, port: int, label: str, timeout: float) -> ProbeResult:
    t0 = now_ms()
    try:
        fut = asyncio.open_connection(ip, port)
        reader, writer = await asyncio.wait_for(fut, timeout=timeout)
        writer.close()
        try:
            await writer.wait_closed()
        except Exception:
            pass
        return ProbeResult(
            name=f"TCP {port} ({label})",
            ok=True,
            elapsed_ms=now_ms() - t0,
            summary="open",
            detail={"port": port, "label": label, "state": "open"},
        )
    except (asyncio.TimeoutError, OSError) as exc:
        return ProbeResult(
            name=f"TCP {port} ({label})",
            ok=False,
            elapsed_ms=now_ms() - t0,
            summary=f"closed/filtered ({type(exc).__name__})",
            detail={"port": port, "label": label, "state": "closed", "error": str(exc)},
        )


async def tcp_scan(ip: str, timeout: float) -> list[ProbeResult]:
    tasks = [probe_tcp_port(ip, port, label, timeout) for port, label in TCP_PORTS]
    return await asyncio.gather(*tasks)


class UDPJsonRpcProtocol(asyncio.DatagramProtocol):
    def __init__(self) -> None:
        self.responses: list[tuple[bytes, tuple[str, int]]] = []
        self.ready: asyncio.Event = asyncio.Event()
        self.transport: asyncio.DatagramTransport | None = None

    def connection_made(self, transport: asyncio.BaseTransport) -> None:  # type: ignore[override]
        self.transport = transport  # type: ignore[assignment]

    def datagram_received(self, data: bytes, addr: tuple[str, int]) -> None:  # type: ignore[override]
        self.responses.append((data, addr))
        self.ready.set()


async def probe_udp_method(
    ip: str, port: int, method: str, params: dict[str, Any], request_id: int, timeout: float
) -> ProbeResult:
    t0 = now_ms()
    payload = json.dumps({"id": request_id, "method": method, "params": params}).encode("utf-8")
    loop = asyncio.get_running_loop()
    transport = None
    try:
        transport, protocol = await loop.create_datagram_endpoint(
            UDPJsonRpcProtocol,
            local_addr=("0.0.0.0", 0),
        )
        transport.sendto(payload, (ip, port))
        try:
            await asyncio.wait_for(protocol.ready.wait(), timeout=timeout)
        except asyncio.TimeoutError:
            return ProbeResult(
                name=f"UDP {port} {method}",
                ok=False,
                elapsed_ms=now_ms() - t0,
                summary="timeout, no response",
                detail={"method": method, "params": params, "error": "timeout"},
            )

        data, addr = protocol.responses[0]
        try:
            parsed = json.loads(data.decode("utf-8"))
        except Exception:
            return ProbeResult(
                name=f"UDP {port} {method}",
                ok=False,
                elapsed_ms=now_ms() - t0,
                summary=f"{len(data)} bytes, not JSON",
                detail={
                    "method": method,
                    "params": params,
                    "raw_hex": data.hex(),
                    "from": f"{addr[0]}:{addr[1]}",
                },
            )

        error = parsed.get("error") if isinstance(parsed, dict) else None
        if error:
            return ProbeResult(
                name=f"UDP {port} {method}",
                ok=False,
                elapsed_ms=now_ms() - t0,
                summary=f"json-rpc error: {error.get('code')} {error.get('message')}",
                detail={"method": method, "params": params, "response": parsed},
            )

        result_keys: list[str] = []
        if isinstance(parsed, dict) and isinstance(parsed.get("result"), dict):
            result_keys = list(parsed["result"].keys())
        summary = (
            "response OK"
            + (f", keys: {', '.join(result_keys[:6])}" if result_keys else "")
            + ("..." if len(result_keys) > 6 else "")
        )
        return ProbeResult(
            name=f"UDP {port} {method}",
            ok=True,
            elapsed_ms=now_ms() - t0,
            summary=summary,
            detail={"method": method, "params": params, "response": parsed},
        )
    except Exception as exc:
        return ProbeResult(
            name=f"UDP {port} {method}",
            ok=False,
            elapsed_ms=now_ms() - t0,
            summary=f"error: {exc}",
            detail={"method": method, "params": params, "error": str(exc)},
        )
    finally:
        if transport is not None:
            transport.close()


async def udp_jsonrpc_probe(ip: str, port: int, timeout: float) -> list[ProbeResult]:
    results: list[ProbeResult] = []
    for idx, (method, params) in enumerate(UDP_JSONRPC_METHODS, start=1):
        r = await probe_udp_method(ip, port, method, params, idx, timeout)
        results.append(r)
    return results


async def probe_udp_generic_port(ip: str, port: int, timeout: float) -> ProbeResult:
    """Best-effort liveness check for a UDP port by sending a Marstek.GetDevice probe.

    We can't reliably detect "closed" for UDP, so we only report positively when
    the device actually answers.
    """
    return await probe_udp_method(ip, port, "Marstek.GetDevice", {"ble_mac": "0"}, 9999, timeout)


async def probe_http(ip: str, port: int, scheme: str, timeout: float) -> list[ProbeResult]:
    results: list[ProbeResult] = []
    async with httpx.AsyncClient(
        verify=False,
        timeout=timeout,
        follow_redirects=False,
    ) as client:
        for path in HTTP_PATHS:
            t0 = now_ms()
            url = f"{scheme}://{ip}:{port}{path}"
            try:
                resp = await client.get(url)
                body = resp.content[:4096]
                try:
                    preview = body.decode("utf-8", errors="replace")
                except Exception:
                    preview = body.hex()
                results.append(
                    ProbeResult(
                        name=f"HTTP {scheme} {port} {path}",
                        ok=resp.status_code < 500,
                        elapsed_ms=now_ms() - t0,
                        summary=f"{resp.status_code}, {len(resp.content)} bytes, ct={resp.headers.get('content-type', '?')}",
                        detail={
                            "url": url,
                            "status": resp.status_code,
                            "headers": dict(resp.headers),
                            "body_preview": preview,
                        },
                    )
                )
            except Exception as exc:
                results.append(
                    ProbeResult(
                        name=f"HTTP {scheme} {port} {path}",
                        ok=False,
                        elapsed_ms=now_ms() - t0,
                        summary=f"error: {type(exc).__name__}",
                        detail={"url": url, "error": str(exc)},
                    )
                )
    return results


class MDNSCollector(ServiceListener):
    def __init__(self) -> None:
        self.found: list[dict[str, Any]] = []

    def add_service(self, zc: Zeroconf, type_: str, name: str) -> None:
        try:
            info = zc.get_service_info(type_, name, timeout=1000)
        except Exception:
            info = None
        entry: dict[str, Any] = {"type": type_, "name": name}
        if info is not None:
            try:
                addresses = [socket.inet_ntoa(a) for a in info.addresses]
            except Exception:
                addresses = []
            entry.update(
                {
                    "addresses": addresses,
                    "port": info.port,
                    "server": info.server,
                    "properties": {
                        (k.decode() if isinstance(k, bytes) else str(k)): (
                            v.decode("utf-8", errors="replace") if isinstance(v, bytes) else v
                        )
                        for k, v in (info.properties or {}).items()
                    },
                }
            )
        self.found.append(entry)

    def update_service(self, zc: Zeroconf, type_: str, name: str) -> None:
        pass

    def remove_service(self, zc: Zeroconf, type_: str, name: str) -> None:
        pass


async def mdns_browse(timeout: float, target_ip: str | None) -> ProbeResult:
    t0 = now_ms()

    def run_sync() -> list[dict[str, Any]]:
        zc = Zeroconf()
        collector = MDNSCollector()
        browsers = [ServiceBrowser(zc, stype, collector) for stype in MDNS_SERVICE_TYPES]
        time.sleep(timeout)
        for b in browsers:
            try:
                b.cancel()
            except Exception:
                pass
        zc.close()
        return collector.found

    try:
        entries = await asyncio.to_thread(run_sync)
    except Exception as exc:
        return ProbeResult(
            name="mDNS browse",
            ok=False,
            elapsed_ms=now_ms() - t0,
            summary=f"error: {exc}",
            detail={"error": str(exc)},
        )

    relevant: list[dict[str, Any]] = []
    if target_ip:
        for e in entries:
            if target_ip in e.get("addresses", []):
                relevant.append(e)
    summary = (
        f"{len(entries)} services found"
        + (f", {len(relevant)} match {target_ip}" if target_ip else "")
    )
    return ProbeResult(
        name="mDNS browse",
        ok=bool(entries),
        elapsed_ms=now_ms() - t0,
        summary=summary,
        detail={"all": entries, "matching_target": relevant},
    )


async def probe_modbus_tcp(ip: str, port: int, timeout: float) -> ProbeResult:
    """Minimal Modbus TCP probe: try to read holding register 0 for unit IDs 1..5."""
    t0 = now_ms()
    findings: list[dict[str, Any]] = []
    try:
        for unit_id in range(1, 6):
            r = await _modbus_read_once(ip, port, unit_id, timeout)
            findings.append(r)
            if r.get("ok"):
                break
    except Exception as exc:
        return ProbeResult(
            name=f"Modbus TCP {port}",
            ok=False,
            elapsed_ms=now_ms() - t0,
            summary=f"error: {exc}",
            detail={"error": str(exc)},
        )

    any_ok = any(f.get("ok") for f in findings)
    return ProbeResult(
        name=f"Modbus TCP {port}",
        ok=any_ok,
        elapsed_ms=now_ms() - t0,
        summary=(
            "holding reg 0 readable on unit " + str(next(f["unit"] for f in findings if f.get("ok")))
            if any_ok
            else "no unit id 1..5 answered"
        ),
        detail={"attempts": findings},
    )


async def _modbus_read_once(ip: str, port: int, unit_id: int, timeout: float) -> dict[str, Any]:
    """Send a raw Modbus/TCP read-holding-registers (FC=3) request for reg 0, qty 1."""
    try:
        reader, writer = await asyncio.wait_for(
            asyncio.open_connection(ip, port), timeout=timeout
        )
    except Exception as exc:
        return {"unit": unit_id, "ok": False, "error": f"connect: {exc}"}

    try:
        transaction_id = 1
        protocol_id = 0
        length = 6
        pdu = struct.pack(
            ">HHHBBHH",
            transaction_id,
            protocol_id,
            length,
            unit_id,
            3,
            0,
            1,
        )
        writer.write(pdu)
        await writer.drain()
        try:
            header = await asyncio.wait_for(reader.readexactly(8), timeout=timeout)
        except (asyncio.TimeoutError, asyncio.IncompleteReadError) as exc:
            return {"unit": unit_id, "ok": False, "error": f"read header: {exc}"}
        tid, pid, ln, uid, fn = struct.unpack(">HHHBB", header)
        remaining = ln - 2
        try:
            body = await asyncio.wait_for(reader.readexactly(remaining), timeout=timeout)
        except (asyncio.TimeoutError, asyncio.IncompleteReadError) as exc:
            return {"unit": unit_id, "ok": False, "error": f"read body: {exc}"}
        if fn & 0x80:
            return {
                "unit": unit_id,
                "ok": False,
                "error": f"modbus exception fn={fn:#x} code={body[0] if body else '?'}",
            }
        return {
            "unit": unit_id,
            "ok": True,
            "function": fn,
            "bytes": body.hex(),
        }
    finally:
        try:
            writer.close()
            await writer.wait_closed()
        except Exception:
            pass


def colorize(result: ProbeResult) -> str:
    return "[bold green]OK[/bold green]" if result.ok else "[red]--[/red]"


def print_summary(console: Console, report: Report) -> None:
    table = Table(show_header=True, header_style="bold")
    table.add_column("Probe")
    table.add_column("Result", justify="center")
    table.add_column("ms", justify="right")
    table.add_column("Detail")

    for r in report.results:
        table.add_row(r.name, colorize(r), f"{r.elapsed_ms:6.1f}", r.summary)
    console.print(table)


def print_verbose_details(console: Console, report: Report) -> None:
    for r in report.results:
        if not r.ok and "response" not in r.detail and "body_preview" not in r.detail:
            continue
        console.rule(r.name)
        console.print_json(data=r.detail, indent=2)


async def run_probes(host: str, port: int, timeout: float, verbose: bool, console: Console) -> Report:
    started = datetime.now(timezone.utc).isoformat()
    dns_result, ip = await probe_dns(host)
    report = Report(host=host, resolved_ip=ip, started_at=started, finished_at=started)
    report.results.append(dns_result)

    if not ip:
        report.finished_at = datetime.now(timezone.utc).isoformat()
        return report

    console.print(f"[dim]Probing {host} ({ip}) with {timeout:.1f}s per-probe timeout...[/dim]")

    tcp_results = await tcp_scan(ip, timeout)
    report.results.extend(tcp_results)

    open_ports = {r.detail["port"] for r in tcp_results if r.ok}

    udp_results = await udp_jsonrpc_probe(ip, port, timeout)
    report.results.extend(udp_results)

    http_results: list[ProbeResult] = []
    if 80 in open_ports:
        http_results.extend(await probe_http(ip, 80, "http", timeout))
    if 8080 in open_ports:
        http_results.extend(await probe_http(ip, 8080, "http", timeout))
    if 443 in open_ports:
        http_results.extend(await probe_http(ip, 443, "https", timeout))
    report.results.extend(http_results)

    if 502 in open_ports:
        report.results.append(await probe_modbus_tcp(ip, 502, timeout))

    mdns_result = await mdns_browse(timeout=3.0, target_ip=ip)
    report.results.append(mdns_result)

    report.finished_at = datetime.now(timezone.utc).isoformat()
    return report


def write_report(report: Report, output: Path | None) -> Path:
    if output is None:
        reports_dir = Path(__file__).parent / "reports"
        reports_dir.mkdir(parents=True, exist_ok=True)
        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        safe_host = report.host.replace("/", "_")
        output = reports_dir / f"probe-{safe_host}-{stamp}.json"
    output.write_text(json.dumps(report.to_json(), indent=2, default=str), encoding="utf-8")
    return output


def main() -> int:
    parser = argparse.ArgumentParser(description="Marstek B2500 local-interface diagnostic probe")
    parser.add_argument("--host", default="marstek-battery.iot", help="target hostname or IP")
    parser.add_argument("--port", type=int, default=30000, help="Marstek Local API UDP port")
    parser.add_argument("--timeout", type=float, default=2.0, help="per-probe timeout in seconds")
    parser.add_argument("--output", type=Path, help="path for the JSON report (default: ./reports/)")
    parser.add_argument("--verbose", action="store_true", help="also dump per-probe response details")
    args = parser.parse_args()

    console = Console()
    console.rule(f"Marstek probe: {args.host}")

    try:
        report = asyncio.run(
            run_probes(args.host, args.port, args.timeout, args.verbose, console)
        )
    except KeyboardInterrupt:
        console.print("[red]aborted[/red]")
        return 130

    print_summary(console, report)

    if args.verbose:
        print_verbose_details(console, report)

    path = write_report(report, args.output)
    console.print(f"\n[dim]Full report written to[/dim] [bold]{path}[/bold]")
    ok_probes = sum(1 for r in report.results if r.ok)
    console.print(f"[dim]{ok_probes}/{len(report.results)} probes ok.[/dim]")
    return 0


if __name__ == "__main__":
    sys.exit(main())

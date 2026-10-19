#!/usr/bin/env python3
"""Exercise the real stack against a temporary fake daemon, never a working one."""
import base64
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import ipaddress
import os
from pathlib import Path
import re
import secrets
import shutil
import socket
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request


class Fixture(BaseHTTPRequestHandler):
    def do_GET(self):
        body = b"# TYPE gortex_daemon_ready gauge\ngortex_daemon_ready 1\n# TYPE gortex_runtime_goroutines gauge\ngortex_runtime_goroutines 7\n"
        self.send_response(200)
        self.send_header("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def request(url, data=None, headers=None):
    req = urllib.request.Request(url, data=data, headers=headers or {})
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect)
    with opener.open(req, timeout=4) as response:
        return response.read()


def wait_for(check, name, timeout=100):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            if check():
                return
        except (OSError, ValueError, KeyError) as error:
            last = error
        time.sleep(1)
    raise RuntimeError(f"timed out waiting for {name}: {last}")


def listeners():
    found = set()
    for family in ("tcp", "tcp6", "udp", "udp6"):
        path = Path("/proc/net") / family
        if not path.exists():
            continue
        for row in path.read_text().splitlines()[1:]:
            fields = row.split()
            if family.startswith("tcp") and fields[3] != "0A":
                continue
            if family.startswith("udp") and fields[3] != "07":
                continue
            raw, port = fields[1].split(":")
            packed = bytes.fromhex(raw)
            if len(packed) == 4:
                packed = packed[::-1]
            else:
                packed = b"".join(packed[i:i+4][::-1] for i in range(0, 16, 4))
            found.add((family, str(ipaddress.ip_address(packed)), int(port, 16)))
    return found


def isolated_metrics_port(value):
    if not value:
        return None
    match = re.fullmatch(r"http://127\.0\.0\.1:([0-9]+)/metrics", value)
    if not match or not 1 <= int(match.group(1)) <= 65535:
        raise ValueError("GORTEX_OBSERVABILITY_SMOKE_METRICS_URL requires http://127.0.0.1:PORT/metrics from an isolated test daemon")
    return int(match.group(1))


def main():
    metrics_url = os.environ.get("GORTEX_OBSERVABILITY_SMOKE_METRICS_URL", "")
    explicit_port = isolated_metrics_port(metrics_url)
    root = Path(__file__).resolve().parent
    subprocess.run(["python3", str(root / "validate.py")], check=True)
    server = ThreadingHTTPServer(("127.0.0.1", 0), Fixture)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    reservations = []
    target_port = explicit_port or server.server_port
    replacements = {9474: target_port}
    for port in (3000, 9090, 3200, 4318, 9095):
        sock = socket.socket()
        sock.bind(("127.0.0.1", 0))
        reservations.append(sock)
        replacements[port] = sock.getsockname()[1]
    project = "gortex-observability-test-" + secrets.token_hex(5)
    with tempfile.TemporaryDirectory(prefix="gortex-observability-test-") as temporary:
        work = Path(temporary)
        shutil.copytree(root, work, dirs_exist_ok=True, ignore=shutil.ignore_patterns(".state", "__pycache__"))
        for path in work.rglob("*"):
            if path.is_file() and path.suffix in (".yaml", ".json"):
                value = path.read_text()
                for before, after in replacements.items():
                    value = re.sub(r"(?<!\d)" + str(before) + r"(?!\d)", str(after), value)
                path.write_text(value)
        # Scrape only the fixture, using a private project and fresh named volumes.
        prometheus = work / "prometheus.yaml"
        prometheus.write_text(prometheus.read_text().replace("scrape_interval: 5s", "scrape_interval: 1s").replace("scrape_timeout: 3s", "scrape_timeout: 1s"))
        subprocess.run(["sh", str(work / "stack.sh"), "init"], cwd=work, check=True)
        secret = work / ".state/grafana-password"
        password = secret.read_text().strip()
        assert (work / ".state").stat().st_mode & 0o777 == 0o700
        assert secret.stat().st_mode & 0o777 == 0o444
        compose = ["docker", "compose", "-p", project, "--profile", "tracing"]
        for sock in reservations:
            sock.close()
        try:
            subprocess.run(compose + ["config", "-q"], cwd=work, check=True)
            before_listeners = listeners()
            subprocess.run(compose + ["up", "-d"], cwd=work, check=True)
            prom = f"http://127.0.0.1:{replacements[9090]}"
            grafana = f"http://127.0.0.1:{replacements[3000]}"
            tempo = f"http://127.0.0.1:{replacements[3200]}"
            wait_for(lambda: request(prom + "/-/ready"), "Prometheus")
            wait_for(lambda: request(grafana + "/api/health"), "Grafana")
            wait_for(lambda: request(tempo + "/ready"), "Tempo")
            added = listeners() - before_listeners
            exposed = [entry for entry in added if not ipaddress.ip_address(entry[1]).is_loopback]
            if exposed:
                raise RuntimeError(f"new non-loopback listeners appeared during stack startup: {exposed}")
            auth = {"Authorization": "Basic " + base64.b64encode(("admin:" + password).encode()).decode()}
            for path in (work / "grafana/dashboards").glob("*.json"):
                dashboard = json.loads(path.read_text())
                wait_for(lambda: request(grafana + "/api/dashboards/uid/" + dashboard["uid"], headers=auth), dashboard["uid"])
                for panel in dashboard["panels"]:
                    for target in panel.get("targets", []):
                        expression = target["expr"].replace("$__rate_interval", "1m").replace("$operation", ".*").replace("$instance", f"127.0.0.1:{target_port}")
                        result = json.loads(request(prom + "/api/v1/query?" + urllib.parse.urlencode({"query": expression})))
                        if result["status"] != "success":
                            raise RuntimeError("invalid dashboard query: " + expression)
            def scraped():
                result = json.loads(request(prom + "/api/v1/query?query=gortex_daemon_ready"))
                rows = result["data"]["result"]
                return bool(rows) and rows[0]["value"][1] == "1"
            wait_for(scraped, "isolated daemon metrics" if metrics_url else "private fixture metrics")
            if metrics_url:
                sample = request(metrics_url).decode()
                names = set(re.findall(r"^(gortex_[a-zA-Z0-9_]+)(?:\{|\s)", sample, re.MULTILINE))
                required = {"gortex_daemon_ready", "gortex_runtime_goroutines", "gortex_process_resident_memory_bytes", "gortex_process_cpu_seconds_total", "gortex_mcp_requests_total", "gortex_mcp_request_duration_seconds_bucket", "gortex_daemon_aggregate_available"}
                missing = sorted(required - names)
                if missing:
                    raise RuntimeError("actual daemon metrics contract missing: " + ", ".join(missing))
                print(f"Validated actual daemon exposition contract ({len(names)} metric sample names).")
            now = time.time_ns()
            trace_id = secrets.token_hex(16)
            payload = {"resourceSpans": [{"resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "gortex-smoke"}}]}, "scopeSpans": [{"spans": [{"traceId": trace_id, "spanId": secrets.token_hex(8), "name": "isolated-smoke", "kind": 1, "startTimeUnixNano": str(now - 1000000), "endTimeUnixNano": str(now)}]}]}]}
            request(f"http://127.0.0.1:{replacements[4318]}/v1/traces", json.dumps(payload).encode(), {"Content-Type": "application/json"})
            wait_for(lambda: b"isolated-smoke" in request(tempo + "/api/traces/" + trace_id, headers={"Accept": "application/json"}), "ingested trace")
            print("PASS: isolated Prometheus scrape, four provisioned dashboards, all PromQL queries, OTLP ingest and Tempo trace lookup.")
        except Exception:
            logs = subprocess.run(compose + ["logs", "--tail=40"], cwd=work, text=True, capture_output=True)
            print((logs.stdout + logs.stderr).replace(password, "[REDACTED]"))
            raise
        finally:
            subprocess.run(compose + ["down", "--volumes", "--remove-orphans"], cwd=work, check=False)
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)


if __name__ == "__main__":
    main()

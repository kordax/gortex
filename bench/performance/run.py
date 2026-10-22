#!/usr/bin/env python3
"""Run isolated local Gortex workloads; never attach to an installed daemon."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import subprocess
import sys
import tempfile
import time

from report import compare_reports, parse_benchmarks

ROOT = Path(__file__).resolve().parents[2]


def bounded_int(minimum, maximum):
    def parse(value):
        number = int(value)
        if not minimum <= number <= maximum:
            raise argparse.ArgumentTypeError(f"value must be in {minimum}..{maximum}")
        return number
    return parse


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--suite", choices=("micro", "transport", "all"), default="micro")
    parser.add_argument("--out", type=Path, help="new or empty artifact directory (default: temporary directory)")
    parser.add_argument("--label", default="local")
    parser.add_argument("--count", type=bounded_int(2, 30), default=5, help="repeated samples per microbenchmark")
    parser.add_argument("--benchtime", default="200ms")
    parser.add_argument("--cpus", type=bounded_int(1, 32), default=4)
    parser.add_argument("--transport-runs", type=bounded_int(1, 10), default=3)
    parser.add_argument("--clients", default="1,4,8")
    parser.add_argument("--files", type=bounded_int(32, 10000), default=64)
    parser.add_argument("--declarations", type=bounded_int(2, 64), default=8)
    parser.add_argument("--rounds", type=bounded_int(3, 1000), default=24)
    parser.add_argument("--compare", type=Path, help="baseline summary.json from the same machine/toolchain/workload")
    args = parser.parse_args()
    try:
        clients = [int(value) for value in args.clients.split(",")]
        if not clients or len(clients) > 8 or len(set(clients)) != len(clients) or any(value < 1 or value > 32 for value in clients):
            raise ValueError
    except ValueError:
        parser.error("--clients must be distinct integers in 1..32, at most eight phases")
    if args.compare and args.suite == "transport":
        parser.error("--compare currently compares microbenchmarks; transport reports retain raw samples for analysis")
    output = args.out.resolve() if args.out else Path(tempfile.mkdtemp(prefix="gortex-performance."))
    if output.exists() and any(output.iterdir()):
        parser.error("--out must be new or empty; existing results are preserved")
    output.mkdir(parents=True, exist_ok=True, mode=0o700)
    env = dict(os.environ, GOMAXPROCS=str(args.cpus), OTEL_TRACES_EXPORTER="none")
    env.setdefault("GOTOOLCHAIN", "go1.27.3")
    # Compiler/module caches may be reused; every executed workload has its own
    # testenv sandbox or the existing private daemon fixture.
    go_version = subprocess.check_output(["go", "version"], cwd=ROOT, env=env, text=True).strip()
    commit = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
    diff = subprocess.check_output(["git", "diff", "HEAD", "--binary"], cwd=ROOT)
    report = {
        "schema_version": 1, "fixture_version": 1, "label": args.label,
        "metadata": {"go_version": go_version, "gomaxprocs": args.cpus, "machine": platform.machine(), "system": platform.system(), "logical_cpus": os.cpu_count(), "package_parallelism": 1, "commit": commit, "tracked_diff_sha256": hashlib.sha256(diff).hexdigest()},
        "config": {key: str(value) if isinstance(value, Path) else value for key, value in vars(args).items()},
        "micro": {}, "transport": [], "status": "running",
    }
    summary = output / "summary.json"

    def save():
        summary.write_text(json.dumps(report, indent=2, sort_keys=True) + "\n")

    def run(command, log_name, extra_env=None):
        print(f"[{time.strftime('%H:%M:%S')}] {log_name}", flush=True)
        started = time.monotonic()
        log = output / log_name
        with log.open("w") as stream:
            process = subprocess.run(command, cwd=ROOT, env=env if extra_env is None else dict(env, **extra_env), stdout=stream, stderr=subprocess.STDOUT, timeout=25*60)
        if process.returncode:
            raise RuntimeError(f"command exited {process.returncode}; see {log}")
        print(f"  completed in {time.monotonic()-started:.2f}s", flush=True)
        return log

    save()
    try:
        if args.suite in ("micro", "all"):
            log = run(["go", "test", "-p=1", "-run", "^$", "-bench", ".", "-benchmem", f"-benchtime={args.benchtime}", f"-count={args.count}", f"-cpu={args.cpus}", "-timeout=15m", "./bench/workloads", "./internal/observability"], "micro.txt")
            text = log.read_text()
            report["metadata"]["cpu_model"] = next((line.removeprefix("cpu:").strip() for line in text.splitlines() if line.startswith("cpu:")), None)
            report["micro"] = parse_benchmarks(text)
            for name, result in report["micro"].items():
                print(f"  {name}: {result['median']['ns/op']:.1f} ns/op ({len(result['samples'])} samples)")
            save()
        if args.suite in ("transport", "all"):
            for index in range(args.transport_runs):
                artifact = output / f"transport-{index+1}"
                run(["bash", "scripts/test-agent-transport.sh"], f"transport-{index+1}.log", {
                    "GORTEX_AGENT_SCENARIO": "mixed", "GORTEX_AGENT_RACE": "0", "GORTEX_AGENT_ARTIFACT_DIR": str(artifact),
                    "GORTEX_AGENT_MIXED_CLIENTS": ",".join(map(str, clients)), "GORTEX_AGENT_FILES": str(args.files),
                    "GORTEX_AGENT_DECLARATIONS": str(args.declarations), "GORTEX_AGENT_ROUNDS": str(args.rounds),
                })
                matches = list(artifact.glob("gx767-*/mixed-transport.json"))
                if len(matches) != 1:
                    raise RuntimeError(f"expected exactly one mixed transport report under {artifact}")
                data = json.loads(matches[0].read_text())
                report["transport"].append({"run": index+1, "report": str(matches[0].relative_to(output)), "data": data})
                save()
        if args.compare:
            report["comparison"] = compare_reports(json.loads(args.compare.read_text()), report)
            for name, units in report["comparison"].items():
                change = units.get("ns/op", {}).get("change_percent")
                if change is not None:
                    print(f"  delta {name}: {change:+.1f}% median time (descriptive, no automatic performance gate)")
        report["status"] = "passed"
        save()
        print(f"Results: {summary}")
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as error:
        report["status"] = "failed"
        report["failure"] = str(error)
        save()
        print(f"FAILED: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

"""Parse repeated Go benchmarks and compare compatible local measurements."""
import math
import re
import statistics

LINE = re.compile(r"^(Benchmark\S+)\s+(\d+)\s+(.+)$")


def parse_benchmarks(text):
    rows = {}
    for line in text.splitlines():
        match = LINE.match(line.strip())
        if not match:
            continue
        name, iterations, tail = match.groups()
        fields = tail.split()
        metrics = {}
        if len(fields) % 2:
            raise ValueError(f"incomplete benchmark row: {name}")
        for index in range(0, len(fields), 2):
            value = float(fields[index])
            if not math.isfinite(value) or value < 0:
                raise ValueError(f"invalid benchmark measurement: {name}")
            metrics[fields[index + 1]] = value
        if "ns/op" not in metrics or int(iterations) < 1:
            raise ValueError(f"benchmark has no completed operations: {name}")
        rows.setdefault(name, []).append({"iterations": int(iterations), "metrics": metrics})
    if not rows:
        raise ValueError("no Go benchmark results found")
    result = {}
    for name, samples in sorted(rows.items()):
        units = set(samples[0]["metrics"])
        if any(set(sample["metrics"]) != units for sample in samples):
            raise ValueError(f"inconsistent benchmark units: {name}")
        result[name] = {
            "samples": samples,
            "median": {unit: statistics.median(sample["metrics"][unit] for sample in samples) for unit in sorted(units)},
            "min_ns_per_op": min(sample["metrics"]["ns/op"] for sample in samples),
            "max_ns_per_op": max(sample["metrics"]["ns/op"] for sample in samples),
        }
    return result


def compare_reports(baseline, candidate):
    for key in ("go_version", "gomaxprocs", "machine", "cpu_model", "logical_cpus", "package_parallelism"):
        if baseline["metadata"].get(key) != candidate["metadata"].get(key):
            raise ValueError(f"incompatible benchmark environment: {key}")
    if baseline["fixture_version"] != candidate["fixture_version"]:
        raise ValueError("incompatible fixture versions")
    changes = {}
    common = set(baseline.get("micro", {})) & set(candidate.get("micro", {}))
    for name in sorted(common):
        old = baseline["micro"][name]["median"]
        new = candidate["micro"][name]["median"]
        changes[name] = {}
        for unit in sorted(set(old) & set(new)):
            changes[name][unit] = {
                "baseline": old[unit], "candidate": new[unit],
                "change_percent": None if old[unit] == 0 else 100 * (new[unit] / old[unit] - 1),
            }
    if not changes:
        raise ValueError("no common microbenchmarks to compare")
    return changes

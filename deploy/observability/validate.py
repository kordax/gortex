#!/usr/bin/env python3
"""Validate provisioned dashboard contracts without starting containers."""
import json
from pathlib import Path
import re


def main():
    root = Path(__file__).resolve().parent
    dashboards = sorted((root / "grafana/dashboards").glob("*.json"))
    assert len(dashboards) == 4, "expected runtime, MCP, repository and SQLite dashboards"
    uids = set()
    expressions = 0
    for path in dashboards:
        dashboard = json.loads(path.read_text())
        assert dashboard["uid"] not in uids, "duplicate dashboard UID"
        uids.add(dashboard["uid"])
        ids = set()
        for panel in dashboard["panels"]:
            assert panel["id"] not in ids, "duplicate panel ID"
            ids.add(panel["id"])
            if panel["type"] == "text":
                continue
            assert panel["datasource"]["uid"] == "gortex-prometheus"
            for target in panel["targets"]:
                expr = target["expr"]
                assert 'job="gortex"' in expr
                assert '$instance' in expr
                assert not re.search(r"\bor\s+(?:vector\(0\)|0)\b", expr), "do not hide unavailable metrics as zeros"
                expressions += 1
    print(f"Validated {len(dashboards)} dashboards, {expressions} scoped PromQL expressions.")


if __name__ == "__main__":
    main()

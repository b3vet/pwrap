#!/usr/bin/env python3
"""Parity gate for the cross-SDK conformance suite.

Reads scenarios.json plus each runner's report-<sdk>.json and fails when an SDK
did not pass a scenario it is declared to support. Also fails when a runner
reports a scenario the registry does not know about, or claims to have run one
that scenarios.json lists as missing from that SDK — both mean the registry and
the code have drifted apart, which is the thing this file exists to prevent.

Usage: python3 conformance/check.py [report-dir]
"""
from __future__ import annotations

import json
import pathlib
import sys

HERE = pathlib.Path(__file__).parent


def load_report(report_dir: pathlib.Path, sdk: str) -> dict | None:
    path = report_dir / f"report-{sdk}.json"
    if not path.exists():
        return None
    try:
        return json.loads(path.read_text())
    except json.JSONDecodeError as exc:
        print(f"  {sdk}: report is not valid JSON — {exc}")
        return {}


def main() -> int:
    report_dir = pathlib.Path(sys.argv[1]) if len(sys.argv) > 1 else HERE
    registry = json.loads((HERE / "scenarios.json").read_text())
    scenarios = registry["scenarios"]
    all_sdks = registry["all_sdks"]
    known_ids = {s["id"] for s in scenarios}

    failures: list[str] = []
    print(f"conformance: {len(scenarios)} scenarios, {len(all_sdks)} SDKs\n")

    reports: dict[str, dict] = {}
    for sdk in all_sdks:
        report = load_report(report_dir, sdk)
        if report is None:
            failures.append(f"{sdk}: no report-{sdk}.json — did the runner crash before writing?")
            reports[sdk] = {}
        else:
            reports[sdk] = report.get("results", {})

    width = max(len(s["id"]) for s in scenarios)
    for scenario in scenarios:
        sid = scenario["id"]
        expected = set(scenario["sdks"])
        missing_from = scenario.get("missing_from", {})
        cells = []
        for sdk in all_sdks:
            outcome = reports[sdk].get(sid)
            if sdk in expected:
                if outcome == "pass":
                    cells.append(f"{sdk}:ok")
                elif outcome is None:
                    cells.append(f"{sdk}:MISSING")
                    failures.append(
                        f"{sid}: {sdk} is declared to support this but did not report it"
                    )
                else:
                    cells.append(f"{sdk}:FAIL")
                    failures.append(f"{sid}: {sdk} reported {outcome!r}")
            else:
                # Not expected. If the runner reported it anyway, the registry is stale.
                if outcome == "pass":
                    cells.append(f"{sdk}:UNDECLARED")
                    failures.append(
                        f"{sid}: {sdk} passed it, but scenarios.json lists it as missing "
                        f"({missing_from.get(sdk, 'no reason recorded')}). "
                        f"Move {sdk!r} into this scenario's \"sdks\" list."
                    )
                else:
                    cells.append(f"{sdk}:--")
        print(f"  {sid:<{width}}  {'  '.join(cells)}")

    # Any scenario a runner invented that the registry doesn't know about.
    for sdk in all_sdks:
        for sid in reports[sdk]:
            if sid not in known_ids:
                failures.append(f"{sdk} reported unknown scenario {sid!r}; add it to scenarios.json")

    gaps = sum(len(s.get("missing_from", {})) for s in scenarios)
    print(f"\n  {gaps} tracked parity gap(s):")
    for scenario in scenarios:
        for sdk, reason in scenario.get("missing_from", {}).items():
            print(f"    {scenario['id']} / {sdk}: {reason}")

    if failures:
        print(f"\nFAIL — {len(failures)} problem(s):")
        for f in failures:
            print(f"  - {f}")
        return 1

    print("\nPASS — every SDK covers every scenario it declares.")
    return 0


if __name__ == "__main__":
    sys.exit(main())

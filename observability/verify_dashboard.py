#!/usr/bin/env python3
"""Structural regression checks for the generated setup dashboard."""

import json
import sys
from pathlib import Path


def fail(message: str) -> None:
    print(f"dashboard verification failed: {message}", file=sys.stderr)
    raise SystemExit(1)


if len(sys.argv) != 2:
    fail("usage: verify_dashboard.py DASHBOARD.json")

path = Path(sys.argv[1])
dashboard = json.loads(path.read_text())


def walk_panels(panels):
    for panel in panels:
        yield panel
        yield from walk_panels(panel.get("panels", []))


panels = list(walk_panels(dashboard.get("panels", [])))
by_title = {panel.get("title"): panel for panel in panels}

if '"No data"' in path.read_text():
    fail('panels must use semantic empty states instead of "No data"')

for panel in panels:
    for target in panel.get("targets", []):
        expression = target.get("expr", "")
        if "label_replace(" in expression:
            if "absent(" not in expression:
                fail(f"{panel.get('title')!r} has a labeled fallback that is not gated by absent(real)")
            if "label_replace(vector(" in expression or "label_replace(label_replace(vector(" in expression:
                fail(f"{panel.get('title')!r} has an unconditional labeled vector fallback")

for title in ("User setup p95", "Platform setup p95"):
    expression = by_title[title]["targets"][0]["expr"]
    if "increase(" not in expression or "$__range" not in expression or "$__rate_interval" in expression:
        fail(f"{title!r} must summarize completions over the selected dashboard range")

for title in ("Completed setups", "In flight", "Overdue (>60s)", "Oldest unfinished"):
    expression = by_title[title]["targets"][0]["expr"]
    if "and on() (max(service_catalog_setup_observer_healthy) == 1)" not in expression:
        fail(f"{title!r} must suppress stale values while the observer is unhealthy")
    if "(max(service_catalog_setup_observer_healthy) == 0) - 1" not in expression:
        fail(f"{title!r} must render unhealthy observer telemetry as unavailable")

completion = by_title["Setup completion volume by class"]["targets"][0]["expr"]
if "service_catalog_setup_attempts_total" not in completion or "service_catalog_setup_duration_seconds_count" in completion:
    fail("setup completion volume must count successful attempts exactly once")
if "clock" in completion:
    fail("setup completion volume must not split a single completion into clock observations")

unfinished = by_title["Unfinished attempts by blocking stage over time"]
if "lastNotNull" in unfinished.get("options", {}).get("legend", {}).get("calcs", []):
    fail("unfinished-stage legend must not present stale last-non-null values as current")

throttle = by_title["Client throttle wait p95"]
throttle_expression = throttle["targets"][0]["expr"]
if "absent(rest_client_rate_limiter_duration_seconds_bucket" not in throttle_expression or "* -1" not in throttle_expression:
    fail("missing client-throttle telemetry must render unavailable rather than zero")

for title in ("Leadership", "Controller-wide API and throttling", "Rollout and version"):
    panel = by_title[title]
    if not panel.get("collapsed") or not panel.get("panels"):
        fail(f"diagnostic row {title!r} must be collapsed with nested panels")

runbook = next((link for link in dashboard.get("links", []) if link.get("title") == "Runbook"), None)
if runbook is None or runbook.get("includeVars") or runbook.get("keepTime"):
    fail("external Runbook link must not append Grafana variables or time parameters")

print(f"verified {len(panels)} panels in {path}")

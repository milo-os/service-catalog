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
titles = [panel.get("title") for panel in panels]
if len(titles) != len(set(titles)):
    duplicates = sorted({title for title in titles if titles.count(title) > 1})
    fail(f"panel titles must be unique; duplicates: {duplicates}")
by_title = {panel.get("title"): panel for panel in panels}


def require_panel(title):
    panel = by_title.get(title)
    if panel is None:
        fail(f"required panel {title!r} is missing")
    return panel

if '"No data"' in path.read_text():
    fail('panels must use semantic empty states instead of "No data"')

for panel in panels:
    if panel.get("type") in {"stat", "timeseries", "table"}:
        no_value = panel.get("fieldConfig", {}).get("defaults", {}).get("noValue")
        if not no_value:
            fail(f"{panel.get('title')!r} must declare a semantic noValue message")
    for target in panel.get("targets", []):
        expression = target.get("expr", "")
        if "label_replace(" in expression:
            if "absent(" not in expression:
                fail(f"{panel.get('title')!r} has a labeled fallback that is not gated by absent(real)")
            if "label_replace(vector(" in expression or "label_replace(label_replace(vector(" in expression:
                fail(f"{panel.get('title')!r} has an unconditional labeled vector fallback")

for title in ("User setup p95", "Platform setup p95"):
    expression = require_panel(title)["targets"][0]["expr"]
    if "increase(" not in expression or "$__range" not in expression or "$__rate_interval" in expression:
        fail(f"{title!r} must summarize completions over the selected dashboard range")

for title in ("Completed setups", "Slow right now", "Stalled (>1h)"):
    expression = require_panel(title)["targets"][0]["expr"]
    if "and on() (max(service_catalog_setup_observer_healthy) == 1)" not in expression:
        fail(f"{title!r} must suppress stale values while the observer is unhealthy")
    if "(max(service_catalog_setup_observer_healthy) == 0) - 1" not in expression:
        fail(f"{title!r} must render unhealthy observer telemetry as unavailable")

completion = require_panel("Setup completion volume by class")["targets"][0]["expr"]
if "service_catalog_setup_attempts_total" not in completion or "service_catalog_setup_duration_seconds_count" in completion:
    fail("setup completion volume must count successful attempts exactly once")
if "clock" in completion:
    fail("setup completion volume must not split a single completion into clock observations")

slow = require_panel("Slow right now")["targets"][0]["expr"]
if "service_catalog_setup_stalled" not in slow:
    fail("slow setups must exclude stalled setups so known blockers do not read as an outage")

unfinished = require_panel("Unfinished attempts by blocking stage over time")
if unfinished.get("options", {}).get("legend", {}).get("calcs") != ["max"]:
    fail("unfinished-stage legend must show only the range maximum, never a stale current value")

throttle = require_panel("Client throttle wait p95")
throttle_expression = throttle["targets"][0]["expr"]
if "absent(rest_client_rate_limiter_duration_seconds_bucket" not in throttle_expression or "* -1" not in throttle_expression:
    fail("missing client-throttle telemetry must render unavailable rather than zero")

for title in ("API round-trip p95 by verb and scope", "Client throttle wait p95 by verb and scope"):
    no_value = require_panel(title)["fieldConfig"]["defaults"]["noValue"]
    if "No requests in range" not in no_value or "not exported yet" not in no_value:
        fail(f"{title!r} must cover both an idle client and missing client-go metrics")

stalled_unavailable = "absent(service_catalog_setup_stalled) * -1 and on() count(service_catalog_setup_inflight)"
for title in ("Stalled (>1h)", "Stalled setups by blocking stage"):
    if stalled_unavailable not in require_panel(title)["targets"][0]["expr"]:
        fail(f"{title!r} must render unavailable, not zero, while unfinished work lacks the stalled gauge")

for expected_y, title in enumerate(("Observer and scrape health", "Rollout and version"), start=89):
    panel = require_panel(title)
    if not panel.get("collapsed") or not panel.get("panels"):
        fail(f"diagnostic row {title!r} must be collapsed with nested panels")
    if panel.get("gridPos", {}).get("y") != expected_y:
        fail(f"collapsed diagnostic row {title!r} must be adjacent at y={expected_y}")

runbook = next((link for link in dashboard.get("links", []) if link.get("title") == "Runbook"), None)
if runbook is None or runbook.get("includeVars") or runbook.get("keepTime"):
    fail("external Runbook link must not append Grafana variables or time parameters")

print(f"verified {len(panels)} panels in {path}")

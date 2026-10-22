#!/usr/bin/env python3
"""Write the rule groups of a PrometheusRule manifest as a plain rules file."""

import json
import sys

import yaml

if len(sys.argv) != 3:
    raise SystemExit("usage: extract_rules.py PROMETHEUSRULE.yaml RULES.json")

with open(sys.argv[1]) as source:
    manifest = yaml.safe_load(source)
if manifest.get("kind") != "PrometheusRule":
    raise SystemExit(f"{sys.argv[1]} is not a PrometheusRule")
with open(sys.argv[2], "w") as target:
    json.dump({"groups": manifest["spec"]["groups"]}, target, indent=2)

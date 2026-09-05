#!/usr/bin/env python3
"""Cheap, dependency-free contract checks.

The Go contract test does the structural comparison (it has the route table in
hand and a real YAML parser). This script is the layer that runs anywhere,
including hooks and CI images with no Go toolchain: duplicate operation IDs in
the spec, and coverage in both directions between the spec and the committed
route manifest. It never inspects Python; v1 is not this repository's contract.
"""
import json
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SPEC = os.path.join(ROOT, "api", "openapi.yaml")
MANIFEST = os.path.join(ROOT, "api", "routes.json")

OPERATION = re.compile(r"^\s*operationId:\s*([A-Za-z0-9_]+)\s*$")


def spec_operation_ids():
    ids = []
    with open(SPEC, encoding="utf-8") as handle:
        for number, line in enumerate(handle, start=1):
            match = OPERATION.match(line)
            if match:
                ids.append((match.group(1), number))
    return ids


def main():
    problems = []

    ids = spec_operation_ids()
    if not ids:
        problems.append("the spec declares no operationId at all")

    seen = {}
    for operation_id, line in ids:
        if operation_id in seen:
            problems.append(
                f"operationId {operation_id} appears at lines {seen[operation_id]} and {line}")
        seen[operation_id] = line

    with open(MANIFEST, encoding="utf-8") as handle:
        manifest = json.load(handle)
    routes = manifest["routes"]

    registered = {route["operation_id"] for route in routes}
    declared = set(seen)

    for missing in sorted(declared - registered):
        problems.append(
            f"{missing} is in the spec but not in the route manifest; "
            "a generated client would call a 404")
    for missing in sorted(registered - declared):
        problems.append(
            f"{missing} is registered but not in the spec; "
            "regenerate with: go test ./internal/httpapi -update, or add the operation")

    for route in routes:
        if not route["path"].startswith("/api/v2/"):
            problems.append(f"{route['path']} is outside /api/v2; v1 belongs to Python")
        if route.get("auth") not in {"public", "bearer", "share-token", "public-share"}:
            problems.append(f"{route['path']} has auth mode {route.get('auth')!r}")

    if problems:
        print("contract problems:\n")
        for problem in problems:
            print(f"  {problem}")
        return 1

    print(f"contract holds: {len(routes)} routes, {len(declared)} operations, no duplicates")
    return 0


if __name__ == "__main__":
    sys.exit(main())

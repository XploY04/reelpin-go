#!/usr/bin/env python3
"""Compare the Go route manifest with the Python API surface.

Every canonical Python route must be either implemented in Go or listed in the
deferred allowlist with the task that owns it. A Go route that Python never had
must be declared in go-only-routes.json. Aliases must match.

Usage: scripts/check_route_coverage.py [contract-dir]
"""

import json
import pathlib
import sys


def load(directory, name):
    return json.loads((directory / name).read_text())


def key(route):
    return route["method"], route["path"]


def main():
    directory = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else "api/contract")

    python_routes = {key(r): r for r in load(directory, "python-routes.json")["routes"]}
    go_routes = {key(r): r for r in load(directory, "go-routes.json")["routes"]}
    deferred = {key(r): r for r in load(directory, "deferred-python-routes.json")["routes"]}
    go_only = {key(r): r for r in load(directory, "go-only-routes.json")["routes"]}

    problems = []

    for route_key, route in sorted(python_routes.items()):
        implemented = route_key in go_routes
        postponed = route_key in deferred
        if implemented and postponed:
            problems.append(f"{route_key[0]} {route_key[1]} is both implemented and deferred")
        elif not implemented and not postponed:
            problems.append(
                f"{route_key[0]} {route_key[1]} is neither implemented nor deferred; "
                "implement it or add it to deferred-python-routes.json with an owner"
            )
        elif implemented and route["alias"] and not go_routes[route_key].get("alias"):
            problems.append(
                f"{route_key[0]} {route_key[1]} loses its bare alias {route['alias']}"
            )

    for route_key in sorted(deferred):
        if route_key not in python_routes:
            problems.append(
                f"{route_key[0]} {route_key[1]} is deferred but no longer exists in Python"
            )
    for route_key, route in sorted(deferred.items()):
        if not route.get("owner") or not route.get("reason"):
            problems.append(f"{route_key[0]} {route_key[1]} is deferred without an owner and reason")

    for route_key in sorted(go_routes):
        if route_key not in python_routes and route_key not in go_only:
            problems.append(
                f"{route_key[0]} {route_key[1]} exists only in Go; "
                "add it to go-only-routes.json with a reason"
            )

    # go_routes includes the Go-only routes, so reporting it as "implemented"
    # counted /metrics as a ported Python route. The two are separate numbers.
    ported = len([key for key in go_routes if key in python_routes])
    print(
        f"python canonical routes: {len(python_routes)}  "
        f"ported to go: {ported}  deferred: {len(deferred)}  "
        f"go-only: {len(go_only)}  (go routes total: {len(go_routes)})"
    )
    if problems:
        print("\nroute coverage problems:")
        for problem in problems:
            print(f"  - {problem}")
        return 1
    print("route coverage is complete")
    return 0


if __name__ == "__main__":
    sys.exit(main())

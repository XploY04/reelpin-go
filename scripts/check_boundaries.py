#!/usr/bin/env python3
"""Fail when a package imports something its layer is not allowed to import.

The rule is in docs/decisions/0001-layered-packages.md: dependencies point
inward. This checks it against the real import graph rather than trusting that
everyone remembered.
"""
import json
import subprocess
import sys

MODULE = "github.com/XploY04/reelpin-go/"

# package -> substrings it must never import, and why.
FORBIDDEN = {
    "internal/reels": [
        ("net/http", "the domain does not know about transport"),
        ("github.com/jackc/pgx", "the domain does not know about the driver"),
        ("internal/postgres", "the domain does not depend on its storage"),
        ("internal/httpapi", "the domain does not depend on transport"),
    ],
    "internal/jobs": [
        ("net/http", "the domain does not know about transport"),
        ("github.com/jackc/pgx", "the domain does not know about the driver"),
        ("internal/postgres", "the domain does not depend on its storage"),
        ("internal/httpapi", "the domain does not depend on transport"),
    ],
    "internal/httpapi": [
        ("internal/postgres", "handlers talk to the reader interfaces, not to SQL"),
        ("github.com/jackc/pgx", "handlers never touch the driver"),
    ],
    "internal/postgres": [
        ("net/http", "storage does not know about transport"),
        ("internal/httpapi", "storage does not depend on transport"),
    ],
    "internal/config": [
        ("internal/", "config is read at startup and depends on nothing internal"),
    ],
    "internal/spend": [
        ("net/http", "the cost gate does not know about transport"),
        ("github.com/jackc/pgx", "the ledger takes a store port, not a driver"),
        ("internal/postgres", "the cost gate does not depend on its storage"),
        ("internal/httpapi", "the cost gate does not depend on transport"),
    ],
    "internal/auth": [
        ("internal/postgres", "token verification does not touch storage"),
    ],
}


def packages():
    out = subprocess.run(
        ["go", "list", "-json", "./..."],
        capture_output=True, text=True, check=True,
    ).stdout
    decoder = json.JSONDecoder()
    index = 0
    while index < len(out):
        while index < len(out) and out[index].isspace():
            index += 1
        if index >= len(out):
            break
        package, index = decoder.raw_decode(out, index)
        yield package


def main():
    problems = []
    seen = set()

    for package in packages():
        name = package["ImportPath"].replace(MODULE, "")
        rules = FORBIDDEN.get(name)
        if not rules:
            continue
        seen.add(name)

        imports = [i.replace(MODULE, "") for i in package.get("Imports", [])]
        for banned, why in rules:
            for imported in imports:
                if imported.startswith(banned) and imported != name:
                    problems.append(f"{name} imports {imported}: {why}")

    # A rule for a package that no longer exists is a rule nobody is checking.
    for name in sorted(set(FORBIDDEN) - seen):
        problems.append(f"{name} has a boundary rule but no such package exists")

    if problems:
        print("package boundary violations:\n")
        for problem in problems:
            print(f"  {problem}")
        print("\nSee docs/decisions/0001-layered-packages.md.")
        return 1

    print(f"package boundaries hold ({len(seen)} packages checked)")
    return 0


if __name__ == "__main__":
    sys.exit(main())

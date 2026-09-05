#!/usr/bin/env python3
"""Fail on a broken link between committed docs, and on a broken CLAUDE.md.

A doc that points at a file that moved is worse than no doc: it costs a reader
the time to discover the link is dead, and it silently stops being read.
"""
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

# Markdown links, minus images and anything with a scheme or an anchor-only target.
LINK = re.compile(r"(?<!\!)\[[^\]]*\]\(([^)]+)\)")
SKIP_PREFIXES = ("http://", "https://", "mailto:", "#")


def markdown_files():
    for base, dirs, files in os.walk(ROOT):
        dirs[:] = [d for d in dirs if d not in {".git", "node_modules", "drills"}]
        for name in files:
            if name.endswith(".md"):
                yield os.path.join(base, name)


def check_links():
    problems = []
    for path in sorted(markdown_files()):
        relative = os.path.relpath(path, ROOT)
        with open(path, encoding="utf-8") as handle:
            for number, line in enumerate(handle, start=1):
                for target in LINK.findall(line):
                    if target.startswith(SKIP_PREFIXES):
                        continue
                    target = target.split("#", 1)[0]
                    if not target:
                        continue
                    resolved = os.path.normpath(
                        os.path.join(os.path.dirname(path), target))
                    if not os.path.exists(resolved):
                        problems.append(
                            f"{relative}:{number} links to {target}, which does not exist")
    return problems


def check_symlink():
    """CLAUDE.md must be a symlink to AGENTS.md.

    Two real files drift: reelpin-api keeps both by hand and its AGENTS.md spent
    weeks crediting a provider the code had stopped using.
    """
    path = os.path.join(ROOT, "CLAUDE.md")
    if not os.path.islink(path):
        return ["CLAUDE.md is not a symlink; two real files will disagree"]
    target = os.readlink(path)
    if target != "AGENTS.md":
        return [f"CLAUDE.md points at {target}, want AGENTS.md"]
    if not os.path.exists(os.path.join(ROOT, "AGENTS.md")):
        return ["CLAUDE.md points at AGENTS.md, which does not exist"]
    return []


def check_nested_agents():
    """Every nested AGENTS.md must sit in a directory that still exists and has
    Go files. A rules file for a package that moved is a rules file nobody
    reads."""
    problems = []
    for path in sorted(markdown_files()):
        if os.path.basename(path) != "AGENTS.md":
            continue
        directory = os.path.dirname(path)
        if directory == ROOT:
            continue
        has_go = any(
            name.endswith(".go") or os.path.isdir(os.path.join(directory, name))
            for name in os.listdir(directory)
        )
        if not has_go:
            relative = os.path.relpath(path, ROOT)
            problems.append(f"{relative} documents a directory with no code in it")
    return problems


def main():
    problems = check_links() + check_symlink() + check_nested_agents()
    if problems:
        print("documentation problems:\n")
        for problem in problems:
            print(f"  {problem}")
        return 1
    print("documentation links, the CLAUDE.md symlink and nested AGENTS.md all check out")
    return 0


if __name__ == "__main__":
    sys.exit(main())

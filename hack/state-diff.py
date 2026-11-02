#!/usr/bin/env python3
"""Compare two wiring state snapshots, ignoring fields that move on their own.

The e2e captures every endpoint the wiring Job may write to, runs the Job a second
time, and captures them again. Filesystem free space is excluded: the hardlink
proof writes a file, logs are written, and the number moves without anything being
reconfigured.
"""
import json
import os
import sys

VOLATILE = {"freeSpace"}


def scrub(value):
    if isinstance(value, dict):
        return {k: scrub(v) for k, v in value.items() if k not in VOLATILE}
    if isinstance(value, list):
        return [scrub(v) for v in value]
    return value


def load(directory):
    out = {}
    for name in sorted(os.listdir(directory)):
        with open(os.path.join(directory, name), encoding="utf-8") as fh:
            out[name] = scrub(json.load(fh))
    return out


def main():
    if len(sys.argv) != 3:
        sys.exit("usage: state-diff.py <before-dir> <after-dir>")
    before, after = load(sys.argv[1]), load(sys.argv[2])
    if before == after:
        print("identical")
        return 0
    for name in sorted(set(before) | set(after)):
        if before.get(name) != after.get(name):
            print(f"--- {name} before\n{json.dumps(before.get(name), indent=2, sort_keys=True)}")
            print(f"+++ {name} after\n{json.dumps(after.get(name), indent=2, sort_keys=True)}")
    return 1


if __name__ == "__main__":
    sys.exit(main())

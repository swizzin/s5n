#!/usr/bin/env python3
"""Compare two wiring state snapshots, ignoring fields that move on their own.

The e2e captures every endpoint the wiring Job may write to, runs the Job a second
time, and captures them again. Some of what an app reports is its own live state
rather than a setting, and it moves without anything being reconfigured:

  * filesystem free space: the hardlink proof writes a file and logs are written;
  * qui's live view of qBittorrent -- whether its connection is up and how it
    classifies it -- which its own poll loop updates;
  * the public peer address qui resolves from an outside service, and the
    operator's own override for it. The Job writes neither: its instance spec is
    a name, a host, no credential and one filesystem flag.

So they are excluded here. The proof that the Job wrote nothing is its own
`writes` counter in the receipt, which this comparison backs up on everything
else: a setting the Job owns cannot change on a run that reported zero writes.

The differing paths are listed before the documents, so a failure names the
field rather than leaving a reader to diff two dumps by eye.
"""
import json
import os
import sys

VOLATILE = {
    "freeSpace",
    "connected",
    "connectionStatus",
    "publicPeerAddress",
    "resolvedPublicPeerAddress",
}


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


def paths(value, prefix=""):
    """Yield one dotted path per leaf, with list items numbered."""
    if isinstance(value, dict):
        for key in sorted(value):
            yield from paths(value[key], f"{prefix}.{key}" if prefix else key)
    elif isinstance(value, list):
        for i, item in enumerate(value):
            yield from paths(item, f"{prefix}[{i}]")
    else:
        yield prefix, value


def differing(before, after):
    """The leaf paths whose value differs, as (path, before, after)."""
    a = dict(paths(before))
    b = dict(paths(after))
    out = []
    for path in sorted(set(a) | set(b)):
        if a.get(path) != b.get(path):
            out.append((path, a.get(path), b.get(path)))
    return out


def main():
    if len(sys.argv) != 3:
        sys.exit("usage: state-diff.py <before-dir> <after-dir>")
    before, after = load(sys.argv[1]), load(sys.argv[2])
    if before == after:
        print("identical")
        return 0
    for name in sorted(set(before) | set(after)):
        if before.get(name) == after.get(name):
            continue
        print(f"{name}:")
        for path, old, new in differing(before.get(name), after.get(name))[:40]:
            print(f"  {path}: {old!r} -> {new!r}")
        print(f"--- {name} before\n{json.dumps(before.get(name), indent=2, sort_keys=True)}")
        print(f"+++ {name} after\n{json.dumps(after.get(name), indent=2, sort_keys=True)}")
    return 1


if __name__ == "__main__":
    sys.exit(main())

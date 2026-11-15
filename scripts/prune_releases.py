#!/usr/bin/env python3
"""Plan release deletions from a complete GitHub API snapshot; never delete tags."""
import json
import re
import sys


def version(release):
    match = re.fullmatch(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", release["tag_name"])
    if not match or release["draft"] or release["prerelease"]:
        return None
    return tuple(map(int, match.groups()))


def complete(release):
    number = release["tag_name"][1:]
    expected = {"SHA256SUMS", f"sshore_{number}_linux_amd64.tar.gz", f"sshore_{number}_windows_amd64.zip"}
    uploaded = {a["name"] for a in release["assets"] if a["state"] == "uploaded" and a["size"] > 0}
    return expected <= uploaded


def plan(releases, current):
    published = [r for r in releases if version(r) is not None]
    if not any(r["tag_name"] == current and complete(r) for r in published):
        raise ValueError("new release is absent, not public, or missing completed assets; nothing pruned")
    groups = {}
    for release in published:
        groups.setdefault(version(release)[:2], []).append(release)
    removals = []
    for group in groups.values():
        newest = max(group, key=version)
        if not complete(newest):
            continue
        removals.extend(r for r in group if version(r) < version(newest))
    return sorted(removals, key=version)


if __name__ == "__main__":
    pages = json.load(sys.stdin)
    releases = [release for page in pages for release in page]
    for release in plan(releases, sys.argv[1]):
        identity = release["id"]
        if not isinstance(identity, int) or identity <= 0:
            raise ValueError("invalid release ID")
        print(f'{identity}\t{release["tag_name"]}')

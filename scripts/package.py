#!/usr/bin/env python3
"""Package a built CLI without adding a release-tool dependency."""
import argparse
import hashlib
from pathlib import Path
import re
import tarfile
import zipfile


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--os", choices=["linux", "windows"], required=True)
    parser.add_argument("--arch", choices=["amd64", "arm64"], required=True)
    parser.add_argument("--output", type=Path, default=Path("packages"))
    args = parser.parse_args()
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9.+_-]*", args.version):
        parser.error("invalid version")
    if not args.binary.is_file():
        parser.error("binary is missing")
    args.output.mkdir(parents=True, exist_ok=True)
    stem = f"sshore_{args.version}_{args.os}_{args.arch}"
    binary_name = "sshore.exe" if args.os == "windows" else "sshore"
    files = [(args.binary, binary_name), (Path("README.md"), "README.md"), (Path("SECURITY.md"), "SECURITY.md"), (Path("LICENSE"), "LICENSE")]
    if args.os == "windows":
        archive = args.output / (stem + ".zip")
        with zipfile.ZipFile(archive, "w", compression=zipfile.ZIP_DEFLATED) as output:
            for path, name in files:
                output.write(path, arcname=name)
    else:
        archive = args.output / (stem + ".tar.gz")
        with tarfile.open(archive, "w:gz") as output:
            for path, name in files:
                info = output.gettarinfo(str(path), arcname=name)
                info.uid = info.gid = 0
                info.uname = info.gname = ""
                info.mode = 0o755 if name == binary_name else 0o644
                with path.open("rb") as source:
                    output.addfile(info, source)
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    archive.with_name(archive.name + ".sha256").write_text(f"{digest}  {archive.name}\n", encoding="ascii")
    print(archive)


if __name__ == "__main__":
    main()

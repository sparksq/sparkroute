#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright 2026 Scitrera LLC
# Copyright 2026 Fox Engine Ltd.

"""Build reproducible SparkRoute archives from a clean public-root checkout."""

from __future__ import annotations

import argparse
import gzip
import hashlib
import io
import json
import os
import re
import subprocess
import tarfile
import tempfile
import zipfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
PLATFORMS = tuple(f"{system}/{arch}" for system in ("linux", "darwin", "windows") for arch in ("amd64", "arm64"))


def identity(root: Path) -> tuple[str, str]:
    git_root = Path(subprocess.check_output(["git", "-C", str(root), "rev-parse", "--show-toplevel"], text=True).strip())
    if git_root.resolve() != root.resolve() or (root / "enterprise").exists():
        raise ValueError("release builds require an independently exported OSS repository root")
    if subprocess.check_output(["git", "-C", str(root), "status", "--porcelain"], text=True).strip():
        raise ValueError("release source has uncommitted changes")
    match = re.search(r"^sparkroute: ([0-9]+\.[0-9]+\.[0-9]+)$", (root / "versions.yaml").read_text(), re.MULTILINE)
    if not match:
        raise ValueError("versions.yaml must declare a release version")
    version = match[1]
    tag = os.environ.get("GITHUB_REF_NAME", "") if os.environ.get("GITHUB_REF_TYPE") == "tag" else ""
    if tag and tag != "v" + version:
        raise ValueError("release tag does not match versions.yaml")
    commit = subprocess.check_output(["git", "-C", str(root), "rev-parse", "HEAD"], text=True).strip()
    return version, commit


def write_archive(path: Path, files: dict[str, tuple[bytes, int]]) -> None:
    if path.suffix == ".zip":
        with zipfile.ZipFile(path, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as archive:
            for name, (content, mode) in sorted(files.items()):
                member = zipfile.ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
                member.create_system = 3
                member.external_attr = (0o100000 | mode) << 16
                member.compress_type = zipfile.ZIP_DEFLATED
                archive.writestr(member, content)
        return
    with path.open("wb") as output, gzip.GzipFile(filename="", mode="wb", fileobj=output, mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode="w") as archive:
            for name, (content, mode) in sorted(files.items()):
                member = tarfile.TarInfo(name)
                member.size = len(content)
                member.mode = mode
                member.mtime = 0
                archive.addfile(member, io.BytesIO(content))


def build(go: str, platforms: list[str], output: Path) -> list[Path]:
    version, commit = identity(ROOT)
    output.mkdir(parents=True, exist_ok=True)
    identity_file = json.dumps({
        "name": "sparkroute", "version": version, "commit": commit,
        "source": "https://github.com/sparksq/sparkroute/tree/" + commit,
        "license": "AGPL-3.0-only",
    }, sort_keys=True, indent=2).encode() + b"\n"
    files = {"build-info.json": (identity_file, 0o644)}
    for name in ("LICENSE", "NOTICE", "COPYRIGHT", "THIRD_PARTY_NOTICES.md", "README.md"):
        files[name] = ((ROOT / name).read_bytes(), 0o644)
    for path in sorted((ROOT / "LICENSES").glob("*")):
        if path.is_file():
            files[path.relative_to(ROOT).as_posix()] = (path.read_bytes(), 0o644)
    artifacts = []
    with tempfile.TemporaryDirectory(prefix="sparkroute-release-") as temporary:
        binary = Path(temporary) / "sparkroute"
        for platform in platforms:
            goos, goarch = platform.split("/")
            env = {**os.environ, "GOWORK": "off", "CGO_ENABLED": "0", "GOOS": goos, "GOARCH": goarch}
            flags = "-s -w -buildid= -X github.com/sparksq/sparkroute/pkg/version.Version=%s -X github.com/sparksq/sparkroute/pkg/version.Commit=%s" % (version, commit)
            subprocess.run([go, "build", "-mod=readonly", "-trimpath", "-ldflags", flags, "-o", str(binary), "./cmd/sparkroute"], cwd=ROOT, env=env, check=True)
            suffix = ".zip" if goos == "windows" else ".tar.gz"
            executable = "sparkroute.exe" if goos == "windows" else "sparkroute"
            archive = output / ("sparkroute_%s_%s_%s%s" % (version, goos, goarch, suffix))
            write_archive(archive, {**files, executable: (binary.read_bytes(), 0o755)})
            artifacts.append(archive)
    # git archive includes precisely the release source and build scripts.
    source = output / ("sparkroute_%s_source.tar.gz" % version)
    content = subprocess.check_output(["git", "-C", str(ROOT), "archive", "--format=tar", "--prefix=sparkroute-%s/" % version, commit])
    with source.open("wb") as stream, gzip.GzipFile(filename="", mode="wb", fileobj=stream, mtime=0) as zipped:
        zipped.write(content)
    artifacts.append(source)
    checksums = "".join("%s  %s\n" % (hashlib.sha256(path.read_bytes()).hexdigest(), path.name) for path in sorted(artifacts))
    (output / "checksums.txt").write_text(checksums)
    return artifacts


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--go", default="go")
    parser.add_argument("--platform", action="append", choices=PLATFORMS)
    parser.add_argument("--output", type=Path, default=ROOT / "dist")
    args = parser.parse_args()
    for artifact in build(args.go, args.platform or list(PLATFORMS), args.output):
        print(artifact)

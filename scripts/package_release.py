#!/usr/bin/env python3
"""Build checksum-verified release archives using only Python and Go."""
import argparse
import hashlib
import os
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parent.parent


def package(version, output):
    if not re.fullmatch(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?", version):
        raise ValueError("version must be a semver tag, e.g. v0.1.0")
    output.mkdir(parents=True, exist_ok=True)
    checksums = []
    with tempfile.TemporaryDirectory(prefix="issue-bot-build-") as temporary:
        binary = Path(temporary) / "bib"
        for system in ("linux", "darwin"):
            for arch in ("amd64", "arm64"):
                env = dict(os.environ, GOOS=system, GOARCH=arch, CGO_ENABLED="0")
                subprocess.run(["go", "build", "-trimpath", "-ldflags=-s -w", "-o", str(binary), "./cmd/bib"], cwd=ROOT, env=env, check=True)
                asset = output / f"brokk-issue-bot-{version}-{system}-{arch}.tar.gz"
                with tarfile.open(asset, "w:gz") as archive:
                    archive.add(binary, arcname="bib")
                    archive.add(ROOT / "LICENSE", arcname="LICENSE")
                    archive.add(ROOT / "README.md", arcname="README.md")
                digest = hashlib.sha256(asset.read_bytes()).hexdigest()
                checksums.append(f"{digest}  {asset.name}\n")
                print(asset, flush=True)
    (output / "checksums.txt").write_text("".join(checksums))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("version")
    parser.add_argument("--output", type=Path, default=ROOT / "dist")
    args = parser.parse_args()
    package(args.version, args.output.resolve())

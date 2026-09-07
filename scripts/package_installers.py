#!/usr/bin/env python3
"""Build npm packages from verified native release assets."""

import argparse
import json
from pathlib import Path
import subprocess
import tarfile
import tempfile

import package_release as release

ROOT = Path(__file__).resolve().parent.parent
NPM_ROOT = "@brokkai/issue-bot"


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")


def package(tag, assets, output, sha):
    release.validate_tag(tag)
    npm_version = tag[1:]
    release.verify_local(tag, assets, sha)
    if output.exists() and any(output.iterdir()):
        raise ValueError("installer output directory must be empty")
    output.mkdir(parents=True, exist_ok=True)
    npm_output = output / "npm"
    npm_output.mkdir()
    packages = []
    base = {
        "version": npm_version, "license": "Apache-2.0",
        "repository": {"type": "git", "url": "git+https://github.com/BrokkAi/issue-bot.git"},
        "publishConfig": {"access": "public"},
    }
    with tempfile.TemporaryDirectory() as temporary:
        staging = Path(temporary)

        def npm_pack(name, fields, files):
            directory = staging / name.split("/")[-1]
            directory.mkdir()
            write_json(directory / "package.json", dict(base, name=name, **fields))
            for filename, data in files.items():
                path = directory / filename
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_bytes(data)
                path.chmod(0o755 if filename.startswith("bin/") else 0o644)
            result = subprocess.check_output([
                "npm", "pack", "--ignore-scripts", "--json", "--pack-destination", str(npm_output.resolve()),
            ], cwd=directory)
            info = json.loads(result)[0]
            tarball = npm_output / info["filename"]
            packages.append({"name": name, "version": npm_version, "filename": tarball.name,
                             "sha256": release.digest(tarball.read_bytes()), "integrity": info["integrity"]})

        dependencies = {}
        for target in release.TARGETS:
            name = release.archive_name(tag, target)
            with tarfile.open(assets / name, "r:gz") as bundle:
                files = {member: bundle.extractfile(member).read() for member in ("bib", "LICENSE", "README.md", "BUILD.json")}
            system, go_arch = target.split("-")
            arch = {"amd64": "x64", "arm64": "arm64"}[go_arch]
            package_name = f"{NPM_ROOT}-{system}-{arch}"
            dependencies[package_name] = npm_version
            npm_pack(package_name, {"os": [system], "cpu": [arch], "description": f"Brokk Issue Bot native binary for {system}/{arch}"},
                     {"bin/bib": files["bib"], "LICENSE": files["LICENSE"], "README.md": files["README.md"], "BUILD.json": files["BUILD.json"]})
        npm_pack(NPM_ROOT, {
            "description": "Brokk Issue Bot: autonomous issue fixes through Agent Client Protocol",
            "bin": {"bib": "bin/bib.cjs"}, "engines": {"node": ">=18"},
            "os": ["linux", "darwin"], "cpu": ["x64", "arm64"], "optionalDependencies": dependencies,
        }, {"bin/bib.cjs": (ROOT / "npm/bib.cjs").read_bytes(), "LICENSE": files["LICENSE"], "README.md": files["README.md"]})
        write_json(npm_output / "manifest.json", {"tag": tag, "commit": sha, "packages": packages})

    print(f"Built {len(packages)} npm packages for {tag} at {sha}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("tag")
    parser.add_argument("assets", type=Path)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    package(args.tag, args.assets, args.output, release.commit())


if __name__ == "__main__":
    main()

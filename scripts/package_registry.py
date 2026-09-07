#!/usr/bin/env python3
"""Check, publish, and verify the built npm packages; retries require identical bytes."""

import argparse
import base64
import hashlib
import json
from pathlib import Path
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request

import package_installers
import package_release as release


def fetch_json(url):
    try:
        with urllib.request.urlopen(url, timeout=60) as response:
            return json.load(response)
    except urllib.error.HTTPError as error:
        error.close()
        if error.code == 404:
            return None
        raise


def npm_exists(package):
    name = urllib.parse.quote(package["name"], safe="")
    record = fetch_json(f"https://registry.npmjs.org/{name}/{package['version']}")
    if record is None:
        return False
    if record.get("name") != package["name"] or record.get("version") != package["version"] or record.get("dist", {}).get("integrity") != package["integrity"]:
        raise ValueError(f"published npm package differs from staged bytes: {package['name']}")
    return True


def wait_visible(check):
    # npm can accept a signed publication before its processing queue exposes
    # the immutable version. Allow that queue to finish before treating it as
    # a failure; never re-upload just because public reads lag behind.
    for attempt in range(61):
        if check():
            return
        if attempt < 60:
            if attempt % 6 == 0:
                print("Publication accepted; waiting for registry visibility...", flush=True)
            time.sleep(10)
    raise ValueError("published package did not become visible within 10 minutes; retry verification")


def run(command, directory):
    manifest = json.loads((directory / "npm/manifest.json").read_text())
    release.validate_tag(manifest["tag"])
    npm_version = manifest["tag"][1:]
    expected_names = {package_installers.NPM_ROOT} | {
        f"{package_installers.NPM_ROOT}-{system}-{arch}" for system in ("linux", "darwin") for arch in ("x64", "arm64")
    }
    packages = manifest["packages"]
    if len(packages) != 5 or {p["name"] for p in packages} != expected_names:
        raise ValueError("manifest must contain all five npm packages exactly once")
    packages.sort(key=lambda p: p["name"] == package_installers.NPM_ROOT)
    for package in packages:
        if package["version"] != npm_version or Path(package["filename"]).name != package["filename"]:
            raise ValueError("invalid npm package version or filename")
        data = (directory / "npm" / package["filename"]).read_bytes()
        integrity = "sha512-" + base64.b64encode(hashlib.sha512(data).digest()).decode()
        if release.digest(data) != package["sha256"] or integrity != package["integrity"]:
            raise ValueError(f"corrupt staged npm package: {package['filename']}")
    # Discover conflicts in every destination before making the first write.
    existing = {p["name"]: npm_exists(p) for p in packages}
    if command == "check":
        print("Package versions are available or identical. This checks availability, not publishing authorization.")
        return
    if command == "verify":
        if not all(existing.values()):
            raise ValueError("publication is incomplete: an npm package is missing")
        print("All five npm packages match the staged bytes")
        return
    # Submit independent platform packages together so their processing queues
    # overlap. The root launcher must wait until every dependency is verified.
    groups = ([p for p in packages if p["name"] != package_installers.NPM_ROOT],
              [p for p in packages if p["name"] == package_installers.NPM_ROOT])
    for group in groups:
        for package in group:
            if not existing[package["name"]]:
                subprocess.run(["npm", "publish", str((directory / "npm" / package["filename"]).resolve()),
                                "--access", "public", "--registry", "https://registry.npmjs.org",
                                "--tag", "next" if "-" in npm_version else "latest"], check=True)
        for package in group:
            wait_visible(lambda: npm_exists(package))
    print("Published and verified npm packages")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("check", "publish", "verify"))
    parser.add_argument("directory", type=Path)
    args = parser.parse_args()
    run(args.command, args.directory)


if __name__ == "__main__":
    main()

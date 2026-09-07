import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import package_installers
import package_release as release


class ReleaseAssets(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.assets = self.root / "assets"
        self.assets.mkdir()
        self.tag, self.sha = "v0.2.1", "a" * 40
        self.manifest = {"tag": self.tag, "commit": self.sha, "assets": []}
        for target in release.TARGETS:
            name = release.archive_name(self.tag, target)
            release.archive(self.assets / name, {
                "bib": b"#!/bin/sh\nexit 0\n", "LICENSE": b"license", "README.md": b"readme",
                "BUILD.json": json.dumps({"tag": self.tag, "commit": self.sha, "target": target}).encode(),
            }, 0)
            data = (self.assets / name).read_bytes()
            self.manifest["assets"].append({"name": name, "size": len(data), "sha256": release.digest(data)})
        self.write_manifest()

    def write_manifest(self):
        (self.assets / "release.json").write_text(json.dumps(self.manifest))
        (self.assets / "checksums.txt").write_text("".join(
            f"{a['sha256']}  {a['name']}\n" for a in self.manifest["assets"]))

    def test_verified_assets_must_match_the_checkout(self):
        release.verify_local(self.tag, self.assets, self.sha)
        with self.assertRaisesRegex(ValueError, "exact checkout commit"):
            release.verify_local(self.tag, self.assets, "b" * 40)

    def test_corrupt_assets_cannot_reach_npm_pack(self):
        (self.assets / self.manifest["assets"][0]["name"]).write_bytes(b"corrupted")
        with patch.object(package_installers.subprocess, "check_output") as npm:
            with self.assertRaisesRegex(ValueError, "corrupt release asset"):
                package_installers.package(self.tag, self.assets, self.root / "packages", self.sha)
            npm.assert_not_called()

    def test_swapped_platform_payload_is_rejected_even_with_valid_checksums(self):
        first, second = self.manifest["assets"][:2]
        data = (self.assets / second["name"]).read_bytes()
        (self.assets / first["name"]).write_bytes(data)
        first.update(size=len(data), sha256=release.digest(data))
        self.write_manifest()
        with self.assertRaisesRegex(ValueError, "build metadata"):
            release.verify_local(self.tag, self.assets, self.sha)

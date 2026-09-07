import base64
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
import urllib.error
from unittest.mock import patch

import package_installers
import package_registry
import package_release as release


class PackageRegistry(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        (self.root / "npm").mkdir()
        self.packages = []
        names = [package_installers.NPM_ROOT] + [
            f"{package_installers.NPM_ROOT}-{system}-{arch}"
            for system in ("linux", "darwin") for arch in ("x64", "arm64")
        ]
        for index, name in enumerate(names):
            filename = f"package-{index}.tgz"
            data = name.encode()
            (self.root / "npm" / filename).write_bytes(data)
            self.packages.append({"name": name, "version": "0.1.0", "filename": filename,
                                  "sha256": release.digest(data), "integrity": "sha512-" + base64.b64encode(hashlib.sha512(data).digest()).decode()})
        (self.root / "npm/manifest.json").write_text(json.dumps({"tag": "v0.1.0", "commit": "a" * 40, "packages": self.packages}))

    def test_read_only_check_never_publishes(self):
        with patch.object(package_registry, "npm_exists", return_value=False), \
                patch.object(package_registry.subprocess, "run") as command:
            package_registry.run("check", self.root)
            command.assert_not_called()
            with self.assertRaisesRegex(ValueError, "incomplete"):
                package_registry.run("verify", self.root)

    def test_conflicts_in_later_destination_prevent_all_writes(self):
        with patch.object(package_registry, "npm_exists", side_effect=[False, False, False, False, ValueError("conflicting npm bytes")]), \
                patch.object(package_registry.subprocess, "run") as command:
            with self.assertRaisesRegex(ValueError, "conflicting"):
                package_registry.run("publish", self.root)
            command.assert_not_called()

    def test_native_packages_publish_before_root(self):
        events = []
        with patch.object(package_registry, "npm_exists", return_value=False), \
                patch.object(package_registry, "wait_visible", side_effect=lambda check: events.append("verify")) as wait, \
                patch.object(package_registry.subprocess, "run", side_effect=lambda *args, **kwargs: events.append("publish")) as command:
            package_registry.run("publish", self.root)
            calls = [call.args[0] for call in command.call_args_list]
            self.assertEqual(len(calls), 5)
            self.assertTrue(all(call[:2] == ["npm", "publish"] for call in calls[:5]))
            self.assertTrue(calls[4][2].endswith("package-0.tgz"))
            self.assertEqual(wait.call_count, 5)
            self.assertEqual(events, ["publish"] * 4 + ["verify"] * 4 + ["publish", "verify"])

    def test_identical_existing_packages_are_verified_without_upload(self):
        with patch.object(package_registry, "npm_exists", return_value=True), \
                patch.object(package_registry.subprocess, "run") as command:
            package_registry.run("publish", self.root)
            package_registry.run("verify", self.root)
            command.assert_not_called()

    def test_changed_local_tarball_prevents_remote_reads(self):
        (self.root / "npm" / self.packages[0]["filename"]).write_bytes(b"corrupted")
        with patch.object(package_registry, "fetch_json") as fetch:
            with self.assertRaisesRegex(ValueError, "corrupt staged"):
                package_registry.run("publish", self.root)
            fetch.assert_not_called()

    def test_npm_integrity_conflict(self):
        record = dict(self.packages[0], dist={"integrity": "sha512-different"})
        with patch.object(package_registry, "fetch_json", return_value=record):
            with self.assertRaisesRegex(ValueError, "differs"):
                package_registry.npm_exists(self.packages[0])

    def test_only_404_means_version_is_missing(self):
        for code in (404, 403, 500):
            with patch.object(package_registry.urllib.request, "urlopen", side_effect=urllib.error.HTTPError("https://registry.test", code, "error", {}, None)):
                if code == 404:
                    self.assertIsNone(package_registry.fetch_json("https://registry.test"))
                else:
                    with self.assertRaises(urllib.error.HTTPError):
                        package_registry.fetch_json("https://registry.test")

    def test_registry_processing_can_take_several_minutes(self):
        from unittest.mock import Mock
        check = Mock(side_effect=[False] * 30 + [True])
        with patch.object(package_registry.time, "sleep") as sleep:
            package_registry.wait_visible(check)
        self.assertEqual(sum(call.args[0] for call in sleep.call_args_list), 300)

    def test_registry_wait_is_bounded_and_does_not_hide_conflicts(self):
        from unittest.mock import Mock
        with patch.object(package_registry.time, "sleep") as sleep:
            with self.assertRaisesRegex(ValueError, "10 minutes"):
                package_registry.wait_visible(lambda: False)
            self.assertEqual(sum(call.args[0] for call in sleep.call_args_list), 600)
            with self.assertRaisesRegex(ValueError, "conflict"):
                package_registry.wait_visible(Mock(side_effect=ValueError("conflict")))

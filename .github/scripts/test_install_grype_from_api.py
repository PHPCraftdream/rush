"""CI-only regression проверки fail-closed установки Grype."""
import hashlib
import importlib.util
import io
from pathlib import Path
import tarfile
import tempfile
import unittest

SPEC = importlib.util.spec_from_file_location("grype_api_installer", Path(__file__).with_name("install_grype_from_api.py"))
INSTALLER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(INSTALLER)


class GrypeAPIInstallerTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.cache = self.root / "cache"
        self.work = self.root / "downloads"
        self.work.mkdir()
        self.marker = self.cache / "grype" / "0.118.0" / "x64.complete"
        self.filename = "grype_0.118.0_linux_amd64.tar.gz"
        self.payload = b"verified fixture executable; never executed"
        self.calls = []

    def archive(self, name="grype", symlink=False):
        output = io.BytesIO()
        with tarfile.open(fileobj=output, mode="w:gz") as package:
            member = tarfile.TarInfo(name)
            if symlink:
                member.type = tarfile.SYMTYPE
                member.linkname = "../outside"
                package.addfile(member)
            else:
                member.size = len(self.payload)
                package.addfile(member, io.BytesIO(self.payload))
        return output.getvalue()

    def sources(self, archive=None, checksum=None):
        archive = self.archive() if archive is None else archive
        checksum = hashlib.sha256(archive).hexdigest() if checksum is None else checksum
        self.assets = {
            1: f"{checksum}  {self.filename}\n".encode(),
            2: archive,
        }
        release = {
            "tag_name": INSTALLER.VERSION,
            "assets": [
                {"id": 1, "name": "grype_0.118.0_checksums.txt", "size": len(self.assets[1])},
                {"id": 2, "name": self.filename, "size": len(archive)},
            ],
        }

        def metadata(version):
            self.assertEqual(INSTALLER.VERSION, version)
            return release

        def download(asset, destination):
            self.calls.append(asset["id"])
            destination.write_bytes(self.assets[asset["id"]])

        return release, metadata, download

    def test_verified_asset_is_registered_after_copy(self):
        _, metadata, download = self.sources()
        result = INSTALLER.install(self.cache, self.work, metadata, download)
        self.assertEqual(self.payload, result.read_bytes())
        self.assertEqual(self.cache / "grype" / "0.118.0" / "x64" / "grype", result)
        self.assertTrue(result.stat().st_mode & 0o111)
        self.assertTrue(self.marker.is_file())
        self.assertEqual([1, 2], self.calls)

    def test_wrong_checksum_never_registers_cache(self):
        _, metadata, download = self.sources(checksum="0" * 64)
        with self.assertRaisesRegex(ValueError, "SHA256 mismatch"):
            INSTALLER.install(self.cache, self.work, metadata, download)
        self.assertFalse(self.marker.exists())

    def test_wrong_release_never_downloads(self):
        release, metadata, download = self.sources()
        release["tag_name"] = "v0.118.1"
        with self.assertRaisesRegex(ValueError, "release identity"):
            INSTALLER.install(self.cache, self.work, metadata, download)
        self.assertEqual([], self.calls)
        self.assertFalse(self.marker.exists())

    def test_duplicate_release_asset_is_rejected(self):
        release, metadata, download = self.sources()
        release["assets"].append(dict(release["assets"][1]))
        with self.assertRaisesRegex(ValueError, "exactly one release asset"):
            INSTALLER.install(self.cache, self.work, metadata, download)
        self.assertFalse(self.marker.exists())

    def test_missing_release_asset_is_rejected(self):
        release, metadata, download = self.sources()
        release["assets"].pop()
        with self.assertRaisesRegex(ValueError, "exactly one release asset"):
            INSTALLER.install(self.cache, self.work, metadata, download)
        self.assertFalse(self.marker.exists())

    def test_symlink_executable_is_not_installed(self):
        _, metadata, download = self.sources(archive=self.archive(symlink=True))
        with self.assertRaisesRegex(ValueError, "regular grype executable"):
            INSTALLER.install(self.cache, self.work, metadata, download)
        self.assertFalse(self.marker.exists())

    def test_parent_path_is_not_extracted(self):
        _, metadata, download = self.sources(archive=self.archive(name="../grype"))
        with self.assertRaisesRegex(ValueError, "regular grype executable"):
            INSTALLER.install(self.cache, self.work, metadata, download)
        self.assertFalse((self.root / "grype").exists())
        self.assertFalse(self.marker.exists())

    def test_download_failure_leaves_no_marker(self):
        _, metadata, download = self.sources()

        def broken(asset, destination):
            if asset["id"] == 2:
                destination.write_bytes(b"partial")
                raise OSError("download interrupted")
            download(asset, destination)

        with self.assertRaisesRegex(OSError, "download interrupted"):
            INSTALLER.install(self.cache, self.work, metadata, broken)
        self.assertFalse(self.marker.exists())

    def test_scan_workflow_keeps_version_and_security_policy(self):
        workflow = Path(__file__).parents[1] / "workflows" / "ci-security.yml"
        text = workflow.read_text(encoding="utf-8")
        self.assertIn("grype-version: " + INSTALLER.VERSION, text)
        self.assertIn("fail-build: true", text)
        self.assertIn("severity-cutoff: critical", text)
        self.assertIn("anchore/scan-action@27805bf3b4e84b4a5c980df22ed233c00390a439", text)


if __name__ == "__main__":
    unittest.main()

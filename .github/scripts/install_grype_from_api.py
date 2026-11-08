"""Установка закреплённого Grype через официальный release API с SHA256-проверкой."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile

VERSION = "v0.118.0"
REPOSITORY = "anchore/grype"
MAX_BINARY_BYTES = 256 * 1024 * 1024


def release_metadata(version):
    result = subprocess.run(
        ["gh", "api", f"repos/{REPOSITORY}/releases/tags/{version}"],
        check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=120,
    )
    return json.loads(result.stdout)


def download_asset(asset, destination):
    with destination.open("wb") as output:
        subprocess.run(
            ["gh", "api", "-H", "Accept: application/octet-stream",
             f"repos/{REPOSITORY}/releases/assets/{asset['id']}"],
            check=True, stdout=output, stderr=subprocess.PIPE, timeout=180,
        )
    if destination.stat().st_size != asset["size"]:
        raise ValueError(f"Incomplete release asset: {asset['name']}")


def select_asset(release, name):
    matches = [asset for asset in release.get("assets", []) if asset.get("name") == name]
    if len(matches) != 1:
        raise ValueError(f"Expected exactly one release asset: {name}")
    asset = matches[0]
    if (type(asset.get("id")) is not int or asset["id"] <= 0
            or type(asset.get("size")) is not int or not 0 < asset["size"] <= MAX_BINARY_BYTES):
        raise ValueError(f"Invalid release asset metadata: {name}")
    return asset


def expected_checksum(text, filename):
    matches = []
    for line in text.splitlines():
        fields = line.split()
        if len(fields) == 2 and fields[1].lstrip("*") == filename:
            if not re.fullmatch(r"[0-9a-fA-F]{64}", fields[0]):
                raise ValueError("Invalid SHA256 checksum")
            matches.append(fields[0].lower())
    if len(matches) != 1:
        raise ValueError(f"Expected exactly one checksum: {filename}")
    return matches[0]


def install(cache_root, temporary_root, metadata=release_metadata, download=download_asset):
    release = metadata(VERSION)
    if release.get("tag_name") != VERSION or release.get("draft") or release.get("prerelease"):
        raise ValueError("Unexpected Grype release identity")
    numeric = VERSION.removeprefix("v")
    archive_name = f"grype_{numeric}_linux_amd64.tar.gz"
    checksums_name = f"grype_{numeric}_checksums.txt"
    checksums_asset = select_asset(release, checksums_name)
    archive_asset = select_asset(release, archive_name)
    with tempfile.TemporaryDirectory(prefix="grype-api-", dir=temporary_root) as work:
        work = Path(work)
        checksums = work / "checksums.txt"
        archive = work / "grype.tar.gz"
        download(checksums_asset, checksums)
        if checksums.stat().st_size > 1024 * 1024:
            raise ValueError("Checksum document exceeds limit")
        expected = expected_checksum(checksums.read_text(encoding="utf-8"), archive_name)
        download(archive_asset, archive)
        with archive.open("rb") as source:
            actual = hashlib.file_digest(source, "sha256").hexdigest()
        if actual != expected:
            raise ValueError("Grype archive SHA256 mismatch")
        binary = work / "grype"
        with tarfile.open(archive, "r:gz") as package:
            members = [member for member in package.getmembers() if member.name in ("grype", "./grype")]
            if len(members) != 1 or not members[0].isfile() or not 0 < members[0].size <= MAX_BINARY_BYTES:
                raise ValueError("Archive must contain exactly one regular grype executable")
            with package.extractfile(members[0]) as source, binary.open("wb") as output:
                shutil.copyfileobj(source, output)
            if binary.stat().st_size != members[0].size:
                raise ValueError("Incomplete grype executable")
        # Контракт actions/tool-cache: marker публикуется после полной записи binary.
        destination = Path(cache_root) / "grype" / numeric / "x64"
        destination.mkdir(parents=True, exist_ok=True)
        fd, staging = tempfile.mkstemp(prefix=".grype-", dir=destination)
        try:
            with os.fdopen(fd, "wb") as output, binary.open("rb") as source:
                shutil.copyfileobj(source, output)
                output.flush()
                os.fsync(output.fileno())
            os.chmod(staging, 0o755)
            os.replace(staging, destination / "grype")
        finally:
            if os.path.exists(staging):
                os.unlink(staging)
        (destination.parent / "x64.complete").write_bytes(b"")
        return destination / "grype"


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--version", default=VERSION, choices=[VERSION])
    parser.parse_args()
    if os.environ.get("GITHUB_ACTIONS") != "true":
        raise RuntimeError("This installer may run only in GitHub Actions")
    if sys.platform != "linux" or platform.machine() not in ("x86_64", "amd64"):
        raise RuntimeError("This CI installer supports Linux amd64 only")
    install(os.environ["RUNNER_TOOL_CACHE"], os.environ["RUNNER_TEMP"])
    print(f"Prepared SHA256-verified Grype {VERSION} in the Actions tool cache")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, RuntimeError, subprocess.SubprocessError, tarfile.TarError) as error:
        print(f"Grype installation failed: {error}", file=sys.stderr)
        raise SystemExit(1)

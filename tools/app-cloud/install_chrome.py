#!/usr/bin/env python3
"""Official Chrome for Testing fallback for a failed Playwright CDN download.

Outputs PLAYWRIGHT_EXECUTABLE_PATH=... for GitHub's environment file. No test
behavior is changed. Source: github.com/GoogleChromeLabs/chrome-for-testing.
"""

import argparse
import hashlib
import json
from pathlib import Path
import re
import sys
import tempfile
import urllib.request
import zipfile

METADATA_URL = "https://googlechromelabs.github.io/chrome-for-testing/last-known-good-versions-with-downloads.json"
DOWNLOAD_PREFIX = "https://storage.googleapis.com/chrome-for-testing-public/"


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise ValueError("browser installer does not follow redirects")


def install(destination: Path) -> Path:
    opener = urllib.request.build_opener(NoRedirect)
    with opener.open(METADATA_URL, timeout=60) as response:
        metadata = json.load(response)
    stable = metadata["channels"]["Stable"]
    version = stable["version"]
    if not re.fullmatch(r"\d+\.\d+\.\d+\.\d+", version):
        raise ValueError("unexpected official browser version format")
    downloads = [item for item in stable["downloads"]["chrome-headless-shell"] if item["platform"] == "linux64"]
    expected = f"{DOWNLOAD_PREFIX}{version}/linux64/chrome-headless-shell-linux64.zip"
    if len(downloads) != 1 or downloads[0]["url"] != expected:
        raise ValueError("browser download must be the expected official Linux64 asset")
    destination.mkdir(parents=True, exist_ok=True)
    digest = hashlib.sha256()
    with tempfile.TemporaryDirectory(prefix="chrome-download-", dir=destination) as temp:
        archive = Path(temp) / "chrome.zip"
        size = 0
        with opener.open(expected, timeout=60) as response, archive.open("wb") as target:
            while chunk := response.read(1024 * 1024):
                size += len(chunk)
                if size > 1024 * 1024 * 1024:
                    raise ValueError("browser archive exceeds the 1 GiB limit")
                digest.update(chunk)
                target.write(chunk)
        with zipfile.ZipFile(archive) as bundle:
            for member in bundle.infolist():
                resolved = (destination / member.filename).resolve()
                if not resolved.is_relative_to(destination.resolve()):
                    raise ValueError("browser archive contains an invalid path")
            bundle.extractall(destination)
            for member in bundle.infolist():
                if (member.external_attr >> 16) & 0o111:
                    (destination / member.filename).chmod(0o755)
    executable = destination.resolve() / "chrome-headless-shell-linux64" / "chrome-headless-shell"
    if not executable.is_file():
        raise ValueError("official archive did not contain the expected executable")
    record = {
        "metadataSource": METADATA_URL,
        "downloadSource": expected,
        "version": version,
        "downloadedBytes": size,
        "downloadedArchiveSha256": digest.hexdigest(),
        "checksumNote": "Computed from downloaded bytes; not an upstream signature.",
    }
    (destination / "source.json").write_text(json.dumps(record, indent=2) + "\n")
    print(json.dumps(record), file=sys.stderr)
    return executable


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--destination", required=True, type=Path)
    args = parser.parse_args()
    if "\n" in str(args.destination) or "\r" in str(args.destination):
        parser.error("destination cannot contain line breaks")
    print(f"PLAYWRIGHT_EXECUTABLE_PATH={install(args.destination)}")


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""Offline installer tests: fake release transport, real tar/hash/install tools."""
import hashlib
import io
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / "install.sh"


class InstallerTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="sshore-installer-test-")
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.home = self.root / "home"
        self.home.mkdir()
        self.bin = self.root / "tools"
        self.bin.mkdir()
        self.release = self.root / "release"
        self.release.mkdir()
        self.log = self.root / "downloads"
        self.env = dict(os.environ, HOME=str(self.home),
                        PATH=str(self.bin) + ":" + os.environ["PATH"],
                        TEST_RELEASE=str(self.release), TEST_DOWNLOAD_LOG=str(self.log),
                        TEST_OS="Linux", TEST_ARCH="x86_64")
        self.tool("uname", '#!/bin/sh\ncase "$1" in -s) echo "$TEST_OS";; -m) echo "$TEST_ARCH";; esac\n')
        self.tool("curl", '''#!/usr/bin/env python3
import os, pathlib, shutil, sys
args=sys.argv[1:]
url=next(a for a in args if a.startswith('https://'))
assert '--proto' in args and args[args.index('--proto')+1]=='=https'
assert '--proto-redir' in args and args[args.index('--proto-redir')+1]=='=https'
with open(os.environ['TEST_DOWNLOAD_LOG'],'a') as log: log.write(url+'\\n')
if os.environ.get('TEST_DOWNLOAD_FAIL'):
    sys.exit(22)
source=pathlib.Path(os.environ['TEST_RELEASE'])/url.rsplit('/',1)[-1]
shutil.copyfile(source,args[args.index('-o')+1])
''')
        self.asset("0.3.1", "amd64")

    def tool(self, name, content):
        path = self.bin / name
        path.write_text(content)
        path.chmod(0o755)

    def asset(self, version, arch):
        filename = f"sshore_{version}_linux_{arch}.tar.gz"
        payload = f"#!/bin/sh\necho 'sshore {version}'\n".encode()
        with tarfile.open(self.release / filename, "w:gz") as tar:
            item = tarfile.TarInfo("sshore")
            item.size = len(payload)
            item.mode = 0o755
            tar.addfile(item, io.BytesIO(payload))
        digest = hashlib.sha256((self.release / filename).read_bytes()).hexdigest()
        (self.release / "SHA256SUMS").write_text(f"{digest}  {filename}\n")

    def run_install(self, *args, success=True, piped=False):
        command = ["sh", "-s", "--", *args] if piped else ["sh", str(SCRIPT), *args]
        result = subprocess.run(command, input=SCRIPT.read_text() if piped else None,
                                env=self.env, text=True, capture_output=True)
        self.assertEqual(result.returncode == 0, success, result.stdout + result.stderr)
        return result

    def test_piped_version_and_atomic_replacement(self):
        target = self.home / ".local/bin/sshore"
        target.parent.mkdir(parents=True)
        target.write_text("#!/bin/sh\necho 'sshore 0.3.0'\n")
        target.chmod(0o755)
        result = self.run_install("--version", "0.3.1", piped=True)
        self.assertIn("Installed sshore 0.3.1", result.stdout)
        self.assertEqual(subprocess.check_output([str(target), "--version"], text=True).strip(), "sshore 0.3.1")
        backup = target.with_name("sshore-0.3.0")
        self.assertIn("0.3.0", backup.read_text())
        self.assertTrue(all("/download/v0.3.1/" in u for u in self.log.read_text().splitlines()))
        self.assertFalse(list(target.parent.glob(".sshore-install.*")))

    def test_latest_and_custom_directory(self):
        self.env["TEST_ARCH"] = "x86_64"
        self.asset("0.3.1", "amd64")
        dest = self.home / "directory with spaces"
        self.run_install("--bin-dir", str(dest))
        self.assertTrue((dest / "sshore").is_file())
        urls = self.log.read_text().splitlines()
        self.assertIn("/latest/download/SHA256SUMS", urls[0])
        self.assertIn("/download/v0.3.1/sshore_0.3.1_linux_amd64.tar.gz", urls[1])

    def test_checksum_failure_preserves_existing_install(self):
        target = self.home / ".local/bin/sshore"
        target.parent.mkdir(parents=True)
        target.write_text("original")
        with (self.release / "sshore_0.3.1_linux_amd64.tar.gz").open("ab") as f:
            f.write(b"tampered")
        result = self.run_install("--version", "0.3.1", success=False)
        self.assertIn("SHA256 mismatch", result.stderr)
        self.assertEqual(target.read_text(), "original")
        self.assertEqual([p.name for p in target.parent.iterdir()], ["sshore"])

    def test_failures_before_install(self):
        for args in [("--version", "../main"), ("--version", "0.3.1\ninvalid"),
                     ("--version",), ("--unknown",)]:
            with self.subTest(args=args):
                self.run_install(*args, success=False)
        self.assertFalse(self.log.exists())
        self.env["TEST_ARCH"] = "aarch64"
        self.run_install(success=False)
        self.env["TEST_ARCH"] = "x86_64"
        self.env["TEST_OS"] = "Darwin"
        self.run_install(success=False)
        self.env["TEST_OS"] = "Linux"
        self.env["TEST_DOWNLOAD_FAIL"] = "1"
        self.run_install("--version", "0.3.1", success=False)
        self.assertFalse((self.home / ".local").exists())

    def test_duplicate_manifest_entry_is_rejected(self):
        manifest = self.release / "SHA256SUMS"
        manifest.write_text(manifest.read_text() * 2)
        self.run_install("--version", "0.3.1", success=False)
        self.run_install(success=False)
        self.assertFalse((self.home / ".local").exists())


if __name__ == "__main__":
    unittest.main()

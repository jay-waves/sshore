"""Verify release preservation offline; never contact GitHub."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / "scripts/release.sh"


class ReleaseTest(unittest.TestCase):
    def run_release(self, fail=False, stale=False, published=False):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "VERSION").write_text("0.4.1\n")
            (root / "packages").mkdir()
            for name in ("linux.tar.gz", "windows.zip", "linux.sha256"):
                (root / "packages" / name).write_text("test\n")
            gh = root / "gh"
            gh.write_text('''#!/usr/bin/env python3
import json, os, sys
args = sys.argv[1:]
with open("calls", "a") as f:
    f.write(json.dumps(args) + "\\n")
if "git/ref/heads/main" in " ".join(args):
    print("old" if os.environ["STALE"] == "1" else "abc")
elif args[:2] == ["release", "create"] and os.environ["FAIL"] == "1":
    sys.exit(1)
elif args[:2] == ["release", "list"] and os.environ["PUBLISHED"] == "1":
    print("v0.4.1")
''')
            gh.chmod(0o755)
            env = dict(os.environ, PATH=tmp + os.pathsep + os.environ["PATH"],
                       GH_REPO="test/repo", GITHUB_SHA="abc",
                       FAIL=str(int(fail)), STALE=str(int(stale)), PUBLISHED=str(int(published)))
            result = subprocess.run(["bash", str(SCRIPT)], cwd=root, env=env,
                                    capture_output=True, text=True)
            import json
            calls = [json.loads(line) for line in (root / "calls").read_text().splitlines()]
            return result, calls

    def test_publish_preserves_previous_releases(self):
        result, calls = self.run_release()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(any(c[:2] == ["release", "create"] for c in calls))
        self.assertFalse(any("DELETE" in c or "--paginate" in c for c in calls))

    def test_published_release_is_immutable(self):
        result, calls = self.run_release(published=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(any(c[:2] in (["release", "create"], ["release", "upload"],
                                      ["release", "edit"]) for c in calls))

    def test_failed_publish_preserves_releases(self):
        result, calls = self.run_release(fail=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any("DELETE" in c or "--paginate" in c for c in calls))

    def test_stale_commit_preserves_releases(self):
        result, calls = self.run_release(stale=True)
        self.assertEqual(result.returncode, 0)
        self.assertFalse(any("DELETE" in c or "--paginate" in c for c in calls))


if __name__ == "__main__":
    unittest.main()

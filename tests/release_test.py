"""Verify release retention offline; never contact GitHub."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / "scripts/release.sh"


class ReleaseTest(unittest.TestCase):
    def run_release(self, fail=False, stale=False, published=False, annotated=False, incomplete=False):
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
elif "git/matching-refs/tags/v0.4.1" in " ".join(args) and os.environ["ANNOTATED"] == "1":
    print("tag-object-sha")
elif "repos/test/repo/commits/v0.4.1" in " ".join(args):
    print("abc")
elif args[:2] == ["api", "--paginate"]:
    def release(tag, identity, draft=False, prerelease=False):
        number = tag[1:]
        assets = [{"name": name, "state": "uploaded", "size": 1} for name in
                  ["SHA256SUMS", f"sshore_{number}_linux_amd64.tar.gz", f"sshore_{number}_windows_amd64.zip"]]
        if os.environ["INCOMPLETE"] == "1" and tag == "v0.4.1": assets = []
        return dict(tag_name=tag, id=identity, draft=draft, prerelease=prerelease, assets=assets)
    print(json.dumps([[release("v0.4.0", 40), release("v0.4.1", 41)],
                      [release("v0.3.9", 39), release("v0.3.10", 310),
                       release("v0.4.2", 42, draft=True), release("v0.5.0", 50, prerelease=True)]]))
elif args[:2] == ["release", "create"] and os.environ["FAIL"] == "1":
    sys.exit(1)
elif args[:2] == ["release", "list"] and os.environ["PUBLISHED"] == "1":
    print("v0.4.1")
''')
            gh.chmod(0o755)
            env = dict(os.environ, PATH=tmp + os.pathsep + os.environ["PATH"],
                       GH_REPO="test/repo", GITHUB_SHA="abc",
                       FAIL=str(int(fail)), STALE=str(int(stale)), PUBLISHED=str(int(published)),
                       ANNOTATED=str(int(annotated)), INCOMPLETE=str(int(incomplete)))
            result = subprocess.run(["bash", str(SCRIPT)], cwd=root, env=env,
                                    capture_output=True, text=True)
            import json
            calls = [json.loads(line) for line in (root / "calls").read_text().splitlines()]
            return result, calls

    def test_publish_prunes_older_patches_only(self):
        result, calls = self.run_release()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(any(c[:2] == ["release", "create"] for c in calls))
        self.assertEqual([c[-1] for c in calls if "DELETE" in c],
                         ["repos/test/repo/releases/39", "repos/test/repo/releases/40"])

    def test_published_release_is_immutable(self):
        result, calls = self.run_release(published=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(any(c[:2] in (["release", "create"], ["release", "upload"],
                                      ["release", "edit"]) for c in calls))

    def test_incomplete_assets_prevent_cleanup(self):
        result, calls = self.run_release(incomplete=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any("DELETE" in c for c in calls))

    def test_annotated_tag_is_dereferenced(self):
        result, calls = self.run_release(annotated=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(any("repos/test/repo/commits/v0.4.1" in c for c in calls))
        self.assertTrue(any(c[:2] == ["release", "create"] for c in calls))

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

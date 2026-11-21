"""Check publication and immutable-tag handling without contacting GitHub."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "scripts/release.sh"

class ReleaseTest(unittest.TestCase):
    def run_release(self, ref='refs/tags/v0.4.1', fail=False):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "packages").mkdir()
            for name in ("linux.tar.gz", "windows.zip", "linux.sha256"):
                (root / "packages" / name).write_text("test\n")
            (root / "calls").touch()
            env = dict(os.environ, GITHUB_SHA="abc", GITHUB_REF=ref, FAIL=str(int(fail)))
            driver = '''gh() {
  printf '%s\\n' "$*" >> calls
  [[ "$FAIL" == 0 ]]
}
source "$1"
'''
            shell = os.environ.get("NIU_SHELL", "bash")
            result = subprocess.run([shell, "-c", driver, "release-test", SCRIPT.as_posix()],
                                    cwd=root, env=env, capture_output=True, text=True)
            calls = (root / "calls").read_text().splitlines()
            return result, calls

    def test_publish_new_release(self):
        result, calls = self.run_release()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(calls), 1)
        self.assertTrue(calls[0].startswith("release create v0.4.1 --verify-tag "))

    def test_invalid_tag_does_not_publish(self):
        for ref in ("refs/tags/v0.4", "refs/tags/v0.0.0"):
            with self.subTest(ref=ref):
                result, calls = self.run_release(ref=ref)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(calls, [])

    def test_publish_failure_is_reported(self):
        result, _ = self.run_release(fail=True)
        self.assertNotEqual(result.returncode, 0)


if __name__ == "__main__":
    unittest.main()

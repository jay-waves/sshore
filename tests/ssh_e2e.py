#!/usr/bin/env python3
"""Linux end-to-end CLI smoke test using an isolated localhost OpenSSH server.
Requires ssh, sshd, ssh-keygen and a built binary (default dist/sshore).
Run after go build -buildvcs=false -o dist/sshore . with:
    python3 tests/ssh_e2e.py
Creates temporary keys and an SSH server bound only to 127.0.0.1.
Covers CLI/PTY behavior, uploads, downloads, batches, download directories,
overwrite protection, path prompts, terminal restoration, and transfers after
passphrase authentication without reauthentication.
Does not modify ~/.ssh or system sshd configuration.
"""
import argparse
import errno
import fcntl
import os
from pathlib import Path
import pty
import pwd
import re
import select
import shlex
import shutil
import signal
import socket
import struct
import subprocess
import tempfile
import termios
import time


class Terminal:
    def __init__(self, args, wait_ready=True):
        self.pid, self.fd = pty.fork()
        if self.pid == 0:
            os.execv(args[0], args)
        self.initial_mode = termios.tcgetattr(self.fd)
        self.buffer = b""
        self.alive = True
        if wait_ready:
            self.expect(r"\[sshore\] Connected")

    def send(self, value):
        os.write(self.fd, value.encode() if isinstance(value, str) else value)

    def expect(self, pattern, timeout=15):
        pattern = re.compile(pattern.encode())
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            match = pattern.search(self.buffer)
            if match:
                self.buffer = self.buffer[match.end():]
                return match
            if select.select([self.fd], [], [], 0.1)[0]:
                try:
                    data = os.read(self.fd, 65536)
                except OSError as e:
                    if e.errno == errno.EIO:
                        break
                    raise
                if not data:
                    break
                self.buffer += data
        raise AssertionError(f"missing {pattern.pattern!r}; output={self.buffer[-12000:]!r}")

    def wait(self, expected=0):
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            pid, status = os.waitpid(self.pid, os.WNOHANG)
            if pid:
                self.alive = False
                assert os.waitstatus_to_exitcode(status) == expected, status
                assert termios.tcgetattr(self.fd) == self.initial_mode, "terminal mode not restored"
                return
            time.sleep(0.02)
        raise AssertionError("client did not exit")

    def close(self):
        if self.alive:
            try:
                os.kill(self.pid, signal.SIGKILL)
                os.waitpid(self.pid, 0)
            except ProcessLookupError:
                pass
            self.alive = False
        os.close(self.fd)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", default="dist/sshore")
    args = parser.parse_args()
    binary = str(Path(args.binary).resolve())
    sshd = shutil.which("sshd")
    if not sshd:
        raise SystemExit("sshd is required")
    with tempfile.TemporaryDirectory(prefix="sshore-e2e-") as root:
        root = Path(root)
        key = root / "key"
        hostkey = root / "hostkey"
        for path in (key, hostkey):
            subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(path)], check=True)
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            port = sock.getsockname()[1]
        config = root / "sshd_config"
        config.write_text(f"""ListenAddress 127.0.0.1
Port {port}
HostKey {hostkey}
PidFile {root / 'sshd.pid'}
AuthorizedKeysFile {key}.pub
StrictModes no
PasswordAuthentication no
KbdInteractiveAuthentication no
PubkeyAuthentication yes
UsePAM no
LogLevel ERROR
""")
        log = open(root / "sshd.log", "w+")
        daemon = subprocess.Popen([sshd, "-D", "-e", "-f", str(config)], stdout=log, stderr=log)
        terms = []
        shell_pid = None
        runtime = root / "runtime"
        try:
            for _ in range(100):
                if daemon.poll() is not None:
                    log.seek(0)
                    raise AssertionError("sshd failed: " + log.read())
                try:
                    with socket.create_connection(("127.0.0.1", port), timeout=0.1):
                        break
                except OSError:
                    time.sleep(0.02)
            wrapper = root / "server"
            wrapper.write_text("#!/bin/sh\n" +
                               f"export SSHORE_RUNTIME_DIR={shlex.quote(str(runtime))}\n" +
                               "export SHELL=/bin/sh\n" +
                               f"exec {shlex.quote(binary)} \"$@\"\n")
            wrapper.chmod(0o700)
            known = root / "known_hosts"
            known.write_text(f"[127.0.0.1]:{port} " + hostkey.with_suffix(".pub").read_text())
            command = [binary, pwd.getpwuid(os.getuid()).pw_name + "@127.0.0.1", "-p", str(port),
                       "-i", str(key), "-F", "/dev/null", "-o", "IdentitiesOnly=yes",
                       "-o", "StrictHostKeyChecking=yes", "-o", f"UserKnownHostsFile={known}",
                       "--server", str(wrapper)]
            base_command = command[:]
            auto_names = []
            for _ in range(2):
                auto = Terminal(base_command, wait_ready=False)
                terms.append(auto)
                match = auto.expect(r"Connected [^\r\n]+ / ([a-z]+) ")
                auto_names.append(match.group(1).decode())
            assert auto_names[0] != auto_names[1]
            for auto in terms[:]:
                auto.send("exit\n")
                auto.wait()
                terms.remove(auto)
            command = base_command + ["-s", "main"]
            term = Terminal(command)
            terms.append(term)
            term.send("stty -echo; KEEP=shore; printf 'PID:%s\\n' $$\n")
            shell_pid = int(term.expect(r"PID:(\d+)").group(1))
            fcntl.ioctl(term.fd, termios.TIOCSWINSZ, struct.pack("HHHH", 37, 103, 0, 0))
            time.sleep(0.3)
            term.send("stty size\n")
            term.expect("37 103")
            term.send("printf 'RUN_%s\\n' sleep; sleep 30\n")
            term.expect("RUN_sleep")
            term.send(b"\x03")
            term.send("printf 'INT_%s\\n' survived\n")
            term.expect("INT_survived")

            data = bytes(range(256)) * 4097
            remote = root / "remote.bin"
            remote.write_bytes(data)
            downloaded = root / "downloaded.bin"
            term.send(f"sshore download {shlex.quote(str(remote))}\n")
            term.expect("Save as")
            term.send(str(downloaded) + "\n")
            term.expect("Saved .*downloaded.bin")
            assert downloaded.read_bytes() == data

            local = root / "local \u6587\u4ef6.bin"
            local.write_bytes(data[::-1])
            uploads = root / "uploads"
            uploads.mkdir()
            term.send(f"sshore upload {shlex.quote(str(uploads))}\n")
            term.expect("Local file to upload")
            term.send(str(local) + "\n")
            term.expect("Upload complete")
            assert (uploads / local.name).read_bytes() == data[::-1]

            term.send(f"sshore upload {shlex.quote(str(uploads))}\n")
            term.expect("Local file to upload")
            term.send(b"\x03")
            term.expect("Transfer cancelled")
            term.send("printf 'AFTER_%s\\n' cancel\n")
            term.expect("AFTER_cancel")

            # Both remote helpers are active before local path confirmation.
            # Prompts are serialized, while confirmed files use separate streams.
            first = root / "first.bin"
            second = root / "second.bin"
            first.write_bytes(data * 4)
            second.write_bytes(data[::-1] * 4)
            parallel = root / "parallel"
            parallel.mkdir()
            term.send(f"sshore download {shlex.quote(str(first))} & sshore download {shlex.quote(str(second))} & wait; printf 'BATCH_%s\\n' done\n")
            term.expect("Save as")
            term.send(str(parallel) + "\n")
            term.expect("Save as")
            term.send(str(parallel) + "\n")
            term.expect("BATCH_done")
            assert (parallel / first.name).read_bytes() == first.read_bytes()
            assert (parallel / second.name).read_bytes() == second.read_bytes()

            term.send(b"\x1d")
            term.expect("Detached")
            term.wait()
            automatic = root / "automatic"
            automatic.mkdir()
            term = Terminal(command + ["--download-dir", str(automatic), "--quiet", "--no-escape"])
            terms.append(term)
            term.send("printf 'RESUME:%s:%s\\n' $$ \"$KEEP\"\n")
            term.expect(f"RESUME:{shell_pid}:shore")
            batch = []
            for index in range(8):
                path = root / f"batch-{index}.bin"
                path.write_bytes(bytes([index]) * (index * 100))
                batch.append(path)
            term.send("sshore download " + " ".join(shlex.quote(str(p)) for p in batch) + "; printf 'AUTO_%s\\n' $?\n")
            term.expect("AUTO_0")
            for path in batch:
                assert (automatic / path.name).read_bytes() == path.read_bytes()
            term.send(f"sshore download {shlex.quote(str(batch[0]))}; printf 'EXISTS_%s\\n' $?\n")
            term.expect("EXISTS_1")
            term.send("stty -icanon min 1 -echo; printf 'KEY_%s\\n' ready; od -An -tx1 -N1; stty sane -echo\n")
            term.expect("KEY_ready")
            term.send(b"\x1d")
            term.expect("1d")
            listing = subprocess.check_output(base_command + ["list"], text=True)
            assert "main" in listing and str(shell_pid) in listing
            term.close()
            terms.remove(term)
            time.sleep(0.2)
            term = Terminal(command)
            terms.append(term)
            term.send("printf 'EOF_PID:%s\\n' $$\n")
            term.expect(f"EOF_PID:{shell_pid}")
            term.send("sshore detach\n")
            term.expect("Detached")
            term.wait()
            term = Terminal(command)
            terms.append(term)
            term.send("printf 'SIGNAL_READY:%s\\n' $$\n")
            term.expect(f"SIGNAL_READY:{shell_pid}")
            os.kill(term.pid, signal.SIGTERM)
            term.wait(130)
            time.sleep(0.2)
            subprocess.run(["ssh-keygen", "-q", "-p", "-P", "", "-N", "sshore-test-passphrase", "-f", str(key)], check=True, stdout=subprocess.DEVNULL)
            term = Terminal(command, wait_ready=False)
            terms.append(term)
            term.expect("Enter passphrase")
            term.send("sshore-test-passphrase\n")
            term.expect(r"\[sshore\] Connected")
            term.send("printf 'AUTH_PID:%s\\n' $$\n")
            term.expect(f"AUTH_PID:{shell_pid}")
            term.send(f"sshore download {shlex.quote(str(remote))}\n")
            term.expect("Save as")
            term.send(str(root / "after-auth.bin") + "\n")
            term.expect("Saved .*after-auth.bin")
            assert (root / "after-auth.bin").read_bytes() == data
            term.send("exit 7\n")
            term.wait(7)
            shell_pid = None
            print("PASS: real SSH + PTY, resize, upload/download, cancel, named reconnect, PID persistence, detach, exit code")
        finally:
            for term in terms:
                term.close()
            if shell_pid:
                try:
                    os.kill(shell_pid, signal.SIGHUP)
                except ProcessLookupError:
                    pass
            daemon.terminate()
            daemon.wait(timeout=5)
            log.close()
            deadline = time.monotonic() + 3
            while runtime.exists() and list(runtime.glob("*.sock")) and time.monotonic() < deadline:
                time.sleep(0.05)


if __name__ == "__main__":
    main()

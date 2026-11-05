#!/usr/bin/env python3
"""Linux end-to-end CLI smoke test using an isolated localhost OpenSSH server.
Requires ssh, sshd, ssh-keygen and a built binary (default dist/sshore).
Run after go build -tags=sshoretest -buildvcs=false -o dist/sshore . with:
    python3 tests/ssh_e2e.py
Creates temporary keys and an SSH server bound only to 127.0.0.1.
Covers SSH/PTY, approval detach, local path selection, one-shot execution,
attach/stay behavior, batches, rejection, forwarding, and SSH reauthentication.
Does not modify ~/.ssh or system sshd configuration.
"""
import argparse
import errno
import fcntl
import json
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
import threading
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
    metadata = subprocess.check_output(["go", "version", "-m", binary], text=True)
    if "-tags=sshoretest" not in metadata:
        raise SystemExit("Refusing unisolated test binary; build with -tags=sshoretest")
    sshd = shutil.which("sshd")
    if not sshd:
        raise SystemExit("sshd is required")
    with tempfile.TemporaryDirectory(prefix="sshore-e2e-") as root:
        root = Path(root)
        os.environ["SSHORE_STATE_DIR"] = str(root / "state")
        os.environ.pop("SSH_CONNECTION", None)
        os.environ.pop("SSHORE_SOCKET", None)
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
            for _ in range(2):
                auto = Terminal(base_command, wait_ready=False)
                terms.append(auto)
                match = auto.expect(r"Connected [^\r\n]+ / ([a-z]+) ")
                assert match.group(1) == b"main"
                auto.send(b"\x1d")
                auto.wait()
                auto.close()
                terms.remove(auto)
            command = base_command + ["-s", "main"]
            # Native forwarding, compression and diagnostics are passed to the
            # same interactive OpenSSH process, including attached value forms.
            native_ports = []
            probes = []
            for _ in range(3):
                probe = socket.socket()
                probe.bind(("127.0.0.1", 0))
                probes.append(probe)
                native_ports.append(probe.getsockname()[1])
            for probe in probes:
                probe.close()
            local_port, reverse_port, socks_port = native_ports
            ssh_log = root / "ssh debug.log"
            with socket.socket() as echo:
                echo.bind(("127.0.0.1", 0))
                echo.listen()
                echo.settimeout(10)
                service_port = echo.getsockname()[1]
                def echo_connections():
                    for _ in range(3):
                        peer, _ = echo.accept()
                        with peer:
                            peer.settimeout(5)
                            peer.sendall(peer.recv(1024))
                worker = threading.Thread(target=echo_connections, daemon=True)
                worker.start()
                term = Terminal(command + [
                    "-C", "-vvv", "-E", str(ssh_log),
                    f"-L127.0.0.1:{local_port}:127.0.0.1:{service_port}",
                    "-R", f"127.0.0.1:{reverse_port}:127.0.0.1:{service_port}",
                    f"-D127.0.0.1:{socks_port}", "-oExitOnForwardFailure=yes",
                ])
                terms.append(term)
                for native_port in (local_port, reverse_port):
                    with socket.create_connection(("127.0.0.1", native_port), timeout=5) as peer:
                        peer.sendall(b"native-forward")
                        assert peer.recv(1024) == b"native-forward"
                with socket.create_connection(("127.0.0.1", socks_port), timeout=5) as peer:
                    stream = peer.makefile("rb")
                    peer.sendall(b"\x05\x01\x00")
                    assert stream.read(2) == b"\x05\x00"
                    peer.sendall(b"\x05\x01\x00\x01" + socket.inet_aton("127.0.0.1") + struct.pack("!H", service_port))
                    reply = stream.read(10)
                    assert len(reply) == 10 and reply[:4] == b"\x05\x00\x00\x01", reply
                    peer.sendall(b"socks-forward")
                    assert stream.read(len(b"socks-forward")) == b"socks-forward"
                    stream.close()
                worker.join(timeout=5)
                assert not worker.is_alive()
                assert ssh_log.stat().st_size > 0
            term.send("stty -echo; KEEP=shore; printf 'PID:%s\\n' $$\n")
            shell_pid = int(term.expect(r"PID:(\d+)").group(1))
            fcntl.ioctl(term.fd, termios.TIOCSWINSZ, struct.pack("HHHH", 37, 103, 0, 0))
            time.sleep(0.3)
            term.send("stty size\n")
            term.expect("37 103")
            term.send("printf 'RUN_%s\\n' sleep; sleep 30\n")
            term.expect("RUN_sleep")
            deadline = time.monotonic() + 5
            while True:
                listing = subprocess.check_output(base_command + ["list"], text=True)
                assert "FOREGROUND" in listing
                if re.search(r"^main\s+.*\bsleep\b", listing, re.MULTILINE):
                    break
                if time.monotonic() >= deadline:
                    raise AssertionError(f"missing foreground sleep: {listing}")
                time.sleep(0.05)
            term.send(b"\x03")
            term.send("printf 'INT_%s\\n' survived\n")
            term.expect("INT_survived")

            def queue_request(term, command):
                term.send(command + "\n")
                request_id = term.expect(r"Request saved: ([a-f0-9]{32})").group(1).decode()
                term.expect("Detached")
                term.wait()
                return request_id

            def approve(request_id, paths=(), stay=False, passphrase=None):
                approval = Terminal([binary, "approve", request_id] + (["--stay"] if stay else ["--attach"]), wait_ready=False)
                terms.append(approval)
                approval.expect("Local approval")
                for kind, path in paths:
                    approval.expect("Save as" if kind == "download" else "Local file to upload")
                    approval.send(str(path) + "\n")
                if len(paths) != 1 or paths[0][0] != "upload":
                    approval.expect("Type yes:")
                    approval.send("yes\n")
                if passphrase:
                    approval.expect("Enter passphrase")
                    approval.send(passphrase + "\n")
                return approval

            data = bytes(range(256)) * 4097
            remote = root / "remote.bin"
            remote.write_bytes(data)
            downloaded = root / "downloaded.bin"
            request_id = queue_request(term, f"sshore download {shlex.quote(str(remote))}")
            assert not downloaded.exists(), "download occurred before approval"
            listed = subprocess.check_output([binary, "approve", "--list"], text=True)
            assert request_id in listed
            term = approve(request_id, [("download", downloaded)])
            term.expect("Saved .*downloaded.bin")
            term.expect(r"\[sshore\] Connected")
            assert downloaded.read_bytes() == data
            term.send("printf 'RESUME:%s:%s\\n' $$ \"$KEEP\"\n")
            term.expect(f"RESUME:{shell_pid}:shore")
            replay = subprocess.run([binary, "approve", request_id], capture_output=True)
            assert replay.returncode != 0

            local = root / "local 文件.bin"
            local.write_bytes(data[::-1])
            uploads = root / "uploads"
            uploads.mkdir()
            request_id = queue_request(term, f"sshore upload {shlex.quote(str(uploads))}")
            assert not (uploads / local.name).exists()
            approved = approve(request_id, [("upload", local)], stay=True)
            approved.expect("Upload complete")
            approved.expect("Staying local")
            approved.wait()
            assert (uploads / local.name).read_bytes() == data[::-1]

            term = Terminal(command)
            terms.append(term)
            term.send("printf 'STAY_PID:%s\\n' $$\n")
            term.expect(f"STAY_PID:{shell_pid}")
            request_id = queue_request(term, f"sshore download {shlex.quote(str(remote))}")
            subprocess.run([binary, "approve", request_id, "--reject"], check=True)
            assert request_id not in subprocess.check_output([binary, "approve", "--list"], text=True)

            # A download directory supplies defaults, never automatic approval.
            automatic = root / "automatic"
            automatic.mkdir()
            term = Terminal(command + ["--download-dir", str(automatic), "--quiet", "--no-escape"])
            terms.append(term)
            # A compromised remote peer can still speak the old wire format.
            # Neither a legacy file offer nor forward-add can bypass approval.
            def unsolicited(metadata):
                with socket.socket(socket.AF_UNIX) as peer:
                    peer.settimeout(10)
                    peer.connect(str(runtime / "main.sock"))
                    body = json.dumps(metadata).encode()
                    peer.sendall(b"\x00" + struct.pack("!I", len(body)) + body)
                    header = peer.recv(5)
                    if header:
                        assert len(header) == 5
                        size = struct.unpack("!I", header[1:])[0]
                        data = b""
                        while len(data) < size:
                            part = peer.recv(size - len(data))
                            if not part:
                                break
                            data += part
                        reply = json.loads(data)
                        assert reply.get("Type", reply.get("type")) != "accept"
                        assert reply.get("error"), reply
            unsolicited({"type": "offer", "name": "unapproved.txt", "size": 5})
            assert not (automatic / "unapproved.txt").exists()
            with socket.socket() as probe:
                probe.bind(("127.0.0.1", 0))
                forbidden_port = probe.getsockname()[1]
            unsolicited({"type": "forward", "action": "add", "port": forbidden_port})
            with socket.socket() as probe:
                probe.bind(("127.0.0.1", forbidden_port))
            term.send("printf 'UNSOLICITED_%s\\n' survived\n")
            term.expect("UNSOLICITED_survived")
            batch = []
            for index in range(3):
                path = root / f"batch-{index}.bin"
                path.write_bytes(bytes([index]) * (index * 100))
                batch.append(path)
            request_id = queue_request(term, "sshore download " + " ".join(shlex.quote(str(p)) for p in batch))
            assert list(automatic.iterdir()) == []
            term = approve(request_id, [("download", "")] * len(batch))
            term.expect(r"\[sshore\] Connected")
            for path in batch:
                assert (automatic / path.name).read_bytes() == path.read_bytes()
            term.send("stty -icanon min 1 -echo; printf 'KEY_%s\\n' ready; od -An -tx1 -N1; stty sane -echo\n")
            term.expect("KEY_ready")
            term.send(b"\x1d")
            term.expect("1d")

            # Local overwrite is rejected before connecting or consuming approval.
            request_id = queue_request(term, f"sshore download {shlex.quote(str(batch[0]))}")
            rejected = Terminal([binary, "approve", request_id], wait_ready=False)
            terms.append(rejected)
            rejected.expect("Save as")
            rejected.send("\n")
            rejected.expect("destination already exists")
            rejected.wait(1)
            subprocess.run([binary, "approve", request_id, "--reject"], check=True)

            # Forward --stay owns a foreground tunnel; Ctrl-C closes it without
            # ending the persistent shell. No listener exists before approval.
            term = Terminal(command)
            terms.append(term)
            with socket.socket() as probe:
                probe.bind(("127.0.0.1", 0))
                forward_port = probe.getsockname()[1]
            request_id = queue_request(term, f"sshore forward {forward_port}")
            with socket.socket() as probe:
                probe.bind(("127.0.0.1", forward_port))
            tunnel = approve(request_id, stay=True)
            tunnel.expect("Forwarding is active")
            assert not (termios.tcgetattr(tunnel.fd)[3] & termios.ISIG), "Ctrl-C must be handled locally, not sent to the SSH process group"
            with socket.socket() as probe:
                try:
                    probe.bind(("127.0.0.1", forward_port))
                except OSError as e:
                    assert e.errno == errno.EADDRINUSE
                else:
                    raise AssertionError("approved listener was not opened")
            tunnel.send(b"\x03")
            tunnel.wait()
            with socket.socket() as probe:
                probe.bind(("127.0.0.1", forward_port))

            term = Terminal(command)
            terms.append(term)
            request_id = queue_request(term, f"sshore forward {forward_port}")
            term = approve(request_id)
            term.expect("Forwarding 127.0.0.1")
            term.send("sshore forward --list\n")
            term.expect(f"127.0.0.1:{forward_port} -> remote")
            term.send(f"sshore forward --stop {forward_port}\n")
            term.expect("Stopped forwarding")
            term.send("printf 'FINAL_PID:%s\\n' $$\n")
            term.expect(f"FINAL_PID:{shell_pid}")
            term.send("sshore detach\n")
            term.expect("Detached")
            term.wait()

            # A new SSH connection uses OpenSSH's normal key-passphrase prompt.
            subprocess.run(["ssh-keygen", "-q", "-p", "-P", "", "-N", "sshore-test-passphrase", "-f", str(key)], check=True, stdout=subprocess.DEVNULL)
            term = Terminal(command, wait_ready=False)
            terms.append(term)
            term.expect("Enter passphrase")
            term.send("sshore-test-passphrase\n")
            term.expect(r"\[sshore\] Connected")
            request_id = queue_request(term, f"sshore download {shlex.quote(str(remote))}")
            term = approve(request_id, [("download", root / "after-auth.bin")], passphrase="sshore-test-passphrase")
            term.expect("Saved .*after-auth.bin")
            term.expect(r"\[sshore\] Connected")
            assert (root / "after-auth.bin").read_bytes() == data
            term.send("exit 7\n")
            term.wait(7)
            shell_pid = None
            print("PASS: SSH + PTY, local approval, attach/stay, replay rejection, files, batches, forwarding, PID persistence, key authentication")
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

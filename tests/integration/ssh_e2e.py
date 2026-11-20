#!/usr/bin/env python3
"""Linux end-to-end CLI smoke test using an isolated localhost OpenSSH server.
Run via tests/integration/ssh_docker.sh. Each scenario uses two container accounts.
Creates temporary keys and an SSH server bound only to 127.0.0.1.
Covers SSH/PTY, approval detach, local path selection, one-shot execution,
explicit reattachment, batches, rejection, forwarding, and SSH reauthentication.
Does not modify ~/.ssh or system sshd configuration.
"""
import argparse
import errno
import fcntl
import hashlib
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
import tarfile
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


def handoff_smoke(binary, host, options, root):
    terminal = Terminal([binary, "spawn", ".:"], wait_ready=False)
    local_ref = terminal.expect(r"Connected (\S+) \(spawn\)").group(1).decode()
    try:
        marker = root / "handoff-accepted"
        nested = [binary, "spawn", host + ":", "--quiet", *options]
        terminal.send(shlex.join(nested) + "; printf '%s' \"$?\" > " + shlex.quote(str(marker)) + "\n")
        remote_ref = terminal.expect(r"Connected (\S+) \(spawn\)").group(1).decode()
        deadline = time.monotonic() + 5
        while not marker.exists() and time.monotonic() < deadline:
            time.sleep(0.02)
        assert marker.read_text() == "0", "nested command did not exit after handoff"
        rows = json.loads(subprocess.check_output([binary, "list", "here:", "--json"], text=True))
        local_name = local_ref.split(":", 1)[1]
        assert any(row["name"] == local_name and row["state"] == "detached"
                   and row.get("connected_host") == host for row in rows), rows
        tree = subprocess.check_output([binary, "pstree"], text=True)
        assert "-> " + host in tree, tree
        terminal.send("printf 'HANDOFF-%s\\n' ready\n")
        terminal.expect("HANDOFF-ready")
        terminal.send(b"\x02d")
        terminal.expect("Connected " + re.escape(local_ref))
        terminal.send(shlex.join([binary, "attach", remote_ref, "--quiet", *options]) + "\n")
        terminal.expect("Connected " + re.escape(remote_ref))
        terminal.send("exit\n")
        terminal.expect("Connected " + re.escape(local_ref))
        terminal.send("exit\n")
        terminal.wait()
        print("PASS: local frontend handoff, single-prefix remote keys, return and reattach over SSH")
    finally:
        terminal.close()


def nested_smoke(binary, host, options, root, remote_options):
    terminal = Terminal([binary, "launch", host + ":", *options], wait_ready=False)
    refs = {"nested-root": terminal.expect(r"Connected (\S+) \(launch\)").group(1).decode()}
    def processes():
        found = {}
        for entry in Path("/proc").iterdir():
            if not entry.name.isdigit():
                continue
            try:
                if os.readlink(entry / "exe") == binary:
                    found[entry.name] = (entry / "cmdline").read_bytes().replace(b"\0", b" ")
            except (OSError, PermissionError):
                pass
        return found
    try:
        baseline = processes()
        for name in ("nested-child", "nested-grandchild", "nested-local"):
            marker = root / "remote-files" / (name + "-returned")
            args = [binary, "spawn", ".:"] if name == "nested-local" else [binary, "spawn", host + ":", *remote_options]
            command = shlex.join(args)
            terminal.send(command + "; printf '%s' \"$?\" > " + shlex.quote(str(marker)) + "\n")
            refs[name] = terminal.expect(r"Connected (\S+) \(spawn\)").group(1).decode()
            deadline = time.monotonic() + 5
            while not marker.exists() and time.monotonic() < deadline:
                time.sleep(0.02)
            assert marker.read_text() == "0", "nested launcher did not exit successfully"
            deadline = time.monotonic() + 5
            while len(processes()) > len(baseline) and time.monotonic() < deadline:
                time.sleep(0.02)
            assert len(processes()) <= len(baseline), ("spawn added resident processes", baseline, processes())
        terminal.send(b"\x02d")
        terminal.expect("Connected " + re.escape(refs["nested-grandchild"]))
        reattached = Terminal([binary, "attach", refs["nested-local"]])
        try:
            payload, received = root / "nested-payload", root / "nested-received"
            payload.write_bytes(b"routed approval data")
            reattached.send(shlex.join([binary, "cp", ".:" + str(payload), "origin:"]) + "\n")
            request = reattached.expect(r"Request saved: ([a-f0-9]{32})").group(1).decode()
            reattached.expect("Background;")
            reattached.wait()
            reattached.close()
            reattached = None
            approval = Terminal([binary, "review", request], wait_ready=False)
            try:
                approval.expect("Save as")
                approval.send("\x15" + str(received) + "\n")
                approval.expect("Saved ")
                approval.wait()
                assert received.read_bytes() == payload.read_bytes()
            finally:
                approval.close()
            reattached = Terminal([binary, "attach", refs["nested-local"]])
            reattached.send("exit\n")
            reattached.wait()
        finally:
            if reattached is not None:
                reattached.close()
        cycle = sorted(refs[name] for name in ("nested-root", "nested-child", "nested-grandchild"))
        next_ref = cycle[(cycle.index(refs["nested-grandchild"]) + 1) % len(cycle)]
        terminal.send(b"\x02n" if next_ref == refs["nested-root"] else b"\x02p")
        terminal.expect("Connected " + re.escape(refs["nested-root"]))
        terminal.send(b"\x02l")
        terminal.expect("Connected " + re.escape(refs["nested-grandchild"]))
        for name in ("nested-child", "nested-root"):
            terminal.send("exit\n")
            terminal.expect("Connected " + re.escape(refs[name]))
        terminal.send("exit\n")
        terminal.wait()
        print("PASS: remote nested spawn exits launcher and returns through two levels")
    finally:
        terminal.close()


def spawn_smoke(binary, host, options):
    def cli(action, *extra):
        return [binary, action, host, *extra, *options]

    def inventory():
        return json.loads(subprocess.check_output(cli("list", "--json"), text=True))

    terminal = Terminal([binary, "spawn", host + ":", *options], wait_ready=False)
    foreground = terminal.expect(r"Connected (\S+) \(spawn\)").group(1).decode().rsplit(":", 1)[-1]
    try:
        terminal.send("sleep 2; printf 'SPAWN-%s\\n' retained\n")
        terminal.expect("SPAWN-retained")
        rows = {row["name"]: row for row in inventory()["shells"]}
        assert rows[foreground]["state"] == "attached", rows
        terminal.send("exit\n")
        terminal.wait()
    finally:
        terminal.close()

    started = subprocess.check_output([binary, "launch", host + ":", "--detach", *options], text=True)
    persistent_ref = re.search(r"Started (\S+)", started).group(1)
    persistent = persistent_ref.rsplit(":", 1)[-1]
    parent = subprocess.Popen(
        ["sh", "-c", '\"$@\"; code=$?; echo READY:$code; read answer', "parent",
         binary, "spawn", host + ":", "--detach", *options],
        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    try:
        output = b""
        deadline = time.monotonic() + 20
        while b"READY:" not in output and time.monotonic() < deadline:
            if select.select([parent.stdout], [], [], 0.1)[0]:
                chunk = os.read(parent.stdout.fileno(), 8192)
                if not chunk:
                    break
                output += chunk
        assert b"READY:0" in output and b"(spawn)" in output, output
        temporary_ref = re.search(rb"Started (\S+)", output).group(1).decode()
        temporary = temporary_ref.rsplit(":", 1)[-1]
        rows = {row["name"]: row for row in inventory()["shells"]}
        assert rows[temporary]["temporary"] and rows[temporary]["state"] == "detached", rows
        assert not rows[persistent].get("temporary"), rows
        pid = rows[temporary]["pid"]
        terminal = Terminal([binary, "attach", temporary_ref, *options])
        try:
            terminal.send("printf 'SPAWN-%s\\n' alive\n")
            terminal.expect("SPAWN-alive")
            terminal.send(b"\x02d")
            terminal.wait()
        finally:
            terminal.close()
        assert next(row for row in inventory()["shells"] if row["name"] == temporary)["pid"] == pid
        parent.stdin.close()
        parent.wait(timeout=5)
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            rows = {row["name"]: row for row in inventory()["shells"]}
            if temporary not in rows:
                assert persistent in rows, rows
                print("spawn: real SSH detach, reattach, owner cleanup and persistent sibling OK")
                break
            time.sleep(0.1)
        else:
            raise AssertionError("spawn survived original parent exit")
    finally:
        if parent.poll() is None:
            parent.kill()
        parent.wait()
        parent.stdout.close()
        parent.stderr.close()
        subprocess.run([binary, "kill", persistent_ref, *options], check=True)


def setup_smoke(binary, host, options, root):
    release = subprocess.check_output([binary, "--version"], text=True).strip().split()[1]
    target = root / "remote-home" / ".local" / "bin" / "sshore"
    shell_options = ["--server", str(target), *options]
    for action in ("launch", "spawn"):
        terminal = Terminal([binary, action, host + ":", *shell_options], wait_ready=False)
        try:
            terminal.expect("sshore install")
            terminal.wait(expected=1)
        finally:
            terminal.close()
        assert not target.exists()
    mountpoint = root / "mountpoint"
    mountpoint.mkdir()
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        local_port = listener.getsockname()[1]
    old_remote = root / "remote-tools" / "sshore"
    old_remote.write_text("#!/bin/sh\necho 'sshore 0.0.1'\n")
    old_remote.chmod(0o755)
    try:
        result = subprocess.run([binary, "mount", f"{host}:/", ".:" + str(mountpoint), *options],
                                capture_output=True, text=True, timeout=15)
        assert result.returncode == 1 and "sshore install" in result.stderr, result.stderr
        result = subprocess.run([binary, "forward", f".:{local_port}", f"{host}:1", *options],
                                capture_output=True, text=True, timeout=15)
        assert result.returncode == 1 and "sshore install" in result.stderr, result.stderr
        assert old_remote.read_text() == "#!/bin/sh\necho 'sshore 0.0.1'\n"
    finally:
        old_remote.unlink()

    assert not (root / "remote-home" / "curl-calls").exists()
    assets = root / "release"
    assets.mkdir(mode=0o755)
    archive = assets / f"sshore_{release}_linux_amd64.tar.gz"
    with tarfile.open(archive, "w:gz") as bundle:
        bundle.add(binary, arcname="sshore")
    (assets / "SHA256SUMS").write_text(f"{hashlib.sha256(archive.read_bytes()).hexdigest()}  {archive.name}\n")
    curl = root / "remote-tools" / "curl"
    curl.write_text("#!/bin/sh\n" +
                    "while [ \"$#\" -gt 0 ]; do case \"$1\" in -o) out=$2; shift;; https://*) url=$1;; esac; shift; done\n" +
                    f"cp {shlex.quote(str(assets))}/\"${{url##*/}}\" \"$out\"\n")
    subprocess.run([binary, "install", host + ":" + str(target), *options], check=True, timeout=15)
    assert subprocess.check_output([str(target), "--version"], text=True).strip() == f"sshore {release}"
    # Exercise the standalone installer against an actual registered remote task.
    started = subprocess.check_output([binary, "launch", host + ":", "--detach", *shell_options],
                                      text=True, timeout=15)
    shell_ref = re.search(r"Started (\S+) \(launch\)", started).group(1)
    shell_name = shell_ref.rsplit(":", 1)[-1]
    installer_target = target.with_name("other install's path")
    original = b"#!/bin/sh\necho 'sshore 0.0.1'\n"
    installer_target.write_bytes(original)
    installer_target.chmod(0o700)
    owner = target.stat()
    os.chown(installer_target, owner.st_uid, owner.st_gid)
    installer_command = [binary, "install", host + ":" + str(installer_target), *options]
    installed = [binary, "list", "--json", host, "--server", str(target), *options]

    def installer_tasks():
        return {row["name"]: row["pid"] for row in json.loads(
            subprocess.check_output(installed, timeout=15))["shells"]}

    original_pid = installer_tasks()[shell_name]
    result = subprocess.run(installer_command, input="no\n",
                            capture_output=True, text=True, timeout=15)
    assert result.returncode == 1 and "cancelled" in result.stderr, result.stderr
    assert "stop the account daemon? [Y/n]" in result.stderr and shell_name in result.stdout, result
    assert installer_target.read_bytes() == original
    assert installer_tasks()[shell_name] == original_pid
    result = subprocess.run(installer_command, input="yes\n",
                            capture_output=True, text=True, timeout=15)
    assert result.returncode == 0 and shell_name in result.stdout, result
    assert subprocess.check_output([str(installer_target), "--version"], text=True).strip() == f"sshore {release}"
    assert not list(target.parent.glob(".sshore-update-*"))
    target.write_bytes(original)
    subprocess.run([binary, "install", host + ":", *options], check=True, timeout=15)
    assert subprocess.check_output([str(target), "--version"], text=True).strip() == f"sshore {release}"
    print("setup: explicit remote update, target paths and task confirmation passed")


def reset_smoke(binary, root):
    subprocess.run([binary, "launch", "here:", "--detach"], check=True)
    state = root / "state"
    history = state / "shells" / "reset-history"
    history.parent.mkdir(mode=0o700, exist_ok=True)
    history.write_text("clear")
    unrelated = state / "user-file"
    unrelated.write_text("keep")
    os.environ["XDG_CACHE_HOME"] = str(root / "cache")
    cache = Path(os.environ["XDG_CACHE_HOME"]) / "sshore"
    cache.mkdir(parents=True, mode=0o700)
    daemon = Path(pwd.getpwuid(os.getuid()).pw_dir) / ".sshore" / "daemon" / "daemon.json"
    assert daemon.is_file()
    subprocess.run([binary, "reset"], check=True, timeout=15)
    assert unrelated.read_text() == "keep"
    assert not cache.exists()
    assert not daemon.exists()
    assert not (state / "shells").exists()
    print("PASS: release reset ended the local daemon and cleared only application data")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", default="dist/sshore")
    scenarios = ("core", "spawn", "identity", "nested", "handoff", "setup")
    selection = parser.add_mutually_exclusive_group()
    for scenario in scenarios:
        selection.add_argument(f"--{scenario}-only", action="store_true")
    args = parser.parse_args()
    if not Path("/.dockerenv").is_file() or os.getuid() != 0:
        parser.error("use tests/integration/ssh_docker.sh to run inside a disposable container")
    if not any(getattr(args, f"{scenario}_only") for scenario in scenarios):
        parser.error("select one scenario per container")
    remote_account = pwd.getpwnam("sshore-fixture")
    binary = str(Path(args.binary).resolve())
    sshd = shutil.which("sshd")
    if not sshd:
        raise SystemExit("sshd is required")
    with tempfile.TemporaryDirectory(prefix="sshore-e2e-") as root:
        root = Path(root)
        root.chmod(0o755)
        for name in ("remote-home", "remote-state", "runtime", "remote-files"):
            directory = root / name
            directory.mkdir(mode=0o700)
            os.chown(directory, remote_account.pw_uid, remote_account.pw_gid)
        # The daemon prepends its executable directory to the shell PATH.
        # Name the fixture binary sshore so helpers cannot resolve an installed release.
        fixture_binary = root / "sshore"
        shutil.copy2(binary, fixture_binary)
        binary = str(fixture_binary)
        os.environ["SSHORE_STATE_DIR"] = str(root / "state")
        os.environ["SSHORE_RUNTIME_DIR"] = str(root / "local-runtime")
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
AuthorizedKeysFile {key}.pub {root / 'remote-home' / '.ssh' / 'authorized_keys'}
StrictModes no
PasswordAuthentication no
KbdInteractiveAuthentication no
PubkeyAuthentication yes
UsePAM no
LogLevel ERROR
""")
        home = root / "remote-home"
        tools = root / "remote-tools"
        tools.mkdir(mode=0o755)
        curl = tools / "curl"
        curl.write_text("#!/bin/sh\n" + f"echo attempt >> {shlex.quote(str(home / 'curl-calls'))}\n" +
                        "echo 'fixture: remote download offline' >&2\nexit 22\n")
        curl.chmod(0o755)
        scope = root / "remote-environment"
        scope.write_text("#!/bin/sh\n" +
                         f"export HOME={shlex.quote(str(home))}\n" +
                         f"export SSHORE_RUNTIME_DIR={shlex.quote(str(root / 'runtime'))}\n" +
                         f"export SSHORE_STATE_DIR={shlex.quote(str(root / 'remote-state'))}\n" +
                         f"export PATH=\"$HOME/.local/bin\":{shlex.quote(str(tools))}:{shlex.quote(str(root))}:$PATH\n" +
                         "export SHELL=/bin/sh\n" +
                         'exec sh -c "$SSH_ORIGINAL_COMMAND"\n')
        scope.chmod(0o755)
        with config.open("a") as output:
            output.write(f"ForceCommand {scope}\n")
        log = open(root / "sshd.log", "w+")
        daemon = subprocess.Popen([sshd, "-D", "-e", "-f", str(config)], stdout=log, stderr=log)
        terms = []
        shell_pid = None
        runtime = root / "runtime"
        wrapper = root / "server"
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
            wrapper.write_text("#!/bin/sh\n" +
                               f"export SSHORE_RUNTIME_DIR={shlex.quote(str(runtime))}\n" +
                               f"export SSHORE_STATE_DIR={shlex.quote(str(root / 'remote-state'))}\n" +
                               "export SHELL=/bin/sh\n" +
                               f"exec {shlex.quote(binary)} \"$@\"\n")
            wrapper.chmod(0o755)
            known = root / "known_hosts"
            known.write_text(f"[127.0.0.1]:{port} " + hostkey.with_suffix(".pub").read_text())
            username = remote_account.pw_name
            command = [binary, "launch", username + "@127.0.0.1", "--server", str(wrapper), "--", "-p", str(port),
                       "-i", str(key), "-F", "/dev/null", "-o", "IdentitiesOnly=yes",
                       "-o", "StrictHostKeyChecking=yes", "-o", f"UserKnownHostsFile={known}"]
            if args.identity_only:
                direct_options = command[5:]
                authorized = home / ".ssh" / "authorized_keys"
                install = [binary, "copy-id", command[2] + ":", *direct_options]
                subprocess.run(install, check=True, timeout=10)
                installed = authorized.read_text()
                assert installed.split() == key.with_suffix(".pub").read_text().split()[:2], installed
                assert authorized.stat().st_mode & 0o777 == 0o600
                assert authorized.parent.stat().st_mode & 0o777 == 0o700
                authorized.write_text('from="127.0.0.1" ' + installed.rstrip("\n"))
                authorized.parent.chmod(0o777)
                subprocess.run(install, check=True, timeout=10)
                assert authorized.read_text() == 'from="127.0.0.1" ' + installed.rstrip("\n")
                assert authorized.parent.stat().st_mode & 0o777 == 0o700

                authorized.unlink()
                authorized.mkdir()
                failure = subprocess.run(install, capture_output=True, text=True, timeout=10)
                assert failure.returncode != 0 and "not a regular file" in failure.stderr, failure.stderr
                authorized.rmdir()
                authorized.symlink_to(root / "missing-authorized-keys")
                failure = subprocess.run(install, capture_output=True, text=True, timeout=10)
                assert failure.returncode != 0 and "symbolic link" in failure.stderr, failure.stderr
                authorized.unlink()
                authorized.write_text(installed)

                second = root / "second key"
                subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(second)], check=True)
                select_key = Terminal([*install, "-i", str(second)], wait_ready=False)
                terms.append(select_key)
                select_key.expect(re.escape(str(second.with_suffix(".pub"))))
                select_key.expect("Public key: ")
                select_key.send(str(second) + "\t\n")
                select_key.expect("Public key is authorized")
                select_key.wait()
                lines = authorized.read_text().splitlines()
                assert len(lines) == 2, lines
                assert lines[1].split() == second.with_suffix(".pub").read_text().split()[:2], lines

                secret = "sshore-file-key-secret"
                subprocess.run(["ssh-keygen", "-q", "-p", "-P", "", "-N", secret, "-f", str(key)], check=True, stdout=subprocess.DEVNULL)
                direct = Terminal([*command[:2], command[2] + ":", *command[3:]], wait_ready=False)
                terms.append(direct)
                direct.expect("Passphrase for " + re.escape(str(key)) + ": ")
                direct.send(secret + "\n")
                direct.expect(r"\[sshore\] Connected")
                direct.send("exit\n")
                direct.wait()
                print("identity: copy-id selection and idempotence, and encrypted private keys OK")
                return
            if args.handoff_only:
                handoff_smoke(binary, command[2], command[3:], root)
                return
            if args.nested_only:
                remote_key = home / "key"
                shutil.copy2(key, remote_key)
                os.chown(remote_key, remote_account.pw_uid, remote_account.pw_gid)
                remote_options = [str(remote_key) if option == str(key) else option for option in command[3:]]
                nested_smoke(binary, command[2], command[3:], root, remote_options)
                return
            if args.spawn_only:
                spawn_smoke(binary, command[2], command[3:])
                return
            if args.setup_only:
                setup_smoke(binary, command[2], command[5:], root)
                reset_smoke(binary, root)
                return
            base_command = command[:]
            auto_names=[]
            for _ in range(2):
                auto = Terminal([*base_command[:2], base_command[2] + ":", *base_command[3:]], wait_ready=False)
                terms.append(auto)
                match = auto.expect(r"Connected [^\r\n]+:([0-9a-f]{6}) ")
                assert match.group(1).decode() not in auto_names
                auto_names.append(match.group(1).decode())
                auto.send(b"\x02d")
                auto.wait()
                auto.close()
                terms.remove(auto)
            for name in auto_names:
                auto = Terminal([binary, "attach"] + [base_command[2] + ":" + name] + base_command[3:])
                terms.append(auto)
                auto.send("exit\n")
                auto.wait()
                auto.close()
                terms.remove(auto)
            create = [binary, "launch", base_command[2] + ":", *base_command[3:]]
            # SSH configuration forwarding is deliberately disabled. Approved
            # sshore forwarding is exercised below through its public workflow.
            for option in ("-L127.0.0.1:12345:127.0.0.1:12346",
                           "-R127.0.0.1:12345:127.0.0.1:12346", "-D127.0.0.1:12345"):
                rejected = Terminal(create + [option], wait_ready=False)
                terms.append(rejected)
                rejected.expect("forwarding is unsupported")
                rejected.wait(expected=1)
            term = Terminal(create, wait_ready=False)
            main_ref = term.expect(r"Connected (\S+) \(launch\)").group(1).decode()
            main_name = main_ref.rsplit(":", 1)[-1]
            command = [binary, "attach", main_ref, *base_command[3:]]
            terms.append(term)
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
                listing = subprocess.check_output([binary, "list", "--json"] + base_command[2:], text=True)
                if "sleep" in listing:
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

            def approve(request_id, paths=(), passphrase=None, port=None):
                approval = Terminal([binary, "review", request_id], wait_ready=False)
                terms.append(approval)
                for kind, path in paths:
                    if kind == "download":
                        approval.expect("Download ")
                    approval.expect("Save as" if kind == "download" else "Local file to upload")
                    approval.send("\x15" + str(path) + "\n" if path else "\n")
                if port is not None:
                    approval.expect("Local listening port")
                    approval.send("\x15" + str(port) + "\n")
                if passphrase:
                    approval.expect("Enter passphrase")
                    approval.send(passphrase + "\n")
                return approval

            def finish_review(approval):
                approval.wait()
                snapshot = json.loads(subprocess.check_output(
                    [binary, "list", "--json"] + base_command[2:], text=True))
                shell = next(row for row in snapshot["shells"] if row["name"] == main_name)
                assert shell["state"] == "detached" and shell["pid"] == shell_pid, shell

            def attach_shell():
                terminal = Terminal(command)
                terms.append(terminal)
                return terminal

            data = bytes(range(256)) * 4097
            remote = root / "remote.bin"
            remote.write_bytes(data)
            downloaded = root / "downloaded.bin"
            request_id = queue_request(term, f"sshore cp {shlex.quote('.:' + str(remote))} origin:")
            assert not downloaded.exists(), "download occurred before approval"
            listed = subprocess.check_output([binary, "list", "--json"], text=True)
            assert request_id in listed
            term = approve(request_id, [("download", downloaded)])
            term.expect("Saved .*downloaded.bin")
            finish_review(term)
            term = attach_shell()
            assert downloaded.read_bytes() == data
            term.send("printf 'RESUME:%s:%s\\n' $$ \"$KEEP\"\n")
            term.expect(f"RESUME:{shell_pid}:shore")
            replay = subprocess.run([binary, "review", request_id], capture_output=True)
            assert replay.returncode != 0

            local = root / "local 文件.bin"
            local.write_bytes(data[::-1])
            uploads = root / "remote-files"
            request_id = queue_request(term, f"sshore cp :: {shlex.quote('.:' + str(uploads))}")
            assert not (uploads / local.name).exists()
            approved = approve(request_id, [("upload", local)])
            approved.expect("Uploaded ")
            finish_review(approved)
            assert (uploads / local.name).read_bytes() == data[::-1]

            term = Terminal(command)
            terms.append(term)
            term.send("printf 'STAY_PID:%s\\n' $$\n")
            term.expect(f"STAY_PID:{shell_pid}")
            request_id = queue_request(term, f"sshore cp {shlex.quote('.:' + str(remote))} origin:")
            subprocess.run([binary, "kill", "review/" + request_id], check=True)
            assert request_id not in subprocess.check_output([binary, "list", "--json"], text=True)

            # Downloads require review and an explicit destination.
            automatic = root / "automatic"
            automatic.mkdir()
            term = Terminal(command[:2] + ["--quiet", "--no-escape"] + command[2:])
            terms.append(term)
            # An authenticated peer can still send old task messages.
            # Neither a legacy file offer nor forward-add can bypass approval.
            def unsolicited(metadata):
                record = json.loads((Path(remote_account.pw_dir) / ".sshore" / "daemon" / "daemon.json").read_text())
                snapshot = json.loads(subprocess.check_output(
                    [binary, "list", "--json"] + base_command[2:], text=True))
                instance = next(row["instance"] for row in snapshot["shells"] if row["name"] == main_name)
                address, port = record["Address"].rsplit(":", 1)
                with socket.create_connection((address, int(port)), timeout=10) as peer:
                    stream = peer.makefile("rb")
                    peer.sendall((
                        f"POST /tasks/{instance}/open HTTP/1.1\r\n"
                        f"Host: {record['Address']}\r\n"
                        f"Authorization: Bearer {record['Token']}\r\n"
                        f"Sshore-Version: {record['Version']}\r\n"
                        "Connection: Upgrade\r\n"
                        "Upgrade: sshore-stream\r\n"
                        "Content-Length: 0\r\n\r\n"
                    ).encode())
                    status = stream.readline()
                    assert status.startswith(b"HTTP/1.1 101 "), status
                    for header in iter(stream.readline, b"\r\n"):
                        assert header, "truncated upgrade response"
                    peer.sendall(json.dumps(metadata).encode() + b"\n")
                    reply = json.loads(stream.readline())
                    assert reply["type"] == "fatal" and reply.get("error"), reply
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
            request_id = queue_request(term, "sshore cp " + " ".join(shlex.quote(".:" + str(p)) for p in batch) + " ::")
            assert list(automatic.iterdir()) == []
            term = approve(request_id, [("download", automatic / path.name) for path in batch])
            finish_review(term)
            term = attach_shell()
            for path in batch:
                assert (automatic / path.name).read_bytes() == path.read_bytes()
            term.send("stty -icanon min 1 -echo; printf 'KEY_%s\\n' ready; od -An -tx1 -N1; stty sane -echo\n")
            term.expect("KEY_ready")
            term.send(b"\x1d")
            term.expect("1d")

            # Local overwrite is rejected before connecting or consuming approval.
            request_id = queue_request(term, f"sshore cp {shlex.quote('.:' + str(batch[0]))} ::")
            rejected = Terminal([binary, "review", request_id], wait_ready=False)
            terms.append(rejected)
            rejected.expect("Save as")
            rejected.send("\x15" + str(automatic / batch[0].name) + "\n")
            rejected.expect("destination already exists")
            rejected.expect("Save as")
            rejected.send(b"\x03")
            rejected.wait(1)
            subprocess.run([binary, "kill", "review/" + request_id], check=True)

            # Approved forwarding outlives the review process and source shell attachment.
            term = Terminal(command)
            terms.append(term)
            with socket.socket() as probe:
                probe.bind(("127.0.0.1", 0))
                forward_port = probe.getsockname()[1]
            request_id = queue_request(term, f"sshore forward origin: .:{forward_port}")
            with socket.socket() as probe:
                probe.bind(("127.0.0.1", forward_port))
            tunnel = approve(request_id, port=forward_port)
            resource_id = tunnel.expect(r"Started (forward/[^: \r\n]+):").group(1).decode()
            finish_review(tunnel)
            try:
                with socket.socket() as probe:
                    try:
                        probe.bind(("127.0.0.1", forward_port))
                    except OSError as e:
                        assert e.errno == errno.EADDRINUSE
                    else:
                        raise AssertionError("approved listener was not opened")
                assert resource_id in subprocess.check_output([binary, "list", "--json"], text=True)
                term = Terminal(command)
                terms.append(term)
                term.send("printf 'FINAL_PID:%s\\n' $$\n")
                term.expect(f"FINAL_PID:{shell_pid}")
                term.send("sshore detach\n")
                term.expect("Detached")
                term.wait()
            finally:
                subprocess.run([binary, "kill", resource_id], check=True)
            with socket.socket() as probe:
                probe.bind(("127.0.0.1", forward_port))

            # The agent retains the key after its on-disk private file is encrypted.
            subprocess.run(["ssh-keygen", "-q", "-p", "-P", "", "-N", "sshore-test-passphrase", "-f", str(key)], check=True, stdout=subprocess.DEVNULL)
            term = Terminal(command, wait_ready=False)
            terms.append(term)
            term.expect(r"\[sshore\] Connected")
            request_id = queue_request(term, f"sshore cp {shlex.quote('.:' + str(remote))} origin:")
            term = approve(request_id, [("download", root / "after-auth.bin")])
            term.expect("Saved .*after-auth.bin")
            finish_review(term)
            term = attach_shell()
            assert (root / "after-auth.bin").read_bytes() == data
            term.send("exit 7\n")
            term.wait(7)
            shell_pid = None
            print("PASS: SSH + PTY, local approval, explicit reattachment, replay rejection, files, batches, forwarding, PID persistence, key authentication")
        finally:
            try:
                for term in terms:
                    term.close()
            finally:
                daemon.terminate()
                daemon.wait(timeout=5)
                log.close()


if __name__ == "__main__":
    main()

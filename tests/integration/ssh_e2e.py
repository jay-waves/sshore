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
    terminal = Terminal([binary, "spawn", "--local", "handoff-local"])
    try:
        marker = root / "handoff-accepted"
        nested = [binary, "spawn", host, "handoff-remote", "--quiet", *options]
        terminal.send(shlex.join(nested) + "; printf '%s' \"$?\" > " + shlex.quote(str(marker)) + "\n")
        terminal.expect(r"Connected [^\r\n]+ / handoff-remote")
        deadline = time.monotonic() + 5
        while not marker.exists() and time.monotonic() < deadline:
            time.sleep(0.02)
        assert marker.read_text() == "0", "nested command did not exit after handoff"
        terminal.send("printf 'HANDOFF-%s\\n' ready\n")
        terminal.expect("HANDOFF-ready")
        terminal.send(b"\x02d")
        terminal.expect(r"Connected local / handoff-local")
        terminal.send(shlex.join([binary, "attach", host, "handoff-remote", "--quiet", *options]) + "\n")
        terminal.expect(r"Connected [^\r\n]+ / handoff-remote")
        terminal.send("exit\n")
        terminal.expect(r"Connected local / handoff-local")
        terminal.send("exit\n")
        terminal.wait()
        print("PASS: local frontend handoff, single-prefix remote keys, return and reattach over SSH")
    finally:
        terminal.close()


def nested_smoke(binary, host, options, root, agent_socket):
    terminal = Terminal([binary, "launch", host, "nested-root", *options])
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
            command = "export SSH_AUTH_SOCK=" + shlex.quote(str(agent_socket)) + "; "
            args = [binary, "spawn", "--local", name] if name == "nested-local" else [binary, "spawn", host, name, *options]
            command += shlex.join(args)
            terminal.send(command + "; printf '%s' \"$?\" > " + shlex.quote(str(marker)) + "\n")
            terminal.expect(r"Connected [^\r\n]+ / " + name)
            deadline = time.monotonic() + 5
            while not marker.exists() and time.monotonic() < deadline:
                time.sleep(0.02)
            assert marker.read_text() == "0", "nested launcher did not exit successfully"
            deadline = time.monotonic() + 5
            while len(processes()) > len(baseline) and time.monotonic() < deadline:
                time.sleep(0.02)
            assert len(processes()) <= len(baseline), ("spawn added resident processes", baseline, processes())
        terminal.send(b"\x02d")
        terminal.expect(r"Connected [^\r\n]+ / nested-grandchild")
        reattached = Terminal([binary, "attach", host, "nested-local"])
        try:
            payload, received = root / "nested-payload", root / "nested-received"
            payload.write_bytes(b"routed approval data")
            reattached.send(shlex.join([binary, "cp", str(payload), "origin:"]) + "\n")
            request = reattached.expect(r"Request saved: ([a-f0-9]{32})").group(1).decode()
            reattached.expect("Background;")
            reattached.wait()
            reattached.close()
            reattached = None
            approval = Terminal([binary, "review", request], wait_ready=False)
            try:
                approval.expect("Save as")
                approval.send(str(received) + "\n")
                approval.expect("Saved ")
                approval.wait()
                assert received.read_bytes() == payload.read_bytes()
            finally:
                approval.close()
            reattached = Terminal([binary, "attach", host, "nested-local"])
            reattached.send("exit\n")
            reattached.wait()
        finally:
            if reattached is not None:
                reattached.close()
        terminal.send(b"\x02n")
        terminal.expect(r"Connected [^\r\n]+ / nested-root")
        terminal.send(b"\x02l")
        terminal.expect(r"Connected [^\r\n]+ / nested-grandchild")
        for name in ("nested-child", "nested-root"):
            terminal.send("exit\n")
            terminal.expect(r"Connected [^\r\n]+ / " + name)
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

    terminal = Terminal(cli("spawn", "foreground"))
    try:
        terminal.send("sleep 2; printf 'SPAWN-%s\\n' retained\n")
        terminal.expect("SPAWN-retained")
        rows = {row["name"]: row for row in inventory()["shells"]}
        assert rows["foreground"]["state"] == "attached", rows
        terminal.send("exit\n")
        terminal.wait()
    finally:
        terminal.close()

    subprocess.run(cli("launch", "persistent", "--detach"), check=True)
    parent = subprocess.Popen(
        ["sh", "-c", '\"$@\"; code=$?; echo READY:$code; read answer', "parent",
         *cli("spawn", "temporary", "--detach")],
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
        assert b"READY:0" in output and b"spawn/temporary" in output, output
        rows = {row["name"]: row for row in inventory()["shells"]}
        assert rows["temporary"]["temporary"] and rows["temporary"]["state"] == "detached", rows
        assert not rows["persistent"].get("temporary"), rows
        pid = rows["temporary"]["pid"]
        terminal = Terminal(cli("attach", "temporary"))
        try:
            terminal.send("printf 'SPAWN-%s\\n' alive\n")
            terminal.expect("SPAWN-alive")
            terminal.send(b"\x02d")
            terminal.wait()
        finally:
            terminal.close()
        assert next(row for row in inventory()["shells"] if row["name"] == "temporary")["pid"] == pid
        parent.stdin.close()
        parent.wait(timeout=5)
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            try:
                rows = {row["name"]: row for row in inventory()["shells"]}
            except subprocess.CalledProcessError as error:
                snapshot = json.loads(error.output)
                if snapshot.get("errors") != ["shell unavailable: temporary.sock"]:
                    raise
                time.sleep(0.1)
                continue
            if "temporary" not in rows:
                assert "persistent" in rows, rows
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
        subprocess.run(cli("kill", "persistent"), check=True)


def setup_smoke(binary, host, options, root):
    release = subprocess.check_output([binary, "--version"], text=True).strip().split()[1]
    cache_root = root / "cache"
    os.environ["XDG_CACHE_HOME"] = str(cache_root)
    cache = cache_root / "sshore" / "releases" / release
    cache.mkdir(parents=True, mode=0o700)
    (cache_root / "sshore").chmod(0o700)
    archive = cache / f"sshore_{release}_linux_amd64.tar.gz"
    with tarfile.open(archive, "w:gz") as output:
        output.add(binary, arcname="sshore")
    archive.chmod(0o600)
    manifest = cache / "SHA256SUMS"
    manifest.write_text(f"{hashlib.sha256(archive.read_bytes()).hexdigest()}  {archive.name}\n")
    manifest.chmod(0o600)
    for action in ("launch", "spawn"):
        target = root / "remote-home" / "bin" / action
        cli = [binary, action, host, "prepared", "--server", str(target), *options]
        if action == "launch":
            waiting = []
            try:
                for _ in range(2):
                    client = Terminal(cli, wait_ready=False)
                    waiting.append(client)
                    client.expect("Run sshore review")
                review = Terminal([binary, "review"], wait_ready=False)
                try:
                    # One pending request opens directly, without a picker.
                    review.expect("Type install")
                    review.send("\n")
                    review.wait()
                finally:
                    review.close()
                for client in waiting:
                    client.wait(expected=1)
                assert not target.exists()
            finally:
                for client in waiting:
                    client.close()
        else:
            # A published older release requires the typed update decision.
            target.write_text("#!/bin/sh\necho 'sshore 0.0.1'\n")
            target.chmod(0o700)
            owner = target.parent.stat()
            if owner.st_uid != os.getuid():
                os.chown(target, owner.st_uid, owner.st_gid)
        terminal = Terminal(cli, wait_ready=False)
        try:
            terminal.expect("Run sshore review")
            if action == "launch":
                assert not target.exists()
            review = Terminal([binary, "review"], wait_ready=False)
            try:
                decision = "install" if action == "launch" else "update"
                review.expect("Type " + decision)
                review.send(decision + "\n")
                review.wait()
            finally:
                review.close()
            terminal.expect("Connected")
            terminal.send("printf 'SETUP-%s\\n' alive\n")
            terminal.expect("SETUP-alive")
            terminal.send("exit\n")
            terminal.wait()
        finally:
            terminal.close()
        assert subprocess.check_output([str(target), "--version"], text=True).strip() == f"sshore {release}"
        assert not (root / "remote-home" / "curl-calls").exists(), "setup attempted remote download"
        result = subprocess.run([binary, "launch", host, "matching", "--detach", "--server", str(target), *options], capture_output=True, text=True)
        assert result.returncode == 0, result.stderr
        assert not (root / "remote-home" / "curl-calls").exists(), "matching version attempted a download"
        subprocess.run([binary, "kill", host, "matching", "--server", str(target), *options], check=True)
    # Exercise the standalone installer against an actual registered remote task.
    subprocess.run([binary, "launch", host, "installer-existing", "--detach", "--server", str(target), *options],
                   check=True, timeout=15)
    installer_target = target.with_name("installer")
    original = b"#!/bin/sh\necho 'sshore 0.0.1'\n"
    installer_target.write_bytes(original)
    installer_target.chmod(0o700)
    owner = target.stat()
    os.chown(installer_target, owner.st_uid, owner.st_gid)
    installer_command = shlex.join([str(root / "server"), "bootstrap", str(installer_target)])
    installed = [binary, "list", "--json", host, "--server", str(target), *options]

    def installer_tasks():
        return {row["name"]: row["pid"] for row in json.loads(
            subprocess.check_output(installed, timeout=15))["shells"]}

    original_pid = installer_tasks()["installer-existing"]
    for answer in ("", "update"):
        installer = Terminal(["/usr/bin/ssh", "-tt", *options[1:], host, installer_command], wait_ready=False)
        try:
            installer.expect("installer-existing")
            installer.expect("Type update to install")
            installer.send(answer + "\n")
            installer.wait(expected=0 if answer else 1)
        finally:
            installer.close()
        if not answer:
            assert installer_target.read_bytes() == original
            assert installer_tasks()["installer-existing"] == original_pid
    assert "installer-existing" not in installer_tasks()
    assert subprocess.check_output([str(installer_target), "--version"], text=True).strip() == f"sshore {release}"
    assert not list(target.parent.glob(".sshore-update-*"))
    print("setup: reviewed SSH setup from verified client cache, exact version and matching-version reuse OK")


def reset_smoke(binary, root):
    subprocess.run([binary, "launch", "--local", "reset-fixture", "--detach"], check=True)
    state = root / "state"
    history = state / "shells" / "reset-history"
    history.parent.mkdir(mode=0o700, exist_ok=True)
    history.write_text("clear")
    unrelated = state / "user-file"
    unrelated.write_text("keep")
    cache = Path(os.environ["XDG_CACHE_HOME"]) / "sshore"
    assert cache.is_dir()
    daemon = Path(pwd.getpwuid(os.getuid()).pw_dir) / ".sshore" / "daemon" / "daemon.json"
    assert daemon.is_file()
    subprocess.run([binary, "reset"], check=True, timeout=15)
    assert unrelated.read_text() == "keep"
    assert not cache.exists()
    assert not daemon.exists()
    assert not (state / "shells").exists()
    print("PASS: release reset ended the local daemon and cleared only application data")


def mcp_smoke(binary, host, ssh_wrapper):
    env = dict(os.environ, SSHORE_SSH=str(ssh_wrapper))
    proc = subprocess.Popen([binary, "mcp"], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, text=True, env=env, bufsize=1)
    def call(method, params, request_id):
        proc.stdin.write(json.dumps({"jsonrpc": "2.0", "id": request_id,
                                     "method": method, "params": params}) + "\n")
        proc.stdin.flush()
        ready, _, _ = select.select([proc.stdout], [], [], 20)
        if not ready:
            raise AssertionError(f"MCP request {method} timed out")
        reply = json.loads(proc.stdout.readline())
        if "error" in reply or reply.get("id") != request_id:
            raise AssertionError(reply)
        return reply["result"]
    def tool(name, args, request_id):
        result = call("tools/call", {"name": name, "arguments": args}, request_id)
        if result.get("isError"):
            raise AssertionError(result)
        return result["structuredContent"]
    try:
        call("initialize", {"protocolVersion": "2025-11-25", "capabilities": {},
                            "clientInfo": {"name": "test", "version": "1"}}, 1)
        proc.stdin.write(json.dumps({"jsonrpc": "2.0", "method": "notifications/initialized"}) + "\n")
        proc.stdin.flush()
        session = tool("session_open", {"host": host}, 2)
        run = tool("session_execute", {"session_id": session["id"], "code": "x=sshore; printf '%s' \"$x\""}, 3)
        cursor, text = "", ""
        for i in range(4, 104):
            out = tool("session_read", {"session_id": session["id"], "execution_id": run["id"],
                                        "cursor": cursor, "wait_ms": 100}, i)
            text += out["stdout"]
            cursor = out["cursor"]
            if out["state"] == "completed" and out["output_closed"]:
                break
        assert text == "sshore", text
        tool("session_close", {"session_id": session["id"]}, 104)
        print("PASS: MCP over real SSH with persistent Bash")
    finally:
        proc.stdin.close()
        proc.wait(timeout=10)
        if proc.returncode:
            raise AssertionError(proc.stderr.read())


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", default="dist/sshore")
    scenarios = ("core", "spawn", "identity", "nested", "handoff", "setup", "mcp")
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
        agent_socket = root / 'agent.sock'
        agent_process = subprocess.Popen(['ssh-agent', '-D', '-a', str(agent_socket)], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        os.environ['SSH_AUTH_SOCK'] = str(agent_socket)
        for _ in range(100):
            if agent_socket.exists():
                break
            time.sleep(0.01)
        subprocess.run(['ssh-add', str(key)], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
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
                         f"export PATH={shlex.quote(str(tools))}:{shlex.quote(str(root))}:$PATH\n" +
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
                # IdentityFile selects an agent identity; private keys must never
                # be loaded by sshore, including when the agent is disabled.
                rejected = Terminal(command + ["-o", "IdentityAgent=none"], wait_ready=False)
                terms.append(rejected)
                rejected.expect("no usable SSH public-key identity")
                rejected.wait(expected=1)
                secret = "sshore-file-key-secret"
                subprocess.run(["ssh-keygen", "-q", "-p", "-P", "", "-N", secret, "-f", str(key)], check=True, stdout=subprocess.DEVNULL)
                for action in ("launch", "spawn"):
                    agent_command = command[:]
                    agent_command[1] = action
                    terminal = Terminal(agent_command)
                    terms.append(terminal)
                    terminal.send("printf 'AGENT-%s\\n' ready\n")
                    terminal.expect("AGENT-ready")
                    terminal.send("exit\n")
                    terminal.wait()
                print("identity: agent selection with encrypted private key and rejection without agent OK")
                return
            if args.mcp_only:
                ssh_wrapper = root / "mcp-ssh"
                ssh_wrapper.write_text("#!/bin/sh\n" +
                    "exec ssh -F /dev/null -p " + str(port) +
                    " -i " + shlex.quote(str(key)) +
                    " -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o " +
                    shlex.quote("UserKnownHostsFile=" + str(known)) + ' "$@"\n')
                ssh_wrapper.chmod(0o700)
                mcp_smoke(binary, command[2], ssh_wrapper)
                return
            if args.handoff_only:
                handoff_smoke(binary, command[2], command[3:], root)
                return
            if args.nested_only:
                remote_key = home / "key"
                shutil.copy2(key, remote_key)
                os.chown(remote_key, remote_account.pw_uid, remote_account.pw_gid)
                remote_socket = home / "agent.sock"
                run_as_remote = ["runuser", "-u", username, "--"]
                remote_agent = subprocess.Popen(run_as_remote + ["ssh-agent", "-D", "-a", str(remote_socket)],
                                                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                try:
                    for _ in range(100):
                        if remote_socket.exists():
                            break
                        time.sleep(0.01)
                    subprocess.run(run_as_remote + ["env", f"SSH_AUTH_SOCK={remote_socket}", "ssh-add", str(remote_key)], check=True)
                    nested_smoke(binary, command[2], command[3:], root, remote_socket)
                finally:
                    remote_agent.terminate()
                    remote_agent.wait(timeout=5)
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
                auto = Terminal(base_command, wait_ready=False)
                terms.append(auto)
                match = auto.expect(r"Connected [^\r\n]+ / ([0-9a-f]{6}) ")
                assert match.group(1).decode() not in auto_names
                auto_names.append(match.group(1).decode())
                auto.send(b"\x02d")
                auto.wait()
                auto.close()
                terms.remove(auto)
            for name in auto_names:
                auto = Terminal([binary, "attach"] + [base_command[2], name] + base_command[3:])
                terms.append(auto)
                auto.send("exit\n")
                auto.wait()
                auto.close()
                terms.remove(auto)
            command = [binary, "attach"] + [base_command[2], "main"] + base_command[3:]
            # SSH configuration forwarding is deliberately disabled. Approved
            # sshore forwarding is exercised below through its public workflow.
            for option in ("-L127.0.0.1:12345:127.0.0.1:12346",
                           "-R127.0.0.1:12345:127.0.0.1:12346", "-D127.0.0.1:12345"):
                rejected = Terminal([binary, "launch"] + command[2:] + [option], wait_ready=False)
                terms.append(rejected)
                rejected.expect("forwarding is unsupported")
                rejected.wait(expected=1)
            term = Terminal([binary, "launch"] + command[2:])
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
                    approval.expect("Save as" if kind == "download" else "Local file to upload")
                    approval.send(str(path) + "\n")
                if port is not None:
                    approval.expect("Local listening port")
                    approval.send(str(port) + "\n")
                if passphrase:
                    approval.expect("Enter passphrase")
                    approval.send(passphrase + "\n")
                return approval

            def finish_review(approval):
                approval.wait()
                snapshot = json.loads(subprocess.check_output(
                    [binary, "list", "--json"] + base_command[2:], text=True))
                shell = next(row for row in snapshot["shells"] if row["name"] == "main")
                assert shell["state"] == "detached" and shell["pid"] == shell_pid, shell

            def attach_shell():
                terminal = Terminal(command)
                terms.append(terminal)
                return terminal

            data = bytes(range(256)) * 4097
            remote = root / "remote.bin"
            remote.write_bytes(data)
            downloaded = root / "downloaded.bin"
            request_id = queue_request(term, f"sshore cp {shlex.quote(str(remote))} origin:")
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
            request_id = queue_request(term, f"sshore cp : {shlex.quote(str(uploads))}")
            assert not (uploads / local.name).exists()
            approved = approve(request_id, [("upload", local)])
            approved.expect("Uploaded ")
            finish_review(approved)
            assert (uploads / local.name).read_bytes() == data[::-1]

            term = Terminal(command)
            terms.append(term)
            term.send("printf 'STAY_PID:%s\\n' $$\n")
            term.expect(f"STAY_PID:{shell_pid}")
            request_id = queue_request(term, f"sshore cp {shlex.quote(str(remote))} origin:")
            subprocess.run([binary, "kill", "review/" + request_id], check=True)
            assert request_id not in subprocess.check_output([binary, "list", "--json"], text=True)

            # A download directory supplies defaults, never automatic approval.
            automatic = root / "automatic"
            automatic.mkdir()
            term = Terminal(command[:2] + ["--download-dir", str(automatic), "--quiet", "--no-escape"] + command[2:])
            terms.append(term)
            # A compromised remote peer can still speak the old wire format.
            # Neither a legacy file offer nor forward-add can bypass approval.
            def unsolicited(metadata):
                record = json.loads((Path(remote_account.pw_dir) / ".sshore" / "daemon" / "daemon.json").read_text())
                snapshot = json.loads(subprocess.check_output(
                    [binary, "list", "--json"] + base_command[2:], text=True))
                instance = next(row["instance"] for row in snapshot["shells"] if row["name"] == "main")
                address, port = record["Address"].rsplit(":", 1)
                with socket.create_connection((address, int(port)), timeout=10) as peer:
                    stream = peer.makefile("rb")
                    def exchange(message):
                        body = json.dumps(message).encode()
                        peer.sendall(b"\x00" + struct.pack("!I", len(body)) + body)
                        header = stream.read(5)
                        assert len(header) == 5, header
                        return json.loads(stream.read(struct.unpack("!I", header[1:])[0]))
                    reply = exchange({"type": "local_auth", "token": record["Token"],
                                      "software_version": record["Version"]})
                    assert reply["type"] == "local_ready", reply
                    reply = exchange({"type": "task_open", "instance": instance})
                    assert reply["type"] == "task_ready", reply
                    reply = exchange(metadata)
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
            request_id = queue_request(term, "sshore cp " + " ".join(shlex.quote(str(p)) for p in batch) + " :")
            assert list(automatic.iterdir()) == []
            term = approve(request_id, [("download", "")] * len(batch))
            finish_review(term)
            term = attach_shell()
            for path in batch:
                assert (automatic / path.name).read_bytes() == path.read_bytes()
            term.send("stty -icanon min 1 -echo; printf 'KEY_%s\\n' ready; od -An -tx1 -N1; stty sane -echo\n")
            term.expect("KEY_ready")
            term.send(b"\x1d")
            term.expect("1d")

            # Local overwrite is rejected before connecting or consuming approval.
            request_id = queue_request(term, f"sshore cp {shlex.quote(str(batch[0]))} :")
            rejected = Terminal([binary, "review", request_id], wait_ready=False)
            terms.append(rejected)
            rejected.expect("Save as")
            rejected.send(str(automatic / batch[0].name) + "\n")
            rejected.expect("destination already exists")
            rejected.send(b"\x03")
            rejected.wait(1)
            subprocess.run([binary, "kill", "review/" + request_id], check=True)

            # Approved forwarding outlives the review process and source shell attachment.
            term = Terminal(command)
            terms.append(term)
            with socket.socket() as probe:
                probe.bind(("127.0.0.1", 0))
                forward_port = probe.getsockname()[1]
            request_id = queue_request(term, f"sshore forward origin: {forward_port}")
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
            request_id = queue_request(term, f"sshore cp {shlex.quote(str(remote))} origin:")
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
                agent_process.terminate()
                agent_process.wait(timeout=5)
                log.close()
                deadline = time.monotonic() + 3
                while runtime.exists() and list(runtime.glob("*.sock")) and time.monotonic() < deadline:
                    time.sleep(0.05)


if __name__ == "__main__":
    main()

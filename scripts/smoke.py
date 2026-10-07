#!/usr/bin/env python3
"""Exercise real DoucheSync processes. Uses only Python's standard library."""
import json
import pathlib
import secrets
import socket
import subprocess
import sys
import tempfile
import time


def address():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def wait_for(test, processes, logs, label, timeout=50):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        if any(p.poll() is not None for p in processes):
            raise RuntimeError("process exited while waiting for " + label)
        if test():
            print("PASS:", label, flush=True)
            return
        time.sleep(0.2)
    for log in logs:
        print(log.read_text(), file=sys.stderr)
    raise RuntimeError("timed out waiting for " + label)


def content(path):
    try:
        return path.read_text()
    except FileNotFoundError:
        return None


def main():
    binary = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else "./DoucheSync").resolve()
    processes, handles, logs = [], [], []
    with tempfile.TemporaryDirectory(prefix="douchesync-smoke-") as temporary:
        work = pathlib.Path(temporary)
        server_dir = work / "server-workdir"
        server_dir.mkdir()
        token, secret = secrets.token_hex(32), secrets.token_hex(32)
        port = address()
        config = work / "server.toml"
        config.write_text(f'[server]\nlisten = "127.0.0.1:{port}"\ntoken = "{token}"\n')

        def launch(mode, cfg, cwd):
            log = work / f"{mode}-{len(processes)}.log"
            handle = log.open("wb")
            handles.append(handle)
            logs.append(log)
            process = subprocess.Popen([str(binary), mode, "-config=" + str(cfg)], cwd=cwd,
                                       stdout=handle, stderr=subprocess.STDOUT)
            processes.append(process)

        try:
            launch("server", config, server_dir)

            def listening():
                try:
                    with socket.create_connection(("127.0.0.1", port), timeout=0.2):
                        return True
                except OSError:
                    return False

            wait_for(listening, processes, logs, "discovery started", 10)
            folders = []
            for i in range(3):
                root = work / f"client-{i}"
                root.mkdir()
                folders.append(root)
                (root / f"origin-{i}.txt").write_text(f"hello from machine {i}")
                peer_port = address()
                cfg = work / f"client-{i}.toml"
                # JSON-escaped string literals also work as TOML basic strings.
                cfg.write_text(
                    f'[client]\ndevice_id = "machine-{i}"\n'
                    f'listen = "127.0.0.1:{peer_port}"\n'
                    f'advertise_url = "https://127.0.0.1:{peer_port}"\n'
                    f'discovery_url = "http://127.0.0.1:{port}"\n'
                    f'discovery_token = "{token}"\nallow_http_discovery = true\n'
                    'scan_interval = "1s"\ntransfer_timeout = "30s"\n'
                    f'[[folders]]\nid = "shared"\npath = {json.dumps(str(root))}\n'
                    f'secret = "{secret}"\nsync_deletes = true\n')
                launch("client", cfg, work)

            wait_for(lambda: all(content(root / f"origin-{i}.txt") == f"hello from machine {i}"
                                 for root in folders for i in range(3)), processes, logs,
                     "all three clients exchanged their files")
            (folders[1] / "origin-0.txt").write_text("updated on machine 1")
            wait_for(lambda: all(content(root / "origin-0.txt") == "updated on machine 1"
                                 for root in folders), processes, logs, "remote update propagated")
            (folders[2] / "origin-2.txt").unlink()
            wait_for(lambda: all(not (root / "origin-2.txt").exists() for root in folders),
                     processes, logs, "deletion propagated")
            if list(server_dir.iterdir()):
                raise RuntimeError("discovery wrote files to its working directory")
            print("PASS: discovery working directory contains no files", flush=True)
        finally:
            for process in reversed(processes):
                process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
            for handle in handles:
                handle.close()
        if any(p.returncode != 0 for p in processes):
            for log in logs:
                print(log.read_text(), file=sys.stderr)
            raise RuntimeError("process did not shut down cleanly")
        print("PASS: all processes shut down cleanly", flush=True)


if __name__ == "__main__":
    main()

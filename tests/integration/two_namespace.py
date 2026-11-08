"""Two-namespace CI host fixture: a GitLab service container acts as host B.

This is a feasibility probe for the two-namespace lane (docs/internal/ci.md). It proves only
what it measures: whether host B shares the checkout, runs in separate network/PID/
UTS namespaces, and is reachable from host A by its service alias. It is never
physical two-host or Darwin evidence.
"""

import argparse
import hashlib
import http.server
import json
import os
from pathlib import Path
import socket
import sys
import time
import urllib.error
import urllib.request


PORT = 8765
DEADLINE = 300


def identity():
    namespaces = {}
    for name in ("net", "pid", "uts", "mnt"):
        try:
            namespaces[name] = os.readlink(f"/proc/self/ns/{name}")
        except OSError as error:
            namespaces[name] = f"unavailable ({error.errno})"
    return {"hostname": socket.gethostname(), "namespaces": namespaces, "uid": os.getuid()}


def checkout_digest(root):
    marker = root / "go.mod"
    return hashlib.sha256(marker.read_bytes()).hexdigest() if marker.is_file() else None


def host_b(root):
    """Service entry: report what this namespace can see, then serve it to host A."""
    # Services start before the job checks out its sources; wait, bounded.
    deadline = time.monotonic() + DEADLINE
    while checkout_digest(root) is None and time.monotonic() < deadline:
        time.sleep(1)
    report = dict(identity(), checkout_sha256=checkout_digest(root), checkout_path=str(root))
    body = json.dumps(report).encode()

    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            self.send_response(200 if self.path == "/identity" else 404)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            if self.path == "/identity":
                self.wfile.write(body)

        def log_message(self, *_):
            pass

    server = http.server.ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    # Never outlive the job: GitLab removes the service, and this bounds it anyway.
    server.timeout = 1
    stop = time.monotonic() + 3600
    while time.monotonic() < stop:
        server.handle_request()


def probe(root, alias, out):
    """Job side: compare host A with host B and record exactly what was shown."""
    deadline = time.monotonic() + DEADLINE
    remote, error = None, None
    while time.monotonic() < deadline:
        try:
            with urllib.request.urlopen(f"http://{alias}:{PORT}/identity", timeout=5) as response:
                remote = json.load(response)
            break
        except urllib.error.HTTPError as caught:
            # The CI bootstrap's fallback: host B answers but never saw the checkout.
            remote = {"checkout_sha256": None, "namespaces": {}, "hostname": None,
                      "fallback": f"HTTP {caught.code}: checkout not visible to host B"}
            break
        except OSError as caught:
            error = caught
            time.sleep(2)
    local = dict(identity(), checkout_sha256=checkout_digest(root), checkout_path=str(root))
    findings = {
        "host_b_reachable_by_alias": remote is not None,
        "host_b_shares_checkout": bool(remote) and remote["checkout_sha256"] == local["checkout_sha256"]
        and local["checkout_sha256"] is not None,
        "separate_network_namespace": bool(remote) and remote["namespaces"].get("net")
        not in (None, local["namespaces"]["net"]),
        "separate_pid_namespace": bool(remote) and remote["namespaces"].get("pid")
        not in (None, local["namespaces"]["pid"]),
        "separate_hostname": bool(remote) and remote["hostname"] not in (None, local["hostname"]),
    }
    report = {"evidence": "two-namespace feasibility probe; not physical two-host or Darwin",
              "host_a": local, "host_b": remote, "error": None if remote else repr(error),
              "findings": findings}
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report, indent=2))
    required = ("host_b_reachable_by_alias", "host_b_shares_checkout", "separate_network_namespace",
                "separate_pid_namespace", "separate_hostname")
    return 0 if all(findings[name] for name in required) else 1


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("role", choices=["host-b", "probe"])
    parser.add_argument("--root", type=Path, default=Path(os.environ.get("CI_PROJECT_DIR", ".")))
    parser.add_argument("--alias", default="hostb")
    parser.add_argument("--out", type=Path, default=Path("artifacts/two-namespace/probe.json"))
    args = parser.parse_args()
    if args.role == "host-b":
        host_b(args.root)
        return 0
    return probe(args.root, args.alias, args.out)


if __name__ == "__main__":
    sys.exit(main())

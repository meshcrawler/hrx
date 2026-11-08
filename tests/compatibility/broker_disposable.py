"""Manual real-GitLab broker test with an operator-provisioned limited account.

Exercises cached credential restoration, not service-account provisioning.
Never supplies an administrator credential to the runner or model inference.
"""
import argparse
from contextlib import ExitStack
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import secrets
import tempfile
import threading
import urllib.request

from broker_scenario import broker_scenario, private
from gateway import Gateway
from run import Runner
from test_auto_scenario import LAUNCH, PARSER, REPO, Upstream, catalog


def execute(hrx, bootstrap, root):
    host = os.environ.get("HRX_CI_GITLAB_HOST", "")
    if not host:
        raise SystemExit("set HRX_CI_GITLAB_HOST to the disposable GitLab host, e.g. gitlab.example.com")
    token = bootstrap["token"]

    def api(path):
        request = urllib.request.Request("https://" + host + "/api/v4" + path,
                                         headers={"PRIVATE-TOKEN": token,
                                                  "User-Agent": "hrx-disposable-broker/0.1"})
        with urllib.request.urlopen(request, timeout=15) as response:
            return json.load(response)

    user = api("/user")
    memberships = api("/projects?membership=true&per_page=100")
    if user["id"] != bootstrap["user_id"] or user["username"] != bootstrap["username"]:
        raise ValueError("fixture_account_mismatch")
    if user.get("is_admin") or [p["id"] for p in memberships] != [bootstrap["project_id"]]:
        raise ValueError("fixture_account_not_isolated")
    project = api("/projects/" + str(bootstrap["project_id"]))
    if (project["visibility"] != "private" or project["path_with_namespace"] != bootstrap["project_path"]
            or "hrx-disposable-ci" not in project.get("topics", []) or project.get("archived")):
        raise ValueError("fixture_project_not_disposable")

    spec = importlib.util.spec_from_file_location(
        "fixture_broker", REPO / "services/gitlab-service-account-broker/broker.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    identity = "ci/hrx-disposable-broker/agent"
    client = secrets.token_hex(32)
    client_file, unavailable_admin = root / "client-token", root / "no-admin-token"
    private(client_file, client)
    private(unavailable_admin, "no-provisioning-credential-available")
    broker = module.Broker({"gitlab_url": "https://" + host,
                            "gitlab_token_file": str(unavailable_admin), "state_dir": str(root / "broker"),
                            "clients": {identity: hashlib.sha256(client.encode()).hexdigest()},
                            "identity_profiles": {identity: "developer"}})

    def refuse_provisioning(*args, **kwargs):
        raise RuntimeError("provisioning_not_authorized_in_fixture")

    broker.api = refuse_provisioning
    with broker.connect() as db:
        db.execute("INSERT INTO accounts VALUES (?,?,?,?,?,?,?)",
                   (identity, bootstrap["username"], bootstrap["user_id"], token,
                    bootstrap["expires_at"], "ready", json.dumps(["api", "write_repository"])))

    policy = {"max_requests": 1, "wall_seconds": 300, "cell_seconds": 180,
              "request_seconds": 10, "max_request_bytes": 100000, "max_output_tokens": 2048,
              "concurrency": 1, "routes": [PARSER, LAUNCH]}
    with ExitStack() as stack:
        server = module.ThreadingHTTPServer(("127.0.0.1", 0), module.Handler)
        server.broker = broker
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()

        def close_broker():
            server.shutdown()
            server.server_close()
            thread.join(10)
            if thread.is_alive():
                raise RuntimeError("broker_thread_not_stopped")

        stack.callback(close_broker)
        upstream = Upstream()
        stack.callback(upstream.close)
        gateway = Gateway(root, upstream.url, "unused-fixture-secret", catalog(), policy)
        stack.callback(gateway.close)
        fixture = {"gitlab_host": host, "project_id": bootstrap["project_id"],
                   "project_path": bootstrap["project_path"], "identity": identity,
                   "broker_url": "http://127.0.0.1:" + str(server.server_port),
                   "client_token_file": str(client_file), "branch_prefix": "hrx-ci-broker/", "stale_hours": 6}
        result = broker_scenario(Runner(hrx, gateway, policy),
                                 {"harness": "claude", "route": LAUNCH, "reasoning": None},
                                 root / "scenario", fixture, timeout=180)
        result["inference_requests"] = upstream.calls
        if upstream.calls:
            result.update(status="FAIL", reason="unexpected_inference")
    result["fixture_servers_closed"] = True
    result["scope"] = "real_gitlab_cached_broker_credentials_scripted_harness_no_inference"
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--hrx", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    os.umask(0o077)
    output = Path(args.output)
    output.mkdir(parents=True, exist_ok=True)
    result = {"status": "FAIL", "reason": "not_started"}
    try:
        bootstrap = json.loads(Path(os.environ["HRX_BROKER_BOOTSTRAP"]).read_text())
        with tempfile.TemporaryDirectory(prefix="hrx-real-broker-") as temp:
            result = execute(str(Path(args.hrx).resolve()), bootstrap, Path(temp))
        result["private_fixture_removed"] = True
    except Exception as exc:
        # Exception messages may contain upstream response bodies or private paths.
        result = {"status": "FAIL", "reason": "fixture_error:" + type(exc).__name__}
    (output / "report.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(result))
    return 0 if result["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())

"""Exercise enrollment/rotation/revocation against a prepared disposable hub.

Uses isolated local state and the private CA/admin files from the two-host
fixture. No production identities or credentials are changed.
"""

import argparse
import json
import os
from pathlib import Path
import secrets
import ssl
import subprocess
import tempfile
import tomllib
import urllib.error
import urllib.request

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--fixture-dir", type=Path, required=True)
parser.add_argument("--hrx", type=Path, required=True)
parser.add_argument("--url", default="https://127.0.0.1:28790")
options = parser.parse_args()
fixture = options.fixture_dir.resolve()
binary = str(options.hrx.resolve())
context = ssl.create_default_context(cafile=str(fixture / "cert.pem"))
known_secrets = [(fixture / "admin.token").read_text().strip()]


def command(env, *args, succeeds=True):
    result = subprocess.run(
        [binary, "hub", *args], env=env, capture_output=True, text=True, timeout=15
    )
    assert all(token not in result.stdout + result.stderr for token in known_secrets)
    assert (result.returncode == 0) == succeeds, "unexpected CLI result"
    return result


def host(token, status=200):
    request = urllib.request.Request(
        options.url + "/api/v1/host",
        headers={"Authorization": "Bearer " + token},
    )
    try:
        with urllib.request.urlopen(request, context=context, timeout=5) as response:
            assert response.status == status
            return json.load(response)
    except urllib.error.HTTPError as error:
        assert error.code == status, error.code


with tempfile.TemporaryDirectory(prefix="hrx-credential-lifecycle-") as temporary:
    root = Path(temporary).resolve()

    def installation(name):
        home = root / name
        home.mkdir(mode=0o700)
        config = home / "config.toml"
        config.write_text("[permissions]\nyolo=false\n")
        config.chmod(0o600)
        return dict(
            os.environ,
            HOME=str(home),
            XDG_STATE_HOME=str(home / "state"),
            HRX_CONFIG=str(config),
            HRX_INTEGRATION_CA=str(fixture / "cert.pem"),
        )

    enrollment = [
        "enroll",
        "--url",
        options.url,
        "--admin-token-file",
        str(fixture / "admin.token"),
        "--prefix",
        "credential-fixture/",
    ]
    original = installation("original")
    command(original, *enrollment)
    config = tomllib.loads(Path(original["HRX_CONFIG"]).read_text())
    assert config["permissions"]["yolo"] is False
    token_path = Path(config["hub"]["token_file"])
    assert token_path.stat().st_mode & 0o077 == 0
    first = token_path.read_text().strip()
    known_secrets.append(first)
    identity = host(first)["host_id"]
    command(original, *enrollment)
    assert token_path.read_text().strip() == first
    assert host(first)["host_id"] == identity
    command(original, "collect", "--once")

    replacement = secrets.token_hex(32)
    known_secrets.append(replacement)
    new_token = root / "rotated.token"
    new_token.write_text(replacement)
    new_token.chmod(0o600)
    command(
        original,
        "rotate",
        "--host-id",
        identity,
        "--new-token-file",
        str(new_token),
        "--admin-token-file",
        str(fixture / "admin.token"),
    )
    host(first, 401)
    assert host(replacement)["host_id"] == identity
    command(original, "collect", "--once", succeeds=False)
    token_path.write_text(replacement)
    command(original, "collect", "--once")
    command(original, *enrollment)
    assert host(replacement)["host_id"] == identity

    command(
        original,
        "revoke",
        "--host-id",
        identity,
        "--admin-token-file",
        str(fixture / "admin.token"),
    )
    host(replacement, 401)
    command(original, "collect", "--once", succeeds=False)
    command(original, *enrollment, succeeds=False)
    fresh = installation("replacement")
    command(fresh, *enrollment)
    fresh_config = tomllib.loads(Path(fresh["HRX_CONFIG"]).read_text())
    fresh_token = Path(fresh_config["hub"]["token_file"]).read_text().strip()
    known_secrets.append(fresh_token)
    fresh_id = host(fresh_token)["host_id"]
    assert fresh_id != identity
    command(fresh, "collect", "--once")
    command(
        fresh,
        "revoke",
        "--host-id",
        fresh_id,
        "--admin-token-file",
        str(fixture / "admin.token"),
    )
    (fixture / "credential-evidence.json").write_text(
        json.dumps(
            {
                "original_host": identity,
                "replacement_host": fresh_id,
                "enrollment_retry": True,
                "rotation": True,
                "revocation": True,
                "replacement": True,
                "cli_secret_redaction": True,
            },
            indent=2,
        )
    )
print(
    "PASS: CLI enrollment retry, private credentials, rotation, revocation, replacement, and redaction"
)

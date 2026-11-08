#!/usr/bin/env python3
"""Identity-scoped GitLab service account broker (Python 3.11+, stdlib only)."""

import argparse
from contextlib import contextmanager
import datetime as dt
import hashlib
import hmac
import json
import logging
import os
from pathlib import Path
import re
import sqlite3
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.error import HTTPError
from urllib.parse import quote, urlsplit
from urllib.request import Request, build_opener, HTTPRedirectHandler

log = logging.getLogger("gitlab-service-account-broker")


class BrokerError(Exception):
    pass


PROFILES = {
    "observer": ["read_api", "read_repository"],
    "developer": ["api", "write_repository"],
}


# GitLab access levels the broker may grant. Owner (50) is deliberately absent.
MEMBER_ROLES = {"guest": 10, "reporter": 20, "developer": 30, "maintainer": 40}
MEMBER_KINDS = {"project": "projects", "group": "groups"}
# One GitLab path segment: no leading dot/dash, no "." or ".." segments.
PATH_SEGMENT = r"[A-Za-z0-9_][A-Za-z0-9_.-]*"
MEMBERSHIP_BUDGET = 8  # seconds of GitLab I/O per reconciliation; HRX waits 40s in total
MEMBERSHIP_CALL_TIMEOUT = 5


def membership_path(kind, path):
    if not isinstance(path, str) or len(path) > 255 or not re.fullmatch(PATH_SEGMENT + r"(/" + PATH_SEGMENT + r")*", path):
        return False
    segments = path.split("/")
    if kind == "project" and len(segments) < 2:
        return False
    return not any(seg.endswith((".", ".git", ".atom")) for seg in segments)


def parse_memberships(raw):
    """Validate operator-only identity_memberships; fail closed on anything unexpected."""
    if not isinstance(raw, dict):
        raise BrokerError("identity_memberships must be an object")
    parsed = {}
    for key, rules in raw.items():
        prefix = r"(?:[A-Za-z0-9_-][A-Za-z0-9_.-]*/)+"
        if (not isinstance(key, str) or len(key) > 200
                or not (re.fullmatch(prefix, key) or re.fullmatch(r"[A-Za-z0-9_.:-]+(?:/[A-Za-z0-9_.:-]+)*", key))):
            raise BrokerError("identity_memberships keys must be identities or prefixes ending in /")
        if not isinstance(rules, list) or len(rules) > 100:
            raise BrokerError("identity_memberships values must be lists of at most 100 rules")
        desired = {}
        for rule in rules:
            if not isinstance(rule, dict) or len(rule) != 2 or "role" not in rule:
                raise BrokerError("membership rules need exactly one of project/group plus role")
            kind = next((k for k in rule if k != "role"), None)
            if kind not in MEMBER_KINDS or not membership_path(kind, rule[kind]):
                raise BrokerError("membership rules need a valid project or group path")
            if rule["role"] not in MEMBER_ROLES:
                raise BrokerError("membership role must be guest, reporter, developer or maintainer")
            target = (kind, rule[kind])
            if target in desired:
                raise BrokerError("duplicate membership target for one identity")
            desired[target] = MEMBER_ROLES[rule["role"]]
        parsed[key] = desired
    return parsed


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        raise BrokerError("GitLab redirects are not allowed")


def private_file(path):
    p = Path(path)
    if p.is_symlink() or not p.is_file() or p.stat().st_mode & 0o077:
        raise BrokerError("secret files must be regular files with mode 0600")
    return p.read_text().strip()


class Broker:
    def __init__(self, config):
        self.config = config
        self.origin = config["gitlab_url"].rstrip("/")
        url = urlsplit(self.origin)
        if url.scheme != "https" or not url.hostname or url.username or url.password or url.path or url.query or url.fragment:
            raise BrokerError("gitlab_url must be an HTTPS origin")
        self.host = url.netloc
        self.admin_token = private_file(config["gitlab_token_file"])
        self.clients = config.get("clients", {})
        if any(not re.fullmatch(r"[A-Za-z0-9_.:/-]{1,200}", identity)
                                   or not re.fullmatch(r"[0-9a-f]{64}", digest)
                                   for identity, digest in self.clients.items()):
            raise BrokerError("clients must map agent identities to SHA256 bearer digests")
        self.enrollment_clients = config.get("enrollment_clients", {})
        if not isinstance(self.enrollment_clients, dict):
            raise BrokerError("invalid enrollment clients")
        for name, policy in self.enrollment_clients.items():
            if (not isinstance(policy, dict) or set(policy) != {"sha256", "prefix", "profile"}
                    or not re.fullmatch(r"[A-Za-z0-9_.-]+", name)
                    or not re.fullmatch(r"[0-9a-f]{64}", policy.get("sha256", ""))
                    or not re.fullmatch(r"[A-Za-z0-9_-][A-Za-z0-9_.-]*/", policy.get("prefix", ""))
                    or policy.get("profile") not in PROFILES):
                raise BrokerError("invalid enrollment policy")
        if not self.clients and not self.enrollment_clients:
            raise BrokerError("at least one client or enrollment policy is required")
        self.days = config.get("token_days", 30)
        if not isinstance(self.days, int) or not 2 <= self.days <= 365:
            raise BrokerError("token_days must be 2..365")
        self.scopes = config.get("scopes", ["api"])
        if not isinstance(self.scopes, list) or not self.scopes or any(not isinstance(s, str) for s in self.scopes):
            raise BrokerError("scopes must be a nonempty string list")
        self.identity_profiles = config.get("identity_profiles", {})
        if not isinstance(self.identity_profiles, dict) or any(
                identity not in self.clients or profile not in PROFILES
                for identity, profile in self.identity_profiles.items()):
            raise BrokerError("identity_profiles must assign known clients to observer or developer")
        self.ssh_hosts = config.get("ssh_hosts", [])
        if not isinstance(self.ssh_hosts, list) or any(
                not isinstance(host, str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9.-]{0,252}", host)
                for host in self.ssh_hosts):
            raise BrokerError("ssh_hosts must contain hostnames, without users, ports or paths")
        self.identity_memberships = parse_memberships(config.get("identity_memberships", {}))
        self.reconciling = set()
        self.reconciling_lock = threading.Lock()
        group = config.get("group_id")
        if group is not None and (type(group) is not int or group <= 0):
            raise BrokerError("group_id must be a positive integer")
        self.base = f"/groups/{group}/service_accounts" if group else "/service_accounts"
        directory = Path(config["state_dir"]).resolve()
        directory.mkdir(mode=0o700, parents=True, exist_ok=True)
        if directory.stat().st_mode & 0o077:
            raise BrokerError("state_dir must have mode 0700")
        self.db = directory / "accounts.sqlite3"
        if self.db.is_symlink():
            raise BrokerError("database must not be a symlink")
        fd = os.open(self.db, os.O_CREAT | os.O_WRONLY, 0o600)
        os.close(fd)
        if self.db.stat().st_mode & 0o077:
            raise BrokerError("database must have mode 0600")
        with self.connect() as db:
            db.execute("CREATE TABLE IF NOT EXISTS enrolled_clients (identity TEXT PRIMARY KEY, digest TEXT NOT NULL, owner TEXT NOT NULL)")
            db.execute("CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL)")
            # Never reuse credentials if an operator points an existing DB at another issuer.
            issuer = json.dumps([self.origin, self.base])
            db.execute("INSERT OR IGNORE INTO metadata VALUES ('issuer', ?)", (issuer,))
            saved = json.loads(db.execute("SELECT value FROM metadata WHERE key='issuer'").fetchone()[0])
            if saved[:2] != [self.origin, self.base]:
                raise BrokerError("database issuer differs from configuration")
            db.execute("""CREATE TABLE IF NOT EXISTS accounts (
                identity TEXT PRIMARY KEY, username TEXT NOT NULL, user_id INTEGER,
                token TEXT, expires_at TEXT, state TEXT NOT NULL)""")
            if "token_scopes" not in {row[1] for row in db.execute("PRAGMA table_info(accounts)")}:
                db.execute("ALTER TABLE accounts ADD COLUMN token_scopes TEXT")
                # Legacy databases stored scopes in issuer metadata, not individual rows.
                if len(saved) == 3:
                    db.execute("UPDATE accounts SET token_scopes=?", (json.dumps(sorted(saved[2])),))
            # Memberships this broker granted. Rows are the only authority to change or remove
            # a membership: confirmed values were read back or returned by GitLab; pending
            # values were committed before a write whose outcome may be unknown.
            db.execute("""CREATE TABLE IF NOT EXISTS memberships (
                identity TEXT NOT NULL, kind TEXT NOT NULL, path TEXT NOT NULL, user_id INTEGER NOT NULL,
                access_level INTEGER, expires_at TEXT, pending_access_level INTEGER, pending_expires_at TEXT,
                PRIMARY KEY (identity, kind, path))""")
            db.execute("UPDATE metadata SET value=? WHERE key='issuer'", (issuer,))

    @contextmanager
    def connect(self):
        db = sqlite3.connect(self.db, timeout=35)
        try:
            with db:
                yield db
        finally:
            db.close()

    def authorized(self, identity, bearer):
        digest = self.clients.get(identity, "")
        if not digest:
            with self.connect() as db:
                row = db.execute("SELECT digest,owner FROM enrolled_clients WHERE identity=?", (identity,)).fetchone()
            if row and self.enrollment_policy(row[1], identity):
                digest = row[0]
        return bool(digest) and hmac.compare_digest(digest, hashlib.sha256(bearer.encode()).hexdigest())

    def enrollment_policy(self, owner, identity):
        policy = self.enrollment_clients.get(owner)
        return policy if policy and identity.startswith(policy["prefix"]) else None

    def enroll(self, identity, bearer, client_token):
        if (not isinstance(identity, str) or len(identity) > 200
                or not re.fullmatch(r"[A-Za-z0-9_-][A-Za-z0-9_.-]*/[A-Za-z0-9_-][A-Za-z0-9_.-]*/[A-Za-z0-9_-][A-Za-z0-9_.-]*", identity)
                or not isinstance(client_token, str) or not re.fullmatch(r"[A-Za-z0-9_-]{43,128}", client_token)):
            return False
        owner = next((name for name, policy in self.enrollment_clients.items()
                      if self.enrollment_policy(name, identity) and hmac.compare_digest(
                          policy["sha256"], hashlib.sha256(bearer.encode()).hexdigest())), None)
        if owner is None or identity in self.clients:
            return False
        digest = hashlib.sha256(client_token.encode()).hexdigest()
        with self.connect() as db:
            db.execute("BEGIN IMMEDIATE")
            row = db.execute("SELECT digest,owner FROM enrolled_clients WHERE identity=?", (identity,)).fetchone()
            if row:
                return row[1] == owner and hmac.compare_digest(row[0], digest)
            db.execute("INSERT INTO enrolled_clients VALUES (?,?,?)", (identity, digest, owner))
        return True

    def api(self, path, data):
        request = Request(self.origin + "/api/v4" + path,
                          data=json.dumps(data).encode(), method="POST",
                          headers={"PRIVATE-TOKEN": self.admin_token, "Content-Type": "application/json",
                                   "User-Agent": "gitlab-service-account-broker/0.1"})
        try:
            with build_opener(NoRedirect()).open(request, timeout=15) as response:
                raw = response.read(65537)
                if len(raw) > 65536:
                    raise ValueError("response too large")
                return json.loads(raw)
        except Exception:
            # Upstream bodies and URLs can include secrets. Never return them to callers.
            raise BrokerError("GitLab provisioning failed; operator reconciliation required") from None

    def member_api(self, method, path, data=None, timeout=MEMBERSHIP_CALL_TIMEOUT):
        """GitLab members API call. Returns (status, json-or-None); never exposes bodies."""
        request = Request(self.origin + "/api/v4" + path,
                          data=None if data is None else json.dumps(data).encode(), method=method,
                          headers={"PRIVATE-TOKEN": self.admin_token, "Content-Type": "application/json",
                                   "User-Agent": "gitlab-service-account-broker/0.1"})
        try:
            with build_opener(NoRedirect()).open(request, timeout=timeout) as response:
                status, raw = response.status, response.read(65537)
        except HTTPError as error:
            status = error.code
            error.close()
            return status, None
        except Exception:
            raise BrokerError("GitLab membership request failed") from None
        if len(raw) > 65536:
            raise BrokerError("GitLab membership response too large")
        try:
            return status, json.loads(raw) if raw.strip() else None
        except ValueError:
            raise BrokerError("invalid GitLab membership response") from None

    def desired_memberships(self, identity):
        """An exact identity key beats any prefix; otherwise the longest matching prefix wins."""
        if identity in self.identity_memberships:
            return self.identity_memberships[identity]
        prefixes = [key for key in self.identity_memberships if key.endswith("/") and identity.startswith(key)]
        return self.identity_memberships[max(prefixes, key=len)] if prefixes else {}

    def reconcile_memberships(self, identity, user_id, expiry):
        """Best effort: never raises. Failures are logged without content and retried next issuance."""
        with self.reconciling_lock:
            if identity in self.reconciling:
                return  # A concurrent issuance is already reconciling this identity.
            self.reconciling.add(identity)
        try:
            desired = self.desired_memberships(identity)
            with self.connect() as db:
                rows = db.execute("""SELECT kind,path,user_id,access_level,expires_at,pending_access_level,pending_expires_at
                                     FROM memberships WHERE identity=?""", (identity,)).fetchall()
            records = {(r[0], r[1]): r[2:] for r in rows}
            deadline = time.monotonic() + MEMBERSHIP_BUDGET
            for target in sorted(set(desired) | set(records)):
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    log.warning("membership reconciliation for %s deferred: time budget exhausted", identity)
                    return
                try:
                    self.reconcile_membership(identity, user_id, expiry, target, desired.get(target),
                                              records.get(target), min(MEMBERSHIP_CALL_TIMEOUT, remaining))
                except BrokerError as error:
                    # Messages are fixed strings or HTTP status codes, never GitLab bodies or tokens.
                    log.warning("membership reconciliation for %s on %s %s failed: %s; retrying on next issuance",
                                identity, target[0], target[1], str(error)[:120])
        except Exception:
            log.warning("membership reconciliation for %s failed unexpectedly; retrying on next issuance", identity)
        finally:
            with self.reconciling_lock:
                self.reconciling.discard(identity)

    def reconcile_membership(self, identity, user_id, expiry, target, level, record, timeout):
        kind, path = target
        members = f"/{MEMBER_KINDS[kind]}/{quote(path, safe='')}/members"
        def save(confirmed=None, pending=None):
            with self.connect() as db:
                db.execute("""INSERT INTO memberships VALUES (?,?,?,?,?,?,?,?)
                              ON CONFLICT(identity,kind,path) DO UPDATE SET user_id=excluded.user_id,
                              access_level=excluded.access_level, expires_at=excluded.expires_at,
                              pending_access_level=excluded.pending_access_level,
                              pending_expires_at=excluded.pending_expires_at""",
                           (identity, kind, path, user_id, *(confirmed or (None, None)), *(pending or (None, None))))
        def forget():
            with self.connect() as db:
                db.execute("DELETE FROM memberships WHERE identity=? AND kind=? AND path=?", (identity, kind, path))
        def check(status, ok, action):
            if status not in ok:
                raise BrokerError(f"{action} returned HTTP {status}")
        status, member = self.member_api("GET", f"{members}/{user_id}", timeout=timeout)
        check(status, {200, 404}, "membership lookup")
        current = None
        if status == 200:
            if not isinstance(member, dict) or member.get("id") != user_id or type(member.get("access_level")) is not int:
                raise BrokerError("invalid GitLab membership response")
            current = (member["access_level"], member.get("expires_at"))
        # Broker-owned only if GitLab still shows exactly what the broker set (or was setting).
        owned = (record is not None and record[0] == user_id and current is not None
                 and current in {tuple(record[1:3]), tuple(record[3:5])})
        want = (level, expiry)
        if level is None:
            if current is not None and owned:
                status, _ = self.member_api("DELETE", f"{members}/{user_id}", timeout=timeout)
                check(status, {200, 202, 204, 404}, "membership removal")
            forget()  # Absent or human-managed memberships are left alone and no longer tracked.
            return
        if current is None:
            # Commit the intent first so a lost response still leaves the grant recognisable.
            save(confirmed=None, pending=want)
            status, _ = self.member_api("POST", members, {"user_id": user_id, "access_level": level, "expires_at": expiry},
                                        timeout=timeout)
            if 400 <= status < 500:
                forget()  # GitLab conclusively refused: nothing was granted, so claim nothing.
            check(status, {200, 201}, "membership grant")
            save(confirmed=want)
        elif owned:
            if current != want:
                save(confirmed=current, pending=want)
                status, _ = self.member_api("PUT", f"{members}/{user_id}", {"access_level": level, "expires_at": expiry},
                                            timeout=timeout)
                if 400 <= status < 500:
                    save(confirmed=current)  # Conclusively unchanged; keep tracking what GitLab shows.
                check(status, {200}, "membership update")
            save(confirmed=want)
        else:
            if record is not None:
                forget()  # Someone else changed it: it is no longer the broker's to manage.
            log.info("membership for %s on %s %s is externally managed; left unchanged", identity, kind, path)

    def credentials(self, identity):
        result = self.issue(identity)
        self.reconcile_memberships(identity, result["user_id"], result["expires_at"])
        return result

    def issue(self, identity):
        profile = self.identity_profiles.get(identity)
        if identity not in self.clients:
            with self.connect() as db:
                row = db.execute("SELECT owner FROM enrolled_clients WHERE identity=?", (identity,)).fetchone()
            policy = self.enrollment_policy(row[0], identity) if row else None
            if not policy:
                raise BrokerError("enrollment policy unavailable")
            profile = policy["profile"]
        scopes = sorted(PROFILES[profile] if profile else self.scopes)
        def result(username, user_id, token, expiry):
            return dict(identity=identity, username=username, user_id=user_id, token=token,
                        expires_at=expiry, host=self.host, scopes=scopes,
                        profile=profile or "custom", ssh_hosts=self.ssh_hosts)
        with self.connect() as db:
            db.execute("BEGIN IMMEDIATE")
            row = db.execute("SELECT username,user_id,token,expires_at,state,token_scopes FROM accounts WHERE identity=?", (identity,)).fetchone()
            today = dt.datetime.now(dt.timezone.utc).date()
            if row and row[4] == "ready" and row[5] == json.dumps(scopes) and dt.date.fromisoformat(row[3]) > today + dt.timedelta(days=1):
                return result(row[0], row[1], row[2], row[3])
            if row and row[4] != "ready":
                raise BrokerError("provisioning pending; operator reconciliation required")
            username = row[0] if row else "hrx_" + hashlib.sha256(identity.encode()).hexdigest()[:32]
            user_id = row[1] if row else None
            db.execute("INSERT INTO accounts(identity,username,state) VALUES (?,?,'pending') ON CONFLICT(identity) DO UPDATE SET state='pending'", (identity, username))
            # Commit the intent before network I/O: a crash or ambiguous POST never causes duplicate minting.
            db.commit()
            if user_id is None:
                account = self.api(self.base, {"username": username, "name": "HRX " + identity})
                user_id = account.get("id")
                if type(user_id) is not int or user_id <= 0 or account.get("username") != username:
                    raise BrokerError("invalid GitLab account response; reconciliation required")
                db.execute("UPDATE accounts SET user_id=? WHERE identity=?", (user_id, identity))
                db.commit()
            endpoint = (self.base + f"/{user_id}/personal_access_tokens" if self.config.get("group_id")
                        else f"/users/{user_id}/personal_access_tokens")
            expiry = (today + dt.timedelta(days=self.days)).isoformat()
            token = self.api(endpoint, {"name": "hrx-broker", "scopes": scopes, "expires_at": expiry})
            if not isinstance(token.get("token"), str) or not token["token"] or token.get("user_id") != user_id or token.get("expires_at") != expiry:
                raise BrokerError("invalid GitLab token response; reconciliation required")
            db.execute("UPDATE accounts SET token=?,expires_at=?,token_scopes=?,state='ready' WHERE identity=?", (token["token"], expiry, json.dumps(scopes), identity))
            db.commit()
            return result(username, user_id, token["token"], expiry)


class Handler(BaseHTTPRequestHandler):
    def setup(self):
        super().setup()
        self.connection.settimeout(10)

    def log_message(self, *args):
        pass

    def reply(self, status, body):
        raw = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Cache-Control", "no-store")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_POST(self):
        if self.path not in {"/v1/credentials", "/v1/enroll"}:
            return self.reply(404, {"error": "not found"})
        try:
            length = int(self.headers.get("Content-Length", "0"))
            if not 0 < length <= 4096 or self.headers.get("Transfer-Encoding"):
                return self.reply(400, {"error": "invalid request"})
            data = json.loads(self.rfile.read(length))
            if self.path == "/v1/enroll":
                auth = self.headers.get("Authorization", "")
                if not isinstance(data, dict) or set(data) != {"identity", "client_token"}:
                    return self.reply(400, {"error": "invalid enrollment request"})
                if not auth.startswith("Bearer ") or not self.server.broker.enroll(data["identity"], auth[7:], data["client_token"]):
                    return self.reply(403, {"error": "enrollment forbidden or identity already owned"})
                return self.reply(200, {"identity": data["identity"], "enrolled": True})
            if not isinstance(data, dict) or set(data) != {"identity"}:
                return self.reply(400, {"error": "only identity may be requested; access policy is operator controlled"})
            identity = data["identity"]
            auth = self.headers.get("Authorization", "")
            if not isinstance(identity, str) or not auth.startswith("Bearer ") or not self.server.broker.authorized(identity, auth[7:]):
                return self.reply(403, {"error": "forbidden"})
            result = self.server.broker.credentials(identity)
        except (ValueError, KeyError, TypeError):
            return self.reply(400, {"error": "invalid request"})
        except Exception:
            return self.reply(503, {"error": "provisioning unavailable; contact broker operator"})
        self.reply(200, result)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", required=True, help="private JSON configuration file")
    parser.add_argument("--port", type=int, default=8743)
    args = parser.parse_args()
    os.umask(0o077)
    try:
        logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
        broker = Broker(json.loads(private_file(args.config)))
        server = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
        server.broker = broker
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    except Exception:
        parser.exit(1, "broker startup failed; check private configuration and database\n")


if __name__ == "__main__":
    main()

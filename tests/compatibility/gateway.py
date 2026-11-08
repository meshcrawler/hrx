"""Private TLS inference relay: count every request, reject route substitution.

Only evidence counters leave this process. Credentials and model payloads are
never written to compatibility artifacts. The upstream dedicated key supplies
the aggregate monetary budget; this relay adds hard per-run admission limits.
"""
from datetime import datetime, timezone
from pathlib import Path
import hashlib
import http.server
import json
import secrets
import select
import socket
import ssl
import subprocess
import threading
import time
import urllib.error
import urllib.request


def tls_material(root):
    """Use a CA only for trust, never as the TLS server's end-entity certificate."""
    ca, ca_key = root / "ca.pem", root / "ca-key.pem"
    cert, key = root / "cert.pem", root / "key.pem"
    csr, extensions = root / "server.csr", root / "server.ext"

    def openssl(*args):
        subprocess.run(["openssl", *map(str, args)], check=True, timeout=30,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

    openssl("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
            "-subj", "/CN=HRX fixture CA", "-addext", "basicConstraints=critical,CA:TRUE",
            "-addext", "keyUsage=critical,keyCertSign,cRLSign", "-addext", "subjectKeyIdentifier=hash",
            "-keyout", ca_key, "-out", ca)
    ca_key.chmod(0o600)
    openssl("req", "-new", "-newkey", "rsa:2048", "-nodes", "-subj", "/CN=localhost",
            "-keyout", key, "-out", csr)
    key.chmod(0o600)
    extensions.write_text("basicConstraints=critical,CA:FALSE\n"
                          "keyUsage=critical,digitalSignature,keyEncipherment\n"
                          "extendedKeyUsage=serverAuth\nsubjectAltName=IP:127.0.0.1\n"
                          # Explicit: OpenSSL 3 adds key identifiers by default, LibreSSL does
                          # not, and Python 3.13 strict verification requires them.
                          "subjectKeyIdentifier=hash\nauthorityKeyIdentifier=keyid,issuer\n")
    openssl("x509", "-req", "-in", csr, "-CA", ca, "-CAkey", ca_key, "-CAcreateserial",
            "-days", "1", "-sha256", "-extfile", extensions, "-out", cert)
    return ca, cert, key


def tool_results(value):
    if isinstance(value, dict):
        own = int(value.get("role") == "tool" or value.get("type") in
                  ("function_call_output", "tool_result"))
        return own + sum(tool_results(v) for v in value.values())
    if isinstance(value, list):
        return sum(tool_results(v) for v in value)
    return 0


def tool_result_hashes(value):
    if isinstance(value, dict):
        own = ([hashlib.sha256(json.dumps(value, sort_keys=True).encode()).hexdigest()]
               if value.get("role") == "tool" or value.get("type") in
               ("function_call_output", "tool_result") else [])
        return own + [item for child in value.values() for item in tool_result_hashes(child)]
    if isinstance(value, list):
        return [item for child in value for item in tool_result_hashes(child)]
    return []


def load_claude_lease(path, upstream):
    source = Path(path)
    if source.stat().st_mode & 0o077:
        raise ValueError("claude_lease_file_not_private")
    try:
        lease = json.loads(source.read_text())
        expires = datetime.fromisoformat(lease["expires_at"].replace("Z", "+00:00"))
        credentials = lease["credentials"]
        models = lease["models"]
        token = credentials["claude_code_oauth_token"]
        if (expires.tzinfo is None or expires <= datetime.now(timezone.utc) or
                credentials["profile"] != "claude-code" or
                lease["gateway"]["origin"].rstrip("/") != upstream.rstrip("/") or
                not isinstance(models, list) or not models or
                not all(isinstance(model, str) and model.startswith("claude-") for model in models) or
                not isinstance(token, str) or not token or any(c.isspace() for c in token)):
            raise ValueError()
    except (ValueError, TypeError, KeyError):
        raise ValueError("claude_lease_invalid_expired_or_wrong_scope") from None
    # Deliberately discard the lease's separate gateway key and all metadata.
    return {"token": token, "models": set(models), "expires_at": expires.timestamp()}


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


class Gateway:
    def __init__(self, root, upstream, key, catalog, policy, claude_lease=None):
        self.upstream, self.key, self.catalog = upstream.rstrip("/"), key, catalog
        self.policy = policy
        self.claude_lease = claude_lease
        self.claude_routes = {source.get("gateway_route")
                              for model in catalog.get("models", []) for source in model.get("sources", [])
                              if source.get("provider") == "anthropic"}
        self.rejected_routes = {}
        self.secret = secrets.token_hex(32)
        self.lock = threading.Condition()
        self.admitted = 0
        self.waiting = 0
        self.closed = False
        self.requests = []
        self.deadline = time.monotonic() + policy["wall_seconds"]
        self.active = 0
        self.allowed_routes = set(policy["routes"])
        self.http = urllib.request.build_opener(NoRedirect())
        owner = self

        class Handler(http.server.BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.0"

            def log_message(self, *args):
                pass

            def reply(self, status, data):
                raw = json.dumps(data).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def authenticated(self):
                return (self.headers.get("Authorization") == "Bearer " + owner.secret or
                        self.headers.get("x-api-key") == owner.secret or
                        self.headers.get("x-litellm-api-key") == "Bearer " + owner.secret)

            def do_GET(self):
                if self.path == "/dashboard/models.json":
                    self.reply(200, owner.catalog)
                elif self.path == "/v1/models" and self.authenticated():
                    self.reply(200, {"data": [{"id": route} for route in owner.policy["routes"]]})
                else:
                    self.reply(403, {"error": "unavailable fixture route"})

            def do_POST(self):
                if not self.authenticated():
                    self.reply(401, {"error": "invalid fixture credential"})
                    return
                if self.path not in ("/v1/responses", "/v1/chat/completions", "/v1/messages", "/v1/messages?beta=true"):
                    self.reply(403, {"error": "unapproved inference endpoint"})
                    return
                try:
                    size = int(self.headers.get("Content-Length", "0"))
                    if not 0 < size <= owner.policy["max_request_bytes"]:
                        raise ValueError("request size")
                    data = json.loads(self.rfile.read(size))
                    route = data["model"]
                    if route not in owner.allowed_routes:
                        raise ValueError("unapproved model")
                except (ValueError, KeyError, TypeError):
                    self.reply(400, {"error": "invalid bounded inference request"})
                    return
                queued_at = time.monotonic()
                request_deadline = min(owner.deadline, queued_at + owner.policy["request_seconds"])
                event = {"route": route, "protocol": self.path.split("?")[0],
                         "tool_results": tool_results(data),
                         "tool_result_hashes": tool_result_hashes(data), "http_status": None}
                with owner.lock:
                    owner.requests.append(event)
                    if route in owner.rejected_routes:
                        event["http_status"] = owner.rejected_routes[route]
                        event["provider_circuit_open"] = True
                        self.reply(event["http_status"], {"error": "provider route rejected earlier in this run"})
                        return
                    if (self.path.startswith("/v1/messages") and route in owner.claude_routes and
                            (owner.claude_lease is None or route not in owner.claude_lease["models"] or
                             time.time() >= owner.claude_lease["expires_at"])):
                        event.update(local_limit="claude_lease_scope_or_expiry", http_status=403)
                        self.reply(403, {"error": "Claude lease unavailable for this route"})
                        return
                    if owner.waiting >= owner.policy.get("max_queued_requests", 16):
                        event.update(local_limit="queue_capacity", http_status=429)
                        self.reply(429, {"error": {"type": "hrx_ci_limit", "code": "queue_capacity"}})
                        return
                    owner.waiting += 1
                    while owner.active >= owner.policy["concurrency"]:
                        remaining = request_deadline - time.monotonic()
                        if remaining <= 0 or owner.closed:
                            break
                        owner.lock.wait(min(remaining, .1))
                        # HTTP/1.0 has no subsequent request body here. Detect a
                        # waiting harness that exited before spending its key.
                        if select.select([self.connection], [], [], 0)[0]:
                            try:
                                self.connection.settimeout(.01)
                                disconnected = not self.connection.recv(1)
                            except TimeoutError:
                                disconnected = False
                            except OSError:
                                disconnected = True
                            if disconnected:
                                owner.waiting -= 1
                                event["local_limit"] = "client_disconnected"
                                return
                    owner.waiting -= 1
                    event["queue_seconds"] = round(time.monotonic() - queued_at, 6)
                    reason = ("gateway_closed" if owner.closed else
                              "request_budget" if owner.admitted >= owner.policy["max_requests"] else
                              "admission_deadline" if time.monotonic() >= request_deadline else None)
                    if reason:
                        event["local_limit"] = reason
                        event["http_status"] = 429
                        self.reply(429, {"error": {"type": "hrx_ci_limit", "code": reason}})
                        return
                    owner.admitted += 1
                    owner.active += 1
                self.connection.settimeout(max(.1, request_deadline - time.monotonic()))
                try:
                    limit = owner.policy["max_output_tokens"]
                    limit_key = "max_output_tokens" if self.path == "/v1/responses" else "max_tokens"
                    data[limit_key] = min(data.get(limit_key) or limit, limit)
                    if "max_completion_tokens" in data:
                        data["max_completion_tokens"] = min(data["max_completion_tokens"], limit)
                    headers = {"Authorization": "Bearer " + owner.key,
                               "Content-Type": "application/json", "User-Agent": "hrx-compatibility-ci"}
                    if self.path.startswith("/v1/messages"):
                        # LiteLLM's Messages gateway uses its own key header;
                        # never forward the private relay authentication value.
                        headers["x-litellm-api-key"] = "Bearer " + owner.key
                        if owner.claude_lease is not None and route in owner.claude_routes:
                            if time.time() >= owner.claude_lease["expires_at"]:
                                event.update(local_limit="claude_lease_scope_or_expiry", http_status=403)
                                self.reply(403, {"error": "Claude lease expired during admission"})
                                return
                            headers["Authorization"] = "Bearer " + owner.claude_lease["token"]
                    for name in ("anthropic-version", "anthropic-beta"):
                        if self.headers.get(name):
                            headers[name] = self.headers[name]
                    request = urllib.request.Request(owner.upstream + self.path,
                                                     data=json.dumps(data).encode(), headers=headers)
                    with owner.http.open(request, timeout=max(.1, request_deadline - time.monotonic())) as response:
                        # A socket inactivity timeout alone lets trickling SSE
                        # exceed its budget. Interrupt the upstream socket at
                        # the absolute deadline, including a stalled read.
                        upstream_socket = response.fp.raw._sock

                        def interrupt():
                            event["transport_error"] = True
                            try:
                                upstream_socket.shutdown(socket.SHUT_RDWR)
                            except OSError:
                                pass

                        timer = threading.Timer(max(0, request_deadline - time.monotonic()), interrupt)
                        timer.daemon = True
                        timer.start()
                        event["http_status"] = response.status
                        self.send_response(response.status)
                        self.send_header("Content-Type", response.headers.get("Content-Type", "application/json"))
                        self.end_headers()
                        response_bytes = 0
                        try:
                            while True:
                                remaining = request_deadline - time.monotonic()
                                if remaining <= 0:
                                    raise TimeoutError("absolute inference deadline")
                                self.connection.settimeout(remaining)
                                chunk = response.read1(65536)
                                if not chunk:
                                    break
                                response_bytes += len(chunk)
                                if response_bytes > 16 * 1024 * 1024:
                                    raise OSError("inference response limit")
                                self.wfile.write(chunk)
                                self.wfile.flush()
                        finally:
                            timer.cancel()
                except urllib.error.HTTPError as exc:
                    event["http_status"] = exc.code
                    if exc.code in (401, 429):
                        with owner.lock:
                            owner.rejected_routes[route] = exc.code
                    self.reply(exc.code, {"error": "upstream inference rejected request"})
                except (OSError, TimeoutError):
                    event["transport_error"] = True
                finally:
                    with owner.lock:
                        owner.active -= 1
                        owner.lock.notify_all()

        ca, cert, key_path = tls_material(root)
        self.cert = ca
        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(cert, key_path)
        self.server.socket = context.wrap_socket(self.server.socket, server_side=True)
        self.url = f"https://127.0.0.1:{self.server.server_port}"
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def close(self):
        with self.lock:
            self.closed = True
            self.lock.notify_all()
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(5)

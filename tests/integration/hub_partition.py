"""Loopback-only test proxy that can blackhole one fixture host's requests."""

import http.server
import ssl
import threading
import urllib.error
import urllib.request


class PartitionProxy:
    def __init__(self, origin, certificate):
        self.blocked = threading.Event()
        self.recovered = threading.Event()
        self.recovered.set()
        context = ssl.create_default_context(cafile=str(certificate))
        proxy = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                self.forward()

            def do_POST(self):
                self.forward()

            def forward(self):
                length = int(self.headers.get("Content-Length", "0"))
                if length > 1024 * 1024:
                    self.send_error(413)
                    return
                body = self.rfile.read(length) if length else None
                if proxy.blocked.is_set():
                    proxy.recovered.wait(timeout=60)
                    self.close_connection = True
                    return
                request = urllib.request.Request(
                    origin.rstrip("/") + self.path,
                    data=body,
                    method=self.command,
                    headers={
                        "Authorization": self.headers.get("Authorization", ""),
                        "Content-Type": "application/json",
                    },
                )
                try:
                    response = urllib.request.urlopen(
                        request, context=context, timeout=5
                    )
                except urllib.error.HTTPError as error:
                    response = error
                with response:
                    data = response.read(4 * 1024 * 1024 + 1)
                    self.send_response(response.status)
                    self.send_header("Content-Type", "application/json")
                    self.send_header("Content-Length", str(len(data)))
                    self.end_headers()
                    try:
                        self.wfile.write(data)
                    except (BrokenPipeError, ConnectionResetError):
                        pass

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.url = f"http://127.0.0.1:{self.server.server_port}"
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def partition(self):
        self.recovered.clear()
        self.blocked.set()

    def heal(self):
        self.blocked.clear()
        self.recovered.set()

    def close(self):
        self.heal()
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)

"""Tiny HTTP server for the depper basic example.

This file runs on the deploy host, not on your machine, so the host needs
python3. It serves a greeting on / and a JSON status document on /health.
"""

import json
import os
import time
from http.server import BaseHTTPRequestHandler, HTTPServer

STARTED = time.time()
GREETING = os.environ.get("GREETING", "hello from depper")


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):  # noqa: N802 - name required by BaseHTTPRequestHandler
        if self.path == "/health":
            body = json.dumps(
                {"state": "running", "uptime_seconds": round(time.time() - STARTED, 1)}
            ).encode()
            content_type = "application/json"
        else:
            body = (GREETING + "\n").encode()
            content_type = "text/plain; charset=utf-8"
        self.send_response(200)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, format, *args):
        print(format % args, flush=True)


host = os.environ.get("HOST", "127.0.0.1")
port = int(os.environ.get("PORT", "8080"))
HTTPServer((host, port), Handler).serve_forever()

"""Tiny HTTP server for the depper container example (runs inside the image)."""

import json
import os
import time
from http.server import BaseHTTPRequestHandler, HTTPServer

STARTED = time.time()
GREETING = os.environ.get("GREETING", "hello from a container")
TOKEN_SET = bool(os.environ.get("API_TOKEN"))


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):  # noqa: N802 - name required by BaseHTTPRequestHandler
        if self.path == "/health":
            body = json.dumps(
                {
                    "state": "running",
                    "uptime_seconds": round(time.time() - STARTED, 1),
                    "api_token_configured": TOKEN_SET,
                }
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


HTTPServer(("0.0.0.0", 8080), Handler).serve_forever()

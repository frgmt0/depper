"""Tiny HTTP server used by the deployer quick-start example."""

from http.server import BaseHTTPRequestHandler, HTTPServer
import os


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):  # noqa: N802 - required by BaseHTTPRequestHandler
        body = b"deployer is running\n"
        self.send_response(200)
        self.send_header("Content-Type", "text/plain; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, format, *args):
        print(format % args, flush=True)


port = int(os.environ.get("PORT", "8080"))
HTTPServer(("0.0.0.0", port), Handler).serve_forever()

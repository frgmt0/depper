from http.server import BaseHTTPRequestHandler, HTTPServer
import os
class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        body = (os.environ['DEPLOYER_SMOKE_VALUE'] + '\n').encode()
        self.send_response(200)
        self.end_headers()
        self.wfile.write(body)
HTTPServer(('127.0.0.1', 18769), Handler).serve_forever()

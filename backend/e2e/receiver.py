#!/usr/bin/env python3
"""Minimal event target for the end-to-end test: stores every request body."""

import http.server
import sys


class Handler(http.server.BaseHTTPRequestHandler):
    def _store(self):
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length)
        with open(sys.argv[2], "ab") as f:
            f.write(body + b"\n")
        self.send_response(200)
        self.send_header("Content-Length", "2")
        self.end_headers()
        self.wfile.write(b"ok")

    do_POST = _store
    do_PUT = _store

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    server = http.server.ThreadingHTTPServer(("0.0.0.0", int(sys.argv[1])), Handler)
    server.serve_forever()

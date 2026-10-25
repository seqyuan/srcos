#!/usr/bin/env python3
"""Minimal static file server for the scrna-demo UI.

SRCOS assigns the port (SRCOS_PORT) and proxies to it. The service must bind
loopback only; the gateway is the only entry point.
"""
import os
import sys
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))


class Handler(SimpleHTTPRequestHandler):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, directory=HERE, **kwargs)

    def end_headers(self):
        # The page's own state lives in the DOM; a cached app.js after a tool
        # update would be a confusing "why is nothing different" bug.
        self.send_header("Cache-Control", "no-store")
        super().end_headers()

    def log_message(self, fmt, *args):
        print("[ui] " + (fmt % args), flush=True)


def main() -> int:
    raw = os.environ.get("SRCOS_PORT")
    if not raw:
        print("SRCOS_PORT is not set (this is a service tool)", file=sys.stderr)
        return 2
    port = int(raw)
    server = ThreadingHTTPServer(("127.0.0.1", port), Handler)
    print(f"[ui] listening on 127.0.0.1:{port} (root {HERE})", flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()
    return 0


if __name__ == "__main__":
    sys.exit(main())

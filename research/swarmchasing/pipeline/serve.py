#!/usr/bin/env python3
"""Tiny stdlib static server for web/, plus an /api/* proxy to swarmmemo.com (incl. SSE).

swarmmemo.com sends Access-Control-Allow-Origin: *, so the page talks to it directly by default.
The proxy is a fallback for networks that block cross-origin requests: open /?api=same-origin-url,
e.g. http://localhost:8765/?api=http://localhost:8765
Usage: python3 serve.py [port]
"""
import http.server, os, sys, urllib.request

UPSTREAM = "https://swarmmemo.com"
ROOT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "web")


class H(http.server.SimpleHTTPRequestHandler):
    def __init__(self, *a, **k):
        super().__init__(*a, directory=ROOT, **k)

    def do_GET(self):
        if not self.path.startswith("/api/"):
            return super().do_GET()
        req = urllib.request.Request(UPSTREAM + self.path, headers={"User-Agent": "swarmgraph-proxy/0.1", "Accept": self.headers.get("Accept", "*/*")})
        try:
            up = urllib.request.urlopen(req, timeout=None if self.path.startswith("/api/stream") else 30)
        except urllib.error.HTTPError as e:
            up = e
        self.send_response(up.status if hasattr(up, "status") else up.code)
        for h in ("Content-Type", "Cache-Control", "X-Next-Cursor", "Retry-After"):
            if up.headers.get(h):
                self.send_header(h, up.headers[h])
        self.end_headers()
        try:
            while chunk := up.read1(8192) if hasattr(up, "read1") else up.read(8192):
                self.wfile.write(chunk); self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            pass


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8765
    print(f"http://127.0.0.1:{port}/")
    http.server.ThreadingHTTPServer(("127.0.0.1", port), H).serve_forever()

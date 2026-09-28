"""A fake cost plugin for the test suite: describes an `ai` family
("AI models", hint violet) and answers every Run with one estimate line
for the Run's window. luxd is pointed at it as a configured plugin, so its
real describe, report and hourly rollup code runs."""

from __future__ import annotations

import json
import threading
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

NAME = "model-gateway"
FAMILY = {"ai": {"displayName": "AI models", "color": "violet"}}


class FakeCostPlugin:
    def __init__(self, host: str):
        self.server = ThreadingHTTPServer((host, 0), _Handler)
        self.url = f"http://{host}:{self.server.server_address[1]}"
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def config(self) -> str:
        """LUX_COSTS_PLUGINS for this plugin."""
        return json.dumps([{"name": NAME, "url": self.url}])

    def close(self):
        self.server.shutdown()


def _time(s: str | None) -> datetime:
    return datetime.fromisoformat(s.replace("Z", "+00:00")) if s else datetime.now(timezone.utc)


class _Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def _json(self, body: dict):
        b = json.dumps(body).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_GET(self):
        self._json({"protocol": [1], "name": NAME, "families": FAMILY})

    def do_POST(self):
        req = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        runs = []
        for r in req["runs"]:
            frm, to = _time(r["window"]["from"]), _time(r["window"]["to"])
            runs.append({"runId": r["runId"], "status": "ok", "final": False, "lines": [
                {"family": "ai", "item": "large-model", "amount": "0.25", "currency": "USD",
                 "from": frm.isoformat(), "to": max(to, frm).isoformat(), "details": {}}]})
        self._json({"protocol": 1, "runs": runs})

#!/usr/bin/env python3
"""Tiny fake OpenAI-ish backend used by modeldeck smoke tests.

Port comes from $DECK_PORT so the same script serves several entries.
"""
import json
import os
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(os.environ.get("DECK_PORT", "0"))
NAME = os.environ.get("DECK_ENTRY", "fake")
# Optional artificial delay so tests can observe "request in flight" states.
DELAY = float(os.environ.get("FAKE_DELAY", "0"))


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _send(self, code, payload, ctype="application/json"):
        body = payload if isinstance(payload, bytes) else json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/health":
            self._send(200, {"status": "ok"})
            return
        if self.path == "/v1/models":
            self._send(200, {"object": "list", "data": [{"id": NAME, "object": "model"}]})
            return
        self._send(404, {"error": {"message": "not found"}})

    def do_POST(self):
        n = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(n) if n else b""
        try:
            req = json.loads(raw or b"{}")
        except Exception:
            req = {}
        # A request may ask for a delay: {"model": "...", "delay": 3}
        delay = DELAY or float(req.get("delay", 0) or 0)
        if delay > 0:
            time.sleep(delay)
        if self.path == "/v1/chat/completions":
            self._send(200, {
                "id": "chatcmpl-fake",
                "object": "chat.completion",
                "model": req.get("model", NAME),
                "choices": [{
                    "index": 0,
                    "message": {"role": "assistant", "content": "hello from " + NAME},
                    "finish_reason": "stop",
                }],
                "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
            })
            return
        if self.path == "/v1/embeddings":
            self._send(200, {"object": "list", "model": req.get("model", NAME),
                             "data": [{"object": "embedding", "index": 0, "embedding": [0.1, 0.2, 0.3]}]})
            return
        if self.path == "/v1/audio/transcriptions":
            self._send(200, {"text": "transcribed by " + NAME, "bytes": n})
            return
        self._send(404, {"error": {"message": "not found"}})

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    print(f"[fake] {NAME} listening on 127.0.0.1:{PORT}", flush=True)
    ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()

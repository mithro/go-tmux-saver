#!/usr/bin/env python3
"""A minimal fake Anthropic API server for the claude-integration test.

Claude Code is pointed here via ANTHROPIC_BASE_URL (with a dummy
ANTHROPIC_API_KEY), so the test needs no credentials and can never reach
the real API. The suspend/resume lifecycle sends no messages — probing
showed Claude Code 2.1.x makes ZERO API requests just starting up, idling,
/exit-ing and resuming — so this server exists as a safety net: any request
a future Claude version does make gets a well-formed canned answer (and is
logged so the test can report it), and the functionality under test must
not depend on what it returns.

Standalone: `python3 ci/fake_anthropic.py [--port N] [--log FILE]` prints
`listening on 127.0.0.1:<port>`. The integration test imports serve() and
runs it in a thread instead.
"""
import argparse
import http.server
import json
import threading


def _canned_message():
    return {"id": "msg_mock", "type": "message", "role": "assistant",
            "model": "claude-mock",
            "content": [{"type": "text", "text": "mock"}],
            "stop_reason": "end_turn",
            "usage": {"input_tokens": 1, "output_tokens": 1}}


def _sse_events():
    msg = _canned_message() | {"content": [], "stop_reason": None}
    return [
        ("message_start", {"type": "message_start", "message": msg}),
        ("content_block_start", {"type": "content_block_start", "index": 0,
                                 "content_block": {"type": "text", "text": ""}}),
        ("content_block_delta", {"type": "content_block_delta", "index": 0,
                                 "delta": {"type": "text_delta", "text": "mock"}}),
        ("content_block_stop", {"type": "content_block_stop", "index": 0}),
        ("message_delta", {"type": "message_delta",
                           "delta": {"stop_reason": "end_turn"},
                           "usage": {"output_tokens": 1}}),
        ("message_stop", {"type": "message_stop"}),
    ]


class Handler(http.server.BaseHTTPRequestHandler):
    log_path = None

    def log_message(self, *a):  # quiet
        pass

    def _record(self):
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length) if length else b""
        if self.log_path:
            with open(self.log_path, "a") as f:
                f.write(f"{self.command} {self.path}\n")
        return body

    def _json(self, payload):
        data = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        self._record()
        self._json({})

    def do_POST(self):
        body = self._record()
        if "count_tokens" in self.path:
            self._json({"input_tokens": 1})
            return
        if self.path.startswith("/v1/messages"):
            if b'"stream":true' in body.replace(b" ", b""):
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                for name, data in _sse_events():
                    self.wfile.write(
                        f"event: {name}\ndata: {json.dumps(data)}\n\n".encode())
                return
            self._json(_canned_message())
            return
        self._json({})


def serve(port=0, log_path=None):
    """Start the fake API on 127.0.0.1:<port> in a daemon thread; returns
    the bound port."""
    handler = type("H", (Handler,), {"log_path": log_path})
    srv = http.server.ThreadingHTTPServer(("127.0.0.1", port), handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv.server_address[1]


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=0)
    ap.add_argument("--log")
    args = ap.parse_args()
    port = serve(args.port, args.log)
    print(f"listening on 127.0.0.1:{port}", flush=True)
    threading.Event().wait()

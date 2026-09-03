#!/usr/bin/env python3
"""Minimal HTTP server whose shutdown behaviour is selected with -mode.

Mirrors the Go reference server mode for mode, so the conformance harness can
assert that ShutdownCheck reaches the same verdict for the same defect
regardless of stack.

It also documents the trap specific to Python's stdlib server: shutdown() stops
the accept loop but does not wait for handler threads, so draining has to be
arranged explicitly.
"""

import http.server
import os
import signal
import socket
import socketserver
import sys
import threading
import time

STATE = {"ready": True, "stalling": False, "work": 0.06}


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_GET(self):  # noqa: N802 - required name
        if self.path == "/readyz":
            self.send_response(200 if STATE["ready"] else 503)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return

        if STATE["stalling"]:
            # Accepted and never answered: the caller waits out its whole
            # timeout, which is worse for it than failing fast.
            time.sleep(300)
            return

        time.sleep(STATE["work"])
        body = b"ok\n"
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_args):
        pass


class Server(socketserver.ThreadingTCPServer):
    daemon_threads = False
    allow_reuse_address = True


def parse_args(argv):
    args = {"addr": "127.0.0.1:0", "mode": "correct", "lame-duck": "0.4"}
    index = 0
    while index < len(argv) - 1:
        key = argv[index].lstrip("-")
        args[key] = argv[index + 1]
        index += 2
    return args


def main():
    args = parse_args(sys.argv[1:])
    host, port = args["addr"].rsplit(":", 1)
    lame_duck = float(args["lame-duck"])
    mode = args["mode"]

    server = Server((host, int(port)), Handler)
    bound_host, bound_port = server.server_address[:2]
    print(f"listening on {bound_host}:{bound_port}", flush=True)

    threading.Thread(target=server.serve_forever, daemon=True).start()

    if mode == "ignore-signal":
        # A handler that does nothing is how a service ends up needing SIGKILL;
        # the default disposition would at least have exited.
        signal.signal(signal.SIGTERM, lambda *_: None)
        while True:
            time.sleep(3600)

    stop = threading.Event()
    signal.signal(signal.SIGTERM, lambda *_: stop.set())
    signal.signal(signal.SIGINT, lambda *_: stop.set())
    stop.wait()

    shutdown(server, mode, lame_duck)


def drain(server):
    """Stop accepting, then let in-flight handler threads finish."""
    server.shutdown()
    server.server_close()
    for thread in threading.enumerate():
        if thread is not threading.current_thread() and not thread.daemon:
            thread.join(timeout=15)


def shutdown(server, mode, lame_duck):
    if mode == "instant-close":
        # Readiness never flips and the listener shuts at once, so traffic still
        # being routed here is refused.
        drain(server)

    elif mode == "no-readiness-flip":
        time.sleep(lame_duck)
        drain(server)

    elif mode == "abrupt-reset":
        STATE["ready"] = False
        time.sleep(lame_duck)
        # Setting SO_LINGER to zero makes close() send RST rather than FIN,
        # destroying live connections instead of draining them.
        try:
            server.socket.setsockopt(
                socket.SOL_SOCKET, socket.SO_LINGER, b"\x01\x00\x00\x00\x00\x00\x00\x00"
            )
        except OSError:
            pass
        server.server_close()
        os._exit(0)

    elif mode == "slow-drain":
        STATE["ready"] = False
        time.sleep(lame_duck)
        time.sleep(120)
        drain(server)

    elif mode == "early-exit":
        STATE["ready"] = False
        time.sleep(lame_duck)
        os._exit(0)

    elif mode == "listener-never-closes":
        STATE["ready"] = False
        while True:
            time.sleep(3600)

    elif mode == "slow-readiness":
        time.sleep(3)
        STATE["ready"] = False
        drain(server)

    elif mode == "readiness-flap":
        STATE["ready"] = False
        time.sleep(0.3)
        # Recovering re-registers a terminating instance with the balancer.
        STATE["ready"] = True
        time.sleep(lame_duck)
        drain(server)

    elif mode == "nonzero-exit":
        STATE["ready"] = False
        time.sleep(lame_duck)
        drain(server)
        sys.exit(3)

    elif mode == "accept-no-response":
        STATE["ready"] = False
        STATE["stalling"] = True
        while True:
            time.sleep(3600)

    else:
        # 1. signal arrived  2. stop advertising readiness  3. keep serving
        # while de-registration propagates  4-6. stop accepting and drain
        # 7. exit cleanly, inside the budget.
        STATE["ready"] = False
        time.sleep(lame_duck)
        drain(server)


if __name__ == "__main__":
    main()

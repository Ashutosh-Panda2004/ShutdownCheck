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

STATE = {"ready": True, "stalling": False, "work": 0.06, "closing": False}


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def setup(self):
        super().setup()
        self.server.track(self.connection)

    def finish(self):
        try:
            self.server.untrack(self.connection)
        finally:
            super().finish()

    def do_GET(self):  # noqa: N802 - required name
        self.server.set_busy(self.connection, True)
        try:
            self._respond()
        finally:
            self.server.set_busy(self.connection, False)

    def _close_headers(self):
        # Once draining starts, a response must retire its connection: a
        # client under load otherwise reuses the socket immediately, the
        # handler thread serving it never finishes, and the process is still
        # alive when the grace period ends.
        if STATE["closing"]:
            self.send_header("Connection", "close")
            self.close_connection = True

    def _respond(self):
        if self.path == "/readyz":
            self.send_response(200 if STATE["ready"] else 503)
            self._close_headers()
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
        self._close_headers()
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_args):
        pass


class Server(socketserver.ThreadingTCPServer):
    daemon_threads = False
    allow_reuse_address = True

    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self._conns = {}
        self._conns_lock = threading.Lock()

    def track(self, conn):
        with self._conns_lock:
            self._conns[conn] = False

    def untrack(self, conn):
        with self._conns_lock:
            self._conns.pop(conn, None)

    def set_busy(self, conn, busy):
        with self._conns_lock:
            if conn in self._conns:
                self._conns[conn] = busy

    def connection_sockets(self):
        with self._conns_lock:
            return list(self._conns)

    def close_idle_connections(self):
        """Close keep-alive sockets with no request in flight.

        Their handler threads are blocked reading the next request and would
        otherwise sit there until the client gives up, long past the grace
        period. Busy connections are left alone: their responses carry
        Connection: close once closing, so they retire themselves.
        """
        with self._conns_lock:
            idle = [conn for conn, busy in self._conns.items() if not busy]
        for conn in idle:
            try:
                conn.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            try:
                conn.close()
            except OSError:
                pass


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
    STATE["closing"] = True
    server.shutdown()
    server.server_close()
    server.close_idle_connections()
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
        # Close immediately: setting SO_LINGER to zero makes close() send RST
        # rather than FIN, destroying live connections instead of draining
        # them. The abrupt exit is deliberate: server_close() would join the
        # handler threads and turn the reset into a slow drain.
        for sock in [server.socket, *server.connection_sockets()]:
            try:
                sock.setsockopt(
                    socket.SOL_SOCKET, socket.SO_LINGER, b"\x01\x00\x00\x00\x00\x00\x00\x00"
                )
            except OSError:
                pass
        os._exit(0)

    elif mode == "slow-drain":
        STATE["ready"] = False
        time.sleep(lame_duck)
        time.sleep(120)
        drain(server)

    elif mode == "early-exit":
        STATE["ready"] = False
        # Exit at once, abandoning whatever is in flight: sleeping out the
        # lame-duck window first would let every in-flight request finish and
        # there would be no defect left to measure.
        os._exit(0)

    elif mode == "listener-never-closes":
        STATE["ready"] = False
        while True:
            time.sleep(3600)

    elif mode == "slow-readiness":
        time.sleep(3)
        STATE["ready"] = False
        # Wait for the readiness probe to observe the flip before shutting
        # down, so the tool can measure how late the flip was.
        time.sleep(0.5)
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

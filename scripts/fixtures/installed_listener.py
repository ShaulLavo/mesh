"""A listener owned by the retained session, including an active HTTP stream."""
import http.server
import json
import os
import signal
import sys
import threading
from pathlib import Path


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        try:
            if self.path.endswith("/stream"):
                self.stream()
                return
            self.wfile.write((str(os.getpid()) + "\n").encode())
        except (BrokenPipeError, ConnectionResetError):
            pass

    def stream(self):
        while not STOP.is_set():
            self.wfile.write((str(os.getpid()) + "\n").encode())
            self.wfile.flush()
            STOP.wait(0.05)

    def log_message(self, *_arguments):
        pass


STOP = threading.Event()


def interrupted(_number, _frame):
    STOP.set()


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    with http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler) as server:
        server.timeout = 0.1
        Path(sys.argv[1]).write_text(json.dumps({"pid": os.getpid(), "port": server.server_port}))
        while not STOP.is_set():
            server.handle_request()

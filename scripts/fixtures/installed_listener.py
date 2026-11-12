"""A listener owned by the retained session, including an active HTTP stream."""
import os
import sys


def observe():
    if __name__ != "__main__" or len(sys.argv) != 3:
        return None
    try:
        with open(sys.argv[1] + ".entered", "x") as stream:
            stream.write("\n".join((sys.argv[2], str(os.getpid()), str(os.getppid()), str(os.getuid()))))
        import installed_listener_evidence
        return installed_listener_evidence.Observer(sys.argv[1], sys.argv[2])
    except Exception:
        return None


EVIDENCE = observe()


def stage(phase):
    if EVIDENCE is not None:
        EVIDENCE.stage(phase)


def dependencies():
    import http.server
    import json
    import signal
    import threading
    from pathlib import Path
    return http.server, json, signal, threading, Path


http_server, json, signal, threading, Path = dependencies()

stage("imports-complete")


class Handler(http_server.BaseHTTPRequestHandler):
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


def emit_challenge(path, previous):
    if not path.exists():
        return previous
    token = path.read_text()
    if token != previous:
        print("__RESUMED__" + token + "__", flush=True)
    return token


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    stage("listener-create-bind")
    with http_server.ThreadingHTTPServer(("127.0.0.1", 0), Handler) as server:
        stage("listener-bound")
        server.timeout = 0.1
        marker = Path(sys.argv[1])
        temporary = marker.with_suffix(".tmp")
        stage("marker-write")
        temporary.write_text(json.dumps({"pid": os.getpid(), "port": server.server_port}))
        stage("marker-replace")
        temporary.replace(marker)
        stage("marker-published")
        previous = None
        while not STOP.is_set():
            server.handle_request()
            previous = emit_challenge(marker.with_suffix(".challenge"), previous)

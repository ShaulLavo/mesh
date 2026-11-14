"""Observe the original listener through live route loss and reconnection."""
import http.client
import json
import threading
import time


class Observer:
    def __init__(self, port, listener, output, maximum=90):
        self.port, self.listener, self.output, self.maximum = port, listener, output, maximum
        self.lock, self.stopping = threading.Lock(), threading.Event()
        self.connection, self.thread = None, None
        self.pulses, self.gaps, self.failure = 0, [], ""
        self.down, self.expected, self.down_expected = None, False, False
        self.last_pulse = None

    def start(self):
        self.thread = threading.Thread(target=self.run, name="installed-service-stream", daemon=True)
        self.thread.start()
        deadline = time.monotonic() + 10
        while not self.snapshot()["pulses"] and time.monotonic() < deadline:
            self.check()
            self.stopping.wait(0.01)
        if not self.snapshot()["pulses"]:
            raise RuntimeError("original routed listener did not produce its initial live stream pulse")

    def run(self):
        while not self.stopping.is_set():
            try:
                self.consume()
            except (OSError, http.client.HTTPException):
                self.disconnected()
            except Exception as error:
                self.unexpected(error)
                return
            self.stopping.wait(0.05)

    def consume(self):
        connection = http.client.HTTPConnection("127.0.0.1", self.port, timeout=0.5)
        with self.lock:
            self.connection = connection
        try:
            connection.request("GET", "/catchup/stream")
            response = connection.getresponse()
            if response.status != 200:
                raise ConnectionError("routed service is temporarily unavailable")
            self.read_pulses(response)
        finally:
            connection.close()

    def read_pulses(self, response):
        expected = (str(self.listener["pid"]) + "\n").encode()
        while not self.stopping.is_set():
            line = response.readline()
            if not line:
                raise ConnectionError("original service stream disconnected")
            if line != expected:
                raise ValueError("routed stream reached another listener identity")
            self.pulse()

    def pulse(self):
        with self.lock:
            self.pulses += 1
            self.finish_gap()
            self.last_pulse = time.monotonic()

    def finish_gap(self):
        if self.down is None:
            return
        duration = time.monotonic() - self.down
        if len(self.gaps) == 128:
            self.failure = "route outage count exceeds its case bound"
            return
        self.gaps.append({"seconds": duration, "expectedOffline": self.down_expected})
        if not self.down_expected and duration > self.maximum:
            self.failure = "transition route gap exceeded its existing 90-second case bound"
        self.down = None

    def disconnected(self):
        with self.lock:
            if self.down is None and not self.stopping.is_set():
                self.down, self.down_expected = self.last_pulse or time.monotonic(), self.expected

    def failed(self, message):
        with self.lock:
            self.failure = message

    def unexpected(self, error):
        if not self.stopping.is_set():
            self.failed(str(error))

    def offline(self, expected):
        with self.lock:
            self.expected = expected
            self.down_expected = self.down_expected or expected

    def snapshot(self):
        with self.lock:
            return {"listener": self.listener, "pulses": self.pulses, "gaps": list(self.gaps),
                    "maximumTransitionGapSeconds": self.maximum, "failure": self.failure,
                    "connected": self.down is None and self.pulses > 0}

    def check(self):
        value = self.snapshot()
        if value["failure"]:
            raise RuntimeError(value["failure"])
        with self.lock:
            exceeded = self.down is not None and not self.down_expected and time.monotonic() - self.down > self.maximum
        if exceeded:
            raise RuntimeError("transition route did not reconnect within its existing case bound")

    def recovered(self):
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            self.check()
            if self.snapshot()["connected"]:
                return
            self.stopping.wait(0.01)
        raise RuntimeError("original service stream did not reconnect after native readiness")

    def close(self):
        self.stopping.set()
        if self.connection:
            self.connection.close()
        if self.thread:
            self.thread.join(timeout=2)
        value = self.snapshot() | {"observerStopped": self.thread is None or not self.thread.is_alive()}
        self.output.write_text(json.dumps(value, indent=2) + "\n")
        if not value["observerStopped"]:
            raise RuntimeError("owned service stream observer did not stop")

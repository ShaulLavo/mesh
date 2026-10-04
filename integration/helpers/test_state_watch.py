import socket
import unittest
from unittest.mock import patch

import state_watch


class IdleObservationTest(unittest.TestCase):
    def observe(self, messages):
        reader, writer = socket.socketpair()
        self.addCleanup(reader.close)
        self.addCleanup(writer.close)
        initial = {"type": "state.snapshot", "stateSnapshot": {
            "seq": 1, "current": {"sessions": {"ageMillis": 77}, "services": {"ageMillis": 0}}}}
        for message in messages:
            state_watch.send(writer, message)
        with patch.object(state_watch.time, "monotonic", side_effect=[0, 0, 10, 10, 10]):
            return state_watch.observe_idle(reader, initial, 10)

    def test_standalone_current_at_deadline_is_observable(self):
        size, elapsed = self.observe([{"type": "state.current", "stateCurrent": {
            "seq": 2, "sections": {"sessions": {"ageMillis": 77}, "services": {"ageMillis": 77}}}}])
        self.assertGreater(size, 0)
        self.assertEqual(elapsed, 10)

    def test_memory_event_current_at_deadline_confirms_catalogs(self):
        size, elapsed = self.observe([{"type": "state.event", "stateEvent": {
            "seq": 2, "kind": "session.memory", "payload": {}}, "stateCurrent": {
            "seq": 2, "sections": {"sessions": {"ageMillis": 77}, "services": {"ageMillis": 77}}}}])
        self.assertGreater(size, 0)
        self.assertEqual(elapsed, 10)


if __name__ == "__main__":
    unittest.main()

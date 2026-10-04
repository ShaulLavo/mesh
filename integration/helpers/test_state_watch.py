import socket
import unittest
from unittest.mock import patch

import state_watch


class IdleObservationTest(unittest.TestCase):
    def memory_event(self):
        return {"type": "state.event", "stateEvent": {
            "seq": 2, "kind": "session.memory", "payload": {}}, "stateCurrent": {
            "seq": 2, "sections": {"sessions": {"ageMillis": 77}, "services": {"ageMillis": 77}}}}

    def observe(self, messages, times=(0, 0, 10, 10, 10)):
        reader, writer = socket.socketpair()
        self.addCleanup(reader.close)
        self.addCleanup(writer.close)
        initial = {"type": "state.snapshot", "stateSnapshot": {
            "seq": 1, "current": {"sessions": {"ageMillis": 77}, "services": {"ageMillis": 0}}}}
        for message in messages:
            state_watch.send(writer, message)
        with patch.object(state_watch.time, "monotonic", side_effect=times):
            return state_watch.observe_idle(reader, initial, 10)

    def test_standalone_current_at_deadline_is_observable(self):
        size, elapsed = self.observe([{"type": "state.current", "stateCurrent": {
            "seq": 2, "sections": {"sessions": {"ageMillis": 77}, "services": {"ageMillis": 77}}}}])
        self.assertGreater(size, 0)
        self.assertEqual(elapsed, 10)

    def test_memory_event_current_at_deadline_confirms_catalogs(self):
        size, elapsed = self.observe([self.memory_event(), {"type": "state.current", "stateCurrent": {
            "seq": 3, "sections": {"sessions": {"ageMillis": 77}, "services": {"ageMillis": 77}}}}])
        self.assertGreater(size, 0)
        self.assertEqual(elapsed, 10)

    def test_event_without_current_does_not_confirm(self):
        message = self.memory_event()
        del message["stateCurrent"]
        with self.assertRaisesRegex(AssertionError, "unchanged catalog received no current confirmation"):
            self.observe([message])

    def test_current_must_match_event_sequence(self):
        message = self.memory_event()
        message["stateCurrent"]["seq"] = 3
        with self.assertRaises(AssertionError):
            self.observe([message])

    def test_sequence_must_follow_snapshot(self):
        message = self.memory_event()
        message["stateEvent"]["seq"] = 3
        message["stateCurrent"]["seq"] = 3
        with self.assertRaises(AssertionError):
            self.observe([message])

    def test_current_requires_exact_catalog_sections(self):
        for sections in ({}, {"sessions": {"ageMillis": 0}},
                         {"sessions": {"ageMillis": 0}, "other": {"ageMillis": 0}}):
            with self.subTest(sections=sections):
                message = self.memory_event()
                message["stateCurrent"]["sections"] = sections
                with self.assertRaises(AssertionError):
                    self.observe([message])

    def test_each_catalog_must_be_fresh_and_healthy(self):
        for topic in ("sessions", "services"):
            for section in ({"ageMillis": 0, "failing": True}, {"ageMillis": 30000}, {"ageMillis": -1}):
                with self.subTest(topic=topic, section=section):
                    message = self.memory_event()
                    message["stateCurrent"]["sections"][topic] = section
                    with self.assertRaises(AssertionError):
                        self.observe([message])

    def test_earlier_confirmation_does_not_hide_later_failure(self):
        message = self.memory_event()
        message["stateEvent"]["seq"] = 3
        message["stateCurrent"]["seq"] = 3
        message["stateCurrent"]["sections"]["services"]["failing"] = True
        with self.assertRaises(AssertionError):
            self.observe([self.memory_event(), message], times=(0, 0, 5, 5))

    def test_current_must_be_a_watch_control(self):
        message = self.memory_event()
        message["type"] = "host.info.result"
        with self.assertRaises(AssertionError):
            self.observe([message])

    def test_idle_bandwidth_bound_is_preserved(self):
        message = {"type": "state.current", "stateCurrent": self.memory_event()["stateCurrent"],
                   "padding": "x" * 12000}
        with self.assertRaises(AssertionError) as failure:
            self.observe([message])
        self.assertIsInstance(failure.exception.args[0], tuple)


if __name__ == "__main__":
    unittest.main()

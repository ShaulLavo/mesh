#!/usr/bin/env python3
"""Printed naming I/O markers require complete output lines, not echoed input."""
import unittest

from machine_naming import printed_marker


class PrintedMarkerTest(unittest.TestCase):
    marker = "BEFORE_NAME_IO"
    echoed = b"MESH_PROMPT> printf 'BEFORE_NAME_IO\\n'\r\n"

    def test_actual_darwin_output_boundary(self):
        output = self.echoed + b"BEFORE_NAME_IO\r\nMESH_PROMPT> "
        self.assertTrue(printed_marker(output, self.marker))

    def test_actual_linux_readline_boundary(self):
        output = self.echoed + b"\x1b[?2004l\rBEFORE_NAME_IO\r\n\x1b[?2004hMESH_PROMPT> "
        self.assertTrue(printed_marker(output, self.marker))

    def test_wrong_marker_is_rejected(self):
        for output in (b"WRONG_BEFORE_NAME_IO\r\n", b"BEFORE_NAME_IO_WRONG\r\n"):
            with self.subTest(output=output):
                self.assertFalse(printed_marker(self.echoed + output, self.marker))

    def test_absent_marker_is_rejected(self):
        self.assertFalse(printed_marker(b"MESH_PROMPT> ", self.marker))

    def test_echo_only_is_rejected(self):
        self.assertFalse(printed_marker(self.echoed, self.marker))

    def test_partial_line_is_rejected(self):
        self.assertFalse(printed_marker(self.echoed + b"BEFORE_NAME_IO", self.marker))


if __name__ == "__main__":
    unittest.main()

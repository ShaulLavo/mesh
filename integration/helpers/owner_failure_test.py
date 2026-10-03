#!/usr/bin/env python3
import json
import unittest

from owner_failure import escaped_owner_output


class OwnerFailureTest(unittest.TestCase):
    reference = "A" * 26
    marker = "BROKEN_OUTPUT_MARKER"

    def record(self, output, reference=None):
        diagnostic = ('route :12345 did not start: the command exited with status 7 '
                      '(session 7K3D, command "echo BROKEN_OUTPUT_MARKER; exit 7")\n\nlast output:\n' + output)
        return f"timestamp daemon: on-demand failure {reference or self.reference}: {json.dumps(diagnostic)}\n"

    def accepts(self, log):
        return escaped_owner_output(log, self.reference, self.marker)

    def test_plain_output(self):
        self.assertTrue(self.accepts(self.record(self.marker)))

    def test_warning_before_command_output(self):
        warning = "worker: own scope mesh-session-7K3D.scope: Failed to connect to user scope bus\n"
        self.assertTrue(self.accepts(self.record(warning + self.marker)))

    def test_go_hex_escape_and_crlf_output(self):
        log = self.record("\x1b[0mwarning\r\n" + self.marker + "\r\n").replace(r"\u001b", r"\x1b")
        self.assertTrue(self.accepts(log))

    def test_valid_go_unicode_scalar_escapes(self):
        for escape in [r"λ", r"\U0001f600", r"\U0010ffff", r"\\ud800"]:
            with self.subTest(escape=escape):
                log = self.record("warning UNICODE\n" + self.marker).replace("UNICODE", escape)
                self.assertTrue(self.accepts(log))

    def test_rejects_invalid_go_unicode_scalars(self):
        for escape in [r"\ud800", r"\udfff", r"\U0000d800", r"\U0000dfff", r"\U00110000", r"\Uffffffff"]:
            with self.subTest(escape=escape):
                log = self.record("warning UNICODE\n" + self.marker).replace("UNICODE", escape)
                self.assertFalse(self.accepts(log))

    def test_inline_non_lf_controls_do_not_supply_a_marker_line(self):
        for escape in [r"\x85", r"\v", r"\f", r"\r", r" ", r" "]:
            with self.subTest(escape=escape):
                log = self.record("warning SEPARATOR" + self.marker).replace("SEPARATOR", escape)
                self.assertFalse(self.accepts(log))

    def test_non_lf_byte_with_real_lf_boundary(self):
        log = self.record("warning BYTE\n" + self.marker).replace("BYTE", r"\x85")
        self.assertTrue(self.accepts(log))

    def test_duplicate_correlated_failure(self):
        log = self.record(self.marker)
        self.assertFalse(self.accepts(log + log))

    def test_marker_in_command_does_not_prove_captured_output(self):
        self.assertFalse(self.accepts(self.record("worker warning only")))

    def test_another_reference_does_not_supply_output(self):
        log = self.record("worker warning only") + self.record(self.marker, "B" * 26)
        self.assertFalse(self.accepts(log))

    def test_literal_backslash_n_does_not_supply_a_line_break(self):
        self.assertFalse(self.accepts(self.record(r"worker warning\n" + self.marker)))

    def test_rejects_unescaped_and_invalid_quoted_output(self):
        log = self.record(self.marker)
        malformed = {
            "unescaped quotes": log.replace(r'\"echo BROKEN_OUTPUT_MARKER; exit 7\"', '"echo BROKEN_OUTPUT_MARKER; exit 7"'),
            "raw newline": log.replace("status 7", "status\n7"),
            "raw tab": log.replace("status 7", "status\t7"),
            "invalid escape": log.replace("last output:", r"\qlast output:"),
            "missing closing quote": log[:-2] + "\n",
            "trailing text": log[:-1] + " trailing\n",
        }
        for name, record in malformed.items():
            with self.subTest(name=name):
                self.assertFalse(self.accepts(record))


if __name__ == "__main__":
    unittest.main()

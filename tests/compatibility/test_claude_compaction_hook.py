"""Pure hook capture tests using owned temporary files; no native process."""

import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import claude_compaction_hook as hook
from test_claude_compaction_receipts import CWD, OTHER, SESSION, hook as raw_hook


class CaptureTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.root.chmod(0o700)
        self.path = self.root / "hooks.jsonl"
        self.path.touch(mode=0o600)

    def capture(self, event, **changes):
        return hook.capture(raw_hook(event, **changes), self.path, expected_cwd=CWD)

    def observed(self, **changes):
        return hook.observed_hooks(self.path, **{
            "expected_session": SESSION, "expected_cwd": CWD, **changes,
        })

    def test_sanitized_pair_binds_to_controller_session(self):
        self.assertTrue(self.capture("PreCompact"))
        self.assertTrue(self.capture("PostCompact"))
        rows = self.observed()
        self.assertEqual([r["sequence"] for r in rows], [1, 2])
        self.assertNotIn("PRIVATE", self.path.read_text())
        self.assertNotIn(SESSION, self.path.read_text())
        self.assertNotIn(CWD, self.path.read_text())
        with self.assertRaises(ValueError):
            self.observed(expected_session=OTHER)

    def test_ignored_hook_failure_remains_durable(self):
        self.capture("PreCompact")
        self.assertFalse(hook.capture(b"not json", self.path, expected_cwd=CWD))
        self.capture("PostCompact")
        self.assertIn("invalid_hook", self.path.read_text())
        with self.assertRaises(ValueError):
            self.observed()

    def test_duplicate_callback_invalidates_complete_pair(self):
        self.capture("PreCompact")
        self.capture("PostCompact")
        self.capture("PostCompact")
        with self.assertRaises(ValueError):
            self.observed()

    def test_manual_and_reordered_callbacks_are_not_automatic_evidence(self):
        for events in (("PostCompact", "PreCompact"), ("PreCompact", "PostCompact")):
            self.path.write_bytes(b"")
            for event in events:
                self.capture(event, trigger="manual")
            with self.assertRaises(ValueError):
                self.observed()

    def test_symlink_shared_mode_and_hardlink_refused(self):
        link = self.root / "link"
        link.symlink_to(self.path)
        with self.assertRaises(OSError):
            hook.capture(raw_hook("PreCompact"), link, expected_cwd=CWD)
        self.path.chmod(0o644)
        with self.assertRaises(ValueError):
            self.capture("PreCompact")
        self.path.chmod(0o600)
        os.link(self.path, self.root / "hardlink")
        with self.assertRaises(ValueError):
            self.capture("PreCompact")

    def test_partial_write_cannot_be_read_as_success(self):
        real_write = os.write
        with patch.object(hook.os, "write", side_effect=lambda fd, data: real_write(fd, data[:7])):
            with self.assertRaisesRegex(ValueError, "short_write"):
                self.capture("PreCompact")
        with self.assertRaisesRegex(ValueError, "incomplete"):
            self.observed()

    def test_budget_exhaustion_never_replaces_evidence(self):
        for _ in range(hook.MAX_EVENTS):
            self.capture("PreCompact")
        before = self.path.read_bytes()
        with self.assertRaisesRegex(ValueError, "exhausted"):
            self.capture("PostCompact")
        self.assertEqual(self.path.read_bytes(), before)
        with self.assertRaises(ValueError):
            self.observed()

    def test_tampered_sequence_or_clock_refused(self):
        self.capture("PreCompact")
        self.capture("PostCompact")
        rows = [json.loads(line) for line in self.path.read_text().splitlines()]
        rows[1]["at_ns"] = rows[0]["at_ns"]
        self.path.write_text("".join(json.dumps(row) + "\n" for row in rows))
        with self.assertRaisesRegex(ValueError, "order"):
            self.observed()


if __name__ == "__main__":
    unittest.main()

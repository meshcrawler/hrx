"""Pure native-record projections: no harness, listener, database or file IO."""

import json
import unittest

import claude_compaction_receipts as receipts


SESSION = "11111111-2222-3333-4444-555555555555"
OTHER = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
CWD = "/owned/synthetic/project"


def encode(value):
    return json.dumps(value).encode()


def hook(event, **changes):
    return encode({"session_id": SESSION, "cwd": CWD, "hook_event_name": event,
                   "trigger": "auto", "compact_summary": "PRIVATE_SUMMARY_CANARY",
                   "transcript_path": "/PRIVATE_TRANSCRIPT_CANARY", **changes})


def boundary(**changes):
    return encode({"sessionId": SESSION, "version": receipts.VERSION, "uuid": OTHER,
                   "type": "system", "subtype": "compact_boundary",
                   "compactMetadata": {"trigger": "auto", "userContext": "PRIVATE_CONTEXT_CANARY"},
                   **changes})


def sequence(hooks=None, boundaries=None):
    return receipts.automatic_sequence(
        hooks if hooks is not None else [hook("PreCompact"), hook("PostCompact")],
        boundaries if boundaries is not None else [boundary()],
        expected_session=SESSION, expected_cwd=CWD,
    )


class ProjectionTests(unittest.TestCase):
    def test_correlated_records_preserve_only_bounded_projection(self):
        result = sequence()
        text = json.dumps(result)
        for secret in ("PRIVATE", CWD, SESSION, OTHER):
            self.assertNotIn(secret, text)
        self.assertEqual(result["scope"], "projected_records_only")
        self.assertNotIn("status", result)
        self.assertEqual(result["boundary"]["session_digest"], receipts.digest(SESSION))
        self.assertEqual(set(result["boundary"]),
                         {"event", "trigger", "session_digest", "boundary_digest", "version"})

    def test_missing_duplicate_reordered_and_manual_hooks_refused(self):
        for hooks in ([], [hook("PreCompact")], [hook("PreCompact")] * 2,
                      [hook("PostCompact"), hook("PreCompact")],
                      [hook("PreCompact"), hook("PostCompact", trigger="manual")],
                      [hook("PreCompact"), hook("PostCompact", session_id=OTHER)],
                      [hook("PreCompact"), hook("PostCompact", cwd="/foreign")]):
            with self.subTest(hooks=hooks), self.assertRaises(ValueError):
                sequence(hooks=hooks)

    def test_foreign_or_unverified_boundary_refused(self):
        for changes in ({"sessionId": OTHER}, {"version": "2.1.310"},
                        {"uuid": "not-an-id"}, {"isSidechain": True},
                        {"isSidechain": 0}, {"remoteSourced": True},
                        {"subtype": "other"}, {"compactMetadata": None},
                        {"compactMetadata": {"trigger": "manual"}},
                        {"compactMetadata": {"trigger": "unknown"}}):
            with self.subTest(changes=changes), self.assertRaises(ValueError):
                sequence(boundaries=[boundary(**changes)])
        for markers in ([], [boundary(), boundary()]):
            with self.assertRaises(ValueError):
                sequence(boundaries=markers)

    def test_unbounded_ambiguous_or_nonstandard_json_refused(self):
        for raw in (b"", b"[]", b"null", b'{}{}', b'\xff',
                    b'{"trigger":"auto","trigger":"manual"}',
                    b'{"nested":{"x":1,"x":2}}', b'{"x":NaN}',
                    b'{"x":Infinity}', b" " * (receipts.MAX_RECORD_BYTES + 1),
                    b"[" * 2000 + b"]" * 2000):
            with self.subTest(size=len(raw)), self.assertRaises(ValueError):
                receipts.record(raw)


if __name__ == "__main__":
    unittest.main()

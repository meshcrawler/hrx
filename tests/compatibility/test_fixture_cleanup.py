"""Pure failure-injection checks; no database or native process is launched."""
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from conversation import cleanup_fixture
import pitput_exchange


class FixtureCleanupTests(unittest.TestCase):
    def fixture(self):
        return type("Fixture", (pitput_exchange.PitputIntegration,), {
            "pg_ctl": "/fixture/pg_ctl", "pg_data": Path("/fixture/data"),
            "root": Path("/fixture"), "env": {}, "postgres_cleanup_verified": False})

    def test_shutdown_requires_successful_stop_and_absent_postmaster(self):
        for codes, success in [([0, 0, 3], True), ([3], True),
                               ([0, 1, 3], False), ([0, 0, 0], False), ([4], False)]:
            with self.subTest(codes=codes), patch("pitput_exchange.run", side_effect=[
                    SimpleNamespace(returncode=code) for code in codes]) as run:
                fixture = self.fixture()
                if success:
                    fixture.stop_postgres()
                else:
                    with self.assertRaises(RuntimeError):
                        fixture.stop_postgres()
                self.assertEqual(fixture.postgres_cleanup_verified, success)
                self.assertEqual(run.call_count, len(codes))

    def test_unproven_shutdown_preserves_directory_and_fails_report(self):
        fixture = self.fixture()
        fixture.addClassCleanup(fixture.remove_root)
        fixture.addClassCleanup(fixture.stop_postgres)
        result = {"status": "PASS"}
        with patch("pitput_exchange.run", side_effect=TimeoutError("private detail")), \
                patch("pitput_exchange.shutil.rmtree") as remove:
            cleanup_fixture(fixture, result)
        remove.assert_not_called()
        self.assertEqual(result["status"], "FAIL")
        self.assertEqual(result["reason"], "fixture_cleanup_failed")
        self.assertEqual(result["fixture_cleanup"]["errors"], ["TimeoutError", "RuntimeError"])
        self.assertNotIn("private detail", str(result))

    def test_verified_shutdown_removes_directory_and_records_success(self):
        fixture = self.fixture()
        fixture.addClassCleanup(fixture.remove_root)
        fixture.addClassCleanup(fixture.stop_postgres)
        result = {"status": "PASS"}
        with patch("pitput_exchange.run", return_value=SimpleNamespace(returncode=3)), \
                patch("pitput_exchange.shutil.rmtree") as remove:
            cleanup_fixture(fixture, result)
        remove.assert_called_once_with(fixture.root)
        self.assertEqual(result["fixture_cleanup"], {"status": "PASS", "errors": []})

    def test_cleanup_attempts_all_callbacks_and_retains_original_failure(self):
        fixture = self.fixture()
        attempted = []
        fixture.addClassCleanup(lambda: attempted.append(True))

        def fail():
            raise ValueError("private detail")

        fixture.addClassCleanup(fail)
        result = {"status": "FAIL", "reason": "original_failure"}
        cleanup_fixture(fixture, result)
        self.assertEqual(attempted, [True])
        self.assertEqual(result["reason"], "original_failure")
        self.assertEqual(result["fixture_cleanup"], {"status": "FAIL", "errors": ["ValueError"]})

"""Process-ownership boundary interface for the compatibility runners.

Linux uses the child-subreaper supervisor (subreaper.py): every orphan re-parents to
it, and wait() returning ECHILD proves no descendant remains. Darwin has no
equivalent, and no containment backend has a reviewed enforcement proof
(docs/internal/darwin-ownership-proposal.md, review /1327), so Darwin availability is
fail-closed. Other platforms are refused.

The Darwin uid-boundary state machine and final controller classification below
are pure and injected: they define the proposal's invariants for review and tests,
and nothing here launches, signals or scans a real Darwin process.
"""

from collections import namedtuple
import signal

LINUX_SUBREAPER, DARWIN_UID = "linux-subreaper", "darwin-uid"
DARWIN_UNPROVEN = "darwin_containment_unproven"
UNAVAILABLE = "ownership_boundary_unavailable"

Availability = namedtuple("Availability", "mechanism reason")


def available(platform):
    """The verified boundary for platform, or the refusal reason. Callers pass their
    own sys.platform so a platform override applies where the guard lives."""
    if platform.startswith("linux"):
        return Availability(LINUX_SUBREAPER, None)
    if platform == "darwin":
        return Availability(None, DARWIN_UNPROVEN)
    return Availability(None, UNAVAILABLE)


def classify_summary(record, supervisor_exit):
    """clean, failed or unknown from a supervisor summary alone.

    A Linux summary ({"subreaper": true, ...}, unchanged since subreaper.py) is
    clean only when complete with no forced kill and a zero supervisor exit. A
    Darwin helper summary is never sufficient by itself: only classify_darwin_final,
    with the controller's own post-exit proof, can make it clean."""
    if not isinstance(record, dict):
        return "unknown"
    if record.get("boundary") == DARWIN_UID:
        return "unknown"
    if record.get("subreaper") is not True or record.get("complete") is not True:
        return "unknown"
    if record.get("forced") or supervisor_exit != 0:
        return "failed"
    return "clean"


# One entry of a uid scan: pid, start (pid start time, the identity guard), and
# real and effective uid. A real scan unions PROC_RUID_ONLY and PROC_UID_ONLY.
Entry = namedtuple("Entry", "pid start ruid euid")


class ScanError(RuntimeError):
    """The process table could not be enumerated: ownership is unprovable."""


class UidStop:
    """Helper-side stop loop for a uid boundary (proposal invariants 4-6b).

    The helper runs as U, so it excludes exactly its own authenticated identity
    (pid and start time). Its "empty except me" only ends the loop (helper_empty);
    it is never completion. scan() returns Entry values for uid U; kill(sig)
    signals every U process except the caller (kill(-1, sig) as U); clock()
    is monotonic seconds; wait() sleeps one poll interval. A scan is deduplicated by
    pid; the helper's own entry (pid, start, ruid, euid) must appear exactly once in
    every scan, or the run ends with an error."""

    def __init__(self, uid, me, scan, kill, clock, wait, grace=10.0, hard_limit=60.0):
        self.uid, self.me = uid, me
        self.scan, self.kill, self.clock, self.wait = scan, kill, clock, wait
        self.grace, self.hard_limit = grace, hard_limit

    def run(self):
        summary = {"boundary": DARWIN_UID, "helper_empty": False, "forced": False,
                   "residual": None, "transition": False, "error": None}
        started = self.clock()
        self.kill(signal.SIGTERM)
        self.kill(signal.SIGCONT)  # A stopped process acts on SIGTERM only once continued.
        while True:
            try:
                entries = list(self.scan())
            except ScanError:
                summary["error"] = "scan_failed"
                return summary
            # The helper is a U process, so every scan must contain its own identity
            # exactly once. Otherwise the scan cannot be trusted to be complete.
            mine = [e for e in entries if e.pid == self.me.pid]
            if not mine:
                summary["error"] = "self_identity_absent"
                return summary
            if len(mine) > 1:
                summary["error"] = "self_identity_duplicate"
                return summary
            if mine[0] != self.me:
                summary["error"] = "self_identity_mismatch"
                return summary
            if any(e.ruid != e.euid for e in entries):
                # A privilege transition happened: ownership is no longer provable.
                summary["transition"] = True
                summary["error"] = "uid_transition_observed"
                return summary
            others = [e for e in entries if e.pid != self.me.pid]
            summary["residual"] = len(others)
            if not others:
                summary["helper_empty"] = True
                return summary
            elapsed = self.clock() - started
            if elapsed >= self.hard_limit:
                return summary
            if elapsed >= self.grace:
                summary["forced"] = True
                self.kill(signal.SIGKILL)
            self.wait()


HELPER_FIELDS = {"boundary": str, "helper_empty": bool, "forced": bool, "transition": bool}


def classify_darwin_final(helper, helper_exit_at, final_scans, settle, lock_held, launchd_baseline,
                          launchd_after, cron_entries, containment_proof=None):
    """The controller's decision after the helper and sudo have exited (invariant 7).

    It is never clean while Darwin containment is unproven: without a reviewed
    containment_proof (none exists; docs/internal/darwin-ownership-proposal.md, /1327), the
    best possible result is unknown. Every input must be explicit:
    - helper carries every HELPER_FIELDS key with its exact type, and error is None;
    - helper_exit_at is the monotonic time the helper and sudo were reaped;
    - final_scans is a list of (at, entries) with at least two scans, all taken after
      helper_exit_at and spanning at least settle seconds. entries is a list of
      Entry values, or None when the scan failed;
    - the launchd label sets and cron entries are None when unenumerable.
    Returns (result, reasons)."""
    unknown = []
    if not isinstance(helper, dict) or any(not isinstance(helper.get(k), t) for k, t in HELPER_FIELDS.items()) \
            or "error" not in helper or helper.get("boundary") != DARWIN_UID:
        unknown.append("helper_summary_incomplete")
    else:
        if helper["error"] is not None:
            unknown.append("helper_" + str(helper["error"]))
        if helper["transition"]:
            unknown.append("uid_transition_observed")
    if not isinstance(helper_exit_at, (int, float)):
        unknown.append("helper_exit_unproven")
    scans_ok = (isinstance(final_scans, list) and len(final_scans) >= 2
                and all(isinstance(s, tuple) and len(s) == 2 and isinstance(s[0], (int, float)) for s in final_scans))
    if not scans_ok:
        unknown.append("final_scans_missing")
    else:
        times = [at for at, _ in final_scans]
        if isinstance(helper_exit_at, (int, float)) and min(times) <= helper_exit_at:
            unknown.append("final_scan_before_helper_exit")
        if times != sorted(times) or times[-1] - times[0] < settle:
            unknown.append("final_scans_not_settled")
        if any(entries is None for _, entries in final_scans):
            unknown.append("final_scan_unavailable")
    if lock_held is not True:
        unknown.append("pool_lock_not_held")
    if launchd_baseline is None or launchd_after is None:
        unknown.append("launchd_unenumerable")
    if cron_entries is None:
        unknown.append("cron_unenumerable")
    failed = []
    if not unknown:
        if any(entries for _, entries in final_scans):
            failed.append("residual_processes")
        if set(launchd_after) != set(launchd_baseline):
            failed.append("launchd_definitions_remain")
        if cron_entries:
            failed.append("cron_entries_remain")
        if helper["forced"]:
            failed.append("forced")
        if not helper["helper_empty"]:
            failed.append("helper_not_empty")
    if unknown:
        return "unknown", unknown
    if failed:
        return "failed", failed
    if containment_proof is None:
        # Every observable fact is clean, but privilege-changing escapes are not
        # excluded by any reviewed backend.
        return "unknown", ["containment_unproven"]
    return "unknown", ["containment_proof_unreviewed"]

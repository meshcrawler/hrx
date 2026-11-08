"""Real Pitput/Postgres -> HRX targeted launch path -> private Codex/OpenCode -> model fixture.

Run with: make test-pitput-targeted PITPUT_SOURCE=/path/to/pitput
CI runs it as the pitput-targeted-launch native step. No live profiles, forum
messages or paid model endpoints are used.

Optional, for this suite only: HRX_PITPUT_SOURCE_SHA=<full 40-hex commit> builds the
Pitput wheel from exactly that commit, installs it with the commit's locked
dependencies into a private virtualenv, verifies the installed files against the
wheel and re-runs this harness under it. The source is HRX_PITPUT_SOURCE_URL, or in
GitLab CI the server's project named by HRX_PITPUT_PROJECT_PATH, fetched with the job token. The
repository pin (tests/compatibility/ci/toolchain.json) is never changed.

Each native Go test runs inside the reviewed ownership boundary
(tests/compatibility/subreaper.py). On every exit path the supervisor is stopped
(SIGTERM, bounded wait, SIGKILL only if it does not finish) and PASS requires its
summary to prove complete, unforced cleanup. Known secrets (the DM body sentinel) are
redacted from every diagnostic; a leak fails the run without printing the value.

Every case reports PASS, FAIL or BLOCKED; BLOCKED (exit 2) is never a pass.
"""

import argparse
import atexit
import base64
import hashlib
import importlib.metadata
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import unittest

from pitput_exchange import PitputIntegration, pitput, private


ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "tests/compatibility"))
import ownership  # noqa: E402  (reviewed boundary classification, stdlib only)

SUPERVISOR = ROOT / "tests/compatibility/subreaper.py"
GRACE = 10
REDACTED = "[REDACTED]"
FULL_SHA = re.compile(r"[0-9a-f]{40}")
FOLLOWED, UNRELATED, MENTIONED = "/b/exchange/followed", "/b/exchange/unrelated", "/b/exchange/mentioned"
CONTROLLER = "test/controller/agent"
HARNESSES = {"codex": ("TestNativePitputTargetedLaunchCodex", "test/codextgt/agent"),
             "opencode": ("TestNativePitputTargetedLaunchOpenCode", "test/opencodetgt/agent")}
# The Go test writes HARNESS-PHASE.ready in this order; each is answered once.
PHASES = ("fresh-B", "fresh-C", "fresh-D", "fresh-E", "resume-B", "resume-C")
BINARY = None


def checked(args, *, env=None, cwd=None, timeout=300):
    """Run a provenance step; output is suppressed because it may name private paths."""
    result = subprocess.run([str(arg) for arg in args], env=env, cwd=cwd, capture_output=True,
                            text=True, timeout=timeout)
    if result.returncode:
        raise SystemExit(f"pitput source override failed at `{Path(str(args[0])).name} "
                         f"{' '.join(str(a) for a in args[1:3])}` ({result.returncode})")
    return result.stdout.strip()


def sha256_file(path):
    with open(path, "rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def source_remote():
    """Explicit URL, or this GitLab's Pitput project with the job token in a header
    (never in argv, URL or output)."""
    env = dict(os.environ, GIT_TERMINAL_PROMPT="0")
    if os.environ.get("HRX_PITPUT_SOURCE_URL"):
        return os.environ["HRX_PITPUT_SOURCE_URL"], env
    server, token = os.environ.get("CI_SERVER_URL"), os.environ.get("CI_JOB_TOKEN")
    if not server or not token:
        raise SystemExit("HRX_PITPUT_SOURCE_SHA needs HRX_PITPUT_SOURCE_URL outside GitLab CI")
    basic = base64.b64encode(f"gitlab-ci-token:{token}".encode()).decode()
    env.update(GIT_CONFIG_COUNT="1", GIT_CONFIG_KEY_0="http.extraHeader",
               GIT_CONFIG_VALUE_0="Authorization: Basic " + basic)
    project = os.environ.get("HRX_PITPUT_PROJECT_PATH", "")
    if not project:
        raise SystemExit("HRX_PITPUT_SOURCE_SHA in GitLab CI needs HRX_PITPUT_PROJECT_PATH (e.g. acme/pitput)")
    return f"{server.rstrip('/')}/{project}.git", env


VERIFY = r"""
import hashlib, importlib.metadata, json, sys, zipfile, base64
wheel = sys.argv[1]
dist = importlib.metadata.distribution("pitput")
with zipfile.ZipFile(wheel) as archive:
    record = next(n for n in archive.namelist() if n.endswith(".dist-info/RECORD"))
    rows = [line.split(",") for line in archive.read(record).decode().splitlines() if line]
checked = mismatched = 0
for path, digest, _ in rows:
    if not path.startswith("ysg_mail/") or not digest.startswith("sha256="):
        continue
    data = dist.locate_file(path).read_bytes()
    actual = base64.urlsafe_b64encode(hashlib.sha256(data).digest()).rstrip(b"=").decode()
    checked += 1
    mismatched += actual != digest[7:]
import ysg_mail
print(json.dumps({"distribution_version": dist.version, "files_verified": checked,
                  "files_mismatched": mismatched, "module_in_prefix": ysg_mail.__file__.startswith(sys.prefix)}))
"""


def build_override(sha, *, run=None, mkdtemp=tempfile.mkdtemp, rmtree=shutil.rmtree, tmp="/tmp"):
    """Fetch exactly SHA, build its wheel, install with locked dependencies, verify.

    The work directory (source, wheels, venv) is owned from creation: any failure
    removes it before the error propagates. On success it is returned, still owned
    by this process until reexec_override hands it to the re-executed interpreter."""
    work = Path(mkdtemp(prefix="hrx-pitput-src-", dir=tmp)).resolve()
    try:
        return _build_override(sha, work, run or checked)
    except BaseException:
        rmtree(work, ignore_errors=True)
        raise


def _build_override(sha, work, checked):
    source, wheels, venv = work / "source", work / "wheels", work / "venv"
    url, git_env = source_remote()
    checked(["git", "init", "-q", source])
    checked(["git", "-C", source, "fetch", "-q", "--depth", "1", "--no-tags", url, sha], env=git_env)
    checked(["git", "-C", source, "checkout", "-q", "--detach", "FETCH_HEAD"])
    head = checked(["git", "-C", source, "rev-parse", "HEAD"])
    if head != sha or checked(["git", "-C", source, "status", "--porcelain"]):
        raise SystemExit("pitput source override did not check out the exact clean commit")
    checked(["uv", "build", "--wheel", "--out-dir", wheels, source], timeout=900)
    built = sorted(wheels.glob("pitput-*.whl"))
    if len(built) != 1:
        raise SystemExit("pitput source override must build exactly one wheel")
    wheel, requirements = built[0], work / "requirements.txt"
    wheel_sha = sha256_file(wheel)
    checked(["uv", "venv", "-q", "--python", sys.executable, venv])
    python = venv / "bin/python"
    # The commit's own lock (with hashes) pins server dependencies; the project
    # itself comes only from the verified wheel.
    checked(["uv", "export", "--project", source, "--frozen", "--extra", "server", "--no-dev",
             "--no-emit-project", "--no-header", "-o", requirements])
    checked(["uv", "pip", "install", "-q", "--python", python, "-r", requirements], timeout=900)
    checked(["uv", "pip", "install", "-q", "--python", python, "--no-deps", wheel])
    installed = json.loads(checked([python, "-c", VERIFY, wheel]))
    if installed["files_verified"] == 0 or installed["files_mismatched"] or not installed["module_in_prefix"]:
        raise SystemExit("installed Pitput does not match the wheel built from the requested commit")
    checked([venv / "bin/pitput", "--help"])
    provenance = {"mode": "source_override", "requested_commit": sha, "checked_out_commit": head,
                  "wheel": wheel.name, "wheel_sha256": wheel_sha,
                  "lock_sha256": sha256_file(source / "uv.lock"), "python": str(python),
                  "cli_help_ok": True, **installed}
    path = work / "provenance.json"
    path.write_text(json.dumps(provenance, indent=2) + "\n")
    return python, path, work


def reexec_override(sha, *, build=build_override, execve=os.execve, rmtree=shutil.rmtree,
                    entrypoint=None):
    """Build, then replace this process with the venv interpreter. A successful execve
    never returns and runs no atexit handler, so the work directory survives exactly
    into the new image, which adopts it (adopt_override_workdir). If execve fails the
    directory is still ours and is removed here."""
    python, path, work = build(sha)
    env = dict(os.environ, HRX_PITPUT_OVERRIDE_ACTIVE=sha, HRX_PITPUT_OVERRIDE_PROVENANCE=str(path),
               HRX_PITPUT_OVERRIDE_WORKDIR=str(work))
    try:
        execve(str(python), [str(python), str(Path(entrypoint or __file__).resolve()),
                            *sys.argv[1:]], env)
    except OSError:
        rmtree(work, ignore_errors=True)
        raise
    raise RuntimeError("execve returned")


def adopt_override_workdir(env=os.environ, *, register=atexit.register, rmtree=shutil.rmtree,
                           on_signal=signal.signal):
    """The final (re-executed) process owns the override work directory: remove it on
    normal exit, SystemExit and SIGTERM. Called before any verification can fail."""
    work = env.get("HRX_PITPUT_OVERRIDE_WORKDIR")
    if not work or not Path(work).name.startswith("hrx-pitput-src-"):
        raise SystemExit("pitput source override work directory was not handed over")
    register(rmtree, work, True)
    on_signal(signal.SIGTERM, lambda *_: sys.exit(128 + signal.SIGTERM))
    return Path(work)


def pitput_provenance(*, entrypoint=None):
    """Return verified Pitput provenance; for an override, re-exec under its venv."""
    sha = os.environ.get("HRX_PITPUT_SOURCE_SHA", "")
    if not sha:
        pins = json.loads((ROOT / "tests/compatibility/ci/toolchain.json").read_text())
        info = {"mode": "ambient", "repository_pin": pins["pitput_commit"]}
        manifest = Path("/opt/hrx-ci/manifest.json")
        if manifest.exists():
            info["image_manifest_commit"] = json.loads(manifest.read_text()).get("pitput_commit")
        try:
            info["distribution_version"] = importlib.metadata.version("pitput")
        except importlib.metadata.PackageNotFoundError:
            info["distribution_version"] = None
        return info
    if not FULL_SHA.fullmatch(sha):
        raise SystemExit("HRX_PITPUT_SOURCE_SHA must be a full 40-character lowercase hex commit; "
                         "branches, tags, short SHAs and 'latest' are rejected")
    if os.environ.get("HRX_PITPUT_OVERRIDE_ACTIVE") != sha:
        if entrypoint is None:
            reexec_override(sha)
        else:
            reexec_override(sha, entrypoint=entrypoint)
    work = adopt_override_workdir()
    path = Path(os.environ["HRX_PITPUT_OVERRIDE_PROVENANCE"])
    if path.parent != work:
        raise SystemExit("pitput source override provenance is outside its work directory")
    provenance = json.loads(path.read_text())
    # Re-verify inside the re-executed process: this interpreter, this commit.
    import ysg_mail
    if (provenance["requested_commit"] != sha or provenance["checked_out_commit"] != sha
            or Path(sys.executable).resolve() != Path(provenance["python"]).resolve()
            or not ysg_mail.__file__.startswith(sys.prefix)
            or importlib.metadata.version("pitput") != provenance["distribution_version"]):
        raise SystemExit("pitput source override provenance does not match the running interpreter")
    provenance["verified_in_process"] = True
    return provenance


def sanitize(text, secrets_):
    """The one redaction point for every diagnostic: returns (clean text, leaked)."""
    text = "" if text is None else str(text)
    leaked = False
    for secret in secrets_:
        if secret and secret in text:
            leaked = True
            text = text.replace(secret, REDACTED)
    return text, leaked


def read_json(path):
    try:
        return json.loads(Path(path).read_text())
    except (OSError, ValueError):
        return None


def supervisor_running(process):
    """True while the supervisor has not exited. Never reaps (WNOWAIT), so its PID
    stays ours to signal until stop_owned has finished."""
    if process.returncode is not None:
        return False
    try:
        return os.waitid(os.P_PID, process.pid, os.WEXITED | os.WNOHANG | os.WNOWAIT) is None
    except ChildProcessError:
        return False


def start_owned(command, *, summary, env, cwd, log, popen=subprocess.Popen):
    """Run command under the subreaper supervisor: it owns every descendant, including
    HRX bridges and servers that run in their own process groups."""
    return popen([sys.executable, "-I", str(SUPERVISOR), "--summary", str(summary),
                  "--grace", str(GRACE), "--", *command],
                 env=env, cwd=cwd, stdout=log, stderr=subprocess.STDOUT,
                 stdin=subprocess.DEVNULL, start_new_session=True)


def stop_owned(process, summary, *, grace=GRACE, kill=os.kill, alive=supervisor_running, read=read_json):
    """Stop the supervisor and classify cleanup. Only the supervisor's own unreaped PID
    is ever signalled: SIGTERM, a bounded wait, SIGKILL only if it did not finish.

    clean                  summary proves complete, unforced cleanup and exit 0
    owned_cleanup_forced   descendants had to be SIGKILLed
    owned_cleanup_unknown  no/unreadable summary, no boundary, or supervisor killed/unreaped"""
    result = {"signals": [], "supervisor_reaped": False, "supervisor_exit": None,
              "summary": None, "leader_exit": None}
    if alive(process):
        result["signals"].append("SIGTERM")
        kill(process.pid, signal.SIGTERM)
    try:
        process.wait(grace + 20)
    except subprocess.TimeoutExpired:
        result["signals"].append("SIGKILL")
        kill(process.pid, signal.SIGKILL)
        try:
            process.wait(5)
            result["supervisor_reaped"] = True
            result["supervisor_exit"] = process.returncode
        except subprocess.TimeoutExpired:
            pass
        result["classification"] = "owned_cleanup_unknown"
        return result
    result["supervisor_reaped"] = True
    result["supervisor_exit"] = process.returncode
    record = read(summary)
    main = read(str(summary) + ".main")
    result["summary"] = record
    result["leader_exit"] = main.get("status") if isinstance(main, dict) else None
    state = ownership.classify_summary(record, process.returncode)
    if state == "clean":
        result["classification"] = "clean"
    elif isinstance(record, dict) and record.get("forced"):
        result["classification"] = "owned_cleanup_forced"
    else:
        result["classification"] = "owned_cleanup_unknown"
    return result


class ReadyTimeout(AssertionError):
    pass


def wait_ready(path, leader_done, *, timeout=240, clock=time.monotonic, sleep=time.sleep):
    """True once the Go test wrote path; False if the leader finished first."""
    deadline = clock() + timeout
    while not path.exists():
        if leader_done():
            return False
        if clock() > deadline:
            raise ReadyTimeout(f"native test never reached {path.name}")
        sleep(0.1)
    return True


def drive(start, stop, phases, wait, act, publish, wait_leader, state):
    """Run the handshake under an owned process; stop() runs on every exit path
    (ready timeout, act/publish failure, leader timeout, normal completion) and its
    classification lands in state["cleanup"] even when an error propagates."""
    process = start()
    posted = state.setdefault("posted", {})
    try:
        for phase in phases:
            if not wait(process, phase):
                break
            answer = act(phase)
            publish(phase, answer)
            posted[phase] = answer
        wait_leader(process)
    finally:
        state["cleanup"] = stop(process)
    return posted


def evaluate(cleanup, output, test, leaked):
    """Outcome before the receipt cross-check; None means continue to cross-check."""
    if leaked:
        return ("FAIL", "dm_sentinel_leak")
    if cleanup is None or cleanup.get("classification") != "clean":
        return ("FAIL", (cleanup or {}).get("classification") or "owned_cleanup_unknown")
    if "BLOCKED pitput_targeted_unsupported" in output:
        return ("BLOCKED", "pitput_targeted_unsupported")
    if (cleanup.get("leader_exit") != 0 or f"--- PASS: {test}" not in output
            or "--- SKIP" in output):
        return ("FAIL", "native_test_failed")
    return None


# Every phase the Go test persists, in order; PASS needs all of them.
RECEIPT_PHASES = ("fresh-B", "fresh-C", "fresh-D", "fresh-E", "resume-B", "resume-consumers",
                  "resume-C", "negative")


def partial_check(receipt, posted):
    """Cross-check whatever phases the Go receipt persisted against what this harness
    posted. Returns fixed inconsistency codes (never content); an empty list for a
    partial receipt is evidence for the completed phases only, never a PASS."""
    if not isinstance(receipt, dict):
        return ["no_receipt"]
    completed = receipt.get("phases_completed") or []
    seqs = {phase: {post["kind"]: post["seq"] for post in posts} for phase, posts in posted.items()}
    problems = []

    def check(phase, ok, code):
        if not ok:
            problems.append(f"{phase}:{code}")

    for phase in completed:
        generation = phase.split("-", 1)[0]
        section = receipt.get(generation) or {}
        if phase.endswith("-B"):
            quiet, posts = section.get("quiet") or {}, seqs.get(phase, {})
            check(phase, quiet.get("unrelated_seq") == posts.get("unrelated"), "unrelated_seq_mismatch")
            check(phase, quiet.get("self_authored_followed_seq") == posts.get("self_followed"), "self_seq_mismatch")
            check(phase, (quiet.get("observed_window_ms") or 0) >= 20000, "window_too_short")
            check(phase, (quiet.get("post_injection_samples") or 0) >= 10, "too_few_samples")
            check(phase, (quiet.get("injection_after_baseline_ms") or 0) > 0, "injection_before_baseline")
            check(phase, quiet.get("requests_at_baseline") == quiet.get("requests_at_window_end"), "requests_changed")
            check(phase, not any(quiet.get("board_pending_per_sample") or [True])
                  and not any(quiet.get("dm_pending_per_sample") or [True]), "pending_observed")
        elif phase.endswith("-C") or phase == "fresh-D":
            key, kind = ("followed", "followed_no_mention") if phase.endswith("-C") else ("mention", "mention_unfollowed")
            result = section.get(key) or {}
            check(phase, result.get("seq") == seqs.get(phase, {}).get(kind), "seq_mismatch")
            check(phase, result.get("delivered") is True and result.get("handle_bound_to_event") is True,
                  "delivery_not_bound")
            if phase.endswith("-C"):
                quiet = section.get("quiet") or {}
                check(phase, (result.get("seq") or 0) > (quiet.get("max_quiet_seq") or float("inf")),
                      "seq_barrier_not_passed")
        elif phase == "fresh-E":
            result = section.get("dm") or {}
            check(phase, result.get("via_dm_bridge") is True and result.get("handle_bound_to_event") is True
                  and result.get("body_leaked") is False and result.get("message_matches_send") is True
                  and result.get("event_key_matches_send") is True, "dm_not_bound")
        elif phase == "resume-consumers":
            consumers = section.get("consumers") or {}
            check(phase, (consumers.get("board_bridges"), consumers.get("dm_bridges"),
                          consumers.get("max_board_monitors")) == (1, 1, 1)
                  and consumers.get("duplicate_launch_refused") is True
                  and consumers.get("monitor_registration_unchanged") is True, "extra_consumer")
        elif phase == "negative":
            negative = receipt.get("negative") or {}
            check(phase, negative.get("code") == "bridge_selection_migration_required"
                  and negative.get("files_byte_identical") is True, "negative_not_proven")
    return problems


def receipt_complete(receipt):
    """Only a receipt with every phase, in order, can support a PASS."""
    return isinstance(receipt, dict) and tuple(receipt.get("phases_completed") or ()) == RECEIPT_PHASES


# Pitput codex_bridge._event_key.
EVENT_KEY = re.compile(r"[A-Za-z0-9][A-Za-z0-9:._-]{0,255}")


def post_ref(result):
    path = result["path"]
    seq = result.get("seq") or int(path.rsplit("/", 1)[1])
    if not isinstance(seq, int) or seq <= 0:
        raise AssertionError("Pitput post returned no sequence")
    ref = {"path": path, "seq": seq}
    if isinstance(result.get("event_key"), str) and EVENT_KEY.fullmatch(result["event_key"]):
        ref["event_key"] = result["event_key"]
    return ref


def dm_ref(sent):
    """The exact identifiers Pitput's DM send returned (direct_messages.send): only this
    message may satisfy case E. Never includes the body."""
    if (not isinstance(sent, dict) or sent.get("stored") is not True
            or not re.fullmatch(r"dm-[a-z2-7]{12}", str(sent.get("message")))
            or not re.fullmatch(r"dc-[a-z2-7]{12}", str(sent.get("conversation")))
            or not EVENT_KEY.fullmatch(str(sent.get("event_key")))):
        raise AssertionError("DM send did not return a stored message id, conversation and event key")
    return {"kind": "dm", "path": sent["message"], "seq": 0, "event_key": sent["event_key"],
            "conversation": sent["conversation"]}


class TargetedLaunchIntegration(PitputIntegration):
    outcomes = {}
    receipts = {}
    consistency = {}
    cleanups = {}
    leaks = {}

    def test_codex(self):
        self.run_harness("codex")

    def test_opencode(self):
        self.run_harness("opencode")

    def act(self, harness, identity, env, phase, secrets_):
        """Perform one phase's posts and return the bounded handshake (no bodies,
        except the DM sentinel the Go test needs to prove the body never leaks). The
        sentinel is registered for redaction before it is ever used."""
        tag = f"{harness}-{phase}"
        controller = [*self.prefix, "post"]
        sentinel = ""
        if phase.endswith("-B"):
            unrelated = pitput(self.controller, *controller, UNRELATED, "--author", CONTROLLER,
                               "--body", f"Unrelated board activity {tag}", "--request-id", tag + "-unrelated")
            # Self-authored activity in the followed topic, even mentioning itself, is excluded.
            own = pitput(env, "--agent", identity, "post", FOLLOWED, "--author", identity,
                         "--body", f"@{identity} self-authored {tag}", "--request-id", tag + "-self")
            posts = [{"kind": "unrelated", **post_ref(unrelated)}, {"kind": "self_followed", **post_ref(own)}]
        elif phase.endswith("-C"):
            followed = pitput(self.controller, *controller, FOLLOWED, "--author", CONTROLLER,
                              "--body", f"Followed topic activity without mention {tag}",
                              "--request-id", tag + "-followed")
            posts = [{"kind": "followed_no_mention", **post_ref(followed)}]
        elif phase.endswith("-D"):
            mention = pitput(self.controller, *controller, MENTIONED, "--author", CONTROLLER,
                             "--body", f"@{identity} mention outside followed topics {tag}",
                             "--request-id", tag + "-mention")
            posts = [{"kind": "mention_unfollowed", **post_ref(mention)}]
        else:
            sentinel = "dm-body-sentinel-" + secrets.token_hex(8)
            secrets_.append(sentinel)
            sent = pitput(self.controller, *self.prefix, "dm", "send", identity, "--body", sentinel,
                          "--request-id", tag + "-dm")
            posts = [dm_ref(sent)]
        return {"posted_at_ms": int(time.time() * 1000), "posts": posts, "sentinel": sentinel}

    def run_harness(self, harness):
        test, identity = HARNESSES[harness]
        type(self).outcomes[harness] = ("FAIL", "see job log")
        env = self.agent_env("targeted-" + harness)
        home = Path(env["HOME"])
        private(home / "bin/pitput", f"#!/bin/sh\nexec {shlex.quote(sys.executable)} -m ysg_mail.pitput \"$@\"\n")
        (home / "bin/pitput").chmod(0o700)
        handshake, receipt, summary = home / "handshake", home / "receipt.json", home / "supervisor.json"
        handshake.mkdir(mode=0o700)
        env.update(PATH=str(home / "bin") + os.pathsep + self.env["PATH"],
                   HRX_NATIVE_SESSION_TESTS="1", HRX_PITPUT_INTEGRATION="1",
                   HRX_PITPUT_TEST_IDENTITY=identity, HRX_PITPUT_TARGETED_HANDSHAKE=str(handshake),
                   HRX_PITPUT_TARGETED_RECEIPT=str(receipt))
        # Pre-existing history in every topic; a new targeted bridge must skip it.
        for topic in (FOLLOWED, UNRELATED, MENTIONED):
            pitput(self.controller, *self.prefix, "create", topic, "--author", CONTROLLER,
                   "--title", "Targeted fixture " + topic.rsplit("/", 1)[1],
                   "--body", f"@{identity} historical seed", "--request-id", "seed-" + topic.rsplit("/", 1)[1])
        log = open(home / "native.log", "w+")
        self.addCleanup(log.close)
        secrets_, state = [], {}

        def leader_done(process):
            return not supervisor_running(process) or (Path(str(summary) + ".main")).exists()

        def wait_leader(process):
            deadline = time.monotonic() + 500
            while not leader_done(process):
                if time.monotonic() > deadline:
                    raise AssertionError("native test exceeded its deadline")
                time.sleep(0.2)

        def publish(phase, answer):
            target = handshake / f"{harness}-{phase}.posted"
            private(target.with_suffix(".tmp"), json.dumps(answer))
            target.with_suffix(".tmp").rename(target)

        failure = None
        try:
            posted = drive(
                start=lambda: start_owned([BINARY, f"-test.run=^{test}$", "-test.timeout=480s", "-test.v"],
                                          summary=summary, env=env, cwd=ROOT, log=log),
                stop=lambda process: stop_owned(process, summary),
                phases=PHASES,
                wait=lambda process, phase: wait_ready(handshake / f"{harness}-{phase}.ready",
                                                       lambda: leader_done(process)),
                act=lambda phase: self.act(harness, identity, env, phase, secrets_),
                publish=publish, wait_leader=wait_leader, state=state)
        except Exception as error:  # re-raised below, sanitized, after cleanup is classified
            posted, failure = state.get("posted", {}), f"{type(error).__name__}: {error}"
        cleanup = state.get("cleanup")
        type(self).cleanups[harness] = cleanup
        log.seek(0)
        output, leaked = sanitize(log.read(), secrets_)
        receipt_text, receipt_leaked = sanitize(receipt.read_text() if receipt.exists() else "", secrets_)
        failure, failure_leaked = sanitize(failure, secrets_)
        leaked = leaked or receipt_leaked or failure_leaked
        type(self).leaks[harness] = leaked
        print(output[-8000:], flush=True)
        native = json.loads(receipt_text) if receipt_text else None
        if native is not None:
            type(self).receipts[harness] = native
            if native.get("diagnostics"):
                # Content-free by construction and already sanitized.
                print("delivery diagnostics: " + json.dumps(native["diagnostics"], sort_keys=True), flush=True)
        problems = partial_check(native, {p: a["posts"] for p, a in posted.items()})
        type(self).consistency[harness] = {
            "phases_completed": (native or {}).get("phases_completed", []), "inconsistencies": problems}
        outcome = evaluate(cleanup, output, test, leaked)
        if outcome is None and failure:
            outcome = ("FAIL", "harness_error")
        if outcome is not None:
            type(self).outcomes[harness] = outcome
            # Only sanitized text reaches the failure message; `from None` drops the
            # original exception whose message may have carried a secret.
            raise AssertionError(f"{outcome[0]} {outcome[1]}" + (f": {failure}" if failure else "")) from None
        self.assertEqual(list(posted), list(PHASES), "not every handshake phase ran")
        self.assertTrue(receipt_complete(native), "receipt is partial")
        self.assertEqual(problems, [], "receipt disagrees with the harness posts")
        self.cross_check(type(self).receipts.get(harness), {p: a["posts"] for p, a in posted.items()})
        type(self).outcomes[harness] = ("PASS", "")

    def cross_check(self, receipt, posted):
        """The Go receipt must carry the seqs this harness actually posted, and its
        measured windows must meet the requirement independently of the Go assertions."""
        self.assertIsNotNone(receipt, "native test wrote no receipt")
        self.assertTrue(receipt["passed"])
        seqs = {phase: {post["kind"]: post["seq"] for post in posts} for phase, posts in posted.items()}
        for generation in ("fresh", "resume"):
            quiet = receipt[generation]["quiet"]
            followed = receipt[generation]["followed"]
            self.assertEqual(quiet["unrelated_seq"], seqs[generation + "-B"]["unrelated"])
            self.assertEqual(quiet["self_authored_followed_seq"], seqs[generation + "-B"]["self_followed"])
            self.assertEqual(followed["seq"], seqs[generation + "-C"]["followed_no_mention"])
            self.assertGreater(followed["seq"], quiet["unrelated_seq"])
            self.assertTrue(followed["delivered"])
            self.assertGreaterEqual(quiet["observed_window_ms"], 20000)
            self.assertGreaterEqual(quiet["post_injection_samples"], 10)
            self.assertGreater(quiet["injection_after_baseline_ms"], 0)
            self.assertEqual(quiet["requests_at_baseline"], quiet["requests_at_window_end"])
            self.assertFalse(any(quiet["board_pending_per_sample"]) or any(quiet["dm_pending_per_sample"]))
        self.assertEqual(receipt["fresh"]["mention"]["seq"], seqs["fresh-D"]["mention_unfollowed"])
        self.assertTrue(receipt["fresh"]["dm"]["via_dm_bridge"])
        self.assertTrue(receipt["resume"]["selection_identical"])
        consumers = receipt["resume"]["consumers"]
        self.assertTrue(consumers["duplicate_launch_refused"] and consumers["monitor_registration_unchanged"])
        self.assertEqual((consumers["board_bridges"], consumers["dm_bridges"], consumers["max_board_monitors"]), (1, 1, 1))
        negative = receipt["negative"]
        self.assertEqual(negative["code"], "bridge_selection_migration_required")
        self.assertTrue(negative["files_byte_identical"] and negative["session_record_unchanged"])
        self.assertEqual(negative["pending_preserved_seq"], seqs["resume-C"]["followed_no_mention"])
        config = receipt["bridge_config_resume"]
        self.assertTrue(config["board_targeted"] and config["board_followed_matches"])
        self.assertFalse(config["dm_has_selection_keys"])


def blocked_reason(harness):
    """Missing prerequisites are reported as BLOCKED, never as a skipped success."""
    if ownership.available(sys.platform).mechanism != ownership.LINUX_SUBREAPER:
        return "owned_cleanup_boundary_unavailable"
    if not any(shutil.which(name) for name in ("initdb", "pg_ctl")) and not list(
            Path("/opt/homebrew/opt").glob("postgresql*/bin/initdb")):
        return "postgresql_binaries_missing"
    if shutil.which("go") is None:
        return "go_toolchain_missing"
    if shutil.which(harness) is None:
        return harness + "_binary_missing"
    return None


def main():
    global BINARY
    from xml.sax.saxutils import quoteattr
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--junit", type=Path)
    parser.add_argument("--receipt", type=Path, default=ROOT / "artifacts/ci/pitput-targeted-launch.json")
    arguments = parser.parse_args()
    provenance = pitput_provenance()
    scratch = Path(tempfile.mkdtemp(prefix="hrx-targeted-bin-", dir="/tmp"))
    atexit.register(shutil.rmtree, scratch, True)
    results = {}
    for harness in HARNESSES:
        reason = blocked_reason(harness)
        if reason:
            results[harness] = ("BLOCKED", reason)
            continue
        if BINARY is None:
            BINARY = str(scratch / "hrx-native.test")
            subprocess.run(["go", "test", "-c", "-race", "-o", BINARY, "./internal/app"],
                           cwd=ROOT, check=True, timeout=300)
        # A suite runs the class fixtures (disposable PostgreSQL, Pitput server).
        outcome = unittest.TextTestRunner(verbosity=2).run(
            unittest.TestSuite([TargetedLaunchIntegration("test_" + harness)]))
        status = TargetedLaunchIntegration.outcomes.get(harness, ("FAIL", "fixture setup failed"))
        if status[0] == "PASS" and not outcome.wasSuccessful():
            status = ("FAIL", "see job log")
        results[harness] = status
    receipt = {"schema_version": 1, "pitput": provenance,
               "results": {h: {"status": s, "reason": r} for h, (s, r) in results.items()},
               "owned_cleanup": {h: {key: c.get(key) for key in ("classification", "signals", "supervisor_reaped",
                                                                 "supervisor_exit", "leader_exit", "summary")}
                                 for h, c in TargetedLaunchIntegration.cleanups.items() if c},
               "dm_sentinel_leaked": TargetedLaunchIntegration.leaks,
               "consistency": TargetedLaunchIntegration.consistency,
               "native": TargetedLaunchIntegration.receipts}
    arguments.receipt.parent.mkdir(parents=True, exist_ok=True)
    arguments.receipt.write_text(json.dumps(receipt, indent=2) + "\n")
    print(f"receipt: {arguments.receipt}")
    print(json.dumps(receipt, indent=2))
    for harness, (status, reason) in results.items():
        print(f"{status:7} pitput-targeted-launch-{harness}" + (f": {reason}" if reason else ""))
    if arguments.junit:
        lines = [f'<testsuite name="pitput-targeted-launch" tests="{len(results)}">']
        for harness, (status, reason) in results.items():
            lines.append(f'  <testcase classname="pitput-targeted-launch" name="{harness}">')
            if status == "FAIL":
                lines.append(f"    <failure message={quoteattr(reason)}/>")
            elif status == "BLOCKED":
                lines.append(f"    <error message={quoteattr('BLOCKED: ' + reason)}/>")
            lines.append("  </testcase>")
        arguments.junit.parent.mkdir(parents=True, exist_ok=True)
        arguments.junit.write_text("\n".join([*lines, "</testsuite>"]) + "\n")
    statuses = {status for status, _ in results.values()}
    return 1 if "FAIL" in statuses else 2 if "BLOCKED" in statuses else 0


if __name__ == "__main__":
    sys.exit(main())

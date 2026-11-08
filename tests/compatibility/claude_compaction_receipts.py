"""Pure, bounded projections for the pinned Claude compaction fixture.

These projections do not establish live coverage, trigger compaction, or read a
transcript. The fixture must supply records from its owned current session.
"""

import hashlib
import json
import re


VERSION = "2.1.306"
MAX_RECORD_BYTES = 1024 * 1024
UUID = re.compile(r"[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}")


def require(condition, reason):
    if not condition:
        raise ValueError(reason)


def digest(value):
    return hashlib.sha256(value.encode()).hexdigest()


def record(raw):
    require(type(raw) is bytes and 0 < len(raw) <= MAX_RECORD_BYTES, "record_size")

    def pairs(items):
        result = {}
        for key, value in items:
            require(key not in result, "duplicate_key")
            result[key] = value
        return result

    def reject_constant(value):
        raise ValueError("invalid_json_constant")

    try:
        value = json.loads(raw, object_pairs_hook=pairs, parse_constant=reject_constant)
    except (ValueError, UnicodeError, RecursionError):
        raise ValueError("invalid_record") from None
    require(type(value) is dict, "record_type")
    return value


def session(value, expected):
    require(type(expected) is str and UUID.fullmatch(expected) is not None,
            "expected_session_invalid")
    require(type(value) is str and value == expected, "foreign_session")
    return digest(value)


def hook(raw, *, expected_session, expected_cwd):
    value = record(raw)
    bound = session(value.get("session_id"), expected_session)
    require(type(expected_cwd) is str and expected_cwd.startswith("/")
            and value.get("cwd") == expected_cwd, "foreign_cwd")
    event, trigger = value.get("hook_event_name"), value.get("trigger")
    require(event in ("PreCompact", "PostCompact"), "wrong_hook")
    require(trigger in ("auto", "manual"), "unknown_trigger")
    # compact_summary/custom_instructions and transcript paths never escape.
    return {"event": event, "trigger": trigger, "session_digest": bound,
            "cwd_digest": digest(expected_cwd)}


def boundary(raw, *, expected_session):
    value = record(raw)
    bound = session(value.get("sessionId"), expected_session)
    require(value.get("version") == VERSION, "wrong_version")
    require(value.get("type") == "system" and value.get("subtype") == "compact_boundary",
            "wrong_boundary")
    require(value.get("isSidechain", False) is False
            and value.get("remoteSourced", False) is False, "nonlocal_boundary")
    identifier, metadata = value.get("uuid"), value.get("compactMetadata")
    require(type(identifier) is str and UUID.fullmatch(identifier) is not None,
            "boundary_id_invalid")
    require(type(metadata) is dict and metadata.get("trigger") in ("auto", "manual"),
            "boundary_trigger_invalid")
    return {"event": "compact_boundary", "trigger": metadata["trigger"],
            "session_digest": bound, "boundary_digest": digest(identifier),
            "version": VERSION}


def automatic_sequence(hooks, boundaries, *, expected_session, expected_cwd):
    """Reject missing, manual, foreign, duplicate or reordered evidence.

    No completion/continuity claim follows from these records alone: the caller
    must independently prove the post-boundary main request and task handling.
    """
    require(type(hooks) is list and len(hooks) == 2, "hook_count")
    require(type(boundaries) is list and len(boundaries) == 1, "boundary_count")
    projected = [hook(raw, expected_session=expected_session, expected_cwd=expected_cwd)
                 for raw in hooks]
    require([item["event"] for item in projected] == ["PreCompact", "PostCompact"],
            "hook_order")
    marker = boundary(boundaries[0], expected_session=expected_session)
    require(all(item["trigger"] == "auto" for item in projected + [marker]),
            "manual_compaction")
    return {"scope": "projected_records_only", "hooks": projected, "boundary": marker}

"""Tests for the result framing contract.

The framing exists because the container runtime merges stdout and stderr into one
stream, so these tests check the contract holds when the record is surrounded by
unrelated output - which is the condition it was designed for, not an edge case.
"""

# pylint: disable=missing-function-docstring
# pylint: disable=missing-class-docstring

from __future__ import annotations

import json

import pytest

from cairn_sidecar.result import emit_failure, emit_result


def _parse_stream(raw: str) -> list[dict]:
    """Decode a combined output stream the way the Go service does: line by line,
    keeping whatever parses as a JSON object."""
    found = []
    for line in raw.splitlines():
        stripped = line.strip()
        if not stripped:
            continue
        try:
            decoded = json.loads(stripped)
        except json.JSONDecodeError:
            continue
        if isinstance(decoded, dict):
            found.append(decoded)
    return found


def test_result_is_a_single_line(capsys: pytest.CaptureFixture[str]) -> None:
    emit_result({"valid": True, "size": 12, "resolved_path": "/mnt/cairn/ws/a.txt"})
    raw = capsys.readouterr().out

    payload_lines = [line for line in raw.splitlines() if line.strip()]
    assert len(payload_lines) == 1


def test_result_is_blank_line_delimited(capsys: pytest.CaptureFixture[str]) -> None:
    emit_result({"valid": True})
    raw = capsys.readouterr().out

    assert raw.startswith("\n")
    assert raw.endswith("\n")


def test_result_survives_surrounding_noise(
    capsys: pytest.CaptureFixture[str],
) -> None:
    """The whole point of the framing: stray writes on the merged stream must not stop
    the service from finding the result."""
    print("WARNING: something unrelated wrote to the stream")
    emit_result({"valid": True, "size": 7})
    print("another trailing line, not JSON")

    parsed = _parse_stream(capsys.readouterr().out)
    assert parsed == [{"valid": True, "size": 7}]


def test_result_is_not_split_by_a_partial_preceding_line(
    capsys: pytest.CaptureFixture[str],
) -> None:
    """A leading newline means an un-terminated partial write cannot merge into the
    result line and corrupt it."""
    print("partial write with no newline", end="")
    emit_result({"valid": True})

    parsed = _parse_stream(capsys.readouterr().out)
    assert parsed == [{"valid": True}]


def test_failure_carries_reason_and_extra_fields(
    capsys: pytest.CaptureFixture[str],
) -> None:
    emit_failure("source file does not exist", valid=False, resolved_path=None)

    parsed = _parse_stream(capsys.readouterr().out)
    assert parsed == [
        {
            "ok": False,
            "error": "source file does not exist",
            "valid": False,
            "resolved_path": None,
        }
    ]

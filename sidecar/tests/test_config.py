"""Tests for the environment-variable input surface."""

# pylint: disable=missing-function-docstring
# pylint: disable=missing-class-docstring

from __future__ import annotations

from pathlib import Path

import pytest

from cairn_sidecar import config


def test_mount_root_reads_absolute_path(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv(config.ENV_MOUNT_ROOT, "/mnt/cairn/ws")
    assert config.mount_root() == Path("/mnt/cairn/ws")


def test_mount_root_rejects_relative_path(monkeypatch: pytest.MonkeyPatch) -> None:
    """A relative root would anchor every containment check to the process cwd,
    silently defeating the guard rather than failing visibly."""
    monkeypatch.setenv(config.ENV_MOUNT_ROOT, "mnt/cairn/ws")
    with pytest.raises(config.ConfigError, match="must be an absolute path"):
        config.mount_root()


def test_missing_required_variable_is_legible(monkeypatch: pytest.MonkeyPatch) -> None:
    """A misconfigured container must fail with a sentence, not a KeyError."""
    monkeypatch.delenv(config.ENV_MOUNT_ROOT, raising=False)
    with pytest.raises(config.ConfigError, match=config.ENV_MOUNT_ROOT):
        config.mount_root()


def test_empty_required_variable_treated_as_missing(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv(config.ENV_URL, "")
    with pytest.raises(config.ConfigError, match="is not set"):
        config.presigned_url()


def test_object_size_parses_integer(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv(config.ENV_OBJECT_SIZE, "4096")
    assert config.object_size() == 4096


def test_object_size_rejects_non_integer(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv(config.ENV_OBJECT_SIZE, "4096 bytes")
    with pytest.raises(config.ConfigError, match="must be an integer"):
        config.object_size()


def test_object_size_rejects_negative(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv(config.ENV_OBJECT_SIZE, "-1")
    with pytest.raises(config.ConfigError, match="must be non-negative"):
        config.object_size()


def test_content_type_is_optional(monkeypatch: pytest.MonkeyPatch) -> None:
    """Absent must stay distinguishable from empty: the header is sent only when the
    service actually signed one."""
    monkeypatch.delenv(config.ENV_CONTENT_TYPE, raising=False)
    assert config.content_type() is None

    monkeypatch.setenv(config.ENV_CONTENT_TYPE, "")
    assert config.content_type() is None

    monkeypatch.setenv(config.ENV_CONTENT_TYPE, "text/plain")
    assert config.content_type() == "text/plain"

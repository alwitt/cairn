"""Tests for the stat/hash sidecar."""

# pylint: disable=missing-function-docstring
# pylint: disable=missing-class-docstring

from __future__ import annotations

import base64
import hashlib
import os
import subprocess
from pathlib import Path

import pytest

from cairn_sidecar.stat import stat_source


@pytest.fixture(name="mount")
def mount_fixture(tmp_path: Path) -> Path:
    """A stand-in workspace volume.

    The mount root is an injected parameter rather than a module constant, so tests need
    no real /mnt/cairn/ws - and that it is injectable is itself part of the contract.
    """
    root = tmp_path / "ws"
    root.mkdir()
    return root


def test_regular_file(mount: Path) -> None:
    source = mount / "out.txt"
    source.write_bytes(b"hello cairn\n")

    block = stat_source(source, mount)

    assert block["valid"] is True
    assert block["resolved_path"] == str(source)
    assert block["size"] == 12
    assert "error" not in block


def test_empty_file(mount: Path) -> None:
    """A zero-byte artifact is legitimate; the digest is the SHA-256 of no bytes."""
    source = mount / "empty.bin"
    source.touch()

    block = stat_source(source, mount)

    assert block["valid"] is True
    assert block["size"] == 0
    assert (
        block["sha256_b64"] == base64.b64encode(hashlib.sha256(b"").digest()).decode()
    )


def test_digest_matches_hashlib(mount: Path) -> None:
    payload = b"some artifact bytes" * 1000
    source = mount / "big.bin"
    source.write_bytes(payload)

    block = stat_source(source, mount)

    expected = base64.b64encode(hashlib.sha256(payload).digest()).decode()
    assert block["sha256_b64"] == expected
    assert block["size"] == len(payload)


def test_digest_is_base64_not_hex(mount: Path) -> None:
    """Regression guard on the encoding.

    The presigned PUT binds x-amz-checksum-sha256 as base64; emitting the hex that
    sha256sum prints would get every upload rejected by the object store. Cross-checked
    against the actual sha256sum binary so this pins the real-world relationship, not
    just hashlib agreeing with itself.
    """
    source = mount / "out.txt"
    source.write_bytes(b"hello cairn\n")

    block = stat_source(source, mount)

    hex_digest = subprocess.run(
        ["sha256sum", str(source)],
        capture_output=True,
        text=True,
        check=True,
    ).stdout.split()[0]

    assert block["sha256_b64"] == base64.b64encode(bytes.fromhex(hex_digest)).decode()
    assert block["sha256_b64"] != hex_digest


def test_symlink_resolved_to_target(mount: Path) -> None:
    """Symlinks are accepted, and it is the target that gets measured."""
    target = mount / "real.txt"
    target.write_bytes(b"actual content")
    link = mount / "link.txt"
    link.symlink_to(target)

    block = stat_source(link, mount)

    assert block["valid"] is True
    assert block["resolved_path"] == str(target)
    assert block["size"] == len(b"actual content")


def test_symlink_escaping_mount_rejected(mount: Path, tmp_path: Path) -> None:
    """The containment check, and the reason following symlinks is safe: a link out of
    the volume must not let the sidecar's own files be hashed and uploaded."""
    outside = tmp_path / "secret.txt"
    outside.write_bytes(b"sidecar image content")
    link = mount / "escape.txt"
    link.symlink_to(outside)

    block = stat_source(link, mount)

    assert block["valid"] is False
    assert "outside the workspace volume" in block["error"]


def test_symlink_to_prefix_alike_sibling_rejected(mount: Path) -> None:
    """A sibling directory sharing the mount's name as a string prefix is still outside
    it. Containment compares path components, so `<mount>X` must not be mistaken for a
    path under `<mount>`."""
    sibling = mount.parent / (mount.name + "X")
    sibling.mkdir()
    outside = sibling / "secret.txt"
    outside.write_bytes(b"not in the volume")
    link = mount / "escape.txt"
    link.symlink_to(outside)

    block = stat_source(link, mount)

    assert block["valid"] is False
    assert "outside the workspace volume" in block["error"]


def test_mount_root_itself_is_not_treated_as_outside(mount: Path) -> None:
    """The mount root is inside the volume, so it must fail as a *directory* rather than
    as a containment violation - the two produce very different agent-facing errors."""
    block = stat_source(mount, mount)

    assert block["valid"] is False
    assert "directory" in block["error"]
    assert "outside the workspace volume" not in block["error"]


def test_dot_traversal_back_to_mount_root_is_not_treated_as_outside(
    mount: Path,
) -> None:
    """`<mount>/sub/..` normalizes to the mount root, which is inside the volume - so it
    too must fail as a directory rather than as a containment violation."""
    (mount / "sub").mkdir()

    block = stat_source(mount / "sub" / "..", mount)

    assert block["valid"] is False
    assert "directory" in block["error"]
    assert "outside the workspace volume" not in block["error"]


def test_dot_traversal_escaping_mount_is_rejected(mount: Path, tmp_path: Path) -> None:
    """Traversal that genuinely leaves the volume is a containment violation, and must
    still be reported as one."""
    outside = tmp_path / "secret.txt"
    outside.write_bytes(b"sidecar image content")

    block = stat_source(mount / ".." / "secret.txt", mount)

    assert block["valid"] is False
    assert "outside the workspace volume" in block["error"]


def test_missing_path_reports_null_resolved_path(mount: Path) -> None:
    block = stat_source(mount / "nope.txt", mount)

    assert block["valid"] is False
    assert block["resolved_path"] is None
    assert "does not exist" in block["error"]


def test_directory_rejected(mount: Path) -> None:
    subdir = mount / "adir"
    subdir.mkdir()

    block = stat_source(subdir, mount)

    assert block["valid"] is False
    assert "directory" in block["error"]


def test_broken_symlink_rejected(mount: Path) -> None:
    """A link to a missing target reduces to the not-found case."""
    link = mount / "dangling.txt"
    link.symlink_to(mount / "gone.txt")

    block = stat_source(link, mount)

    assert block["valid"] is False


def test_non_regular_file_rejected(mount: Path) -> None:
    """A FIFO has no meaningful size or hash to bind a PUT URL to."""
    fifo = mount / "pipe"
    os.mkfifo(fifo)

    block = stat_source(fifo, mount)

    assert block["valid"] is False
    assert "regular file" in block["error"]


def test_invalid_block_keeps_the_full_schema(mount: Path) -> None:
    """The service reads these fields unconditionally, so a rejection still owes them."""
    block = stat_source(mount / "nope.txt", mount)

    assert set(block) >= {"resolved_path", "valid", "size", "sha256_b64"}

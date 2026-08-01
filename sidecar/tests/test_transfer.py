"""Tests for the upload and download sidecars.

No network: ``requests.put`` / ``requests.get`` are replaced with recorders, so these
assert the request the sidecar *would* send. That request is the whole contract - a
presigned URL rejects anything whose headers do not match what was signed.
"""

# pylint: disable=missing-function-docstring
# pylint: disable=missing-class-docstring
# The stand-ins below mirror the requests API, so they carry parameters they do not use
# and expose only the one method the code under test calls.
# pylint: disable=too-few-public-methods
# pylint: disable=unused-argument

from __future__ import annotations

from pathlib import Path
from typing import Any

import pytest
import requests

from cairn_sidecar.transfer import (
    SignedUpload,
    TransferError,
    download_file,
    upload_file,
)

SIGNED_URL = (
    "https://store.example.com/bucket/staging/ws-1/abc?X-Amz-Signature=deadbeef"
)


@pytest.fixture(name="mount")
def mount_fixture(tmp_path: Path) -> Path:
    root = tmp_path / "ws"
    root.mkdir()
    return root


class FakeResponse:
    """Minimal stand-in for a requests.Response."""

    def __init__(
        self, status_code: int = 200, chunks: list[bytes] | None = None
    ) -> None:
        self.status_code = status_code
        self.ok = 200 <= status_code < 300
        self._chunks = chunks or []
        self.chunk_size = 0

    def iter_content(self, chunk_size: int) -> list[bytes]:
        self.chunk_size = chunk_size
        return self._chunks

    def __enter__(self) -> FakeResponse:
        return self

    def __exit__(self, *_: object) -> None:
        return None


class PutRecorder:
    """Captures the arguments of a single requests.put call."""

    def __init__(self, response: FakeResponse | None = None) -> None:
        self.response = response or FakeResponse()
        self.url: str | None = None
        self.headers: dict[str, str] = {}
        self.body: bytes = b""

    def __call__(self, url: str, **kwargs: Any) -> FakeResponse:
        self.url = url
        self.headers = kwargs.get("headers", {})
        data = kwargs.get("data")
        if data is not None:
            self.body = data.read()
        return self.response


# ======================================================================================
# Signed header set


def test_signed_upload_builds_the_covered_headers() -> None:
    signed = SignedUpload(SIGNED_URL, 12, "DaUp+NSQ=", "text/plain")

    assert signed.headers() == {
        "Content-Length": "12",
        "x-amz-checksum-sha256": "DaUp+NSQ=",
        "Content-Type": "text/plain",
    }


def test_signed_upload_defaults_to_no_content_type() -> None:
    assert "Content-Type" not in SignedUpload(SIGNED_URL, 12, "abc=").headers()


# ======================================================================================
# Upload


def test_upload_sends_exactly_the_signed_headers(
    mount: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """Content-Length and x-amz-checksum-sha256 are both signed into the URL; a request
    missing either fails the signature check, which is why curl -T could not be used."""
    source = mount / "out.txt"
    source.write_bytes(b"hello cairn\n")
    recorder = PutRecorder()
    monkeypatch.setattr(requests, "put", recorder)

    outcome = upload_file(
        source, mount, SignedUpload(SIGNED_URL, 12, "DaUp+NSQ=", "text/plain")
    )

    assert recorder.url == SIGNED_URL
    assert recorder.headers["Content-Length"] == "12"
    assert recorder.headers["x-amz-checksum-sha256"] == "DaUp+NSQ="
    assert recorder.headers["Content-Type"] == "text/plain"
    assert recorder.body == b"hello cairn\n"
    assert outcome["ok"] is True
    assert outcome["size"] == 12


def test_upload_omits_content_type_when_unsigned(
    mount: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """Sending an unsigned Content-Type breaks the signature exactly as omitting a
    signed one does, so absent must mean absent."""
    source = mount / "out.bin"
    source.write_bytes(b"bytes")
    recorder = PutRecorder()
    monkeypatch.setattr(requests, "put", recorder)

    upload_file(source, mount, SignedUpload(SIGNED_URL, 5, "abc="))

    assert "Content-Type" not in recorder.headers


def test_upload_sends_the_signed_size_not_a_restat(
    mount: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """Content-Length must be the signed value. Re-deriving it from the file would mask
    the drift the checksum bind exists to catch."""
    source = mount / "out.txt"
    source.write_bytes(b"file grew since it was stat'd")
    recorder = PutRecorder()
    monkeypatch.setattr(requests, "put", recorder)

    upload_file(source, mount, SignedUpload(SIGNED_URL, 12, "abc="))

    assert recorder.headers["Content-Length"] == "12"


def test_upload_maps_4xx_to_the_file_changed_error(
    mount: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """The TOCTOU case (DESIGN §6.4): the store rejects a checksum mismatch, and the
    agent needs to be told the file changed, not handed an HTTP status."""
    source = mount / "out.txt"
    source.write_bytes(b"hello")
    monkeypatch.setattr(requests, "put", PutRecorder(FakeResponse(400)))

    with pytest.raises(TransferError, match="changed while the upload was in flight"):
        upload_file(source, mount, SignedUpload(SIGNED_URL, 5, "abc="))


def test_upload_5xx_is_not_reported_as_a_changed_file(
    mount: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    source = mount / "out.txt"
    source.write_bytes(b"hello")
    monkeypatch.setattr(requests, "put", PutRecorder(FakeResponse(503)))

    with pytest.raises(TransferError, match="HTTP 503") as caught:
        upload_file(source, mount, SignedUpload(SIGNED_URL, 5, "abc="))
    assert "changed" not in str(caught.value)


def test_upload_never_leaks_the_url_on_failure(
    mount: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """The signature travels in the query string, so it must not reach a message the
    service will log (DESIGN §5.2)."""
    source = mount / "out.txt"
    source.write_bytes(b"hello")

    def explode(url: str, **kwargs: Any) -> FakeResponse:  # noqa: ARG001
        raise requests.ConnectionError(f"failed connecting to {SIGNED_URL}")

    monkeypatch.setattr(requests, "put", explode)

    with pytest.raises(TransferError) as caught:
        upload_file(source, mount, SignedUpload(SIGNED_URL, 5, "abc="))

    assert "X-Amz-Signature" not in str(caught.value)
    assert SIGNED_URL not in str(caught.value)


def test_upload_rejects_source_escaping_mount(
    mount: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    outside = tmp_path / "secret.txt"
    outside.write_bytes(b"sidecar image content")
    link = mount / "escape.txt"
    link.symlink_to(outside)
    monkeypatch.setattr(requests, "put", PutRecorder())

    with pytest.raises(TransferError, match="outside the workspace volume"):
        upload_file(link, mount, SignedUpload(SIGNED_URL, 5, "abc="))


def test_upload_rejects_prefix_alike_sibling(
    mount: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """Containment compares path components, not string prefixes: a sibling directory
    named `<mount>X` is outside the volume even though its path starts with the mount's."""
    sibling = mount.parent / (mount.name + "X")
    sibling.mkdir()
    outside = sibling / "secret.txt"
    outside.write_bytes(b"not in the volume")
    link = mount / "escape.txt"
    link.symlink_to(outside)
    monkeypatch.setattr(requests, "put", PutRecorder())

    with pytest.raises(TransferError, match="outside the workspace volume"):
        upload_file(link, mount, SignedUpload(SIGNED_URL, 5, "abc="))


def test_download_rejects_the_mount_root_itself(
    mount: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """The mount point is a directory, never a file to write.

    It must be named as such rather than caught by the parent-directory containment
    check: the mount root's parent is outside the volume by construction, so that check
    would report a containment violation against a path the caller never supplied.
    """
    monkeypatch.setattr(requests, "get", _fake_get(FakeResponse(chunks=[b"x"])))

    with pytest.raises(TransferError, match="mount point itself") as caught:
        download_file(mount, mount, SIGNED_URL)
    assert "outside the workspace volume" not in str(caught.value)


def test_download_rejects_mount_root_reached_by_dot_traversal(
    mount: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """`<mount>/sub/..` normalizes to the mount root and must be refused the same way."""
    (mount / "sub").mkdir()
    monkeypatch.setattr(requests, "get", _fake_get(FakeResponse(chunks=[b"x"])))

    with pytest.raises(TransferError, match="mount point itself"):
        download_file(mount / "sub" / "..", mount, SIGNED_URL)


def test_download_rejects_prefix_alike_sibling_parent(
    mount: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """The same component-wise guard on the write path's parent directory."""
    sibling = mount.parent / (mount.name + "X")
    sibling.mkdir()
    (mount / "escape_dir").symlink_to(sibling)
    monkeypatch.setattr(requests, "get", _fake_get(FakeResponse(chunks=[b"x"])))

    with pytest.raises(TransferError, match="outside the workspace volume"):
        download_file(mount / "escape_dir" / "in.txt", mount, SIGNED_URL)


def test_upload_rejects_the_mount_root_itself(
    mount: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """The read path resolves the path itself rather than its parent, so the mount root
    passes containment and is refused for what it actually is - not a regular file."""
    monkeypatch.setattr(requests, "put", PutRecorder())

    with pytest.raises(TransferError, match="not a regular file") as caught:
        upload_file(mount, mount, SignedUpload(SIGNED_URL, 5, "abc="))
    assert "outside the workspace volume" not in str(caught.value)


def test_upload_rejects_missing_source(
    mount: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(requests, "put", PutRecorder())

    with pytest.raises(TransferError, match="does not exist"):
        upload_file(mount / "nope.txt", mount, SignedUpload(SIGNED_URL, 5, "abc="))


# ======================================================================================
# Download


def _fake_get(response: FakeResponse) -> Any:
    def _get(url: str, **kwargs: Any) -> FakeResponse:  # noqa: ARG001
        return response

    return _get


def test_download_writes_the_file(mount: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(
        requests, "get", _fake_get(FakeResponse(chunks=[b"art", b"ifact"]))
    )
    target = mount / "in.txt"

    outcome = download_file(target, mount, SIGNED_URL)

    assert target.read_bytes() == b"artifact"
    assert outcome["ok"] is True
    assert outcome["size"] == 8


def test_download_overwrites_a_regular_file(
    mount: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    target = mount / "in.txt"
    target.write_bytes(b"stale content that is longer than the new content")
    monkeypatch.setattr(requests, "get", _fake_get(FakeResponse(chunks=[b"new"])))

    download_file(target, mount, SIGNED_URL)

    assert target.read_bytes() == b"new"


def test_download_replaces_a_symlink_rather_than_following_it(
    mount: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """DESIGN §7.5.1: the link is unlinked and a fresh regular file created, so a
    planted symlink cannot redirect the write outside the volume."""
    outside = tmp_path / "victim.txt"
    outside.write_bytes(b"must not be overwritten")
    target = mount / "in.txt"
    target.symlink_to(outside)
    monkeypatch.setattr(requests, "get", _fake_get(FakeResponse(chunks=[b"payload"])))

    download_file(target, mount, SIGNED_URL)

    assert not target.is_symlink()
    assert target.read_bytes() == b"payload"
    assert outside.read_bytes() == b"must not be overwritten"


def test_download_refuses_missing_parent_directory(
    mount: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """The sidecar must not mkdir -p: it does not control the tool containers' UID, so
    any directory it created would be unusable by them (DESIGN §7.5.1)."""
    monkeypatch.setattr(requests, "get", _fake_get(FakeResponse(chunks=[b"x"])))

    with pytest.raises(TransferError, match="does not exist"):
        download_file(mount / "missing" / "in.txt", mount, SIGNED_URL)


def test_download_refuses_a_directory_target(
    mount: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    subdir = mount / "adir"
    subdir.mkdir()
    monkeypatch.setattr(requests, "get", _fake_get(FakeResponse(chunks=[b"x"])))

    with pytest.raises(TransferError, match="is a directory"):
        download_file(subdir, mount, SIGNED_URL)


def test_download_rejects_target_escaping_mount(
    mount: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(requests, "get", _fake_get(FakeResponse(chunks=[b"x"])))

    with pytest.raises(TransferError, match="outside the workspace volume"):
        download_file(tmp_path / "escape.txt", mount, SIGNED_URL)


def test_download_rejects_target_whose_parent_dir_escapes_mount(
    mount: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """A symlinked *directory* in the path is a real redirect - it is traversed, so the
    write genuinely lands outside the volume. Unlike a link at the final component,
    which gets replaced rather than followed, this one must be refused."""
    outside = tmp_path / "outside_dir"
    outside.mkdir()
    (mount / "escape_dir").symlink_to(outside)
    monkeypatch.setattr(requests, "get", _fake_get(FakeResponse(chunks=[b"x"])))

    with pytest.raises(TransferError, match="outside the workspace volume"):
        download_file(mount / "escape_dir" / "in.txt", mount, SIGNED_URL)


def test_download_surfaces_http_failure(
    mount: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(requests, "get", _fake_get(FakeResponse(404)))

    with pytest.raises(TransferError, match="HTTP 404"):
        download_file(mount / "in.txt", mount, SIGNED_URL)


def test_download_never_leaks_the_url_on_failure(
    mount: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    def explode(url: str, **kwargs: Any) -> FakeResponse:  # noqa: ARG001
        raise requests.ConnectionError(f"failed connecting to {SIGNED_URL}")

    monkeypatch.setattr(requests, "get", explode)

    with pytest.raises(TransferError) as caught:
        download_file(mount / "in.txt", mount, SIGNED_URL)

    assert "X-Amz-Signature" not in str(caught.value)


def test_download_streams_rather_than_buffering(
    mount: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """An artifact can be large, so the body must never be materialized in memory.

    Asserted on observable behavior: the request asks for a stream, and the response is
    consumed in bounded chunks rather than read whole.
    """
    seen: dict[str, Any] = {}
    response = FakeResponse(chunks=[b"a", b"b"])

    def _get(url: str, **kwargs: Any) -> FakeResponse:  # noqa: ARG001
        seen.update(kwargs)
        return response

    monkeypatch.setattr(requests, "get", _get)
    download_file(mount / "in.txt", mount, SIGNED_URL)

    assert seen["stream"] is True
    assert 0 < response.chunk_size <= 8 * 1024 * 1024

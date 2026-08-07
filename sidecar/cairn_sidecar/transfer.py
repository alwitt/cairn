"""Transfer sidecars: move bytes between the workspace volume and the object store.

These talk to the object store and nothing else - never back to the service (DESIGN.md
§5.1). Their only credential is the presigned URL itself, a short-lived bearer token
scoped to one key and one operation (DESIGN.md §5.2).

**Why this is Python rather than ``curl -T``.** A presigned PUT from goutils signs
``Content-Length`` *and* ``x-amz-checksum-sha256`` (plus ``Content-Type`` when the
service fixed one) into the URL. The client must send every signed header with exactly
the signed value or the signature check fails. ``curl -T`` sends no checksum header at
all, so the DESIGN's original sketch could not have worked; the header set is explicit
here instead.

The URL is never written to output. Its signature lives in the query string, so echoing
it into a failure message would leak a usable credential into the service's logs
(DESIGN.md §5.2).
"""

from __future__ import annotations

import errno
import os
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import requests

# Transfers are bounded by the single-PUT size cap but can still be large, so stream in
# both directions rather than materializing a body in memory.
_STREAM_CHUNK_BYTES = 1024 * 1024

# Generous enough for a large transfer over a slow link, but not unbounded - a hung
# object store should fail the sidecar rather than pin a container until the runtime's
# own timeout fires.
_CONNECT_TIMEOUT_SECS = 30
_READ_TIMEOUT_SECS = 900


class TransferError(Exception):
    """A transfer failed. The message is safe to surface (never contains the URL)."""


def _require_inside_mount(candidate: Path, mount: Path, described: Path) -> None:
    """Fail unless ``candidate`` sits inside the workspace volume.

    ``is_relative_to`` is reflexive, so the mount root itself passes without a separate
    equality case. It also compares path *components* rather than string prefixes, which
    is what makes a sibling like ``/mnt/cairn/wsX`` a rejection rather than a match.
    """
    if not candidate.is_relative_to(mount):
        raise TransferError(
            f"path '{described}' resolves to '{candidate}', which is outside the "
            f"workspace volume mounted at '{mount}'"
        )


def _check_read_containment(path: Path, mount_root: Path) -> Path:
    """Resolve a **read** path and require the resolved target to stay in the volume.

    Symlinks are followed here because the upload reads through them (DESIGN.md §7.5.3),
    so the target is what actually gets sent - and a link pointing out of the volume
    would otherwise exfiltrate the sidecar image's own files.
    """
    resolved = path.resolve(strict=False)
    mount = mount_root.resolve(strict=False)
    _require_inside_mount(resolved, mount, path)
    return resolved


def _check_write_containment(path: Path, mount_root: Path) -> Path:
    """Resolve a **write** path without following a symlink at the final component.

    Deliberately different from the read check. A download replaces a symlink sitting at
    the destination rather than writing through it (DESIGN.md §7.5.1), so resolving the
    link would judge containment against a target that is never written - and would
    wrongly reject the perfectly safe case of a planted link pointing outside the
    volume, which is precisely the attack the replace-don't-follow rule defuses.

    So the *parent* directory is resolved (it is traversed, and a link there really does
    redirect the write) while the final component is left untouched.
    """
    mount = mount_root.resolve(strict=False)
    resolved = path.resolve(strict=False)

    # The mount root is a directory, never a file to write, and it must be named before
    # the parent check below: its parent lies outside the volume by construction, so
    # that check would reject it as a containment violation - pointing at a path the
    # caller never supplied, and hiding the actual mistake.
    if resolved == mount:
        raise TransferError(
            f"destination path '{path}' is the workspace volume mount point itself, "
            f"not a file to write"
        )

    parent = path.parent.resolve(strict=False)
    _require_inside_mount(parent, mount, path)
    return parent / path.name


@dataclass(frozen=True)
class SignedUpload:
    """The values a presigned PUT URL was signed for.

    Grouped because they are one indivisible set: the URL's signature covers these
    headers, so sending any of them with a different value - or omitting one - fails the
    signature check just as surely as using the wrong URL. Passing them together makes
    them hard to mismatch.
    """

    url: str
    """Presigned PUT URL. Never logged (DESIGN.md §5.2)."""

    object_size: int
    """Exact byte count, sent as ``Content-Length``."""

    sha256_b64: str
    """Base64 SHA-256, sent as ``x-amz-checksum-sha256``."""

    content_type: str | None = None
    """MIME type, present only when the service signed one."""

    def headers(self) -> dict[str, str]:
        """Build the exact header set the signature covers."""
        signed = {
            "Content-Length": str(self.object_size),
            "x-amz-checksum-sha256": self.sha256_b64,
        }
        # Only when signed: sending an unsigned Content-Type breaks the signature just
        # as omitting a signed one does.
        if self.content_type is not None:
            signed["Content-Type"] = self.content_type
        return signed


def upload_file(
    source: Path,
    mount_root: Path,
    signed: SignedUpload,
) -> dict[str, Any]:
    """PUT a volume-resident file to the object store via a presigned URL.

    The headers are sent exactly as signed. ``Content-Length`` is set from the size the
    service signed rather than from a fresh stat of the file - the signed value is what
    the signature covers, and re-deriving it would only mask the very drift the checksum
    bind exists to catch.

        :param source: file to upload, inside the mount
        :param mount_root: volume mount path supplied by the launching service
        :param signed: the presigned URL and the values it was signed for
        :returns: the result payload to hand back to the service
    """
    resolved = _check_read_containment(source, mount_root)

    if not resolved.exists():
        raise TransferError(f"source file '{source}' does not exist")
    if not resolved.is_file():
        raise TransferError(f"source path '{source}' is not a regular file")

    headers = signed.headers()

    try:
        with resolved.open("rb") as handle:
            # Passing the file object streams it; requests does not buffer the body.
            response = requests.put(
                signed.url,
                data=b"" if signed.object_size < 1 else handle,
                headers=headers,
                timeout=(_CONNECT_TIMEOUT_SECS, _READ_TIMEOUT_SECS),
            )
    except requests.RequestException as exc:
        # Must precede the OSError clause: requests.ConnectionError subclasses OSError,
        # and its str() embeds the request URL - formatting it into the message would
        # leak the signature into the service's logs (DESIGN.md §5.2). Only the
        # exception type is reported.
        raise TransferError(
            f"upload request to the object store failed: {type(exc).__name__}"
        ) from exc
    except OSError as exc:
        raise TransferError(f"failed to read source file '{source}': {exc}") from exc

    if not response.ok:
        raise TransferError(_describe_upload_failure(response.status_code, source))

    return {"ok": True, "uploaded_path": str(resolved), "size": signed.object_size}


def _describe_upload_failure(status_code: int, source: Path) -> str:
    """Turn an object-store rejection into a message the agent can act on.

    A 4xx here is overwhelmingly the TOCTOU case DESIGN.md §6.4 designs for: the file
    changed on the shared volume between the stat sidecar and this upload, so the bytes
    no longer match the signed checksum and the store refuses them. That is an
    operational failure worth naming precisely - "the file changed" tells the agent what
    to do, where a raw HTTP status does not.
    """
    if 400 <= status_code < 500:
        return (
            f"the object store rejected the upload (HTTP {status_code}); the source "
            f"file '{source}' most likely changed while the upload was in flight, so "
            f"its content no longer matches the checksum the upload was signed for"
        )
    return f"the object store returned HTTP {status_code} for the upload"


def _open_for_overwrite(target: Path) -> int:
    """Open a download target, replacing a symlink instead of following it.

    ``O_NOFOLLOW`` makes the open fail rather than write through a link, so a symlink
    planted at the destination cannot redirect the write outside the volume. On that
    failure the link itself is unlinked and a fresh regular file created - which is
    exactly the behavior DESIGN.md §7.5.1 specifies (the link is replaced, not followed).

    Parent directories are never created: the sidecar does not control the UID the tool
    containers run as, so any directory it created would be owned by the sidecar's UID
    and unusable by them (DESIGN.md §7.5.1).
    """
    flags = os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW
    try:
        return os.open(target, flags, 0o644)
    except OSError as exc:
        # ELOOP is the O_NOFOLLOW rejection: a symlink sits at the target.
        if exc.errno != errno.ELOOP and not target.is_symlink():
            raise
        target.unlink()
        return os.open(target, flags, 0o644)


def download_file(target: Path, mount_root: Path, url: str) -> dict[str, Any]:
    """GET an artifact from the object store into the workspace volume.

    A partially written file is left in place on failure. The volume is disposable
    scratch and the agent is told the download failed, so cleanup would add a failure
    mode without adding a guarantee (DESIGN.md §7.5.1).

        :param target: destination path, inside the mount
        :param mount_root: volume mount path supplied by the launching service
        :param url: presigned GET URL (never logged)
        :returns: the result payload to hand back to the service
    """
    resolved = _check_write_containment(target, mount_root)

    if resolved.is_dir():
        raise TransferError(
            f"destination path '{target}' is a directory, not a file to write"
        )
    parent = resolved.parent
    if not parent.is_dir():
        raise TransferError(
            f"destination directory '{parent}' does not exist; create it before "
            f"downloading into it"
        )

    try:
        response = requests.get(
            url,
            stream=True,
            timeout=(_CONNECT_TIMEOUT_SECS, _READ_TIMEOUT_SECS),
        )
    except requests.RequestException as exc:
        raise TransferError(
            f"download request to the object store failed: {type(exc).__name__}"
        ) from exc

    with response:
        if not response.ok:
            raise TransferError(
                f"the object store returned HTTP {response.status_code} for the download"
            )

        written = 0
        try:
            handle = _open_for_overwrite(resolved)
        except OSError as exc:
            raise TransferError(
                f"failed to open destination '{target}': {exc}"
            ) from exc

        try:
            with os.fdopen(handle, "wb") as sink:
                for chunk in response.iter_content(chunk_size=_STREAM_CHUNK_BYTES):
                    if chunk:
                        sink.write(chunk)
                        written += len(chunk)
        except requests.RequestException as exc:
            # Ordered before OSError for the same reason as the upload path:
            # requests.ConnectionError is an OSError whose str() carries the signed URL.
            raise TransferError(
                f"download stream failed after {written} bytes: {type(exc).__name__}"
            ) from exc
        except OSError as exc:
            raise TransferError(
                f"failed writing to destination '{target}': {exc}"
            ) from exc

    return {"ok": True, "downloaded_path": str(resolved), "size": written}

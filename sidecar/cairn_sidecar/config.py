"""Environment-variable input surface for every sidecar command.

Sidecars take **no argv**: the launching service passes every parameter through the
environment (DESIGN.md §5.2). That discipline started with the presigned URL — whose
signature must stay out of ``/proc/<pid>/cmdline`` — and is applied here to the whole
input surface, so no sidecar has an argument an agent-influenced value could reach.

The volume mount path arrives the same way (``CAIRN_MOUNT_ROOT``) rather than being
compiled in. DESIGN.md §4.4 fixes it at ``/mnt/cairn/ws`` for the first cut but wants it
*able* to become configurable; keeping it an input means that change never touches this
image.

Every loader raises :class:`ConfigError` on a missing or malformed value, so a
misconfigured container fails with a legible sentence instead of a ``KeyError``
traceback the service would have to interpret.
"""

from __future__ import annotations

import os
from pathlib import Path

# Environment variable names. Named constants rather than inline literals so the Go
# launcher's key list and this module can be diffed against each other.
ENV_MOUNT_ROOT = "CAIRN_MOUNT_ROOT"
ENV_SOURCE_PATH = "CAIRN_SOURCE_PATH"
ENV_TARGET_PATH = "CAIRN_TARGET_PATH"
ENV_URL = "CAIRN_URL"
ENV_OBJECT_SIZE = "CAIRN_OBJECT_SIZE"
ENV_SHA256_B64 = "CAIRN_SHA256_B64"
ENV_CONTENT_TYPE = "CAIRN_CONTENT_TYPE"


class ConfigError(Exception):
    """A required environment variable is missing or malformed."""


def _require(name: str) -> str:
    """Read a required environment variable.

    An empty value is treated as absent: an unset variable and one set to "" are the
    same mistake from the caller's side, and both are equally unusable.
    """
    value = os.environ.get(name)
    if value is None or value == "":
        raise ConfigError(f"required environment variable {name} is not set")
    return value


def mount_root() -> Path:
    """Read the volume mount path supplied by the launching service.

    Required to be absolute: every containment check resolves against this path, and a
    relative root would silently anchor those checks to the process working directory —
    turning the guard into a no-op rather than a visible failure.
    """
    raw = _require(ENV_MOUNT_ROOT)
    path = Path(raw)
    if not path.is_absolute():
        raise ConfigError(f"{ENV_MOUNT_ROOT} must be an absolute path, got '{raw}'")
    return path


def source_path() -> Path:
    """Read the source file path (stat and upload)."""
    return Path(_require(ENV_SOURCE_PATH))


def target_path() -> Path:
    """Read the destination file path (download)."""
    return Path(_require(ENV_TARGET_PATH))


def presigned_url() -> str:
    """Read the presigned URL (upload and download).

    Never log or echo the return value: the signature travels in the query string
    (DESIGN.md §5.2).
    """
    return _require(ENV_URL)


def object_size() -> int:
    """Read the exact object size the upload must declare as ``Content-Length``.

    The value is signed into the presigned PUT URL, so it is sent verbatim rather than
    re-derived from the file — a mismatch is precisely the condition the object store
    exists to reject (DESIGN.md §6.4).
    """
    raw = _require(ENV_OBJECT_SIZE)
    try:
        size = int(raw)
    except ValueError as exc:
        raise ConfigError(f"{ENV_OBJECT_SIZE} must be an integer, got '{raw}'") from exc
    if size < 0:
        raise ConfigError(f"{ENV_OBJECT_SIZE} must be non-negative, got {size}")
    return size


def sha256_b64() -> str:
    """Read the base64 SHA-256 the upload must send as ``x-amz-checksum-sha256``.

    Base64, not the hex that ``sha256sum`` prints - this is the encoding the presigned
    URL binds (goutils ``GeneratePresignedPutURL``).
    """
    return _require(ENV_SHA256_B64)


def content_type() -> str | None:
    """Read the optional ``Content-Type``.

    Optional by design: the presigned URL only signs this header when the service chose
    to fix the stored MIME type. Sending it when it was not signed breaks the signature
    just as surely as omitting it when it was, so absent must stay distinguishable from
    empty.
    """
    value = os.environ.get(ENV_CONTENT_TYPE)
    if value is None or value == "":
        return None
    return value

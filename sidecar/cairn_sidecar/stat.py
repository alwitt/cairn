"""Stat/hash sidecar: classify and measure a volume-resident upload source.

Why this runs in a container at all: a presigned staging PUT URL is bound to an exact
size and SHA-256 *before* it can be minted (DESIGN.md §6.1), but the bytes live in the
workspace volume, which only a sidecar can reach. So size and hash must be computed here
and handed back for the mint (DESIGN.md §6.4 step 1).

Deliberately **no MIME sniff**. MIME stays server-side at register so it is derived from
the bytes the service itself read, rather than asserted by a sidecar (DESIGN.md §6.4).

No network and no credentials (DESIGN.md §5.1) - this module only reads a file and
writes one result line.
"""

from __future__ import annotations

import base64
import hashlib
from pathlib import Path
from typing import Any

# Hash in chunks rather than reading the file in: an artifact is bounded by the
# single-PUT size cap, but that cap is a deployment setting and can be large.
_HASH_CHUNK_BYTES = 1024 * 1024


def _invalid(error: str, resolved: Path | None = None) -> dict[str, Any]:
    """Build the stat block for a source that cannot be uploaded.

    ``resolved_path`` is null when the path does not exist and the resolved path when it
    does but is unusable (a directory, say) - the service reports the latter back to the
    agent, which is more useful than repeating what it already asked for.
    """
    return {
        "resolved_path": str(resolved) if resolved is not None else None,
        "valid": False,
        "size": 0,
        "sha256_b64": "",
        "error": error,
    }


def _hash_file(path: Path) -> tuple[int, str]:
    """Stream a file through SHA-256, returning its byte count and base64 digest.

    The size is counted from the same read that feeds the hash, so the two can never
    disagree - a stat-then-read would leave a window for the file to change between them,
    and the pair is about to be signed into a URL as a matched set.

    Base64 is what the presigned PUT binds (``x-amz-checksum-sha256``), **not** the hex
    that ``sha256sum`` prints. Emitting hex here would produce a URL the object store
    rejects on every upload.
    """
    digest = hashlib.sha256()
    size = 0
    with path.open("rb") as handle:
        while chunk := handle.read(_HASH_CHUNK_BYTES):
            digest.update(chunk)
            size += len(chunk)
    return size, base64.b64encode(digest.digest()).decode("ascii")


def stat_source(source: Path, mount_root: Path) -> dict[str, Any]:
    """Resolve, validate, and measure an upload source file.

    Returns the stat block described in DESIGN.md §7.5.3. Invalid sources are reported as
    ``valid: false`` rather than raised: "this file cannot be uploaded" is an answer the
    service acts on (it rejects before minting anything), not an error condition.

        :param source: the agent-supplied source path
        :param mount_root: the volume mount path supplied by the launching service
        :returns: the stat block to hand back to the service
    """
    # Symlinks are accepted, and it is the *target* that gets stat'd and hashed
    # (DESIGN.md §7.5.3) - so resolve first and judge everything below on the result.
    # strict=False so a missing path resolves rather than raising, letting it fall
    # through to the exists() check and be reported as a clean "not found".
    resolved = source.resolve(strict=False)
    mount = mount_root.resolve(strict=False)

    # The containment check, and the reason symlinks are safe to follow: a link pointing
    # outside the volume would otherwise let an agent hash - and then upload - the
    # sidecar image's own files. Applied to the resolved target, since that is what would
    # actually be read (DESIGN.md §7.5, §7.5.3). `is_relative_to` compares path
    # components rather than string prefixes, so a sibling like `<mount>X` is rejected.
    #
    # `is_relative_to` is reflexive, so the mount root passes here - correctly, it is
    # inside the volume. It is not a *file*, but that is the directory check below's job
    # to say, and it says so accurately. This is why the read path needs no equality
    # case where the write path does: the write check resolves the destination's
    # parent, and the mount root's parent lies outside the volume by construction, so
    # there the root has to be named before it trips a misleading containment error.
    if not resolved.is_relative_to(mount):
        return _invalid(
            f"source path resolves to '{resolved}', which is outside the workspace "
            f"volume mounted at '{mount}'",
            resolved,
        )

    if not resolved.exists():
        return _invalid(f"source file '{source}' does not exist")

    if resolved.is_dir():
        return _invalid(
            f"source path '{source}' is a directory, not a single uploadable file",
            resolved,
        )

    # Catches sockets, FIFOs, and device nodes, whose size and hash are meaningless.
    if not resolved.is_file():
        return _invalid(f"source path '{source}' is not a regular file", resolved)

    try:
        size, digest_b64 = _hash_file(resolved)
    except OSError as exc:
        return _invalid(f"failed to read source file '{source}': {exc}", resolved)

    return {
        "resolved_path": str(resolved),
        "valid": True,
        "size": size,
        "sha256_b64": digest_b64,
    }

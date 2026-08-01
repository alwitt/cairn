"""``cairn-sidecar`` command-line entry points.

Three commands, one per sidecar: ``cairn-stat``, ``cairn-upload``, ``cairn-download``.

None of them take **any** option or argument - every input arrives through the
environment (see :mod:`cairn_sidecar.config`). That is what keeps each sidecar a fixed
command over server-supplied values (DESIGN.md §5.1): with no argv there is no argument
an agent-influenced value could reach, and the presigned URL never lands in
``/proc/<pid>/cmdline`` (DESIGN.md §5.2).

Each command emits exactly one result line and exits 0 on success, 1 on failure. The
service reads both: the line carries the detail, the exit code the verdict.
"""

from __future__ import annotations

import sys

import click

from . import config
from .result import EXIT_FAILURE, EXIT_OK, emit_failure, emit_result
from .stat import stat_source
from .transfer import SignedUpload, TransferError, download_file, upload_file


@click.group()
def cli() -> None:
    """Support commands executed inside cairn's sidecar containers."""


@cli.command("stat")
def stat_command() -> None:
    """Resolve, validate, and hash an upload source file in the workspace volume."""
    try:
        source = config.source_path()
        mount_root = config.mount_root()
    except config.ConfigError as exc:
        # The stat block's own shape is owed even on a config failure: the service reads
        # `valid` to decide whether to proceed, so it must always be present.
        emit_failure(str(exc), resolved_path=None, valid=False, size=0, sha256_b64="")
        sys.exit(EXIT_FAILURE)

    stat_block = stat_source(source, mount_root)
    emit_result(stat_block)
    # An invalid source is a legitimate answer, not a crash - but it still failed, and
    # the exit code has to agree with the payload or the service gets mixed signals.
    sys.exit(EXIT_OK if stat_block["valid"] else EXIT_FAILURE)


@cli.command("upload")
def upload_command() -> None:
    """Upload a volume-resident file to the object store via a presigned PUT URL."""
    try:
        source = config.source_path()
        mount_root = config.mount_root()
        signed = SignedUpload(
            url=config.presigned_url(),
            object_size=config.object_size(),
            sha256_b64=config.sha256_b64(),
            content_type=config.content_type(),
        )
    except config.ConfigError as exc:
        emit_failure(str(exc))
        sys.exit(EXIT_FAILURE)

    try:
        outcome = upload_file(source, mount_root, signed)
    except TransferError as exc:
        emit_failure(str(exc))
        sys.exit(EXIT_FAILURE)

    emit_result(outcome)


@cli.command("download")
def download_command() -> None:
    """Download an artifact from the object store into the workspace volume."""
    try:
        target = config.target_path()
        mount_root = config.mount_root()
        url = config.presigned_url()
    except config.ConfigError as exc:
        emit_failure(str(exc))
        sys.exit(EXIT_FAILURE)

    try:
        outcome = download_file(target, mount_root, url)
    except TransferError as exc:
        emit_failure(str(exc))
        sys.exit(EXIT_FAILURE)

    emit_result(outcome)


if __name__ == "__main__":
    cli()

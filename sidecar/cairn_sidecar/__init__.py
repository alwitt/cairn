"""cairn sidecar support code.

The programs cairn runs inside its short-lived sidecar containers (DESIGN.md §5, §6.4):
a stat/hash sidecar that measures a volume-resident upload source, and transfer sidecars
that move bytes between the workspace volume and the object store over presigned URLs.

Every parameter arrives through the environment (:mod:`cairn_sidecar.config`), and every
command answers with a single blank-line-delimited JSON line
(:mod:`cairn_sidecar.result`).
"""

from __future__ import annotations

from .result import emit_failure, emit_result
from .stat import stat_source
from .transfer import SignedUpload, TransferError, download_file, upload_file

__all__ = [
    "SignedUpload",
    "TransferError",
    "download_file",
    "emit_failure",
    "emit_result",
    "stat_source",
    "upload_file",
]

"""The sidecar → service result contract.

The launching runtime creates containers with a TTY (``goutils/runtime`` docker.go), so
stdout and stderr are merged into a **single** stream before the service ever sees them;
``SystemCallResp.Output`` is that combined text. There is no stream to separate, which
rules out the usual "the JSON is on stdout" contract - one warning line from a library,
one progress write, and a whole-output parse fails.

So the result is framed instead: a **single line** of compact JSON, with a blank line on
either side. The service scans the combined output line by line and decodes the line that
parses. Interleaved noise is then harmless rather than fatal, and the framing costs
nothing to produce.

Every command emits exactly one result line, on success *and* on failure. A failure line
carries the reason, so the service reports why a sidecar failed instead of surfacing a
bare non-zero exit code.
"""

from __future__ import annotations

import json
import sys
from typing import Any

# Exit codes. The result line is the detailed channel; these only distinguish "the work
# happened" from "it did not", since the service reads both.
EXIT_OK = 0
EXIT_FAILURE = 1


def emit_result(payload: dict[str, Any]) -> None:
    """Write one result record as a single blank-line-delimited JSON line.

    Compact separators keep it to one line regardless of payload size, and the leading
    newline guarantees the record starts a line even if something already wrote a
    partial, un-terminated line to the shared stream.
    """
    line = json.dumps(payload, separators=(",", ":"))
    sys.stdout.write("\n" + line + "\n")
    sys.stdout.flush()


def emit_failure(error: str, **fields: Any) -> None:
    """Emit a result line describing a failed run.

    ``ok`` is always present and false; callers add whatever contract fields their
    command's schema requires (the stat command, for instance, still owes the service a
    ``valid`` field).
    """
    payload: dict[str, Any] = {"ok": False, "error": error}
    payload.update(fields)
    emit_result(payload)

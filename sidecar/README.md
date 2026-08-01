# cairn-sidecar

Support code executed inside cairn's short-lived sidecar containers (see `DESIGN.md`
§5, §6.4). The service launches these; they are never invoked by a client or by an
agent's tool container.

| Command | Sidecar | Does |
|---|---|---|
| `cairn-stat` | stat/hash | Resolves and validates an upload source file in the workspace volume, then reports its size and base64 SHA-256. No network. |
| `cairn-upload` | transfer | PUTs a volume-resident file to the object store through a presigned URL. |
| `cairn-download` | transfer | GETs an artifact from the object store into the workspace volume. |

## Input: environment variables only

The commands take **no arguments or options**. Every parameter is passed by the
launching service through the environment, which keeps the presigned URL's signature out
of `/proc/<pid>/cmdline` and leaves no argv for an agent-influenced value to reach
(DESIGN.md §5.2).

| Variable | Used by | Meaning |
|---|---|---|
| `CAIRN_MOUNT_ROOT` | all | Volume mount path, supplied by the service (e.g. `/mnt/cairn/ws`). Must be absolute. |
| `CAIRN_SOURCE_PATH` | stat, upload | Source file inside the mount. |
| `CAIRN_TARGET_PATH` | download | Destination file inside the mount. |
| `CAIRN_URL` | upload, download | Presigned URL. Never logged. |
| `CAIRN_OBJECT_SIZE` | upload | Exact byte size, sent as `Content-Length`. |
| `CAIRN_SHA256_B64` | upload | Base64 SHA-256, sent as `x-amz-checksum-sha256`. |
| `CAIRN_CONTENT_TYPE` | upload | Optional; sent only when the service signed one. |

The mount path is an input rather than a compiled-in constant, so the same image keeps
working if it ever becomes configurable (DESIGN.md §4.4).

## Output: one JSON line

The container runtime merges stdout and stderr into a single stream (it allocates a
TTY), so there is no stdout to parse in isolation. Each command instead writes **one
line** of compact JSON with a blank line either side; the service scans the combined
output line by line and decodes the line that parses. Stray output from any other source
is then harmless.

Every run emits exactly one line, success or failure, and exits 0 or 1 to match.

```json
{"resolved_path":"/mnt/cairn/ws/out.txt","valid":true,"size":12,"sha256_b64":"DaUp…NSQ="}
```

A failure line carries `"ok": false` and an `"error"` string, so the service can report
*why* rather than only surfacing a non-zero exit code.

### Note on the checksum encoding

`sha256_b64` is **base64**, not the hex `sha256sum` prints. Base64 is what
goutils' `GeneratePresignedPutURL` binds into the URL as `x-amz-checksum-sha256`; hex
would be rejected by the object store on every upload.

## Development

```
make lint    # isort, ruff, pylint, mypy
make test    # pytest
```

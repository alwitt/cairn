#!/usr/bin/env python3
"""Test-drive CLI for the cairn workspace and artifact management API.

Talks to the REST API exposed by `api/workspace.go` and `api/artifact.go`. Built with
`click`, `requests` and `prompt-toolkit` so the endpoints can be exercised by hand against
the dev stack.

Workspaces and artifacts are addressed by name here even though the REST API keys them on
UUID and ULID respectively; each command resolves the name through a list endpoint's
exact-match filter first.

Run under the `cairn` pyenv environment:

    PYENV_VERSION=cairn pyenv exec python demo/cairn-demo.py list workspace

Examples:

    ./cairn-demo.py list workspace --volume-state READY
    ./cairn-demo.py create workspace
    ./cairn-demo.py get workspace my-ws
    ./cairn-demo.py update workspace name my-ws
    ./cairn-demo.py update workspace description my-ws
    ./cairn-demo.py update workspace volume-meta my-ws
    ./cairn-demo.py setup workspace my-ws
    ./cairn-demo.py open workspace my-ws
    ./cairn-demo.py teardown workspace my-ws
    ./cairn-demo.py delete workspace my-ws

    ./cairn-demo.py list artifact my-ws --state RECORDED
    ./cairn-demo.py create artifact -w my-ws --name report ./report.pdf
    ./cairn-demo.py get artifact -w my-ws report --presign
    ./cairn-demo.py update artifact -w my-ws -a report ./report-v2.pdf
    ./cairn-demo.py delete artifact -w my-ws report
"""

import base64
import hashlib
import json
import re
import subprocess
import sys

import click
import requests
from prompt_toolkit import prompt
from prompt_toolkit.validation import ValidationError, Validator

# Default API location, matching the server defaults in models/config.go
# (api.service.appPort 44123, api.apis.endPoint.pathPrefix "/"). The "/" prefix collapses to
# nothing in gorilla/mux, so the real paths start at /v1.
DEFAULT_BASE_URL = "http://localhost:44123"

# Mirrors the server's valid_name rule (models/validate.go), applied to workspace and artifact
# names alike. Note it excludes the dot, so a file's basename is not usable as an artifact name -
# which is why `create artifact` demands an explicit --name rather than deriving one.
WORKSPACE_NAME_RE = re.compile(r"^[a-zA-Z0-9-_]+$")

# The complete WorkspaceVolumeStateENUM (models/workspace.go). NONE means no persistent volume
# has been provisioned; READY means one exists and can be mounted.
VOLUME_STATES = ("NONE", "READY")

# The complete ArtifactStateENUM (models/artifact.go). RECORDED means the backing object is
# present and servable; MISSING_OBJECT means the row outlived its object and the artifact needs
# repairing - which `update artifact` is how you do.
ARTIFACT_STATES = ("RECORDED", "MISSING_OBJECT")

# Matches the sidecar's hashing stride (sidecar/cairn_sidecar/stat.py).
HASH_CHUNK_BYTES = 1024 * 1024

# The canonical path every container mounts a workspace volume at (models/workspace.go).
# `open workspace` mounts there too so a path seen in the shell is the same path the
# volume-based artifact endpoints take.
WORKSPACE_MOUNT_PATH = "/mnt/cairn/ws"

# Throwaway image for `open workspace`: small, and carries a /bin/sh.
WORKSPACE_SHELL_IMAGE = "alpine:latest"

DEFAULT_TIMEOUT_SECS = 30
# Volume provisioning and teardown are synchronous - the handler returns only once Docker is
# done - so those two calls need far more headroom than a metadata update.
VOLUME_TIMEOUT_SECS = 300
# Object store transfer timeouts, matching the sidecar's (sidecar/cairn_sidecar/transfer.py):
# a hung object store should fail the command rather than hang forever.
UPLOAD_CONNECT_TIMEOUT_SECS = 30
UPLOAD_READ_TIMEOUT_SECS = 900


# ======================================================================================
# Endpoint URLs


def workspaces_url(base_url):
    """Workspace collection endpoint: /v1/workspaces"""
    return f"{base_url.rstrip('/')}/v1/workspaces"


def workspace_url(base_url, workspace_id):
    """Single workspace endpoint: /v1/workspaces/{workspaceID}"""
    return f"{workspaces_url(base_url)}/{workspace_id}"


def workspace_name_url(base_url, workspace_id):
    """Workspace rename endpoint: /v1/workspaces/{workspaceID}/name"""
    return f"{workspace_url(base_url, workspace_id)}/name"


def workspace_description_url(base_url, workspace_id):
    """Workspace description endpoint: /v1/workspaces/{workspaceID}/description"""
    return f"{workspace_url(base_url, workspace_id)}/description"


def workspace_volume_meta_url(base_url, workspace_id):
    """Workspace volume metadata endpoint: /v1/workspaces/{workspaceID}/volume-metadata"""
    return f"{workspace_url(base_url, workspace_id)}/volume-metadata"


def workspace_volume_url(base_url, workspace_id):
    """Workspace persistent volume endpoint: /v1/workspaces/{workspaceID}/volume"""
    return f"{workspace_url(base_url, workspace_id)}/volume"


def workspace_staging_url(base_url, workspace_id):
    """Staging upload URL endpoint: /v1/workspaces/{workspaceID}/new-staging"""
    return f"{workspace_url(base_url, workspace_id)}/new-staging"


def workspace_artifacts_url(base_url, workspace_id):
    """Workspace scoped artifact endpoint: /v1/workspaces/{workspaceID}/artifacts"""
    return f"{workspace_url(base_url, workspace_id)}/artifacts"


def artifact_url(base_url, artifact_id):
    """Single artifact endpoint: /v1/artifacts/{artifactID}"""
    return f"{base_url.rstrip('/')}/v1/artifacts/{artifact_id}"


def artifact_content_url(base_url, artifact_id):
    """Artifact content replacement endpoint: /v1/artifacts/{artifactID}/content"""
    return f"{artifact_url(base_url, artifact_id)}/content"


# ======================================================================================
# Request and response handling


def call(method, url, **kwargs):
    """Issue a request, reporting a transport failure rather than tracebacking on it.

    A demo driver is routinely pointed at a server that is not up yet, so a refused
    connection deserves one line of explanation and not a stack trace.
    """
    kwargs.setdefault("timeout", DEFAULT_TIMEOUT_SECS)
    try:
        return requests.request(method, url, **kwargs)
    except requests.exceptions.RequestException as exc:
        click.echo(f"Request to {url} failed: {exc}", err=True)
        sys.exit(1)


def decode(resp):
    """Parse a JSON response body, exiting on anything that is not JSON.

    Not every reply comes from a handler: a mistyped path - a trailing slash is enough -
    is answered by mux itself with plain text, which would otherwise blow up on .json().
    """
    try:
        return resp.json()
    except ValueError:
        click.echo(f"HTTP {resp.status_code}: {resp.text}", err=True)
        sys.exit(1)


def fail(resp, body):
    """Report the API's error envelope on stderr and exit non-zero.

    The server wraps every reply in goutils.RestAPIBaseResponse, whose error detail is
    {code, message, detail} - note `message`, not `msg`, which is what the struct's JSON
    tag actually spells.
    """
    err = body.get("error") or {}
    click.echo(
        f"Request failed (HTTP {resp.status_code}): "
        f"{err.get('message', '')} - {err.get('detail', '')}",
        err=True,
    )
    sys.exit(1)


def show(resp):
    """Pretty-print a JSON response and exit non-zero on API/HTTP error."""
    body = decode(resp)
    click.echo(json.dumps(body, indent=2))
    if not resp.ok or not body.get("success", False):
        fail(resp, body)


def resolve_workspace_id(base_url, name):
    """Resolve a workspace name to the UUID the REST API addresses it by.

    Every per-workspace endpoint is keyed on {workspaceID}, which is the UUID, but typing
    those by hand is miserable. The list endpoint's `name` filter is an exact `name in (...)`
    match over a unique column (db/workspace.go), so it answers with either no rows or
    exactly one - there is never an ambiguous set to disambiguate.
    """
    resp = call("get", workspaces_url(base_url), params={"name": name})
    body = decode(resp)
    if not resp.ok or not body.get("success", False):
        fail(resp, body)

    # `workspaces` is tagged `omitempty`, so an empty result set omits the key altogether.
    entries = body.get("workspaces") or []
    if not entries:
        click.echo(f"No workspace named '{name}'", err=True)
        sys.exit(1)

    return entries[0]["id"]


def resolve_artifact_id(base_url, workspace_id, workspace_name, name):
    """Resolve an artifact name to the ULID the REST API addresses it by.

    Artifact names are unique within a workspace, so the list endpoint's `name` filter picks
    out at most one entry - but the workspace has to be known first, which is why every
    artifact command takes one.

    Both states are named explicitly because the endpoint otherwise defaults to RECORDED
    alone (api/artifact.go), and a MISSING_OBJECT artifact has to stay reachable: repairing
    one by replacing its content is exactly what `update artifact` is for.
    """
    params = [("name", name)] + [("state", state) for state in ARTIFACT_STATES]
    resp = call("get", workspace_artifacts_url(base_url, workspace_id), params=params)
    body = decode(resp)
    if not resp.ok or not body.get("success", False):
        fail(resp, body)

    # `artifacts` is tagged `omitempty`, same as `workspaces`.
    entries = body.get("artifacts") or []
    if not entries:
        click.echo(
            f"No artifact named '{name}' in workspace '{workspace_name}'", err=True
        )
        sys.exit(1)

    return entries[0]["id"]


# ======================================================================================
# Staging upload
#
# Uploading is three steps, not one: mint a presigned PUT, send the bytes straight to the
# object store, then register the staged object with the service (DESIGN §6.1). The service
# never sees the bytes.


def hash_file(path):
    """Stream a file through SHA-256, returning its byte count and base64 digest.

    Base64 is what the presigned PUT binds as `x-amz-checksum-sha256`, not the hex that
    `sha256sum` prints - emitting hex would produce a URL the object store rejects on every
    upload. Mirrors the sidecar's _hash_file (sidecar/cairn_sidecar/stat.py).

    The size is counted from the same read that feeds the digest rather than stat'd
    separately, since the pair is about to be signed into a URL as a matched set.
    """
    digest = hashlib.sha256()
    size = 0
    with open(path, "rb") as handle:
        while chunk := handle.read(HASH_CHUNK_BYTES):
            digest.update(chunk)
            size += len(chunk)
    return size, base64.b64encode(digest.digest()).decode("ascii")


def put_staging_object(put_url, path, size, sha256_b64):
    """Upload a file to a presigned PUT URL.

    The URL signs `Content-Length` and `x-amz-checksum-sha256`, and those are the only
    headers sent: an unsigned header breaks the signature just as surely as omitting a
    signed one. No `Content-Type` is sent because none was requested at mint time. The
    length is the value that was signed, never a fresh stat - re-deriving it would mask the
    very drift the checksum exists to catch.

    Nothing here may print the URL or an exception carrying it (DESIGN §5.2): the signature
    lives in its query string. That is also why this does not go through `call`, whose
    failure line quotes both.
    """
    headers = {"Content-Length": str(size), "x-amz-checksum-sha256": sha256_b64}
    try:
        # Handing over the open file streams it, and lets requests set the same
        # Content-Length the URL was signed with.
        #
        # An empty artifact is legitimate, but it cannot be sent as a file object: requests
        # only sets Content-Length when the body's length is truthy, and falls back to
        # `Transfer-Encoding: chunked` otherwise (models.py prepare_body). At zero bytes that
        # branch fires, and the request goes out carrying both the chunked header and the
        # Content-Length signed into the URL - which the object store waits on forever.
        # Literal bytes take the non-streaming path and leave the signed header alone.
        with open(path, "rb") as handle:
            resp = requests.put(
                put_url,
                data=b"" if size == 0 else handle,
                headers=headers,
                timeout=(UPLOAD_CONNECT_TIMEOUT_SECS, UPLOAD_READ_TIMEOUT_SECS),
            )
    # RequestException first: requests.ConnectionError subclasses OSError, and its message
    # embeds the presigned URL. Only the exception type is safe to report.
    except requests.exceptions.RequestException as exc:
        click.echo(f"Upload to the object store failed: {type(exc).__name__}", err=True)
        sys.exit(1)
    except OSError as exc:
        click.echo(f"Could not read {path}: {exc}", err=True)
        sys.exit(1)

    if resp.status_code < 400:
        return

    # A 4xx here is the object store rejecting the bytes against what was signed, which in
    # practice means the file changed between hashing and uploading.
    if resp.status_code < 500:
        click.echo(
            f"Object store rejected the upload (HTTP {resp.status_code}): the file's size "
            "or checksum no longer matches what was signed; did it change?",
            err=True,
        )
    else:
        click.echo(f"Object store failure (HTTP {resp.status_code})", err=True)
    sys.exit(1)


def stage_file(base_url, workspace_id, path):
    """Hash a file, mint a staging PUT URL for it, and upload it.

    Returns the staging object key, which the caller registers - either as a new artifact or
    as replacement content for an existing one. Progress goes to stderr so stdout stays pure
    JSON, and the mint response is deliberately never shown: it contains the PUT URL.

    No `content_type` is requested. The authoritative MIME type is sniffed server side at
    registration, so supplying one here would buy nothing while adding a header that then
    has to be echoed back exactly.
    """
    click.echo(f"Hashing {path} ...", err=True)
    size, sha256_b64 = hash_file(path)

    resp = call(
        "post",
        workspace_staging_url(base_url, workspace_id),
        json={"size": size, "sha256_b64": sha256_b64},
    )
    body = decode(resp)
    if not resp.ok or not body.get("success", False):
        fail(resp, body)

    staging = body["staging"]
    staging_object_key = staging["staging_object_key"]

    click.echo(f"Uploading {size} bytes ...", err=True)
    put_staging_object(staging["put_url"], path, size, sha256_b64)
    click.echo(f"Staged at {staging_object_key}", err=True)

    return staging_object_key


# ======================================================================================
# Interactive prompt helpers (prompt_toolkit)


class _RegexValidator(Validator):
    """Validate non-blank input against a regex."""

    def __init__(self, pattern, message):
        self.pattern = pattern
        self.message = message

    def validate(self, document):
        text = document.text.strip()
        if not text or not self.pattern.match(text):
            raise ValidationError(
                message=self.message, cursor_position=len(document.text)
            )


class _IntValidator(Validator):
    """Validate an integer, optionally bounded below and optionally allowing a blank entry."""

    def __init__(self, min_value=None, allow_blank=False):
        self.min_value = min_value
        self.allow_blank = allow_blank

    def validate(self, document):
        text = document.text.strip()
        if not text:
            if self.allow_blank:
                return
            raise ValidationError(message="A value is required")
        try:
            value = int(text)
        except ValueError as exc:
            raise ValidationError(
                message="Must be an integer", cursor_position=len(document.text)
            ) from exc
        if self.min_value is not None and value < self.min_value:
            raise ValidationError(
                message=f"Must be >= {self.min_value}",
                cursor_position=len(document.text),
            )


def _prompt_int(message, min_value=None, default=None, allow_blank=False):
    """Prompt for an integer.

    A blank entry returns `default`. Blanks are accepted when a default was supplied, or
    when `allow_blank` is set - the latter being how an optional field is left unset.
    """
    text = prompt(
        message,
        validator=_IntValidator(
            min_value=min_value, allow_blank=allow_blank or default is not None
        ),
        validate_while_typing=False,
    ).strip()
    return default if not text else int(text)


def _prompt_workspace_name(message):
    """Prompt for a workspace name under the server's own charset rule."""
    return prompt(
        message,
        validator=_RegexValidator(
            WORKSPACE_NAME_RE, "Only alphanumeric characters, '-' and '_' are allowed"
        ),
        validate_while_typing=False,
    ).strip()


def interactive(flow, *args):
    """Run an interactive flow, treating Ctrl-C / Ctrl-D as a clean abort."""
    try:
        flow(*args)
    except (KeyboardInterrupt, EOFError):
        click.echo("Aborted.", err=True)
        sys.exit(1)


# ======================================================================================
# Root group


@click.group()
@click.option(
    "--base-url",
    envvar="CAIRN_API_URL",
    default=DEFAULT_BASE_URL,
    show_default=True,
    help="Base URL of the cairn server (env: CAIRN_API_URL).",
)
@click.pass_context
def cli(ctx, base_url):
    """Test CLI for the cairn workspace management API."""
    ctx.ensure_object(dict)
    ctx.obj["base_url"] = base_url


# ======================================================================================
# list


@cli.group("list")
def list_group():
    """List resources."""


@list_group.command("workspace")
@click.option(
    "--volume-state",
    multiple=True,
    type=click.Choice(VOLUME_STATES),
    help="Filter by volume state; repeat to match any of several.",
)
@click.pass_context
def list_workspace(ctx, volume_state):
    """List workspaces, optionally restricted to given volume states.

    The server also accepts offset/limit, which are deliberately not exposed here - a demo
    run wants the whole set.
    """
    # Repeated query parameters, one per requested state, which the handler OR's together.
    params = [("volume_state", state) for state in volume_state]
    resp = call("get", workspaces_url(ctx.obj["base_url"]), params=params)
    show(resp)


@list_group.command("artifact")
@click.argument("workspace")
@click.option(
    "--state",
    multiple=True,
    type=click.Choice(ARTIFACT_STATES),
    help="Filter by artifact state; repeat to match any of several.",
)
@click.pass_context
def list_artifact(ctx, workspace, state):
    """List the artifacts in a workspace.

    Without --state the server lists RECORDED entries only, so a quarantined artifact stays
    out of the way of a caller that did not ask for it. There are only two states, so naming
    both is how you ask for everything.
    """
    base_url = ctx.obj["base_url"]
    workspace_id = resolve_workspace_id(base_url, workspace)

    params = [("state", entry) for entry in state]
    resp = call("get", workspace_artifacts_url(base_url, workspace_id), params=params)
    show(resp)


# ======================================================================================
# create


@cli.group()
def create():
    """Create resources."""


@create.command("workspace")
@click.pass_context
def create_workspace(ctx):
    """Define a new workspace interactively.

    This creates the DB record only - the persistent volume is provisioned separately by
    `setup workspace`, so a new workspace always comes back in volume state NONE.
    """
    interactive(_create_workspace_interactive, ctx)


def _create_workspace_interactive(ctx):
    name = _prompt_workspace_name("Workspace Name: ")

    description = prompt("Description [blank to omit]: ").strip()

    # Volume metadata is recorded now and only read when the volume is provisioned; omitting
    # it takes the deployment's defaults. The server requires size_bytes > 0 when present.
    size_bytes = _prompt_int(
        "Volume Size Bytes [blank to omit]: ", min_value=1, allow_blank=True
    )

    payload = {"name": name}
    if description:
        payload["description"] = description
    if size_bytes is not None:
        payload["volume_metadata"] = {"size_bytes": size_bytes}

    resp = call("post", workspaces_url(ctx.obj["base_url"]), json=payload)
    show(resp)


@create.command("artifact")
@click.option("-w", "--workspace", required=True, help="Workspace to upload into.")
@click.option(
    "--name", required=True, help="Artifact name; see --help for the charset."
)
@click.argument("path", type=click.Path(exists=True, dir_okay=False, readable=True))
@click.pass_context
def create_artifact(ctx, workspace, name, path):
    """Upload a file and register it as a new artifact.

    --name is required rather than derived from the filename: names may only contain
    alphanumerics, '-' and '_', so a basename carrying an extension would be rejected and
    any rule for stripping one would just be a guess.

    Reusing a name already taken in the workspace comes back as a 500 rather than a
    conflict - this path leaves uniqueness to the database index.
    """
    interactive(_create_artifact_interactive, ctx, workspace, name, path)


def _create_artifact_interactive(ctx, workspace, name, path):
    base_url = ctx.obj["base_url"]
    workspace_id = resolve_workspace_id(base_url, workspace)

    description = prompt("Description [blank to omit]: ").strip()

    staging_object_key = stage_file(base_url, workspace_id, path)

    payload = {"staging_object_key": staging_object_key, "name": name}
    if description:
        payload["description"] = description

    resp = call("post", workspace_artifacts_url(base_url, workspace_id), json=payload)
    show(resp)


# ======================================================================================
# get


@cli.group()
def get():
    """Read resources."""


@get.command("workspace")
@click.argument("name")
@click.pass_context
def get_workspace(ctx, name):
    """Fetch one workspace by name.

    The reply carries `mount_count` alongside the workspace. It is -1 whenever Docker cannot
    answer, which includes the ordinary case of a workspace whose volume does not exist yet -
    that is not an error.
    """
    base_url = ctx.obj["base_url"]
    workspace_id = resolve_workspace_id(base_url, name)
    resp = call("get", workspace_url(base_url, workspace_id))
    show(resp)


@get.command("artifact")
@click.option(
    "-w", "--workspace", required=True, help="Workspace holding the artifact."
)
@click.argument("name")
@click.option("--presign", is_flag=True, help="Also mint a presigned GET URL.")
@click.option(
    "--ttl", type=int, default=None, help="Requested GET URL lifetime, in seconds."
)
@click.pass_context
def get_artifact(ctx, workspace, name, presign, ttl):
    """Fetch one artifact by name, optionally with a download URL.

    --ttl can only shorten the link, never extend it past the deployment's configured
    maximum, which is also the default. Asking to presign an artifact in state
    MISSING_OBJECT is a hard 409 rather than a quietly absent URL - there is no object to
    serve.
    """
    base_url = ctx.obj["base_url"]
    workspace_id = resolve_workspace_id(base_url, workspace)
    artifact_id = resolve_artifact_id(base_url, workspace_id, workspace, name)

    params = {}
    if presign:
        params["presign"] = "true"
    if ttl is not None:
        params["ttl"] = ttl

    resp = call("get", artifact_url(base_url, artifact_id), params=params)
    show(resp)


# ======================================================================================
# update


@cli.group()
def update():
    """Update resources."""


@update.group("workspace")
def update_workspace():
    """Update workspace attributes."""


@update_workspace.command("name")
@click.argument("name")
@click.pass_context
def update_workspace_name(ctx, name):
    """Rename a workspace, prompting for the new name."""
    interactive(_update_workspace_name_interactive, ctx, name)


def _update_workspace_name_interactive(ctx, name):
    base_url = ctx.obj["base_url"]
    workspace_id = resolve_workspace_id(base_url, name)

    new_name = _prompt_workspace_name("New Workspace Name: ")

    # The rename endpoint takes its new name as a query parameter, not as a body - unlike the
    # other two update endpoints.
    resp = call(
        "put", workspace_name_url(base_url, workspace_id), params={"name": new_name}
    )
    show(resp)


@update_workspace.command("description")
@click.argument("name")
@click.pass_context
def update_workspace_description(ctx, name):
    """Change a workspace description, prompting for the new text."""
    interactive(_update_workspace_description_interactive, ctx, name)


def _update_workspace_description_interactive(ctx, name):
    base_url = ctx.obj["base_url"]
    workspace_id = resolve_workspace_id(base_url, name)

    description = prompt("Description [blank to clear]: ").strip()
    if not description:
        click.echo("Clearing the description.", err=True)

    # The field is sent even when null, which is how the server is told to clear it - hence a
    # blank entry becomes an explicit None rather than an omitted key.
    resp = call(
        "put",
        workspace_description_url(base_url, workspace_id),
        json={"description": description or None},
    )
    show(resp)


@update_workspace.command("volume-meta")
@click.argument("name")
@click.pass_context
def update_workspace_volume_meta(ctx, name):
    """Change a workspace's volume provisioning metadata, prompting for the new values.

    Only permitted while the workspace has no volume: the metadata describes how to provision
    one, so the server answers 409 once volume state is anything but NONE.
    """
    interactive(_update_workspace_volume_meta_interactive, ctx, name)


def _update_workspace_volume_meta_interactive(ctx, name):
    base_url = ctx.obj["base_url"]
    workspace_id = resolve_workspace_id(base_url, name)

    size_bytes = _prompt_int(
        "Volume Size Bytes [blank to reset to deployment default]: ",
        min_value=1,
        allow_blank=True,
    )
    if size_bytes is None:
        click.echo(
            "Clearing the volume metadata; deployment defaults will apply.", err=True
        )

    # Note the wrapper key: the payload is not a bare {"size_bytes": N}.
    metadata = None if size_bytes is None else {"size_bytes": size_bytes}
    resp = call(
        "put",
        workspace_volume_meta_url(base_url, workspace_id),
        json={"volume_metadata": metadata},
    )
    show(resp)


@update.command("artifact")
@click.option(
    "-w", "--workspace", required=True, help="Workspace holding the artifact."
)
@click.option(
    "-a", "--artifact", required=True, help="Name of the artifact to replace."
)
@click.argument("path", type=click.Path(exists=True, dir_okay=False, readable=True))
@click.pass_context
def update_artifact(ctx, workspace, artifact, path):
    """Replace an artifact's content with a new file.

    The upload is the same two steps as a create; only the final call differs, since the
    name and description stay as they were. The server copies the staged bytes to a fresh
    object and flips the row over to it in one update, so there is no window where the
    artifact points at nothing - but concurrent updates are last writer wins.

    This is also the repair path for an artifact in state MISSING_OBJECT, which returns to
    RECORDED once it has an object again.
    """
    base_url = ctx.obj["base_url"]
    workspace_id = resolve_workspace_id(base_url, workspace)
    artifact_id = resolve_artifact_id(base_url, workspace_id, workspace, artifact)

    staging_object_key = stage_file(base_url, workspace_id, path)

    resp = call(
        "put",
        artifact_content_url(base_url, artifact_id),
        json={"staging_object_key": staging_object_key},
    )
    show(resp)


# ======================================================================================
# setup / teardown


@cli.group()
def setup():
    """Provision resources."""


@setup.command("workspace")
@click.argument("name")
@click.pass_context
def setup_workspace(ctx, name):
    """Provision a workspace's persistent volume.

    Synchronous - the call returns only once Docker has created the volume - and idempotent,
    an existing volume being adopted rather than rejected.
    """
    base_url = ctx.obj["base_url"]
    workspace_id = resolve_workspace_id(base_url, name)
    resp = call(
        "post",
        workspace_volume_url(base_url, workspace_id),
        timeout=VOLUME_TIMEOUT_SECS,
    )
    show(resp)


@cli.group("open")
def open_group():
    """Open an interactive session against a resource."""


@open_group.command("workspace")
@click.argument("name")
@click.pass_context
def open_workspace(ctx, name):
    """Open a shell in a throwaway container with the workspace's volume mounted.

    The volume is mounted where every cairn container mounts it, so a path written here is
    the same path the volume-based artifact endpoints take as a source or target - which is
    what makes this useful for staging files before an upload, or checking what a download
    actually landed.

    The container is interactive, so this needs a terminal; it replaces this command's
    stdin, stdout and stderr for as long as the shell runs, and exits with its status.
    """
    base_url = ctx.obj["base_url"]
    workspace_id = resolve_workspace_id(base_url, name)

    resp = call("get", workspace_url(base_url, workspace_id))
    body = decode(resp)
    if not resp.ok or not body.get("success", False):
        fail(resp, body)

    workspace = body["workspace"]
    volume_state = workspace["volume_state"]

    # Docker creates a named volume on demand, so mounting one that was never provisioned
    # would quietly manufacture an empty volume outside cairn's control - and `setup
    # workspace` adopts an existing volume, so the stray would later be taken for the real
    # thing. Refusing here keeps that from happening.
    if volume_state != "READY":
        click.echo(
            f"Workspace '{name}' has no persistent volume (volume_state {volume_state}); "
            "run `setup workspace` first.",
            err=True,
        )
        sys.exit(1)

    command = [
        "docker",
        "run",
        "--rm",
        "-it",
        "-v",
        f"{workspace['volume_name']}:{WORKSPACE_MOUNT_PATH}",
        WORKSPACE_SHELL_IMAGE,
        "/bin/sh",
    ]
    click.echo(f"$ {' '.join(command)}", err=True)

    try:
        # No capture and no redirection: the child inherits this process's streams, which is
        # what `-it` needs to hand the terminal over to the shell.
        completed = subprocess.run(command, check=False)
    except FileNotFoundError:
        click.echo("docker was not found on PATH", err=True)
        sys.exit(1)

    sys.exit(completed.returncode)


@cli.group()
def teardown():
    """Tear down resources."""


@teardown.command("workspace")
@click.argument("name")
@click.pass_context
def teardown_workspace(ctx, name):
    """Tear down a workspace's persistent volume.

    Synchronous and idempotent - an already absent volume is not an error. The server answers
    409 while Docker still reports the volume as mounted.
    """
    base_url = ctx.obj["base_url"]
    workspace_id = resolve_workspace_id(base_url, name)
    resp = call(
        "delete",
        workspace_volume_url(base_url, workspace_id),
        timeout=VOLUME_TIMEOUT_SECS,
    )
    show(resp)


# ======================================================================================
# delete


@cli.group()
def delete():
    """Delete resources."""


@delete.command("workspace")
@click.argument("name")
@click.option("--yes", "-y", is_flag=True, help="Skip the confirmation prompt.")
@click.pass_context
def delete_workspace(ctx, name, yes):
    """Delete a workspace and, by cascade, its artifact records.

    Tear the persistent volume down first: the server answers 409 while the workspace still
    has one. The reply is the bare response envelope, with no workspace in it.
    """
    if not yes and not click.confirm(
        f"Delete workspace '{name}' and all its artifacts?"
    ):
        click.echo("Aborted.", err=True)
        return

    base_url = ctx.obj["base_url"]
    workspace_id = resolve_workspace_id(base_url, name)
    resp = call("delete", workspace_url(base_url, workspace_id))
    show(resp)


@delete.command("artifact")
@click.option(
    "-w", "--workspace", required=True, help="Workspace holding the artifact."
)
@click.argument("name")
@click.option("--yes", "-y", is_flag=True, help="Skip the confirmation prompt.")
@click.pass_context
def delete_artifact(ctx, workspace, name, yes):
    """Delete an artifact from a workspace.

    The record goes immediately; the backing object is left for the maintenance sweep to
    reclaim. The reply is the bare response envelope, with no artifact in it.
    """
    if not yes and not click.confirm(f"Delete artifact '{name}' from '{workspace}'?"):
        click.echo("Aborted.", err=True)
        return

    base_url = ctx.obj["base_url"]
    workspace_id = resolve_workspace_id(base_url, workspace)
    artifact_id = resolve_artifact_id(base_url, workspace_id, workspace, name)
    resp = call("delete", artifact_url(base_url, artifact_id))
    show(resp)


if __name__ == "__main__":
    cli()

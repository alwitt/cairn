# cairn

> Shared workspaces and durable artifacts for agent tool calls.

[![Go Version](https://img.shields.io/badge/go-1.26-00ADD8?logo=go)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/license-MIT-green.svg)](./LICENSE)

`cairn` gives containerized AI-agent tool calls **a shared place to leave and pick up work, and a
durable place to keep what they produce**. Like the trail-marking stone stacks it is named for, it
lets one tool leave something a later tool — in a different container, possibly much later — can
reliably find.

Sandboxed tool runners put the host filesystem off-limits, which is the whole point of sandboxing
them; that leaves those tools with no way to hand a file to one another and no way to keep a result.
`cairn` is that substrate. It pairs an **object store** for durable artifacts with **Docker named
volumes** for shared POSIX scratch space, and owns everything in between: the metadata database,
the object-store credentials, presigned-URL minting, volume lifecycle, and the transfer sidecars
that move bytes between the two.

Its clients — [`multitool`](https://github.com/alwitt/multitool) today,
[`rest-pty`](https://github.com/alwitt/rest-pty) later — only **mount** volumes. They never touch
the object store and hold no credentials. The agent itself talks to `cairn`'s own MCP endpoint for
every artifact operation.

## Contents

- [Concepts](#concepts) — workspaces, artifacts, and the two storage layers
- [Architecture](#architecture)
- [Requirements](#requirements)
- [Quick start](#quick-start)
- [Configuration](#configuration)
- [REST API](#rest-api) · [MCP API](#mcp-api)
- [Worked example](#worked-example)
- [The maintenance daemon](#the-maintenance-daemon)
- [Development](#development)

## Concepts

### Workspace

A **workspace** is the scratch space for a **scope of work** — a chat session, an agent together
with its sub-agents, a whole project. It is deliberately *not* per-tool-call and *not* per-agent: it
is the common ground those calls and agents work over, the way a developer's project directory is
shared by every tool they run against it.

Everything in a workspace is **mutually accessible to every participant in that scope of work** —
readable, modifiable, and deletable by any container that mounts it. That is the intent, not a
concession: a tool that writes a file expects the next tool, a sub-agent, or a shell session to be
able to pick it up, rewrite it, or throw it away. Sizing that scope — and so deciding who shares one
— is the caller's decision, not the service's.

| Field | Notes |
|---|---|
| `name` | How you refer to it. Globally unique; charset `[A-Za-z0-9_-]` only — **no dots**. |
| `description` | Free text, optional. |
| `volume_name` | The Docker volume backing it, derived from the workspace's immutable ID. The service owns this name and returns it; clients never guess it. |
| `volume_state` | `NONE` or `READY` — whether the volume currently exists. **Only mount it when `READY`.** |
| `volume_metadata` | Optional provisioning hints, e.g. `size_bytes`. Read when the volume is provisioned. |

A workspace is created as a **database row only**; its volume is provisioned separately, so a new
workspace always comes back `volume_state: NONE`. Renaming is a pure database update — the volume
name comes from the ID, not the name, so it survives a rename untouched.

### Artifact

An **artifact** is a named, durable file belonging to one workspace.

| Field | Notes |
|---|---|
| `name` | Unique **per workspace**, same charset as a workspace name. Has **no relationship** to the underlying object key, so renaming moves no bytes. |
| `description` | Free text, optional. |
| `mime_type` | Sniffed by the server. Advisory metadata — a label and download-name hint, never a security boundary. |
| `size` | Size in bytes. |
| `state` | `RECORDED` — the normal live state — or `MISSING_OBJECT`. |

`MISSING_OBJECT` is a **quarantine flag**, not garbage: it means the row's backing object is gone.
The metadata is deliberately preserved as evidence of the loss rather than auto-remediated. You can
find such artifacts through the state filter on any listing, then either repair the artifact by
uploading content again or delete the row.

### The two storage layers

This is the distinction to internalize before anything else. A workspace has **two** places bytes
can live, and they exist for different reasons.

| | **Persistent volume** (scratch) | **Object store** (storage) |
|---|---|---|
| What it is | One Docker named volume per workspace | Keys in a single S3-compatible bucket |
| Reached how | Mounted at `/mnt/cairn/ws` in every container | Never mounted; reached only by presigned URL |
| Who touches it | Tool containers, shell sessions, and cairn's sidecars | cairn's service process, and nothing else |
| What it is for | A POSIX filesystem tool calls share **right now** | The durable record of what was **produced** |
| Lifetime | Disposable — gone when the operator tears it down | Survives volume teardown, and service restarts |
| Authoritative? | **No** — it is a cache | **Yes** — it is the record |

**The rule that follows: anything that must survive has to be saved as an artifact.** Un-saved
scratch in a volume is ephemeral by definition and may be lost whenever the volume is reaped. The
volume is where work *happens*; the object store is where results *stay*.

Bytes move between the two in both directions, and cairn does the moving:

```
  object store  ──  download_artifact  ──►  /mnt/cairn/ws/…   (materialize for a tool to read)
  object store  ◄──  upload_artifact   ──   /mnt/cairn/ws/…   (promote scratch to a durable artifact)
```

**One mount path, everywhere: `/mnt/cairn/ws`.** Every container that mounts a workspace volume
mounts it there — the tool container where an agent's tool writes a file, and cairn's own transfer
sidecars alike. That is what makes paths round-trip: a file a tool wrote at `/mnt/cairn/ws/out.txt`
is visible to the upload sidecar at exactly that path, so cairn uses the path you name **verbatim**
with no translation. Every artifact path you supply must therefore be absolute and under
`/mnt/cairn/ws`. It is a single service-wide constant (`models.WorkspaceMountPath`), which also
means **at most one workspace can be mounted into a given container**.

## Architecture

```
   agent ─────── MCP ───────────────────────┐
                                            │
 ┌─────────────┐                            ▼
 │  multitool  │──── look up workspace ─► ┌──────────────────────────────┐
 │  (MCP srv)  │      mount volume        │            cairn             │
 └─────────────┘                          │                              │
 ┌─────────────┐                          │  Owns:                       │
 │  rest-pty   │──── look up workspace ─► │   - Postgres metadata DB     │
 │  (future)   │      mount volume        │   - Object store + creds     │
 └─────────────┘                          │   - Presigned URL minting    │
                                          │   - Volume lifecycle         │
                                          │   - Transfer sidecars        │
                                          │                              │
                                          │  REST (operator)             │
                                          │  MCP  (agent)                │
                                          └───────────┬──────────────────┘
                                                      │
                          ┌───────────────────────────┼────────────────────────┐
                          ▼                           ▼                        ▼
                 ┌─────────────────┐        ┌──────────────────┐     ┌──────────────────┐
                 │ Docker volumes  │        │   object store   │     │  Postgres  x2    │
                 │ (scratch)       │        │   (durable)      │     │  app + tasking   │
                 └─────────────────┘        └──────────────────┘     └──────────────────┘
```

Clients only **mount** volumes. They never touch the object store, run no sidecars, and hold no
credentials.

### Two processes

`cairn` ships as one binary with two sub-commands, and a deployment runs both:

| Process | What it does | Scaling |
|---|---|---|
| `cairn server` | Serves the REST API (operator, ID-addressed) and the MCP endpoint (agent, name-addressed). Launches transfer sidecars. | Horizontally scalable — replicas are interchangeable. |
| `cairn maintainer` | The periodic reconciliation loop and object reclamation. | **Single instance.** Its Task Engine worker name must be unique per replica, and a second replica's sweep would only re-raise work the first already covers. |

### Where the credentials are

The service process is the **sole credential holder**. Everything else works from short-lived,
key- and operation-scoped presigned URLs:

- **Tool containers** — no network, no credentials, and they run agent-influenced code.
- **Stat/hash sidecar** — **no network at all**; runs a fixed command that reports a file's size
  and checksum so an upload URL can be bound to them.
- **Transfer sidecars** — reach the object store through a presigned URL and nothing else; they
  never call back into the service.

Sidecars run with a read-only root filesystem, no host mounts, no writable storage of any kind, and
every Linux capability dropped except `DAC_OVERRIDE` — enough to read and write files in a volume
whose contents other containers own, and nothing more.

## Requirements

- **Go 1.26** — to build from source.
- **Docker daemon** — workspace volumes and transfer sidecars.
- **Two Postgres instances** — cairn's own schema, and the [`tasking`](https://github.com/alwitt/tasking)
  Task Engine's. `tasking` defines and migrates its schema itself, so the two have separate
  migration histories and must not share a target.
- **Redis** — backs the Task Engine's queues. Only the `maintainer` reads it.
- **An S3-compatible object store**, and a bucket that already exists — cairn never creates one.
- **The `alwitt/cairn-sidecar` image**, pullable by the deployment, and able to reach the object
  store on the configured `artifact.sidecar.networkMode`.

For the development stack, additionally:

- **[`atlas`](https://atlasgo.io/)** on `PATH` — `make dev-migrate`.
- **Python with `poetry`** — to build the sidecar image (`sidecar/`).
- **Python with `pyenv`** — to run the demo CLI (`demo/cairn-demo.py`).
- **`pip3`** — `make` bootstraps `pre-commit` hooks on first run.

## Quick start

### Before the first `make`

Four things the `make` sequence assumes but does not do for you:

**1. Create `.env`.** The [`Makefile`](./Makefile) does `include .env`, so *every* target fails
without it. It carries the passwords and object-store credentials, which are required flags.

```bash
cp .env.example .env
```

**2. Build the sidecar image.** The default config names `alwitt/cairn-sidecar:latest`; without it,
every volume-based artifact operation fails when its sidecar is created.

```bash
cd sidecar && make docker && cd -
```

**3. Create the bucket.** `cairn` never creates one. For the dev stack that is a bucket named
`cairn-dev`, made through the RustFS console at `http://172.90.0.1:9101` (`admin` / `password`).

**4. Make the object store's hostname resolve.** The compose stack publishes RustFS at
`172.90.0.1:9100` under the hostname `cairn-dev-s3.local-stack.org`, and both
`artifact.s3.endpoint` and the sidecars address it by that name. Add a `/etc/hosts` entry (or
equivalent) pointing it at `172.90.0.1`.

### The sequence

```bash
make                     # verify the project builds (lints first)
make test                # verify all tests pass
make up                  # stand up the local docker compose stack
make dev-migrate         # apply cairn's DB migration
make dev-tasking-migrate # apply tasking's DB migration
make api &               # launch cairn's API server
make mtn &               # launch cairn's maintenance runner
```

`make api` and `make mtn` both rebuild first and then run in the foreground, so give each its own
terminal or background it as above. Both read [`demo/server_config.yml`](./demo/server_config.yml).

Tear the stack down with `make down`.

### Development stack ports

| Service | Address |
|---|---|
| REST + MCP API | `127.0.0.1:44123` |
| Prometheus metrics | `127.0.0.1:3101` |
| Application Postgres | `127.0.0.1:6532` (`cairn` / `cairn`) |
| Task Engine Postgres | `127.0.0.1:6542` (`tasking` / `tasking`) |
| Redis | `127.0.0.1:3379` |
| Object store (RustFS) | `172.90.0.1:9100`, console `172.90.0.1:9101` (`admin` / `password`) |

The demo config shortens two maintenance windows well below their defaults so a development session
actually sees a sweep run and its finished tasks reaped.

## Configuration

Two inputs: a YAML config file (loaded through [Viper](https://github.com/spf13/viper)) and a
handful of CLI flags. Anything omitted from the file falls back to a built-in default — see
`InstallDefaultServerConfigValues` in [`models/config.go`](./models/config.go). Secrets are **never**
in the file; they arrive as flags or environment variables.

### Config file

```yaml
# Namespaces this deployment's volumes. Charset [a-zA-Z0-9-_].
appName: cairn

persistence:
  sql:
    # cairn's own schema. Password supplied separately.
    app:
      debugLog: false        # emit ORM query logs
      host: 127.0.0.1
      port: 5432
      db: cairn
      user: cairn
      ssl:
        enabled: false
        # caFile: /path/to/ca.crt

    # The tasking Task Engine's schema — a SEPARATE database, migrated by tasking
    # itself, and free to live on a different server under a different user.
    tasking:
      debugLog: false
      host: 127.0.0.1
      port: 5432
      db: tasking
      user: cairn

  # Backs the Task Engine's queues. Only the maintainer reads this.
  redis:
    host: 127.0.0.1
    port: 6379
    dbNumber: 0

metrics:
  service:
    listenOn: 0.0.0.0
    appPort: 3001
    timeoutSecs: { read: 60, write: 60, idle: 60 }
  metricsEndpoint: /metrics
  maxRequests: 4
  features:
    enableAppMetrics: false   # Go runtime metrics
    enableHTTPMetrics: true   # HTTP request metrics

api:
  service:
    listenOn: 0.0.0.0
    appPort: 44123
    # write is generous because the slowest request is a volume-based upload:
    # two sidecar runs back to back, plus the object-store copy.
    timeoutSecs: { read: 60, write: 660, idle: 60 }
  apis:
    endPoint:
      pathPrefix: /           # prepended to every route below
    requestLogging:
      logLevel: warn
      healthLogLevel: debug
      requestIDHeader: X-Request-ID
      logRequestPayload: false
    mcp:
      enable: false           # the agent-facing surface; OFF by default
      enableDNSRebindGuard: true

workspace:
  volumeType: docker          # the only supported backing today

artifact:
  s3:
    clientTTL: 3600           # rebuild the client this often, so rotated creds get picked up
    endpoint: s3.example.org:9000   # no default — you are setting this
    useTLS: true              # defaults secure; getting it wrong fails to connect
    # region: us-east-1

  store:
    bucket: cairn             # no default; must already exist
    putUrlTTLSecs: 360        # only has to outlive the transfer it was minted for
    getUrlMaxTTLSecs: 900     # ceiling on a download link; callers may ask for less
    maxObjectSize: 1073741824 # 1 GiB. Single PUT — multipart upload is out of scope.
    prefix:
      # base: <optional leading segment for every key>
      staging: staging        # in-flight uploads
      store: store            # final artifact objects

  sidecar:
    image: alwitt/cairn-sidecar:latest
    timeoutSecs: 300
    networkMode: bridge       # must be able to reach the object store
    # envs:  [{ name: ..., value: ... }]
    # hosts: [{ host: ..., address: ... }]

# tasking Task Engine. One worker and one scheduler serve the deployment; the
# maintainer is the process that runs them.
tasking:
  # MUST be unique per replica and stable across that replica's restarts — a worker
  # reclaims the executions this name held before the last one.
  workerName: cairn-worker
  schedulerQueue: cairn-task-scheduler
  taskQueue: cairn-tasks
  schedulerMaintenanceIntSec: 30

maintenance:
  sweepIntSec: 300            # how often the reconciliation loop runs
  # The grace window separating an in-flight upload from an orphan. MUST comfortably
  # exceed the slowest upload, or a sweep will flag a transfer that is still running.
  objAgeOutSec: 3600
  taskAgeOutSec: 86400        # how long finished maintenance tasks are kept
```

### CLI flags

| Flag | Alias | Env var | Scope | Default |
|---|---|---|---|---|
| `--log-level` | `-l` | `LOG_LEVEL` | global | `warn` |
| `--json-log` | `-j` | `LOG_AS_JSON` | global | `false` |
| `--sql-pw` | — | `CAIRN_APP_SQL_PASSWORD` | global, **required** | — |
| `--s3-access-key` | — | `CAIRN_S3_ACCESS_KEY` | global, **required** | — |
| `--s3-secret-key` | — | `CAIRN_S3_SECRET_KEY` | global, **required** | — |
| `--config-file` | `-c` | `CAIRN_CONFIG_FILE` | both sub-commands, **required** | — |
| `--tasking-sql-pw` | — | `CAIRN_TASKING_SQL_PASSWORD` | `maintainer` only, **required** | — |

```bash
./cairn -l info server -c demo/server_config.yml
./cairn -l info maintainer -c demo/server_config.yml   # alias: mtn
```

`--tasking-sql-pw` is scoped to `maintainer` rather than global on purpose: the server opens no Task
Engine database, so it is never asked for a credential it could not use.

## REST API

The operator-facing surface. It addresses entries by **ID** — a caller here has already resolved
names to IDs in its own UI or context. All `v1` paths are relative to the configured `pathPrefix`.

| Area | Method & path |
|---|---|
| Health | `GET /liveness/alive`, `GET /liveness/ready` |
| Workspaces | `POST /v1/workspaces`, `GET /v1/workspaces` |
| Workspace | `GET` / `DELETE /v1/workspaces/{workspaceID}` |
| Workspace attributes | `PUT /v1/workspaces/{workspaceID}/{name,description,volume-metadata}` |
| Volume lifecycle | `POST` / `DELETE /v1/workspaces/{workspaceID}/volume` |
| Staging URL | `POST /v1/workspaces/{workspaceID}/new-staging` |
| Artifacts | `POST` / `GET /v1/workspaces/{workspaceID}/artifacts` |
| Save from volume | `POST /v1/workspaces/{workspaceID}/artifact-from-volume` |
| Artifact | `GET` / `DELETE /v1/artifacts/{artifactID}` |
| Artifact attributes | `PUT /v1/artifacts/{artifactID}/{name,description}` |
| Replace content | `PUT /v1/artifacts/{artifactID}/content` |
| Volume transfer | `POST /v1/artifacts/{artifactID}/{load-in-volume,update-from-volume}` |
| MCP | `POST /v1/mcp` — only when `api.apis.mcp.enable` is true |

Three things worth knowing before you call it:

- **Uploading a file you hold is two calls.** First presign a staging PUT — bound to an exact byte
  size and SHA-256 — then register the artifact from that staging key. No database row exists until
  the bytes do, so there is no such thing as a half-created artifact.
- **Uploading a file already in a volume is one call.** `artifact-from-volume` and
  `update-from-volume` hand the work to cairn, which runs the sidecars; you only name a path.
- **Volume create and delete are synchronous and blocking.** The call returns once Docker has acted.
  Delete refuses — rather than waits — while any container still mounts the volume, and deleting a
  workspace refuses unless its `volume_state` is already `NONE`. Tear down bottom-up: stop
  workloads, delete the volume, then delete the workspace.

## MCP API

The agent-facing surface, exposing the artifact operations as
[Model Context Protocol](https://modelcontextprotocol.io) tools. It is **disabled by default**:

```yaml
api:
  apis:
    mcp:
      enable: true
```

Once enabled, an MCP server (streamable HTTP transport, stateless) is mounted at **`POST /v1/mcp`**,
relative to the configured `pathPrefix`.

**Workspaces**

| Tool | Purpose |
|---|---|
| `list_workspaces` | List available workspaces, optionally filtered by name or volume state |
| `get_workspace` | Fetch one workspace and whether its volume is ready to mount |

**Artifacts**

| Tool | Purpose |
|---|---|
| `list_artifacts` | List a workspace's artifacts (metadata only); also how you check one exists |
| `download_artifact` | Object store → volume. Writes an artifact's content where a tool can read it |
| `upload_artifact` | Volume → object store, as a **new** artifact. The name must be free |
| `update_artifact` | Volume → object store, replacing an **existing** artifact's content |
| `delete_artifact` | Delete an artifact |
| `rename_artifact` | Change an artifact's name; content and description untouched |

### How it differs from the REST API

- **Name-addressed, not ID-addressed.** Agents cannot reliably hold and echo ULIDs, so every tool
  takes workspace and artifact *names* and resolves them at the boundary.
- **No volume lifecycle.** An agent can neither create nor destroy a volume — that is the operator's
  job, deliberately, because other systems may also mount a workspace's volume and cairn cannot know
  every mounter. `get_workspace` reports `volume_state` so an agent knows whether it can proceed.
- **Every path is absolute and under `/mnt/cairn/ws`.** The volume is mounted there in both the tool
  container and cairn's sidecars, so the path an agent names is the path the sidecar uses.
- **`download_artifact` will not create directories.** The destination's parent must already exist.
- **Every call is synchronous.** cairn launches the sidecars and waits; the work is finished when
  the tool returns.

## Worked example

[`demo/cairn-demo.py`](./demo/cairn-demo.py) is a CLI for driving the REST API by hand. It addresses
things by name — resolving each through a listing endpoint first — so it reads much like the MCP
surface. It defaults to `http://localhost:44123`; override with `--base-url` or `CAIRN_API_URL`.

```bash
alias cairn-demo='PYENV_VERSION=cairn pyenv exec python demo/cairn-demo.py'
```

The walkthrough below takes a file from nothing, through scratch space, to a durable artifact, and
back — which is the volume/object-store distinction end to end.

**1. Create a workspace.** Interactive; prompts for a description and optional volume size. This
writes the database row only, so it comes back `volume_state: NONE`.

```bash
cairn-demo create workspace
```

**2. Provision its volume** — synchronous, and idempotent (an existing volume is adopted rather than
rejected). `volume_state` goes `NONE` → `READY`.

```bash
cairn-demo setup workspace my-ws
```

**3. Open a shell on the volume.** A throwaway `alpine` container with the volume mounted at
`/mnt/cairn/ws` — the same path every cairn container uses, which is what makes the paths in the
next steps work. This is where you `mkdir` a directory a download will need.

```bash
cairn-demo open workspace my-ws
/ # echo 'hello from the scratch volume' > /mnt/cairn/ws/notes.txt
/ # mkdir -p /mnt/cairn/ws/out
/ # exit
```

**4. Promote scratch to a durable artifact.** cairn runs a stat/hash sidecar and then an upload
sidecar; `notes.txt` becomes an artifact that outlives the volume. `--name` is required rather than
derived from the filename, because artifact names allow no dots.

```bash
cairn-demo volume upload -w my-ws --name notes /mnt/cairn/ws/notes.txt
```

**5. Confirm it, and mint a download link.** `--ttl` can only shorten the link, never extend it past
the deployment's configured maximum.

```bash
cairn-demo list artifact my-ws
cairn-demo get artifact -w my-ws notes --presign
```

**6. Materialize it back into the volume.** The target's parent directory must already exist — hence
the `mkdir` in step 3.

```bash
cairn-demo volume download -w my-ws -a notes /mnt/cairn/ws/out/notes.txt
```

**7. Tear down bottom-up** — the volume first, then the workspace. `delete workspace` refuses while
`volume_state` is `READY`.

```bash
cairn-demo teardown workspace my-ws
cairn-demo delete workspace my-ws
```

The point of step 7: **the volume goes and the artifact stays.** Anything left in `/mnt/cairn/ws`
that was never uploaded is gone with it — deleting the workspace is what finally drops the
artifacts too.

To skip the volume entirely and upload a file from this host, use the staging path instead:

```bash
cairn-demo create artifact -w my-ws --name report ./report.pdf
cairn-demo update artifact -w my-ws -a report ./report-v2.pdf
```

Run `cairn-demo --help` for the full command set, including renames, description updates, and the
listing filters.

## The maintenance daemon

`make mtn` runs the second process. It reconciles the database against the two external systems
cairn tracks — the object store and Docker — and reclaims orphaned objects through a `tasking` task.
Three consequences you will actually observe:

- **Deleting an artifact or a workspace deletes rows, not objects.** The freed objects are reclaimed
  on a later sweep, so reclamation is eventually consistent, bounded by `maintenance.sweepIntSec`
  plus `maintenance.objAgeOutSec`. Every deletion is gated by that grace window, which is what
  separates an orphan from an upload still in flight.
- **A volume removed behind cairn's back self-heals.** A `READY` workspace whose volume has vanished
  is corrected to `NONE`; a volume found for a `NONE` workspace is adopted back to `READY`. A volume
  is disposable scratch, so neither case is flagged as an incident.
- **An artifact whose object has vanished is flagged, not deleted.** It moves to `MISSING_OBJECT`
  and stays there as evidence. Find them with the state filter on any listing; repair one by
  uploading content for it again, or delete the row.

The loop is level-triggered — every run re-derives what needs doing from durable state — so a
restart resumes cleanly and a missed sweep simply gets redone.

## Development

Common tasks are wrapped in the [`Makefile`](./Makefile):

| Target | Does |
|---|---|
| `make` / `make build` | Lint, then build the `cairn` binary |
| `make lint` | `go mod tidy`, `go fmt`, `go vet`, `revive`, `golangci-lint` |
| `make fix` | Lint, applying `golangci-lint --fix` |
| `make test` | Run the unit tests |
| `make one-test FILTER=<TestName>` | Run a single test, verbosely |
| `make test-package PKG=<pkg>` | Run one package's tests |
| `make mock` | Regenerate mocks (`mockery`) |
| `make up` / `make down` | Start / stop the docker compose development stack |
| `make gen-migrate` | Generate a new migration from the GORM models (`atlas`) |
| `make dev-migrate` | Apply cairn's migration to the dev Postgres |
| `make dev-tasking-migrate` | Apply tasking's migration to its dev Postgres |
| `make api` | Build, then run the API server against the demo config |
| `make mtn` | Build, then run the maintenance daemon against the demo config |
| `make help` | List every target |

The first `make` also installs the repo's `pre-commit` hooks, which needs `pip3`.

- **Sidecar code** lives in [`sidecar/`](./sidecar) and has its own
  [README](./sidecar/README.md) and Makefile; `make docker` there builds the image.
- **Architecture and design rationale** — every decision above, with the reasoning behind it — is in
  [`DESIGN.md`](./DESIGN.md).

## License

Released under the [MIT License](./LICENSE).

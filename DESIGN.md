# cairn — Agent Workspace & Artifact Service

**`cairn`** is a standalone microservice that gives AI-agent tool calls a shared,
durable place to leave and pick up work. Like the trail-marking stone stacks it is
named for, it lets one containerized tool leave an artifact that another tool —
possibly in a different container, possibly much later — can reliably find. It
pairs an **object store** (the durable, authoritative record of artifacts) with
**Docker named volumes** (an ephemeral POSIX scratchpad that tool containers
mount and share), and owns everything in between: the Postgres metadata DB,
object-store credentials, presigned-URL minting, volume lifecycle, and the
transfer sidecars that move bytes between the two.

Its clients are the sandboxed-execution services in the same family —
**`multitool`** (its first client) and, later, **`rest-pty`** — which only *mount*
volumes and never touch storage directly. Agents talk to `cairn`'s own MCP
endpoint for all artifact operations.

> **Status:** working design, captured mid-discussion. `cairn` is a **new,
> standalone microservice** — *not* part of `multitool`, which is its first
> *client*.

---

## 0. Motivation

Containerized tool-call instances (e.g. `multitool`'s sandboxed tools) need a way
to **share files between calls** — one tool produces a file, another consumes it —
and to **persist** produced artifacts durably. The host filesystem is off-limits
(the whole point of the sandboxing effort), so a dedicated storage substrate is
needed.

Two distinct needs, deliberately separated:

- **Durable record** — an **object store** holds persisted artifacts. This is the
  system of record; it survives container teardown, workspace teardown, and
  service restarts.
- **Shared scratchpad** — a **Docker named volume** gives containers a POSIX
  filesystem they can mount and share. It is an ephemeral cache, not the record.

**Authority rule:** the **object store is authoritative**; the named volume is a
**disposable cache**. Any file that must survive has to be `upload`ed to the
store. Un-saved scratch in a volume is ephemeral by definition, and may be lost
whenever the volume is reaped.

---

## 1. Architecture: a standalone microservice

```
┌─────────────┐   query workspace → volume name     ┌────────────────────────┐
│  multitool  │───────────────────────────────────▶ │        cairn           │
│  (MCP srv)  │            mount volume             │                        │
└─────────────┘                                     │                        │
┌─────────────┐   (retrofitted, future)             │  Owns:                 │
│  rest-pty   │───────────────────────────────────▶ │  - Postgres DB         │
└─────────────┘                                     │  - Object store + creds│
                                                    │  - Presigned URL mint  │
   Agent talks to the service's MCP endpoint        │  - Volume lifecycle    │
   directly for ALL artifact operations.            │  - Transfer sidecars   │
                                                    │  REST (operator) +     │
                                                    │  MCP (agent)           │
                                                    └────────────────────────┘
```

- `cairn` is **self-contained**: it owns the DB, the object store (and its
  credentials), presigned-URL minting, named-volume lifecycle, and **all
  transfer-sidecar Docker work**.
- **Clients (`multitool`, later `rest-pty`) only mount volumes.** They never touch
  the object store, never run transfer sidecars, never manage volume lifecycle.
- `multitool` exposes **zero** artifact-related MCP tools. The **agent talks to
  the service's own MCP endpoint** for artifact operations, and to `multitool`
  only for tool execution.
- The two MCP servers coordinate **through the shared named volume**, plus
  `multitool`'s single lookup of a workspace (by name → reads the stored
  `VolumeName`, mounts only when `VolumeState = READY`) — they never call each
  other on the artifact data path. The service **owns and returns** the volume
  name; clients never guess or derive it.

### Docker responsibility split

| Component | Docker responsibilities |
|---|---|
| **`cairn`** | Volume lifecycle (create/delete); transfer sidecars (download artifact→volume, upload volume→artifact); the in-use check for volume deletion. |
| **`multitool`** | Runs tool containers; **mounts** workspace volumes into them. No volume lifecycle, no transfer sidecars. |
| **`rest-pty`** (future) | Runs shell-session containers; **mounts** workspace volumes. Same mount-only posture. |

> Three services now need Docker integration (`multitool`, `rest-pty`, `cairn`).
> Reusable Docker components should later be factored into `goutils` —
> **deferred to a separate effort.**

---

## 2. Data model

### 2.1 Workspace

| Field | Type | Notes |
|---|---|---|
| `ID` | string (UUID) | Primary identity; drives object-key construction **and** the volume name. |
| `Name` | string, unique | How the agent refers to it. Charset restricted to `[A-Za-z0-9_-]`. |
| `Description` | string, optional | Free-text. |
| `VolumeName` | string | The associated volume's name — **derived from `ID`** (`<app-name>-<workspace ID>`) and **persisted** at workspace-create. See below. |
| `State` | string enum | Workspace lifecycle. See §2.1.1. |
| `VolumeState` | string enum | Associated volume's lifecycle. Independent axis from `State`. See §2.1.2. |
| `CreatedAt` | timestamp | — |
| `UpdatedAt` | timestamp | — |
| `PurgedAt` | timestamp, nullable | Set when the workspace reaches `PURGED`. **Deliberately named `PurgedAt`, not `DeletedAt`** — a `DeletedAt time.Time` field triggers GORM's soft-delete behavior, which we do not want. |

**`VolumeName` is ID-derived and persisted (a convenience cache).** It is computed
once as `<app-name>-<workspace ID>` at workspace-create and stored, so **no client
ever guesses or re-derives it** — `multitool`/`rest-pty` fetch the workspace and
read `VolumeName` directly (the service owns and returns the name). The
application/project name is chosen when the repo is created, and namespaces the
service's volumes for `VolumeList` filtering.

- **Why `ID`, not `Name`:** the ID is **immutable**, so the volume name is stable
  across a workspace rename. This means rename **never touches the volume** — the
  old `Name`-requires-`VolumeState=NONE` rename guard is **gone** (§2.1.1, §7.1).
- **Existence is `VolumeState`, not null-ness:** `VolumeName` is **always
  populated** (present from create, even while `VolumeState = NONE`). It is a name
  cache, not an existence flag. **Reader contract: only mount `VolumeName` when
  `VolumeState = READY`.** Keeping the two concerns separate (name = identity,
  state = existence) also lets §8.2.2 reconciliation always know which volume to
  `VolumeInspect`.

**No `Artifact Object Key Prefix` column.** The key prefix is **service-level
config** (same for every workspace); the `<ws-id>` segment of the key already
isolates workspaces. Final object key = `<config prefix>/<workspace ID>/<random>`.

#### 2.1.1 Workspace `State` enum
`ACTIVE` → `PENDING_PURGE` → `PURGING` → `PURGED`

| State | Meaning |
|---|---|
| `ACTIVE` | Defined and ready for operations. |
| `PENDING_PURGE` | Marked for deletion; **queued** for the maintenance system, not yet being worked. |
| `PURGING` | Maintenance system is **actively** purging it. |
| `PURGED` | Purge complete (terminal). Row + associated data entries removed on the next cleanup round. |

- The `PENDING_PURGE` / `PURGING` split lets the maintenance system cheaply tell
  **waiting-work** from **in-flight-work** (and makes a post-crash `PURGING` row an
  obvious resume/inspect signal).
- **Guard:** a workspace can enter `PENDING_PURGE` **only from `ACTIVE` with
  `VolumeState = NONE`** — it must have no live volume before it can be marked.

#### 2.1.2 Workspace `VolumeState` enum
`NONE` ⇄ `READY`

| State | Meaning |
|---|---|
| `NONE` | No volume exists (workspace may still be `ACTIVE`). |
| `READY` | Volume exists and is mountable. |

**No transient `CREATING`/`DELETING` states.** Volume create and delete are
**synchronous, blocking REST operations** (§4.2) — the call does not return until
Docker has actually created/removed the volume, so the DB column is only written
**after** Docker succeeds. There is no window where the column claims a state
Docker has not reached, and a failed op leaves the column at its prior value. (If
create/delete latency ever becomes unacceptable, async transient states can be
reintroduced — deferred until then.)

This is a **separate axis** from `State`: an `ACTIVE` workspace can have a volume
in either state. See §4 for the operator-controlled lifecycle and the teardown
guards, and §8.2.2 for reconciliation when Docker drifts from the column.

### 2.2 Artifact

| Field | Type | Notes |
|---|---|---|
| `ID` | string (ULID) | **Sortable** primary identity. |
| `WorkspaceID` | string (UUID), FK | Parent workspace. Backs `list_artifacts`, the `(WorkspaceID, Name)` uniqueness constraint, and cascade-on-workspace-purge. Not derived from the object key. |
| `Name` | string, unique per workspace | Display name; how the agent refers to it. **No relationship to the object key.** |
| `Description` | string, optional | Free-text. |
| `ObjectKey` | string | The **complete** object key in the store. |
| `MIMEType` | string | **Server-sniffed** content type (§6). Stored on the row so `list`/`fetch` report it without a `HeadObject`. Also set as the object's `Content-Type`. |
| `Size` | int64 | Size in bytes (obtained during the sniff/ranged-GET). |
| `State` | string enum | Artifact lifecycle. See §2.2.1. |
| `CreatedAt` | timestamp | — |
| `UpdatedAt` | timestamp | — |
| `PurgedAt` | timestamp, nullable | Set when the artifact reaches `PURGED`. **Named `PurgedAt`, not `DeletedAt`**, to avoid GORM's soft-delete behavior (same as workspace, §2.1). |

- `ObjectKey` is `<config prefix>/<ws-id>/<random>` — a **random suffix, not the
  name** — so **rename is a pure DB update** (no object move).
- **Mutable** ≈ `Name`, `Description`. **Immutable** ≈ `ID`, `WorkspaceID`,
  `ObjectKey` (replaced wholesale on update, §6.3, never edited in place).

#### 2.2.1 Artifact `State` enum
```
RECORDED ─────────────▶ PENDING_PURGE ─▶ PURGING ─▶ PURGED
    │                        ▲
    └──▶ MISSING_OBJECT ─────┘
```

| State | Meaning |
|---|---|
| `RECORDED` | Registered and stored — the sole **live** state. |
| `MISSING_OBJECT` | Data-consistency flag: the row's backing object is **gone** (§8.2.1 item 3). A **quarantine** state — not auto-remediated, preserves the metadata as evidence of the loss. Can still be moved to `PENDING_PURGE` so an operator can purge it. |
| `PENDING_PURGE` | Marked for purging; **queued** for lifecycle management. |
| `PURGING` | Lifecycle management is **actively** deleting it. |
| `PURGED` | Purge complete (terminal); row removed on the next cleanup round. |

- There is **no `PENDING` state.** The two-call upload (§6.1) defers the DB insert
  until **after** `CopyObject`, so a row only ever appears already-committed as
  `RECORDED` — no dangling pending rows by construction. The object/staging
  reconciliation in §8.2.1 governs the **object/staging** lifecycle, not an
  artifact row state.
- Mirrors the workspace `PENDING_PURGE`/`PURGING` split (§2.1.1): the maintenance
  system distinguishes queued from in-flight, and a post-crash `PURGING` row is a
  resume/inspect signal.

### 2.3 No tenancy / no ownership
The service represents **no tenancy model** — no owner/user/org column, no user
table, no user CRUD. Any such model would dictate multitenancy for consuming
systems (e.g. an org→group→user hierarchy would not fit a flat `owner`). Instead:

- Names are **globally unique** (workspaces) / **unique-per-workspace**
  (artifacts). Uniqueness is **unqualified**.
- **Cross-tenant name collision is explicitly NOT solved here.** A shared instance
  assumes a single tenant, or a caller that pre-namespaces names. This is a
  **documented deployment constraint**, not logic in the service (cf. concurrency
  deferred to an upstream proxy).

### 2.4 No authn / authz
The service performs **no authentication or authorization** of its callers. Like
`multitool`, it runs behind a reverse proxy where ingress, TLS, and any access
control live — that is not this service's job.

- Operator (REST) and agent (MCP) requests are trusted at the point they reach the
  service; the proxy is responsible for gating them.
- Communication between `multitool` (and later `rest-pty`) and this service is
  **inter-service communication**, explicitly **not** covered by authn/authz here.
- This is orthogonal to §5's *sidecar* trust posture, which is about credential
  containment in transfer containers, not caller identity.

---

## 3. Addressing conventions

| Surface | References entries by | Why |
|---|---|---|
| **REST** (operator/frontend) | **ID** | The caller already resolved names→IDs in its own UI/context. |
| **MCP** (agent) | **Name** | Agents cannot reliably hold/echo ULIDs. |

Name→ID resolution is therefore **confined to the MCP layer**. Every MCP handler
resolves `(workspace-name[, artifact-name]) → IDs` once at the boundary; all
downstream logic (sidecars, core functions) is ID/key-addressed. REST never
resolves names.

---

## 4. Lifecycle & teardown

### 4.1 Workspace record
- **Created** (REST) → DB row only, `State = ACTIVE`, `VolumeState = NONE`.
- **Active** → has artifacts and/or a live volume (`VolumeState = READY`).
- **Marked for deletion** (REST) → `State = PENDING_PURGE` (guard: only from
  `ACTIVE` with `VolumeState = NONE`). The maintenance system then drives the
  multi-phase purge (§8.3.3): `PURGING` → cascade artifacts → completion poll →
  `PURGED`; the record and its data entries are removed on the next cleanup round
  (§8.3.4).

### 4.2 Named volume — created & destroyed by the **operator** via REST
Volume lifecycle is **not** in the agent's control. This is deliberate: other
systems (e.g. `rest-pty`) may also mount a workspace's volume, so the service
**cannot know all mounters** and therefore **cannot auto-reap**. Volumes are
created and deleted **explicitly by the operator**.

- Volume **created** on operator request — **synchronous & blocking**: the REST
  call returns only when Docker has created the volume (or failed). On success the
  DB flips `NONE → READY`; on failure the column stays `NONE`. Idempotent.
- Volume **deleted** only on operator request — **synchronous & blocking**: the
  service verifies no container mounts it (§4.3), removes it, and returns only when
  Docker has removed it (or failed/refused). On success the DB flips `READY →
  NONE`; on failure the column stays `READY`.

Because both ops write the column **only after** Docker succeeds, `VolumeState`
has no transient states (§2.1.2). Drift can still arise from *outside* the service
mutating Docker (a human `docker volume rm`, host pruning, an orphan from a prior
incarnation) — reconciled in §8.2.

### 4.3 The teardown dependency chain (hard guards)
```
containers mounting volume  →  volume  →  workspace record
   (must all stop)              (deletable)   (destroyable)
```
- **`delete volume` refuses while any container mounts it.** The authoritative
  gate is **`VolumeRemove(force=false)`** — the daemon checks in-use atomically and
  returns a "volume is in use" error (no TOCTOU). `ContainerList{volume=<name>}`
  supplies the **human-readable detail** ("held by containers A, B") for the
  operator; `RefCount` via `system/df` gives a **cheap bulk count** for the
  maintenance system's read-only scans (`-1` when unavailable; counts *referencing*,
  not just running, containers). The DB cannot answer this because mounts come from
  multiple clients the service didn't launch. Docker is the **sole source of truth
  for volume in-use state.**
- **`mark workspace for deletion` refuses unless `VolumeState = NONE`** (§2.1.1
  guard — the volume must already be gone).
- These are **refusals, not waits** — REST returns an error; the operator retries.
- Operator tears down **bottom-up**: stop workloads → delete volume
  (`VolumeState → NONE`) → mark workspace for deletion (`State → PENDING_PURGE`).

---

## 5. Transfer sidecars & security posture

Byte movement between volume and object store is done by short-lived,
**service-launched** sidecar containers (never a client, never the tool
container).

### 5.1 Trust tiers
| Container | Network | Credentials | Runs agent-influenced code |
|---|---|---|---|
| **Tool container** (in `multitool`) | none | none | yes |
| **Transfer sidecar** | **object-store only** (scoped egress) | **none** (uses presigned URLs) | no — fixed `curl` over server-provided URLs |
| **Service process** | yes | **yes** (sole credential holder) | no |

### 5.2 Presigned URLs eliminate in-sandbox credentials
The service holds object-store credentials and mints **short-lived, key- and
operation-scoped presigned URLs**. The sidecar receives only a URL — a bearer
token, no durable secret. Enforcement of *which key/operation* lives in the
**minting step** (server-side), never agent-supplied.

Disciplines:
- URL passed to the sidecar via **env var, not argv** (keep it out of
  `/proc/<pid>/cmdline`).
- **Redact URLs from logs** (signature is in the query string).
- **Short TTL**, matched to the transfer.
- **Single-PUT size cap** for the first cut (multipart deferred). "Too big" is an
  error, not something to engineer around.

> Note: sidecars do **NOT** call back into the service's REST API (see §7.3). So
> sidecar egress is object-store-only; it does not need to reach the service.

---

## 6. Upload path: staging + server-side MIME sniff

Because the system is **human- and browser-facing**, an artifact's stored
`Content-Type` must be **trustworthy** — it drives browser rendering and is a
security boundary (a spoofed `text/html` artifact viewed directly = stored XSS).
The upload source is **not trusted** to declare MIME. Therefore the server derives
MIME from the bytes.

### 6.1 Two REST calls (no DB write until bytes exist)
1. **Request staging upload URL** (workspace by ID) — server presigns a PUT to a
   **server-generated, workspace-scoped staging key**, returns
   `{ url, staging_key, hmac }`. **No DB operation.**
2. **Register artifact from staging** (workspace by ID) — caller sends
   `staging_key` + `hmac` (+ name, description). Server:
   1. Verifies the HMAC (proves the staging key was server-issued for *this*
      workspace — the HMAC covers `(staging_key, workspace_id)`).
   2. **Ranged-GET** the staging object prefix, **sniff MIME** (magic-number lib).
   3. Generate final key `<prefix>/<ws-id>/<random>`.
   4. **`CopyObject`** staging → final with the sniffed `Content-Type`.
   5. **Insert** the artifact DB row (uniqueness enforced by the DB constraint on
      `(WorkspaceID, Name)`; a handler pre-check is only a fail-fast optimization).
   6. **Delete** the staging object (best-effort).

**Ordering rule:** copy → insert (constraint-guarded) → best-effort staging
delete. Never insert before copy (would yield a row pointing at nothing).

**Deferring the DB row to call 2 means no dangling `pending` rows** for abandoned
uploads — an abandoned upload leaves only a staging object, reaped by the
data-consistency job for aged staging objects (§8.2.1 item 1).

### 6.2 `CopyObject` mechanics
- `CopyObject` **into a new key** with the sniffed `Content-Type` — server-side,
  metadata-cheap (no byte re-transfer through the service).
- Same-key `CopyObject` **is** allowed by S3-compatible stores but **requires
  `MetadataDirective=REPLACE`** (self-copy with the `COPY` directive is rejected).
  The design uses **new keys** (see update, §6.3) to stay atomic and sidestep the
  self-copy versioning caveat.

### 6.3 Update = register minus the insert
Update reuses the staging flow but does **not** create a new DB row:
- New staging upload → sniff → **`CopyObject` to a NEW final key** → **update the
  row's `object_key`/MIME/size** to the new key → delete staging.
- The flip from old key to new key is a single row update → **atomic** (readers
  always see a complete object).
- The **old object is orphaned by design** → reaped by the maintenance system
  (§8.2.1 item 2).

---

## 7. API surfaces

### 7.1 REST API (operator; ID-addressed)
| Endpoint | Notes |
|---|---|
| Create workspace | DB row only, no volume. |
| List workspaces | — |
| Fetch workspace | Record + derived state (volume exists? mounted?). |
| Rename workspace (`Name`) | Pure DB; **no volume guard**. The volume name is derived from the immutable `ID` (§2.1), so rename never affects the volume — safe even with a live, mounted volume. Object key is also ID-based, untouched. |
| Update workspace `Description` | Pure DB; no guards. |
| Mark workspace for deletion | Refuse unless `VolumeState = NONE`; cascade artifacts → `PENDING_PURGE`. |
| Create volume | Provision named volume (idempotent). |
| Delete volume | **Refuse if mounted** (Docker-side check). |
| **Generate staging PUT URL** | Pure object-store; returns `{url, staging_key, hmac}`; no DB. |
| **Register artifact from staging** | §6.1 step 2. |
| List artifacts | Metadata only; excludes non-`RECORDED`. |
| Fetch artifact (opt. `?presign` GET URL) | Non-`RECORDED` artifacts not served. |
| Delete artifact | **Marks** for deletion (`PENDING_PURGE`); mechanism → maintenance system (§8.3.2); idempotent. |
| Update artifact from staging | §6.3 (replaces the bytes/`ObjectKey`). |
| Rename artifact (`Name`) | Pure DB; the object key is random-suffixed so it is untouched (§2.2). Uniqueness enforced by `(WorkspaceID, Name)`. |
| Update artifact `Description` | Pure DB; no guards. |
| **Load artifact → volume** | Shared data path with MCP (§7.4). |
| **Save artifact ← volume** | Shared data path with MCP (§7.4). |

### 7.2 MCP API (agent; name-addressed)
Resolve names→IDs at the handler, then everything downstream is ID/key-addressed.

| Tool | Behavior |
|---|---|
| `list_artifacts` | List artifacts in a workspace. |
| `download_artifact` | Object → volume. Presign **GET**; sidecar mounts volume, `curl -o /vol/<path>`. Read direction, no core-register step. |
| `upload_artifact` | Volume → object, **new** artifact. See §7.3. |
| `update_artifact` | Volume → object, replaces an **existing** artifact's bytes. Same as upload but calls the **update** core function. |
| `delete_artifact` | Marks an artifact for deletion. |
| `rename_artifact` | Updates an artifact's name (pure DB update). |
| `list_workspaces` | Read-only. Lists workspaces the agent can use. |
| `get_workspace` | Read-only. Confirms a workspace exists and reports its `VolumeState` (whether a volume is mounted-ready). |

### 7.3 Synchronous MCP model — call the core function directly (no self-REST hop)
An MCP tool call is **synchronous and server-orchestrated**: the service launches
the sidecar, waits for it, and knows exactly when the upload finished. The REST
staging→upload→**notify** callback exists only because REST clients are
asynchronous/external; that asymmetry does not apply to MCP. So the MCP tool does
**not** curl back into its own REST endpoint.

`upload_artifact` flow:
1. Resolve workspace name → ID; **pre-check** `(workspace, name)` availability.
2. **Presign** staging PUT URL (in-process).
3. **Sidecar**: mount volume, `curl -T /vol/<path> <staging-url>`, exit. Service
   waits (`ContainerWait`).
4. On success → **call `RegisterArtifactFromStaging(...)` directly** (in-process):
   sniff → `CopyObject` → insert (constraint-guarded) → delete staging.
5. Return the tool result from the core function's outcome.

`update_artifact` is identical but calls `UpdateArtifactFromStaging(...)`.

**Reuse pattern — one core function, two front-doors:**
```
RegisterArtifactFromStaging(ctx, workspace, name, staging_key, …) → (artifact, error)
   ├── REST handler → thin HTTP shim (unmarshal → core → marshal)
   └── MCP upload   → calls core directly after the upload container exits
```
Same DRY benefit as the abandoned callback design, but shared at the
**function-call layer** — no auth/network/async cost, and sidecar egress stays
object-store-only.

### 7.4 Shared data path (REST ⇄ MCP volume transfers)
The REST "load into volume" / "save from volume" endpoints and the MCP
`download_artifact` / `upload_artifact` tools are **thin front-doors onto one
shared transfer engine**. They differ only in **addressing** (REST: IDs; MCP:
names→IDs) and **caller** (operator vs. agent). Designed once, so REST and MCP
cannot drift.

### 7.5 Handler-level validation & preconditions (all volume-touching ops)
- **Volume path validation** — agent-supplied `/vol/<path>` must be under the
  mount, no `..` traversal. Enforced in the handler.
- **Volume precondition** — the workspace's volume must **exist and be mountable**;
  otherwise a legible "workspace has no runtime volume" error (the agent cannot
  provision it — operator's REST job), not a raw Docker mount failure.
- **Name uniqueness** — pre-check pre-sidecar (fail fast); DB constraint is the
  real guard (handles the TOCTOU race).
- **Failure surfacing** — the service orchestrates each step and reads each
  outcome directly (sidecar exit code, then sniff/copy/insert results), composing
  a coherent tool result.

#### 7.5.1 Download write-path semantics (`download_artifact` → volume)
The download sidecar runs `curl -o /vol/<path>` after `..`-traversal validation.
Behavior when `/vol/<path>` already exists:

| Existing path | Behavior |
|---|---|
| nothing | Create the file. |
| regular file | **Overwrite.** |
| **symlink** | **Link is replaced by a regular file**, *not* followed — `curl -o` `unlink()`s the link and creates a fresh file, so a planted symlink **cannot** be used to write outside the volume. Safe to allow. |
| **directory** | **Failure** — `curl -o` cannot target a directory. |

On a mid-transfer failure the error is returned to the agent; the sidecar does
**no** cleanup (a partial file may remain in the volume — acceptable, the volume is
disposable scratch, §0).

#### 7.5.2 Single-artifact concurrency: last-writer-wins
Concurrent `update_artifact` (or update/register racing on the same name) is
**last-writer-wins**. Each writer stages and `CopyObject`s to its **own new key**
(§6.3), then updates the row; the final row update decides the winner. The loser's
new object is immediately unreferenced and is reaped as an aged final-object with
no row (§8.2.1 item 2). No optimistic locking / version column.

---

## 8. Maintenance

Maintenance is defined here as **what needs to be done**, not **how it is
scheduled**. The service does **not** implement a scheduler/loop: a separate
**workflow & task-execution engine** (another in-flight project) drives these jobs.
This document only enumerates the maintainable aspects and their remedies.

Two independent categories:
- **§8.2 Data-consistency management** — reconcile the DB against the two external
  systems it tracks: the **object store** (artifact objects — the dual-write
  problem) and **Docker** (workspace volumes). Discipline: **ordering + async
  reconciliation**, **not** distributed transactions.
- **§8.3 Lifecycle management** — execute the *forward* purge path
  (`PENDING_PURGE → PURGING → PURGED`, object deletion, row removal). Defined in a
  later pass.

### 8.1 Single-bucket layout
Staging and final storage share **one bucket** (multiple buckets would complicate
configuration, not planned). The two populations are distinguished by **key
prefix**: staging keys under a staging prefix, final keys under
`<config prefix>/<ws-id>/…`. The bucket is a single service-level value; there is
**no per-workspace bucket**. Reconciliation does **not** rely on a bucket
lifecycle-expiry rule as a backstop — cleanup is the service's responsibility.

### 8.2 Data-consistency management

#### 8.2.1 Object-store reconciliation (DB ⇄ object store)
Each job compares object-store contents against DB rows. All object deletions are
gated by a **configurable grace period** — an object younger than the grace window
may simply be an in-flight operation a moment from completing, so it is skipped.

1. **Aged staging objects** — a staging object still present under the staging
   prefix means an upload/update aborted before its best-effort cleanup (§6.1
   step 6). Remedy: **delete** it once older than the grace period.

2. **Aged final objects with no backing row** — a final-prefix object whose key is
   referenced by no artifact row. Two causes, **one remedy**:
   - *Failed register/update* — `CopyObject` succeeded but the insert/row-update
     did not (§6.1 ordering: copy → insert; §6.3 update).
   - *Update orphan* — the previous object after §6.3 flipped `ObjectKey` to a new
     key.
   Remedy: **delete** the object once older than the grace period.

3. **Artifact row with a missing object** — a `RECORDED` row whose `ObjectKey`
   resolves to no object. Detected for free by the same join. This is a
   **data-loss signal**, not routine garbage, so it is **not** auto-purged:
   - Remedy: transition the row `RECORDED → MISSING_OBJECT` (§2.2.1) — a quarantine
     that **preserves the metadata as evidence** and surfaces the incident. An
     operator may then move it to `PENDING_PURGE`.
   - Rows already in `PENDING_PURGE`/`PURGING` with a missing object are **not**
     flagged — a missing object there is expected (the purge partially ran); that
     belongs to lifecycle management (§8.3).

#### 8.2.2 Volume-state reconciliation (DB ⇄ Docker)
`VolumeState` is a DB column but the volume is a Docker object; with synchronous
create/delete (§4.2) the service's **own** transitions can't drift, but something
**outside** the service can (a human `docker volume rm`, host pruning, an orphan
volume from a prior incarnation). This job compares each workspace's `VolumeState`
against `VolumeInspect(VolumeName)` existence (the stored, ID-derived name) and
corrects the column.
Both drift cases **self-heal silently** — a volume is disposable scratch (§0), not
irreplaceable data, so neither is flagged as an incident (unlike a missing artifact
object, §8.2.1 item 3):

| DB `VolumeState` | Volume exists? | Action |
|---|---|---|
| `NONE` | no | consistent — no-op |
| `READY` | yes | consistent — no-op |
| `READY` | **no** | **correct → `NONE`** (volume vanished; it was transient). |
| `NONE` | **yes** | **adopt → `READY`.** Deleting would be dangerous (may be mounted — the auto-reap we forbade, §4.2); leaving `NONE` permanently blocks `create volume` (the derived name would always collide). Adopting is correct. |

**Adoption is sound because the volume name is derived from the immutable workspace
`ID`** (`<app>-<workspace ID>`, §2.1) and persisted in `VolumeName`. The ID is
unique and never changes, so `VolumeName` unambiguously identifies *this*
workspace's volume — reconciliation just `VolumeInspect`s the stored `VolumeName`.
(This is strictly safer than a name-derived scheme, which would shift under a
rename.)

### 8.3 Lifecycle management
The forward purge path. Three operations, all idempotent and re-runnable (the
external engine may retry any of them).

**Invariants relied on:**
- **Object delete is idempotent** — deleting a backing object treats
  **404 / no-such-key as success**. This makes every purge step safe to re-run
  and lets a `MISSING_OBJECT → PENDING_PURGE` artifact (§2.2.1 — object already
  gone) purge cleanly instead of wedging. This idempotency is **load-bearing** for
  the crash-recovery reset below.
- **A workspace can only reach `PENDING_PURGE` with `VolumeState = NONE`** (§2.1.1
  guard) — so workspace purge never has to deal with a live volume; the volume is
  gone by the time purge runs.
- Purging touches **the object store + the DB row only**. It never touches a
  volume (artifacts live in the object store; the volume is disposable cache, §0,
  and is already gone before a workspace purges).

#### 8.3.1 Crash recovery (maintenance-system init)
Only the maintenance system ever sets `PURGING`. Therefore, on **cold init**, any
artifact still in `PURGING` is by definition **orphaned** by a failed run (a live
run would not be mid-init). Init resets **all `PURGING` artifacts back to
`PENDING_PURGE`** so they are re-picked next round. Safe precisely because object
delete is idempotent (above).

Workspaces need **no** such reset: a `PURGING` workspace is self-healing via the
completion poll (§8.3.3), which re-evaluates it every round regardless of when it
crashed.

#### 8.3.2 Purging an artifact
1. User marks the artifact for purging → `PENDING_PURGE` (§7.1 delete artifact).
2. Maintenance picks up `PENDING_PURGE` artifacts → `PURGING`.
3. **Delete the backing object** (404-tolerant, per the invariant).
4. Mark the artifact `PURGED`.

#### 8.3.3 Purging a workspace (multi-phase, reuses artifact purge)
Assumes the volume is already deleted (`VolumeState = NONE` invariant).

- **Phase 1 — mark & cascade.** User marks the workspace for purging →
  `PENDING_PURGE`. Maintenance picks it up → `PURGING`, then marks **all its
  artifacts** `PENDING_PURGE`. Phase ends. The workspace does **not** delete
  objects itself — it relies on the artifact-purge task (§8.3.2) to drain them.
- **Phase 2 — drain.** The artifact-purge task purges those artifacts on its own
  schedule.
- **Phase 3 — completion poll.** Maintenance periodically re-examines `PURGING`
  workspaces. A workspace is **complete** when **no artifact row remains that is
  not `PURGED`** — i.e. either zero artifact rows remain (rows already removed by
  §8.3.4) or every remaining row is `PURGED`. (Stated as a negative predicate on
  purpose: do **not** implement it as "count of `PURGED` == original count," which
  breaks once rows are cleaned up.) On completion → mark the workspace `PURGED`.

#### 8.3.4 Deleting purged entries from the DB
Maintenance periodically deletes rows — workspace or artifact — in the `PURGED`
state.

**FK-safe ordering:** delete `PURGED` **artifact** rows before (or independently
of) the `PURGED` **workspace** row that parents them, so a workspace row is never
removed while artifacts still reference its `WorkspaceID`. The §8.3.3 completion
gate already ensures a workspace only becomes `PURGED` after its artifacts are
done, so this ordering just protects the row-deletion step itself.

---

## 9. Deferred / out of scope (for now)

- **Maintenance scheduling** — how/when the jobs in §8 run is owned by a separate
  workflow & task-execution engine, **not** this service.
- **Error taxonomy** — REST HTTP status codes and the MCP structured-error shape
  for the various refusals (mounted-volume, `VolumeState ≠ NONE`, name collision,
  no-volume precondition, …) are left to **implementation time**.
- **`goutils` Docker factoring** — extract reusable Docker components shared by
  `multitool`, `rest-pty`, and this service (§1) — separate effort.
- **Cross-tenant name-collision** — deployment concern, not solved in-service
  (§2.3).
- **Multipart / massive uploads** — single-PUT size cap for the first cut (§5.2).
- **How the agent *requests* a transfer at a higher level** — the ergonomics of
  chaining tool → save → load across a pipeline (parked earlier).
- **`resources/read` exposure of artifacts** — per-tool opt-in, now cleaner with
  an object store + DB behind it; not yet revisited.

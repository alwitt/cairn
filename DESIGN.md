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

> **Status:** working design. `cairn` is a **new, standalone microservice** —
> *not* part of `multitool`, which is its first *client*.

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
> The reusable Docker components have been factored into **`goutils/runtime`**:
> `docker.go` provides the sandboxed container runtime, and `docker_volume.go`
> provides a `VolumeManager` (create / delete / list / inspect a volume, plus
> the mounter lookup that backs the in-use check). `cairn` builds its volume
> lifecycle and transfer sidecars on these rather than re-implementing Docker
> integration.

---

## 2. Data model

### 2.1 Workspace

| Field | Type | Notes |
|---|---|---|
| `ID` | string (UUID) | Primary identity; drives object-key construction **and** the volume name. |
| `Name` | string, unique | How the agent refers to it. Charset restricted to `[A-Za-z0-9_-]`. |
| `Description` | string, optional | Free-text. |
| `VolumeName` | string | The associated volume's name — **derived from `ID`** (`<app-name>-<workspace ID>`) and **persisted** at workspace-create. See below. |
| `VolumeState` | string enum | The workspace's **sole state** — whether its associated volume exists. See §2.1.1. |
| `CreatedAt` | timestamp | — |
| `UpdatedAt` | timestamp | — |

**`VolumeName` is ID-derived and persisted (a convenience cache).** It is computed
once as `<app-name>-<workspace ID>` at workspace-create and stored, so **no client
ever guesses or re-derives it** — `multitool`/`rest-pty` fetch the workspace and
read `VolumeName` directly (the service owns and returns the name). `app-name` is a
**per-deployment config value** (charset `[a-zA-Z0-9-_]`, same class as the
service-level object-key prefix below) that namespaces this deployment's volumes
for `VolumeList` filtering.

- **Why `ID`, not `Name`:** the ID is **immutable**, so the volume name is stable
  across a workspace rename. Rename is therefore a **pure DB update** that never
  touches the volume and needs no volume-state guard (§7.1).
- **Existence is `VolumeState`, not null-ness:** `VolumeName` is **always
  populated** (present from create, even while `VolumeState = NONE`). It is a name
  cache, not an existence flag. **Reader contract: only mount `VolumeName` when
  `VolumeState = READY`.** Keeping the two concerns separate (name = identity,
  state = existence) also lets §8.2.2 reconciliation always know which volume to
  `VolumeInspect`.

**No `Artifact Object Key Prefix` column.** The key prefix is **service-level
config** (same for every workspace); the `<ws-id>` segment of the key already
isolates workspaces. Final object key = `<config prefix>/<workspace ID>/<random>`.

#### 2.1.1 Workspace `VolumeState` enum (the sole state)
`NONE` ⇄ `READY`

| State | Meaning |
|---|---|
| `NONE` | No volume exists. |
| `READY` | Volume exists and is mountable. |

A workspace's existence **is** its row: it exists (row present) or it doesn't (row
deleted, §4.1). The only thing left to track is whether the workspace's **volume**
exists, which is exactly `VolumeState`; deletion is a plain, atomic row delete
(§4.1), not a state.

**No transient `CREATING`/`DELETING` states.** Volume create and delete are
**synchronous, blocking REST operations** (§4.2) — the call does not return until
Docker has actually created/removed the volume, so the DB column is only written
**after** Docker succeeds. There is no window where the column claims a state
Docker has not reached, and a failed op leaves the column at its prior value. (If
create/delete latency ever becomes unacceptable, async transient states can be
added — deferred until then.)

See §4 for the operator-controlled lifecycle and the teardown guards, and §8.2.2
for reconciliation when Docker drifts from the column.

### 2.2 Artifact

| Field | Type | Notes |
|---|---|---|
| `ID` | string (ULID) | **Sortable** primary identity. |
| `WorkspaceID` | string (UUID), FK | Parent workspace. Backs `list_artifacts`, the `(WorkspaceID, Name)` uniqueness constraint, and `ON DELETE CASCADE` when the workspace row is deleted (§4.1). Not derived from the object key. |
| `Name` | string, unique per workspace | Display name; how the agent refers to it. **No relationship to the object key.** |
| `Description` | string, optional | Free-text. |
| `ObjectKey` | string | The **complete** object key in the store. |
| `MIMEType` | string | **Server-sniffed** content type (§6), stored on the row so `list`/`fetch` report it without a `HeadObject`. **Advisory metadata only** — a label/download-name hint, **not a security boundary** (serving forces `attachment`, §6/§6.5). Also set as the object's `Content-Type`. |
| `Size` | int64 | Size in bytes (from `GetObjectStat` on the staging object, §6.1). |
| `State` | string enum | `RECORDED` or `MISSING_OBJECT`. See §2.2.1. |
| `CreatedAt` | timestamp | — |
| `UpdatedAt` | timestamp | — |

- `ObjectKey` is `<config prefix>/<ws-id>/<random>` — a **random suffix, not the
  name** — so **rename is a pure DB update** (no object move).
- **Mutable** ≈ `Name`, `Description`. **Immutable** ≈ `ID`, `WorkspaceID`,
  `ObjectKey` (replaced wholesale on update, §6.3, never edited in place).

#### 2.2.1 Artifact `State` enum
```
RECORDED ⇄ MISSING_OBJECT
```

| State | Meaning |
|---|---|
| `RECORDED` | Registered and stored — the normal live state. |
| `MISSING_OBJECT` | Data-consistency flag: the row's backing object is **gone** (§8.2.1). A **quarantine** state — not auto-remediated, preserves the metadata as evidence of the loss. Discoverable via the `list_artifacts` state-filter option (§7.1); an operator can then **delete** the row (§7.1). |

- **Purge is not a state.** Deleting an artifact is a plain **row delete** (§4.1,
  §7.1). The object the row referenced is left in the store and reclaimed later as
  an unassociated object by the object-reaping GC (§8.2.1, §8.3). This is the
  **eventual-consistency** model: the DB is authoritative for what exists; the
  object store is reconciled toward it.
- There is **no `PENDING` state**. The two-call upload (§6.1) defers the DB insert
  until **after** `CopyObject`, so a row only ever appears already-committed as
  `RECORDED` — no dangling pending rows by construction.

### 2.3 No tenancy / no ownership
The service represents **no tenancy model** — no owner/user/org column, no user
table, no user CRUD. Any such model would dictate multitenancy for consuming
systems (e.g. an org→group→user hierarchy would not fit a flat `owner`). Instead:

- Names are **globally unique** (workspaces) / **unique-per-workspace**
  (artifacts). Uniqueness is **unqualified**.
- **Cross-tenant name collision is explicitly NOT solved here.** A shared instance
  assumes a single tenant, or a caller that pre-namespaces names. This is a
  **documented deployment constraint**, not logic in the service.

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
- **Created** (REST) → DB row only, `VolumeState = NONE`.
- **Live** → has artifacts and/or a live volume (`VolumeState = READY`).
- **Deleted** (REST) → the workspace **row is deleted** (guard: only when
  `VolumeState = NONE`, §4.3), and its artifact rows go with it via
  `ON DELETE CASCADE` — one atomic DB transaction, no object-store interaction. The
  objects those artifacts referenced are left in the store and reclaimed later as
  unassociated objects by the object-reaping GC (§8.2.1, §8.3). Likewise, deleting
  an **artifact** is a plain row delete.

### 4.2 Named volume — created & destroyed by the **operator** via REST
Volume lifecycle is **not** in the agent's control. This is deliberate: other
systems (e.g. `rest-pty`) may also mount a workspace's volume, so the service
**cannot know all mounters** and therefore **cannot auto-reap**. Volumes are
created and deleted **explicitly by the operator**.

- Volume **created** on operator request — **synchronous & blocking**: the REST
  call returns only when Docker has created the volume (or failed). On success the
  DB flips `NONE → READY`; on failure the column stays `NONE`. Idempotent.
- Volume **deleted** only on operator request — **synchronous & blocking**: the
  service does **not** pre-check for mounts; it issues `VolumeRemove(force=false)`
  and lets the **Docker daemon** decide atomically (§4.3) — the daemon removes the
  volume or refuses with a "volume is in use" error. The call returns only when
  Docker has removed it (or failed/refused). On success the DB flips `READY →
  NONE`; on failure/refusal the column stays `READY`.

Because both ops write the column **only after** Docker succeeds, `VolumeState`
has no transient states (§2.1.1). Drift can still arise from *outside* the service
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
- **`delete workspace` refuses unless `VolumeState = NONE`** (§2.1.1 guard — the
  volume must already be gone, or deleting the row would orphan the Docker volume:
  its name is ID-derived, so with no row §8.2.2 could never adopt it).
- These are **refusals, not waits** — REST returns an error; the operator retries.
- Operator tears down **bottom-up**: stop workloads → delete volume
  (`VolumeState → NONE`) → delete workspace (row removed).

### 4.4 Canonical mount path (`/mnt/cairn/ws`)
Artifact paths only round-trip if **every** container that mounts a workspace
volume mounts it at the **same** path — the tool container where the agent's tool
writes/reads a file, **and** cairn's own stat/hash and transfer sidecars. A file
the agent's tool wrote at `/mnt/cairn/ws/out.txt` must be visible to the upload
sidecar at that exact path, or no artifact operation works.

- **The mount path is a fixed constant: `/mnt/cairn/ws`.** It is **not** a data-model
  field and **not** per-workspace — it is one service-wide value used by clients and
  sidecars alike. (Kept a named constant, not scattered literals, so it *can* become
  configurable later; fixed for the first cut.)
- **One workspace per containerized tool call.** Because the mount path is a single
  fixed location, at most one workspace volume can be mounted into a given container.
  This is an accepted, explicit constraint — multi-workspace mounting would require a
  per-workspace (or per-mount) path and is deferred.
- **Client contract.** `multitool` (and later `rest-pty`) mount the workspace volume
  at `/mnt/cairn/ws`. Workspace-enabled MCP tools advertise it, e.g.: *"if you elect
  to execute the tool in a shared workspace, provide the workspace name; the workspace
  volume is mounted at `/mnt/cairn/ws`."*
- **Path contract.** Every agent-supplied artifact path is **absolute and under
  `/mnt/cairn/ws`**. cairn validates it (§7.5) and uses it **verbatim** in the
  sidecar — no prefix translation, because the sidecar mounts at the same path.

---

## 5. Transfer sidecars & security posture

Byte movement between volume and object store is done by short-lived,
**service-launched** sidecar containers (never a client, never the tool
container).

### 5.1 Trust tiers
| Container | Network | Credentials | Runs agent-influenced code |
|---|---|---|---|
| **Tool container** (in `multitool`) | none | none | yes |
| **Stat/hash sidecar** (volume-based upload/update only) | **none** | **none** | no — fixed command; resolves + validates a volume source file, emits a `{resolved_path, valid, size, sha256}` stat block on stdout (§6.4, §7.5.3) |
| **Transfer sidecar** | reaches the **object store** (via its presigned URL); does **not** call back into the service | **none** (uses presigned URLs) | no — fixed `curl` over server-provided URLs |
| **Service process** | yes | **yes** (sole credential holder) | no |

The **stat/hash sidecar** exists only on the MCP upload/update path: a presigned
PUT URL is bound to an exact `Content-Length` + SHA-256 (§6.4), and the bytes live
in the volume, reachable only from a sidecar — so their size and hash must be
computed in a sidecar *before* the PUT URL can be minted. It needs **no network at
all** (it neither reaches the object store nor calls back into the service — it
only writes stdout, which the service reads via `ContainerWait`, §7.3). Like the
transfer sidecar it runs a **fixed command over a server-supplied path**, not
agent-supplied code, which is what keeps it in the "no agent-influenced code"
tier.

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

> Note: sidecars do **NOT** call back into the service's REST API (see §7.3) — a
> transfer sidecar's only outbound traffic is to the object store (its presigned
> URL), and the stat/hash sidecar makes no network calls at all. Network-level
> *enforcement* of this (locking a sidecar down to object-store-only egress) is
> **not implemented in the first pass** — it is an OPS/deployment concern, not an
> in-service guarantee.

---

## 6. Upload path: staging + server-side MIME sniff

The upload source is **not trusted** to declare MIME, so the server sniffs it from
the bytes (§6.1). The sniffed `MIMEType` is **descriptive metadata** — a label for
listings, a download-filename hint, and programmatic consumers — **not a security
boundary.**

**Serving discipline (the XSS control).** Rather than rely on the stored
`Content-Type` to be safe, the **serving path never renders an artifact inline**:
every presigned **GET** URL is minted with `response-content-disposition=attachment`
(see §6.5), so a browser opening an artifact URL **downloads** it instead of
parsing/executing it. This neutralizes the stored-XSS vector (a `text/html`
artifact downloads rather than running a script) **independent of** what
`Content-Type` the object carries — which is why the MIME value can be demoted to
advisory metadata. **Inline preview is deliberately dropped** in exchange for this
guarantee.

### 6.1 Two REST calls (no DB write until bytes exist)
1. **Request staging upload URL** (workspace by ID) — caller supplies the object's
   exact **size** and base64 **SHA-256** (goutils `GeneratePresignedPutURL` binds
   both into the signed URL: `Content-Length` + `x-amz-checksum-sha256`, so the
   object store **verifies** the uploaded bytes and rejects a size/hash mismatch).
   Server presigns a PUT to a **server-generated, workspace-scoped staging key**
   and returns `{ url, staging_key }`. **No DB operation.** The **single-PUT size
   cap** (§5.2) can be enforced here, at mint time, from the supplied size — a
   fail-fast in addition to the authoritative re-check at register (step 2.2).
2. **Register artifact from staging** (workspace by ID) — caller sends
   `staging_key` (+ name, description). Server:
   1. **Verify the staging key belongs to this workspace** — the key must carry the
      `<staging-prefix>/<ws-id>` prefix for the target workspace. Because the
      staging key is server-generated and workspace-scoped by construction (§8.1), a
      simple prefix match proves the key was issued for *this* workspace and rejects
      a key aimed at another.
   2. **Enforce the single-PUT size cap** — `GetObjectStat` the staging object
      (goutils `S3Client.GetObjectStat` → `S3ObjectStat.Size`) and reject an
      over-cap object here, before any copy (§5.2). "Too big" is an error at this
      step; no `CopyObject` and no row is created.
   3. **Ranged-GET** the staging object's leading bytes, **sniff MIME**
      (magic-number lib).
   4. Generate final key `<prefix>/<ws-id>/<random>`.
   5. **`CopyObject`** staging → final with the sniffed `Content-Type`.
   6. **Insert** the artifact DB row (uniqueness enforced by the DB constraint on
      `(WorkspaceID, Name)`; a handler pre-check is only a fail-fast optimization).
   7. **Delete** the staging object (best-effort).

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
Update reuses the staging flow (including the **same pre-copy checks** as §6.1
step 2 — staging-key ownership verify, then the single-PUT size cap via
`GetObjectStat`) but does **not** create a new DB row:
- New staging upload → **verify staging-key prefix** → **enforce size cap**
  (`GetObjectStat`) → sniff → **`CopyObject` to a NEW final key** → **update the
  row's `object_key`/MIME/size** to the new key → delete staging.
- The flip from old key to new key is a single row update → **atomic** (readers
  always see a complete object).
- The **old object is orphaned by design** → reaped by the maintenance system
  (§8.2.1 item 2).

### 6.4 Volume-based upload/update: two sidecars in front of the same staging flow
This path serves **any upload whose bytes are volume-resident** — the MCP
`upload_artifact` / `update_artifact` tools **and** the REST "Save artifact" /
"Update artifact" endpoints (§7.1, §7.4). The staging-based REST path (§6.1)
assumes the caller already holds the bytes outside a volume and can compute their
size + SHA-256 locally before requesting the staging URL. A volume-based caller
**cannot** — the bytes live in the workspace **volume**, reachable only from a
sidecar, and the presigned staging PUT URL must be bound to the exact size + hash
*before* it can be minted (§6.1 step 1). So the volume-based path prepends a
**stat/hash sidecar** and otherwise reuses the existing staging + register/update
core path unchanged:

1. **Stat/hash sidecar** — mounts the volume, runs a fixed command over the
   agent-supplied source path `/mnt/cairn/ws/<path>`, and emits a JSON stat block on stdout
   (schema and source-file rules in §7.5.3):
   ```json
   { "resolved_path": "<absolute path, symlink-resolved | null>",
     "valid":         true,
     "size":          <uint64>,
     "sha256":        "<sha256sum output>" }
   ```
   Symlinks are **accepted**, but the sidecar stats and hashes the **resolved
   target file**, not the link (`resolved_path` is the symlink-resolved absolute
   path; `null`/absent when the path does not exist). No MIME sniff here — MIME is
   still sniffed **server-side** at register (§6.1 step 2.3), so it stays
   trustworthy rather than sidecar-asserted. No network, no credentials (§5.1). The
   service reads stdout via `ContainerWait` and **rejects the upload if `valid` is
   false** (missing path, directory, non-regular file) before minting anything.
2. **Size cap (fail fast)** — reject on the reported size before minting anything
   (§5.2).
3. **Mint the staging PUT URL** — bound to that exact size + SHA-256 (§6.1 step 1).
4. **Upload sidecar** — `curl -T /mnt/cairn/ws/<path>` to the staging URL. The object store
   **verifies the checksum** and rejects a mismatch.
5. **Register / update from staging** — the **existing** core path (§6.1 step 2 /
   §6.3): staging-key verify → size cap re-check → **server-side MIME sniff** →
   `CopyObject` staging→final → insert/update row → delete staging.

**TOCTOU fails closed (by design).** The staging PUT is bound to the hash the
stat/hash sidecar computed. If the file **changes on the shared volume** (§0)
between the stat sidecar and the upload sidecar, the uploaded bytes no longer
match the signed checksum and the **object store rejects the PUT**. This is the
intended behavior: a mid-flight content change is an **operational failure**
(content is not expected to change under an in-flight upload), surfaced to the
agent as a legible "file changed during upload" error (§7.5), never silent
corruption. Same-length changes are caught too, because the bind is on the hash,
not just `Content-Length`.

**Cost accepted:** the volume file is read **twice** — once by the stat/hash
sidecar (to hash) and once by the upload sidecar (to transfer). `curl` does not
re-hash; it sends the checksum the service supplies, and the object store does the
one authoritative verification. The double read is the price of the presigned-PUT
integrity contract when the bytes are volume-resident; acceptable for a
size-capped artifact service.

### 6.5 Serving artifacts safely (`response-content-disposition=attachment`)
Every presigned **GET** URL cairn mints (REST fetch-with-`?presign`, MCP
`download_artifact` reads the object into the volume, and any UI download) sets the
S3 **`response-content-disposition=attachment`** response override at **mint
time**. Minting-time is essential: the override is a signed query parameter cairn
controls, so neither the uploader nor the sidecar can undermine it — unlike an
object-stored `Content-Disposition`, which the checksum-bound-but-not-disposition-
bound PUT (§6.1) cannot guarantee.

- **The override is a browser-XSS safeguard only.** It matters solely for
  **browser-facing** GETs (the REST `?presign` URL, a UI download) — a browser
  honors `Content-Disposition: attachment` and downloads instead of rendering. The
  MCP `download_artifact` sidecar runs `curl -o /mnt/cairn/ws/<path>`, which **ignores**
  `Content-Disposition` entirely (it writes to the `-o` target regardless), so the
  override is **inert but harmless** on that path. cairn still mints every GET with
  `attachment` uniformly rather than branching on the consumer — there is no path
  where it hurts, and it removes the risk of accidentally minting a
  non-`attachment` browser URL.
- Requires a small **goutils** addition: `GeneratePresignedGetURL` currently passes
  `nil` request parameters; it must accept `response-content-disposition`
  (and optionally other `response-*` overrides) to thread through to the presigned
  URL.
- **`X-Content-Type-Options: nosniff`** is a *response header the object store
  emits*, **not** a signable query parameter, so cairn's S3 client **cannot**
  guarantee it from the presigned URL alone. It is left as an **operational /
  serving-edge configuration** (object-store config, or a proxy/CDN in front of the
  store) — a documented deployment note, not an in-service guarantee. `attachment`
  already makes content-sniffing moot for the execution path, so `nosniff` is
  defense-in-depth on top, not the primary control.

---

## 7. API surfaces

### 7.1 REST API (operator; ID-addressed)
| Endpoint | Notes |
|---|---|
| Create workspace | DB row only, no volume. |
| List workspaces | — |
| Fetch workspace | Record + `VolumeState` (volume ready?) + the **estimated number of containers currently mounting** the workspace volume (from Docker's `RefCount` via `system/df`, §4.3; `-1` when unavailable). |
| Rename workspace (`Name`) | Pure DB; **no volume guard**. The volume name is derived from the immutable `ID` (§2.1), so rename never affects the volume — safe even with a live, mounted volume. Object key is also ID-based, untouched. |
| Update workspace `Description` | Pure DB; no guards. |
| Delete workspace | Refuse unless `VolumeState = NONE` (§4.3); **deletes the workspace row**, cascading to its artifact rows (`ON DELETE CASCADE`) in one transaction. No object-store interaction — the freed objects are reclaimed later by the GC (§8.2.1). |
| Create volume | Provision named volume (idempotent). |
| Delete volume | **Refuse if mounted** (Docker-side check). |
| **Reap unassociated objects** | Operator-triggered: **immediately launch the object-reaping `tasking` Task** (§8.3), optionally scoped to one workspace's key prefix. The same Task the maintenance loop launches periodically — exposed here so an operator can force prompt reclamation instead of waiting for the next sweep. |
| **Generate staging PUT URL** | Caller supplies exact size + base64 SHA-256 (bound into the URL, §6.1 step 1); size cap may fail fast here. Pure object-store; returns `{url, staging_key}`; no DB. |
| **Register artifact from staging** | §6.1 step 2. **Parent workspace must exist** (§7.5) — refuse otherwise. |
| List artifacts | Metadata only. A **state filter is a listing option**: by default only `RECORDED` is returned, but the caller may request other states (e.g. `MISSING_OBJECT` for triage, §8.2.1 item 3). The option — not a hardcoded filter — decides what is returned. |
| Fetch artifact (opt. `?presign` GET URL) | Serves any artifact by ID; a `?presign` GET URL is only minted for a `RECORDED` artifact (a non-`RECORDED` artifact has no servable object), and is minted with `response-content-disposition=attachment` (§6.5). |
| Delete artifact | **Deletes the artifact row** (from `RECORDED` or `MISSING_OBJECT`); no object-store interaction — the freed object is reclaimed later by the GC (§8.2.1). Idempotent (deleting an absent row is a no-op). |
| Update artifact from staging | §6.3 (replaces the bytes/`ObjectKey`). **Parent workspace must exist** (§7.5) — refuse otherwise. |
| Rename artifact (`Name`) | Pure DB; the object key is random-suffixed so it is untouched (§2.2). Uniqueness enforced by `(WorkspaceID, Name)`. |
| Update artifact `Description` | Pure DB; no guards. |
| **Load artifact → volume** | Object → volume (download). Shared data path with MCP `download_artifact` (§7.4). |
| **Save artifact** (volume → object) | Creates a **new** artifact from a volume file. Fails if the name is already taken (uniqueness on `(WorkspaceID, Name)`). REST peer of MCP `upload_artifact` (§7.4). Parent workspace must exist (§7.5). |
| **Update artifact** (volume → object) | Replaces an **existing** artifact's bytes from a volume file (§6.3). Fails if the artifact does not exist. REST peer of MCP `update_artifact` (§7.4). Parent workspace must exist (§7.5). |

### 7.2 MCP API (agent; name-addressed)
Resolve names→IDs at the handler, then everything downstream is ID/key-addressed.
Every volume-touching tool (`upload_artifact`, `update_artifact`,
`download_artifact`) takes a file path that is **absolute and under the canonical
mount `/mnt/cairn/ws`** (§4.4); the volume is mounted there in both the tool
container and cairn's sidecars, so the path the agent names is the path the sidecar
uses.

| Tool | Behavior |
|---|---|
| `list_artifacts` | List artifacts in a workspace. Same state-filter listing option as REST (§7.1): defaults to `RECORDED`, other states requestable. |
| `download_artifact` | Object → volume. Presign **GET** (`attachment`, §6.5); sidecar mounts volume at `/mnt/cairn/ws` (§4.4), `curl -o /mnt/cairn/ws/<path>`. Read direction, single sidecar, no core-register step. **The destination directory must already exist** — the agent prepares it first (§7.5.1). |
| `upload_artifact` | Volume → object, **new** artifact. **Two sidecars** (stat/hash then upload) in front of the staging + register core path, §6.4 / §7.3. Parent workspace must exist (§7.5). |
| `update_artifact` | Volume → object, replaces an **existing** artifact's bytes. Same two-sidecar flow as upload (§6.4) but calls the **update** core function. Parent workspace must exist (§7.5). |
| `delete_artifact` | **Deletes the artifact row** (from `RECORDED` or `MISSING_OBJECT`); the object is reclaimed later by the GC (§8.2.1). Idempotent. |
| `rename_artifact` | Updates an artifact's name (pure DB update). |
| `list_workspaces` | Read-only. Lists workspaces the agent can use. |
| `get_workspace` | Read-only. Confirms a workspace exists and reports its `VolumeState` (whether the volume is ready to mount) plus the **estimated number of containers currently mounting** the workspace volume (Docker `RefCount`, §4.3). |

### 7.3 Synchronous MCP model — call the core function directly (no self-REST hop)
An MCP tool call is **synchronous and server-orchestrated**: the service launches
the sidecar(s), waits for each (`ContainerWait`), and knows exactly when the work
finished (upload = two sidecars in sequence, §6.4; download = one). The REST
staging→upload→**notify** callback exists only because REST clients are
asynchronous/external; that asymmetry does not apply to MCP. So the MCP tool does
**not** curl back into its own REST endpoint — the sidecars only touch the volume
and the object store, never the service (§5.1).

`upload_artifact` flow:
1. Resolve workspace name → ID; **pre-check that the name is *free*** in
   `(workspace, name)` (upload creates a new artifact). This is a pre-sidecar
   fail-fast; the DB uniqueness constraint remains the real guard (§7.5).
2. **Stat/hash sidecar**: mount volume, emit the `{resolved_path, valid, size,
   sha256}` stat block for `/mnt/cairn/ws/<path>` on stdout, exit. Service waits
   (`ContainerWait`), reads the JSON, **rejects if `valid` is false** (§7.5.3),
   then applies the size cap (fail fast) (§6.4).
3. **Presign** staging PUT URL (in-process), bound to that size + SHA-256.
4. **Upload sidecar**: mount volume, `curl -T /mnt/cairn/ws/<path> <staging-url>`, exit.
   Service waits (`ContainerWait`). The object store verifies the checksum; a
   mismatch (changed file) fails the upload (§6.4 — TOCTOU fails closed).
5. On success → **call `RegisterArtifactFromStaging(...)` directly** (in-process):
   verify staging-key prefix → size cap (`GetObjectStat`) → sniff → `CopyObject` →
   insert (constraint-guarded) → delete staging.
6. Return the tool result from the core function's outcome.

`update_artifact` runs the same two-sidecar flow but with the **inverse** name
pre-check and a different core function:
- **Step 1 pre-check is existence, not availability** — update replaces an
  existing artifact, so it pre-checks (pre-sidecar, fail-fast) that an artifact by
  that name **exists** in the workspace *and* the parent workspace exists
  (§7.5), rather than checking the name is free. Spending two sidecars only to have
  the core function reject an unknown/inactive target is wasteful; this fails
  before either sidecar runs.
- **Step 5 calls `UpdateArtifactFromStaging(...)`** — same staging-key verify and
  size cap, then updates the row instead of inserting (§6.3).

Either path's stat/hash sidecar (step 2) is also a **backup validity gate**: its
`valid` bool (§7.5.3) catches a source path that became missing/invalid between the
pre-check and the sidecar run.

**Reuse pattern — one core function, two front-doors:**
```
RegisterArtifactFromStaging(ctx, workspace, name, staging_key, …) → (artifact, error)
   ├── REST handler → thin HTTP shim (unmarshal → core → marshal)
   └── MCP upload   → calls core directly after the upload container exits
```
The register/update core is shared at the **function-call layer** — no
auth/network/async cost, and sidecars never need to reach the service (their only
outbound traffic is a transfer sidecar's object-store upload/download).

### 7.4 Shared core, path-specific front-ends (REST ⇄ MCP)
REST and MCP share the **register/update core** (§7.3) and the **presigned-URL +
sidecar transfer mechanics**, so the security-critical logic is written once and
cannot drift. They are **not** a single uniform "one transfer engine," though —
the front-ends differ by more than addressing:

- **Addressing / caller** — REST: IDs, operator; MCP: names→IDs, agent.
- **Two upload families, by where the bytes live** —
  - *Staging-based (REST only)*: "Generate staging PUT URL" + "Register/Update
    artifact from staging" (§6.1). The caller already holds the bytes **outside a
    volume** and computes size + SHA-256 locally, so **no stat/hash sidecar**.
  - *Volume-based (MCP `upload_artifact`/`update_artifact` **and** REST "Save
    artifact"/"Update artifact")*: the bytes are **volume-resident**, so size +
    SHA-256 must be derived by the **stat/hash sidecar** before the PUT URL is
    minted (§6.4). Both surfaces run the same two-sidecar flow; they differ only in
    addressing/caller. The stat/hash sidecar is a property of the **volume-based**
    path, not of MCP specifically.
- **Download symmetry holds** — MCP `download_artifact` and the REST "Load artifact
  → volume" endpoint are both a single presigned-**GET** sidecar (`attachment`,
  §6.5); no stat step is needed for reads (a GET binds no size/hash up front).

### 7.5 Handler-level validation & preconditions (all volume-touching ops)
- **Parent workspace must exist** — every artifact create/update
  (`RegisterArtifactFromStaging`, `UpdateArtifactFromStaging`, and the
  `upload_artifact` / `update_artifact` MCP tools that call them) refuses unless the
  parent workspace row is present. A workspace delete is an atomic row delete (§4.1),
  so there is no half-deleted window to race: either the row is there (writes
  allowed) or it's gone (writes fail with not-found, and the `(WorkspaceID, …)` FK
  would reject the insert anyway). Read paths (`download_artifact`, list/fetch) are
  not gated on this.
- **Single-PUT size cap** — enforced by both core functions via a `GetObjectStat`
  on the staging object: `RegisterArtifactFromStaging` (§6.1 step 2.2) and
  `UpdateArtifactFromStaging` (§6.3), and therefore by both the `upload_artifact`
  and `update_artifact` MCP tools, which route through them. An over-cap staging
  object is rejected before `CopyObject`; no row is created/updated (§5.2).
- **Volume path validation** — the agent-supplied path must be **absolute and under
  the canonical mount `/mnt/cairn/ws`** (§4.4), with no `..` traversal that escapes
  it. For uploads (where the stat/hash sidecar **resolves symlinks**, §7.5.3), the
  check is applied to the **resolved target**: a symlink whose target lands outside
  `/mnt/cairn/ws` is **rejected**, so it cannot exfiltrate the sidecar image's own
  files. Enforced in the handler.
- **Volume precondition** — the workspace's volume must **exist and be mountable**;
  otherwise a legible "workspace has no runtime volume" error (the agent cannot
  provision it — operator's REST job), not a raw Docker mount failure.
- **Name pre-check (direction depends on the verb)** — create/upload pre-checks
  the name is **free**; update pre-checks the named artifact **exists** (§7.3).
  Both are pre-sidecar fail-fasts; the DB uniqueness constraint (create) and the
  row lookup in the update core (update) are the real guards and handle the TOCTOU
  race.
- **Source-file validity (volume-based uploads)** — the stat/hash sidecar reports
  a `valid` bool over the agent-supplied source path (§7.5.3); the service rejects
  an invalid source (missing / directory / non-regular file) before minting a PUT
  URL. Symlinks are accepted and resolved to their target file **only if the
  resolved target stays under `/mnt/cairn/ws`** (§4.4).
- **Failure surfacing** — the service orchestrates each step and reads each
  outcome directly (sidecar exit code, then sniff/copy/insert results), composing
  a coherent tool result.

#### 7.5.1 Download write-path semantics (`download_artifact` → volume)
The download sidecar runs `curl -o /mnt/cairn/ws/<path>` after path validation
(§7.5). It does **not** create intermediate directories — **the agent must prepare
the destination directory before calling `download_artifact`.** cairn cannot safely
`mkdir -p` the parents: it does not control the UID/GID the tool containers run as
(§4.2 — multiple external mounters), so any directory the sidecar created would be
owned by the sidecar's UID and unwritable/undeletable by the real tool containers —
a downstream permission landmine. Directory layout is the agent's job, done from its
own correctly-UID'd tool container.

Behavior for the target `/mnt/cairn/ws/<path>`:

| Target path | Behavior |
|---|---|
| **parent dir missing** | **Failure** — `curl -o` does not `mkdir -p`. Legible "destination directory does not exist" error; the agent must create it first. |
| parent exists, nothing at path | Create the file. |
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

#### 7.5.3 Upload read-path semantics (source file)
The read-side counterpart to §7.5.1. Applies to **every volume-based upload/update**
— the MCP `upload_artifact`/`update_artifact` tools and the REST `Save artifact` /
`Update artifact` endpoints (§6.4). The **stat/hash sidecar** classifies the
agent-supplied source path `/mnt/cairn/ws/<path>` (after `..`-traversal validation, §7.5)
and emits a stat block; the service acts on it before minting any PUT URL.

| Source path `/mnt/cairn/ws/<path>` | Behavior |
|---|---|
| **does not exist** | **Reject** — `valid = false`, `resolved_path = null`. Legible "source file not found" error; nothing minted. |
| **directory** | **Reject** — `valid = false`. A directory is not a single uploadable object. |
| **regular file** | **Accept** — stat + SHA-256 the file. |
| **symlink** | **Accept only if the resolved target stays under `/mnt/cairn/ws`** (§4.4). The link is **resolved**; the sidecar stats and hashes the **target file**, not the link, and `resolved_path` is the resolved absolute path. A symlink whose target escapes the mount is **rejected** (`valid = false`) — it cannot exfiltrate the sidecar image's files. A symlink to a missing/non-regular target reduces to the reject rows above. |

Emitted stat block (stdout JSON, read via `ContainerWait`):

| Field | Type | Meaning |
|---|---|---|
| `resolved_path` | string \| null | Absolute, symlink-resolved path of the source file. `null`/absent when the path does not exist. |
| `valid` | bool | Whether the source is a valid single-file upload (a regular file, directly or via a symlink whose resolved target stays under `/mnt/cairn/ws`). The service **rejects** the upload when `false`. |
| `size` | uint64 | Byte size of the resolved file; binds the presigned PUT `Content-Length` (§6.4). |
| `sha256` | string | `sha256sum` of the resolved file; binds the presigned PUT checksum (§6.4). |

`valid` is the sidecar's advisory gate (a fast reject for the common cases); it is
**not** the integrity boundary — the checksum-bound PUT (§6.4) is. The size/hash
feed the PUT URL, so a source that changes after this stat still fails closed at
upload time (§6.4 TOCTOU).

---

## 8. Maintenance

Maintenance is defined here as **what needs to be done**, not **how it is
scheduled**. cairn runs its **own lightweight maintenance loop** that reconciles the
DB against the two external systems it tracks, and offloads the one heavy, retriable
job — object deletion — to the separate **`tasking`** task-execution engine as a
fire-and-forget Task. Deletion is **decoupled** from
artifact/workspace deletion entirely: purge is a plain row delete (§4.1), and the
object store is continuously reconciled toward the DB. This document enumerates the
maintainable aspects and their remedies; the loop's cadence is config.

Two categories:
- **§8.2 Data-consistency management** — reconcile the DB against the **object
  store** (artifact objects) and **Docker** (workspace volumes). Discipline:
  **ordering + async reconciliation**, **not** distributed transactions.
- **§8.3 Object reaping** — the `tasking` Task that reclaims unassociated objects,
  plus the maintenance loop that launches it and reaps its terminal Tasks.

### 8.1 Single-bucket layout
Staging and final storage share **one bucket** (multiple buckets would complicate
configuration, not planned). The two populations are distinguished by **key
prefix**: staging keys under `<staging-prefix>/<ws-id>/…` (workspace-scoped by
construction, so §6.1 step 2.1 can verify a supplied `staging_key` belongs to the
target workspace with a plain prefix match), final keys under
`<config prefix>/<ws-id>/…`. The bucket is a single service-level value; there is
**no per-workspace bucket**. Reconciliation does **not** rely on a bucket
lifecycle-expiry rule as a backstop — cleanup is the service's responsibility.

### 8.2 Data-consistency management

#### 8.2.1 Object-store reconciliation (DB ⇄ object store)
This is the **sole object-deleter** in the system — deletion is never tied to
artifact/workspace deletion (those are plain row deletes, §4.1); instead the store
is continuously reconciled toward the DB. Each job compares object-store contents
against DB rows. All object deletions are gated by a **configurable grace period**
that is **load-bearing**: because a purge-by-row-delete removes the row
first, *every* freed object transits through "unassociated," and *every* in-flight
upload is transiently unassociated too (the §6.1 copy→insert window; staging objects
never map to a row at all). The grace window is what separates "in-flight, leave it"
from "orphan, reclaim it," so it **must exceed the slowest upload's copy→insert gap**.

The two deletion directions (items 1–2) are **detection only** — the actual object
deletes are executed by the object-reaping `tasking` Task (§8.3), which **re-validates
at delete time** (an object flagged now may gain a backing row before the delete
runs). Item 3 is a cairn-side DB update.

1. **Aged staging objects** — any object under the staging prefix (a staging key
   never maps to an artifact row by construction) aged past the grace window: an
   upload/update that aborted before its best-effort cleanup (§6.1 step 6).
   Reclaimed by the reap Task.

2. **Aged final objects with no backing row** — a final-prefix object referenced by
   no artifact row, aged past grace. Causes: a **purged** artifact/workspace whose
   row was deleted (§4.1) — the normal case now; a *failed register/update*
   (`CopyObject` succeeded, insert/row-update did not); or an *update orphan* (§6.3
   flipped `ObjectKey` to a new key). One remedy — reclaimed by the reap Task.

3. **Artifact row with a missing object** — a `RECORDED` row whose `ObjectKey`
   resolves to no object. Detected for free by the same join. A **data-loss
   signal**, not routine garbage, so it is **not** auto-remediated: transition the
   row `RECORDED → MISSING_OBJECT` (§2.2.1) — a quarantine that **preserves the
   metadata as evidence** and surfaces the incident; an operator may then **delete**
   the row (§7.1). (Guard the transition with the same grace window, so the reap
   Task's flag→delete gap can't momentarily present a still-in-use object as
   missing.)

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

### 8.3 Object reaping (async, via `tasking`)
Purge deletes rows, never objects (§4.1). Object reclamation is a **single reusable
`tasking` Task** — `reap-objects` — that enacts the deletions §8.2.1 detects. It is
the only place an object is ever deleted, and it is deliberately **level-triggered**:
each run re-derives the orphan set from current state, so a lost, failed, or crashed
reap simply gets redone next run. That property is what lets these Tasks be **fire-and-forget and untracked**: no
tracking tables, no attempt cap, no revive, no per-object bookkeeping.

Only the **Task engine** is used — no Workflows, so cairn embeds `tasking`'s task
client + scheduler + a receiver for the `reap-objects` queue, and needs neither the
workflow engine nor its `notify` feedback wiring.

#### 8.3.1 The `reap-objects` Task
Self-contained, idempotent, parallel-safe. Optional parameter: a workspace ID to
scope the sweep to one `<ws-id>` prefix (else the whole bucket).
1. **List** objects — key-paginated `ListObjects` (pass the previous page's last key
   as `startingKey`), across both the staging and final prefixes (§8.1).
2. **Join + filter** — keep only keys that are **unassociated** (no artifact row
   references this `ObjectKey`; staging keys are unassociated by construction) **and
   aged past the grace window** (§8.2.1). This join is the **re-validation**: a key
   is deleted only if it is *still* orphaned at execution time, closing the
   flag-then-delete race against an in-flight upload.
3. **Delete** — bulk `DeleteObjects(bucket, keys)` per page (goutils S3, wrapping
   minio `RemoveObjects`); **404 / not-found = success**. Per-key hard failures are
   left for the next run to re-derive and retry.

Two launch triggers, same Task:
- **Maintenance loop** — periodically, as the routine reclamation sweep.
- **Operator, on demand** — the "Reap unassociated objects" REST endpoint (§7.1),
  for prompt reclamation without waiting for the next sweep.

Because deletion lags purge by up to a sweep interval, this is **eventually
consistent** reclamation. If a deployment ever needs bounded-latency deletion (e.g.
sensitive data), the fix is a *best-effort* immediate `reap-objects` launch on
purge — a latency optimization, not a correctness dependency, so none of the coupled
machinery returns.

#### 8.3.2 Maintenance loop (the reconciliation backstop)
cairn runs its **own** periodic loop (owned by the orchestration system; a restart
resumes cleanly because every decision is derived from durable DB/store state, never
in-memory progress). It:
- runs the **§8.2 reconciliations** — object detection (launching `reap-objects`),
  `MISSING_OBJECT` flagging, and volume-state drift correction;
- **reaps terminal `tasking` Tasks** — periodically `ListTasks` for cairn's
  `reap-objects` Tasks in a terminal state (`COMPLETE` / `CANCELLED`) and
  `DeleteTask`s them, so its fire-and-forget launches don't accumulate in `tasking`'s
  DB. (These Tasks are never workflow-linked, so `DeleteTask` is never refused.)

**Failure policy.** A `SQLError` from cairn's own DB (see `tasking`'s
`models.SQLError` for the shape) is **not** swallowed: log it and **halt the loop**.
A halted loop is a loud signal — the orchestration system restarts the process, which
draws attention — and is far safer than a loop silently spinning against a broken or
inconsistent database. Transient object-store or `tasking`-reachability failures are
not fatal: the level-triggered sweep simply retries them next run.

---

## 9. Deferred / out of scope (for now)

- **Maintenance scheduling** — object reaping runs on the **`tasking`** task engine
  (§8.3); cairn's own maintenance-loop cadence (reconciliation interval, grace
  window) is deployment config, set at implementation time.
- **Error taxonomy** — REST HTTP status codes and the MCP structured-error shape
  for the various refusals (mounted-volume, `VolumeState ≠ NONE`, name collision,
  no-volume precondition, …) are left to **implementation time**.
- **Cross-tenant name-collision** — deployment concern, not solved in-service
  (§2.3).
- **Multipart / massive uploads** — single-PUT size cap for the first cut (§5.2).
- **How the agent *requests* a transfer at a higher level** — the ergonomics of
  chaining tool → save → load across a pipeline.
- **`resources/read` exposure of artifacts** — per-tool opt-in, now cleaner with
  an object store + DB behind it; not yet revisited.

**Known prerequisites (not deferred — required before/with implementation):**
- **goutils `GeneratePresignedGetURL` must accept response overrides** — today it
  passes `nil` request parameters; it needs to thread
  `response-content-disposition=attachment` (and optionally other `response-*`
  overrides) into the presigned GET so cairn's serving discipline (§6.5) works.
- **`X-Content-Type-Options: nosniff` is an OPS/serving-edge configuration** — it
  cannot be set from the presigned GET URL (§6.5); the deployment (object-store
  config or a fronting proxy/CDN) must add it. Documented, not enforced in-service.

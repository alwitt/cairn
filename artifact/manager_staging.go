package artifact

import (
	"context"
	"fmt"
	"strings"

	"github.com/alwitt/cairn/db"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/apex/log"
)

// ======================================================================================
// Artifact Staging & Registration
//
// The upload path is two calls with no DB write until the bytes exist (see DESIGN §6.1):
// a staging PUT URL is minted first, and only once the caller has uploaded against it does
// registration promote the staged object into storage and record the entry.

/*
GetArtifactStagingPutURL generate a artifact staging PUT URL for a particular workspace.

This function defines a new staging object key within the namespace of this workspace and
produces a upload PUT URL for it with a pre-configured TTL.

Once a user uploads the artifact using the PUT URL, they can then register the artifact in
the requested workspace by pointing the system at the staging object key returned by this
function.

Purely an object-store operation - no DB entry is created here. Deferring the entry to
registration means an abandoned upload leaves only a staging object, which the maintenance
sweep reclaims, rather than a dangling row (see DESIGN §6.1).

The URL binds the upload to the supplied size and checksum, so the HTTP client using it MUST
send these headers with exactly these values or the object store rejects the request:

  - `Content-Length` - `artifactSize`
  - `x-amz-checksum-sha256` - `b64ArtifactSHA256`
  - `Content-Type` - `contentType`, only when one was supplied here

Binding the checksum is what makes the object store verify the uploaded bytes, so a size or
hash mismatch fails the upload rather than storing corrupt content.

	@param ctx context.Context - execution context
	@param workspace models.Workspace - workspace this is for
	@param artifactSize int64 - artifact file size
	@param b64ArtifactSHA256 string - artifact file SHA256SUM in base64 encoding
	@param contentType *string - optionally, specify the Content-Type / MIMEType of the artifact
	@returns parameters for staging the artifact
*/
func (m *managerImpl) GetArtifactStagingPutURL(
	ctx context.Context,
	workspace models.Workspace,
	artifactSize int64,
	b64ArtifactSHA256 string,
	contentType *string,
) (StagingUploadBundle, error) {
	// Fail fast on the caller-declared size, before anything is minted. The authoritative
	// check happens at registration against the bytes that actually landed.
	if err := m.enforceSizeCap(
		artifactSize, fmt.Sprintf("artifact staged for workspace %s", workspace.ID),
	); err != nil {
		return StagingUploadBundle{}, models.NewArtifactMangerError(
			fmt.Sprintf(
				"failed to generate staging upload URL for workspace %s", workspace.ID,
			), err, true,
		)
	}

	stagingObjKey := m.newStagingObjectKey(workspace.ID)

	s3Client, err := m.s3Client(ctx)
	if err != nil {
		return StagingUploadBundle{}, models.NewArtifactMangerError(
			fmt.Sprintf(
				"failed to generate staging upload URL for workspace %s", workspace.ID,
			), err, true,
		)
	}

	// The TTL bounds, the non-negative size, and the checksum's base64 encoding are all
	// validated by the object store client, so they are not re-checked here.
	putURL, err := s3Client.GeneratePresignedPutURL(
		ctx,
		m.storeConfig.Bucket,
		stagingObjKey,
		artifactSize,
		b64ArtifactSHA256,
		m.storeConfig.UploadPutURLTTL(),
		contentType,
	)
	if err != nil {
		return StagingUploadBundle{}, models.NewArtifactMangerError(
			fmt.Sprintf(
				"failed to generate staging upload URL for workspace %s", workspace.ID,
			), err, true,
		)
	}

	log.
		WithFields(m.GetLogTagsForContext(ctx)).
		WithField("workspace", workspace.ID).
		WithField("staging_object_key", stagingObjKey).
		Debug("Generated artifact staging upload URL")

	return StagingUploadBundle{
		StagingObjectKey: stagingObjKey, PutURL: putURL.String(),
	}, nil
}

/*
RegisterNewArtifact register a new artifact within a workspace given a artifact staging
object key.

In preparation, the function verified the staging object key is a valid for objects staged
for this particular workspace; this is based on object key prefix matching.

Once verified, it then extract basic information regarding object from the staged object
  - object size
  - object MIME type - computed using a portion of the object data with a magic number library

With that information, a final storage object key for this workspace is defined, and the
staging object is copied into storage object key with the computed MIME type.

Finally, a new artifact entry is defined in that workspace.

The order is load-bearing: copy, then insert, then a best-effort staging delete. Inserting
before the copy would leave a row pointing at nothing, so it is never done. The accepted
consequence runs the other way - a failure between the copy and the insert, including a
rollback of an enclosing `activeSession`, leaves the final object unreferenced. That is by
design; the object-reaping GC reclaims it as an aged final object with no backing row (see
DESIGN §6.1, §8.2.1).

The parent workspace is taken as already resolved by the caller and is not re-read here; the
foreign key constraint remains the real guard (see DESIGN §7.5).

	@param ctx context.Context - execution context
	@param workspace models.Workspace - workspace this is for
	@param stagingObjKey string - the staging object returned by `GetArtifactStagingPutURL`.
	    The system expects the artifact entry's data file is stored at this location already.
	@param name string - name a user will reference artifact by. This is typically
	    structured as a file system path.
	@param description *string - an optional description for the artifact
	@param activeSession db.Database - if set, this is an existing open DB persistence
	    layer transaction, and function will perform additional persistence operations
	    within it.
	@returns the new artifact entry
*/
func (m *managerImpl) RegisterNewArtifact(
	ctx context.Context,
	workspace models.Workspace,
	stagingObjKey string,
	name string,
	description *string,
	activeSession db.Database,
) (models.Artifact, error) {
	logTags := m.GetLogTagsForContext(ctx)

	failure := func(err error) (models.Artifact, error) {
		return models.Artifact{}, models.NewArtifactMangerError(
			fmt.Sprintf(
				"failed to register artifact '%s' in workspace %s", name, workspace.ID,
			), err, true,
		)
	}

	// Prove the caller is not pointing at another workspace's staging object.
	if err := m.verifyStagingKeyOwnership(workspace.ID, stagingObjKey); err != nil {
		return failure(err)
	}

	s3Client, err := m.s3Client(ctx)
	if err != nil {
		return failure(err)
	}

	// Measure what actually landed. The mint-time check trusted a caller-declared size; this
	// one is authoritative, and runs before any copy so an over-cap object costs nothing.
	stagingStat, err := s3Client.GetObjectStat(ctx, m.storeConfig.Bucket, stagingObjKey)
	if err != nil {
		return failure(err)
	}
	if err := m.enforceSizeCap(
		stagingStat.Size, fmt.Sprintf("staged object '%s'", stagingObjKey),
	); err != nil {
		return failure(err)
	}

	mimeType, err := m.sniffObjectMIMEType(ctx, stagingObjKey)
	if err != nil {
		return failure(err)
	}

	storeObjKey := m.newStoreObjectKey(workspace.ID)

	s3Client, err = m.s3Client(ctx)
	if err != nil {
		return failure(err)
	}

	// Staging and storage share one bucket, distinguished by key prefix (see DESIGN §8.1), so
	// this is a server-side copy within the bucket that rewrites the MIME type to the sniffed
	// value.
	if err := s3Client.CopyObject(
		ctx, m.storeConfig.Bucket, stagingObjKey, m.storeConfig.Bucket, storeObjKey, &mimeType,
	); err != nil {
		return failure(err)
	}

	var newEntry models.Artifact
	if err := db.ActiveSessionWrapper(
		ctx, activeSession, m.persistence, func(dbCtx context.Context, dbClient db.Database) error {
			var err error
			newEntry, err = dbClient.DefineNewArtifact(dbCtx, db.NewArtifactParameter{
				WorkspaceID: workspace.ID,
				Name:        name,
				Description: description,
				ObjectKey:   storeObjKey,
				MIMEType:    mimeType,
				Size:        stagingStat.Size,
			})
			if err != nil {
				return goutils.NewPersistenceError(
					fmt.Sprintf(
						"failed to define new artifact '%s' in workspace %s", name, workspace.ID,
					), err, true,
				)
			}
			return nil
		},
	); err != nil {
		return failure(err)
	}

	// Best-effort: the entry is already committed, so failing the call over leftover staging
	// debris would be strictly worse than leaving it. The maintenance sweep reclaims aged
	// staging objects (see DESIGN §6.1 step 7, §8.2.1). Failing to even acquire a client is
	// the same class of failure, so it takes the same path.
	if s3Client, err := m.s3Client(ctx); err != nil {
		log.
			WithError(err).
			WithFields(logTags).
			WithField("staging_object_key", stagingObjKey).
			Warn("Failed to acquire object store client to delete staging object; left for reclamation")
	} else if err := s3Client.DeleteObject(ctx, m.storeConfig.Bucket, stagingObjKey); err != nil {
		log.
			WithError(err).
			WithFields(logTags).
			WithField("staging_object_key", stagingObjKey).
			Warn("Failed to delete staging object after registration; left for reclamation")
	}

	log.
		WithFields(logTags).
		WithField("workspace", workspace.ID).
		WithField("artifact", newEntry.ID).
		WithField("object_key", storeObjKey).
		Debug("Registered new artifact")

	return newEntry, nil
}

/*
UpdateArtifactContent replace an existing artifact's content with a newly staged object.

This is `RegisterNewArtifact` minus the insert: the artifact entry already exists, so the
function only needs to determine the newly staged object's size and MIME type, copy it
into a new final storage object key, and repoint the entry at it (see DESIGN §6.3).

Every pre-copy check `RegisterNewArtifact` performs applies here unchanged - the staging
object key is verified as belonging to this artifact's workspace, and the object's
measured size is checked against the single-PUT size cap before anything is copied, so an
over-cap object never reaches the entry (see DESIGN §7.5).

The content is always copied to a NEW final object key rather than over the existing one.
The entry is then flipped to the new key in a single update, so a reader never observes it
pointing at a half-written object. The old object is left behind by design; the
object-reaping GC reclaims it as an aged final object with no backing row (see DESIGN
§6.2, §6.3, §8.2.1).

Concurrent updates are last-writer-wins. Each writer copies to its own new key, and the
final entry update decides the winner; the loser's object is reclaimed the same way. The
entry update carries no expected revision, so it is unguarded, though it still raises the
entry's revision (see DESIGN §7.5.2).

An artifact quarantined as `MISSING_OBJECT` is a legitimate target: re-uploading its bytes
is exactly how it is brought back into service. The state transition is validated by the
persistence layer, so no state gate is applied here.

	@param ctx context.Context - execution context
	@param artifact models.Artifact - the artifact to update
	@param stagingObjKey string - the staging object returned by `GetArtifactStagingPutURL`.
	    The system expects the artifact entry's new data file is stored at this location
	    already.
	@param activeSession db.Database - if set, this is an existing open DB persistence
	    layer transaction, and function will perform additional persistence operations
	    within it.
	@returns the updated artifact entry
*/
func (m *managerImpl) UpdateArtifactContent(
	ctx context.Context,
	artifact models.Artifact,
	stagingObjKey string,
	activeSession db.Database,
) (models.Artifact, error) {
	return m.updateArtifactContent(ctx, artifact, stagingObjKey, nil, activeSession)
}

/*
updateArtifactContent replace an existing artifact's content with a newly staged object,
optionally as a compare-and-swap.

`UpdateArtifactContent` with one addition: given an expected revision, the entry is only
repointed while it is still at that revision, and otherwise left untouched with an
`ArtifactRevisionConflictError` returned (see DESIGN §7.5.2). The copy to the new final key
has already happened by then, so a refused update leaves that object unreferenced for the
reaping GC, like any other failure after the copy.

	@param ctx context.Context - execution context
	@param artifact models.Artifact - the artifact to update
	@param stagingObjKey string - the staging object holding the new content
	@param expectedRevision *int64 - if set, only update the entry while it is still at this
	    revision
	@param activeSession db.Database - if set, this is an existing open DB persistence
	    layer transaction, and function will perform additional persistence operations
	    within it.
	@returns the updated artifact entry
*/
func (m *managerImpl) updateArtifactContent(
	ctx context.Context,
	artifact models.Artifact,
	stagingObjKey string,
	expectedRevision *int64,
	activeSession db.Database,
) (models.Artifact, error) {
	logTags := m.GetLogTagsForContext(ctx)

	failure := func(err error) (models.Artifact, error) {
		return models.Artifact{}, models.NewArtifactMangerError(
			fmt.Sprintf("failed to update artifact %s content", artifact.ID), err, true,
		)
	}

	// Prove the caller is not pointing at another workspace's staging object.
	if err := m.verifyStagingKeyOwnership(artifact.WorkspaceID, stagingObjKey); err != nil {
		return failure(err)
	}

	s3Client, err := m.s3Client(ctx)
	if err != nil {
		return failure(err)
	}

	// Measure what actually landed, before any copy, so an over-cap object costs nothing and
	// leaves the entry pointing at its existing content.
	stagingStat, err := s3Client.GetObjectStat(ctx, m.storeConfig.Bucket, stagingObjKey)
	if err != nil {
		return failure(err)
	}
	if err := m.enforceSizeCap(
		stagingStat.Size, fmt.Sprintf("staged object '%s'", stagingObjKey),
	); err != nil {
		return failure(err)
	}

	// Re-sniffed rather than carried over from the entry: the new content is new content, and
	// may well be of a different type than what the artifact held before.
	mimeType, err := m.sniffObjectMIMEType(ctx, stagingObjKey)
	if err != nil {
		return failure(err)
	}

	// A NEW key every time. Copying over the existing object would make the update
	// non-atomic, and a same-key copy is its own hazard in an S3-compatible store (see
	// DESIGN §6.2).
	storeObjKey := m.newStoreObjectKey(artifact.WorkspaceID)

	s3Client, err = m.s3Client(ctx)
	if err != nil {
		return failure(err)
	}

	if err := s3Client.CopyObject(
		ctx, m.storeConfig.Bucket, stagingObjKey, m.storeConfig.Bucket, storeObjKey, &mimeType,
	); err != nil {
		return failure(err)
	}

	var updated models.Artifact
	if err := db.ActiveSessionWrapper(
		ctx, activeSession, m.persistence, func(dbCtx context.Context, dbClient db.Database) error {
			if err := dbClient.UpdateArtifactObject(
				dbCtx, artifact.ID, storeObjKey, mimeType, stagingStat.Size, expectedRevision,
			); err != nil {
				return goutils.NewPersistenceError(
					fmt.Sprintf("failed to repoint artifact %s at its new object", artifact.ID),
					err,
					true,
				)
			}
			// Re-read within the same transaction so the returned entry reflects the update.
			var err error
			updated, err = dbClient.GetArtifact(dbCtx, artifact.ID)
			if err != nil {
				return goutils.NewPersistenceError(
					fmt.Sprintf("failed to read back updated artifact %s", artifact.ID), err, true,
				)
			}
			return nil
		},
	); err != nil {
		return failure(err)
	}

	// Best-effort: the entry already points at the new object, so failing the call over
	// leftover staging debris would be strictly worse than leaving it. The maintenance sweep
	// reclaims aged staging objects (see DESIGN §8.2.1). Failing to even acquire a client is
	// the same class of failure, so it takes the same path.
	if s3Client, err := m.s3Client(ctx); err != nil {
		log.
			WithError(err).
			WithFields(logTags).
			WithField("staging_object_key", stagingObjKey).
			Warn("Failed to acquire object store client to delete staging object; left for reclamation")
	} else if err := s3Client.DeleteObject(ctx, m.storeConfig.Bucket, stagingObjKey); err != nil {
		log.
			WithError(err).
			WithFields(logTags).
			WithField("staging_object_key", stagingObjKey).
			Warn("Failed to delete staging object after content update; left for reclamation")
	}

	log.
		WithFields(logTags).
		WithField("workspace", artifact.WorkspaceID).
		WithField("artifact", artifact.ID).
		WithField("old_object_key", artifact.ObjectKey).
		WithField("object_key", storeObjKey).
		Debug("Updated artifact content")

	return updated, nil
}

// ======================================================================================
// Text Artifact Staging & Editing
//
// The inline text path (see DESIGN §6.6): the content travels in the call itself, so the
// service writes the staging object directly - no presigned URL, no sidecar - and then hands it
// to the same register / update core the other upload paths use.

/*
StageTextBlob record a blob of plain text as a staging object within a workspace.

The text counterpart of `GetArtifactStagingPutURL`: the content arrives in the call
rather than through a presigned PUT, so the service writes the staging object itself
(see DESIGN §6.6). `CreateTextArtifact` pairs the returned staging object key with
`RegisterNewArtifact` to record a new text artifact.

The content is held to the rules every text tool applies, before anything is written:
  - it must fit under the text size cap;
  - it must be plain text - valid UTF-8, with no control character other than tab, LF,
    and CR as part of CRLF, and one line ending throughout (see DESIGN §7.6.1);
  - it must sniff as a text MIME type, so the text tools never create an artifact they
    would then refuse to read.

Purely an object-store operation - no DB entry is created here.

	@param ctx context.Context - execution context
	@param workspace models.Workspace - workspace this is for
	@param content string - the full text to stage
	@returns the staging object key
*/
func (m *managerImpl) StageTextBlob(
	ctx context.Context, workspace models.Workspace, content string,
) (string, error) {
	stagingObjKey, err := m.stageTextBlob(ctx, workspace.ID, content)
	if err != nil {
		return "", models.NewArtifactMangerError(
			fmt.Sprintf("failed to stage text for workspace %s", workspace.ID), err, true,
		)
	}
	return stagingObjKey, nil
}

/*
CreateTextArtifact record a new plain text artifact from text sent in the call.

Three steps, the same ones the volume upload runs: the name is checked as free, the text is
staged with `StageTextBlob`, and the staging object is registered with `RegisterNewArtifact`
(see DESIGN §6.6, §7.6.5). The text is held to `StageTextBlob`'s rules, and the new artifact
starts at revision 1.

The name check is a fail-fast, so a taken name costs no staging write. The database's
uniqueness constraint remains the real guard for a caller that races another (see DESIGN
§7.5).

A failure after the text is staged leaves the staging object unreferenced. It is not cleaned
up here: the aged-staging sweep reclaims it (see DESIGN §8.2.1).

The parent workspace is taken as already resolved by the caller (see DESIGN §3).

	@param ctx context.Context - execution context
	@param workspace models.Workspace - workspace this is for
	@param content string - the full text of the new artifact
	@param name string - name a user will reference the artifact by
	@param description *string - an optional description for the artifact
	@param activeSession db.Database - if set, this is an existing open DB persistence
	    layer transaction, and function will perform additional persistence operations
	    within it. Normally pass nil: a transaction handed in here is held open across the
	    object store calls, and the only work inside it is the name check and the final
	    insert.
	@returns the new artifact entry
*/
func (m *managerImpl) CreateTextArtifact(
	ctx context.Context,
	workspace models.Workspace,
	content string,
	name string,
	description *string,
	activeSession db.Database,
) (models.Artifact, error) {
	if err := verifyArtifactNameFree(ctx, m, workspace, name, activeSession); err != nil {
		return models.Artifact{}, models.NewArtifactMangerError(
			fmt.Sprintf(
				"failed to create text artifact '%s' in workspace %s", name, workspace.ID,
			), err, true,
		)
	}

	// Both calls below already return a manager error naming the step that failed, so their
	// errors are passed back as they are rather than wrapped a second time.
	stagingObjKey, err := m.StageTextBlob(ctx, workspace, content)
	if err != nil {
		return models.Artifact{}, err
	}

	return m.RegisterNewArtifact(ctx, workspace, stagingObjKey, name, description, activeSession)
}

/*
stageTextBlob record a blob of plain text as a staging object, by workspace ID.

The body of `StageTextBlob`, taking only the workspace ID so an edit can stage its result
from the artifact entry alone.

	@param ctx context.Context - execution context
	@param workspaceID string - ID of the workspace to stage the text in
	@param content string - the full text to stage
	@returns the staging object key
*/
func (m *managerImpl) stageTextBlob(
	ctx context.Context, workspaceID string, content string,
) (string, error) {
	if err := m.enforceTextSizeCap(
		int64(len(content)), fmt.Sprintf("text staged for workspace %s", workspaceID),
	); err != nil {
		return "", err
	}

	if _, err := checkPlainText([]byte(content)); err != nil {
		return "", goutils.NewBadInputError("text is not plain text", err, true)
	}

	// Sniffed here from the bytes in hand, ahead of the authoritative sniff at register or
	// commit. Both read the same leading bytes through the same detector, so this predicts the
	// MIME type the artifact will carry - and refusing a non-text one now keeps the text tools
	// from creating an artifact they would then refuse to read.
	sniffWindow := content
	if len(sniffWindow) > mimeSniffWindowBytes {
		sniffWindow = sniffWindow[:mimeSniffWindowBytes]
	}
	if mimeType := m.detectMIMEType([]byte(sniffWindow)); !isTextMIMEType(mimeType) {
		return "", goutils.NewBadInputError(
			fmt.Sprintf("text sniffs as '%s', which is not a plain text format", mimeType),
			nil,
			true,
		)
	}

	stagingObjKey := m.newStagingObjectKey(workspaceID)

	s3Client, err := m.s3Client(ctx)
	if err != nil {
		return "", err
	}

	// No content type is set: the staging object's own type is never read, and the artifact's
	// MIME type comes from the server-side sniff at register or commit (see DESIGN §6.4).
	if err := s3Client.PutObject(
		ctx,
		m.storeConfig.Bucket,
		stagingObjKey,
		strings.NewReader(content),
		int64(len(content)),
		nil,
	); err != nil {
		return "", err
	}

	log.
		WithFields(m.GetLogTagsForContext(ctx)).
		WithField("workspace", workspaceID).
		WithField("staging_object_key", stagingObjKey).
		Debug("Staged text blob")

	return stagingObjKey, nil
}

/*
StageUpdatedArtifactText make one attempt at editing a plain text artifact, staging the
result.

The artifact entry is read fresh, its content fetched from the object store, and the
edits applied to it in order, all or nothing (see DESIGN §7.6.4). The edits are matched
against the content with LF line endings, whatever the artifact uses, and the result is
staged in the artifact's own line ending (see DESIGN §7.6.2) under `StageTextBlob`'s
rules.

Nothing is committed: the entry still points at its existing content. The entry returned
is the one the edits were applied to, and committing the staging object guarded on that
entry's revision is what makes the edit a compare-and-swap (see DESIGN §7.5.2).
`EditTextArtifact` does exactly that, starting over on a conflict.

A conflict can also surface here, before any commit: when the entry is not at the base
revision, or when it moved to a new object while its content was being read.

	@param ctx context.Context - execution context
	@param artifactID string - ID of the artifact to edit
	@param edits []models.TextEdit - the edits to apply, in order
	@param baseRevision *int64 - if set, refuse unless the artifact is still at this
	    revision
	@param activeSession db.Database - if set, this is an existing open DB persistence
	    layer transaction, and function will perform additional persistence operations
	    within it.
	@returns the artifact entry the edits were applied to
	@returns the staging object key holding the edited content
*/
func (m *managerImpl) StageUpdatedArtifactText(
	ctx context.Context,
	artifactID string,
	edits []models.TextEdit,
	baseRevision *int64,
	activeSession db.Database,
) (models.Artifact, string, error) {
	failure := func(err error) (models.Artifact, string, error) {
		return models.Artifact{}, "", models.NewArtifactMangerError(
			fmt.Sprintf("failed to stage edit of artifact %s", artifactID), err, true,
		)
	}

	entry, err := m.readArtifactEntry(ctx, artifactID, activeSession)
	if err != nil {
		return failure(err)
	}

	// A fail fast only: the guarded commit's WHERE clause is what enforces the base revision.
	// Refusing here just spares the fetch, the edit, and the staging write.
	if baseRevision != nil && entry.Revision != *baseRevision {
		return failure(models.NewArtifactRevisionConflictError(
			fmt.Sprintf(
				"artifact %s is at revision %d, not the base revision %d",
				artifactID, entry.Revision, *baseRevision,
			),
			goutils.NewConsistencyError(
				fmt.Sprintf("artifact %s changed after revision %d", artifactID, *baseRevision),
				nil,
				false,
			),
			true,
		))
	}

	raw, err := m.readTextObject(ctx, entry, activeSession)
	if err != nil {
		return failure(err)
	}

	lineEnding, err := checkPlainText(raw)
	if err != nil {
		return failure(goutils.NewBadInputError(
			fmt.Sprintf("artifact %s content is not plain text", artifactID), err, true,
		))
	}

	edited, err := applyTextEdits(toLF(string(raw)), edits)
	if err != nil {
		return failure(err)
	}

	stagingObjKey, err := m.stageTextBlob(ctx, entry.WorkspaceID, fromLF(edited, lineEnding))
	if err != nil {
		return failure(err)
	}

	return entry, stagingObjKey, nil
}

/*
EditTextArtifact apply a list of exact-text replacements to a plain text artifact.

Each attempt stages the edit with `StageUpdatedArtifactText`, then commits it as a
compare-and-swap: the entry is repointed at the edited content only while it is still at
the revision the edits were applied to (see DESIGN §7.5.2).

Without a base revision, a conflict starts the attempt over against the newer content,
up to the configured number of attempts. The edits are re-applied, so a concurrent
change elsewhere in the artifact lets the edit through, while one to the text it targets
fails it with a legible error. With a base revision, a conflict is returned at once: the
revision only ever goes up, so a retry could never match it.

There is no `activeSession`. Each step runs in its own short transaction, since holding
one open across object store calls and retries is exactly the long transaction the
compare-and-swap exists to avoid.

	@param ctx context.Context - execution context
	@param artifactID string - ID of the artifact to edit
	@param edits []models.TextEdit - the edits to apply, in order
	@param baseRevision *int64 - if set, refuse unless the artifact is still at this
	    revision
	@returns the updated artifact entry
*/
func (m *managerImpl) EditTextArtifact(
	ctx context.Context, artifactID string, edits []models.TextEdit, baseRevision *int64,
) (models.Artifact, error) {
	maxAttempts := m.storeConfig.TextMaxAttempts

	var lastConflict error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		updated, err := m.editTextArtifactOnce(ctx, artifactID, edits, baseRevision)
		if err == nil {
			return updated, nil
		}

		// Only a conflict is worth starting over on, and only without a base revision: the
		// revision never goes back down, so a newer one can never match the base again.
		if !isRevisionConflict(err) || baseRevision != nil {
			return models.Artifact{}, err
		}

		lastConflict = err
		log.
			WithError(err).
			WithFields(m.GetLogTagsForContext(ctx)).
			WithField("artifact", artifactID).
			WithField("attempt", attempt).
			Debug("Artifact changed during a text edit; starting over")
	}

	return models.Artifact{}, models.NewArtifactMangerError(
		fmt.Sprintf(
			"artifact %s kept changing; gave up editing it after %d attempt(s)",
			artifactID, maxAttempts,
		),
		lastConflict,
		true,
	)
}

/*
editTextArtifactOnce make one complete attempt at a text edit: stage it, then commit it guarded
on the revision it was applied to.

	@param ctx context.Context - execution context
	@param artifactID string - ID of the artifact to edit
	@param edits []models.TextEdit - the edits to apply, in order
	@param baseRevision *int64 - if set, refuse unless the artifact is still at this
	    revision
	@returns the updated artifact entry
*/
func (m *managerImpl) editTextArtifactOnce(
	ctx context.Context, artifactID string, edits []models.TextEdit, baseRevision *int64,
) (models.Artifact, error) {
	entry, stagingObjKey, err := m.StageUpdatedArtifactText(
		ctx, artifactID, edits, baseRevision, nil,
	)
	if err != nil {
		return models.Artifact{}, err
	}

	// Guarded on the revision the edits were applied to, not on the base revision: the two are
	// equal whenever a base was given, and without one this is still what keeps a concurrent
	// writer's change from being overwritten unseen.
	expectedRevision := entry.Revision
	return m.updateArtifactContent(ctx, entry, stagingObjKey, &expectedRevision, nil)
}

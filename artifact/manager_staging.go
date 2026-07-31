package artifact

import (
	"context"
	"fmt"

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

	// The TTL bounds, the non-negative size, and the checksum's base64 encoding are all
	// validated by the object store client, so they are not re-checked here.
	putURL, err := m.s3.GeneratePresignedPutURL(
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

	// Measure what actually landed. The mint-time check trusted a caller-declared size; this
	// one is authoritative, and runs before any copy so an over-cap object costs nothing.
	stagingStat, err := m.s3.GetObjectStat(ctx, m.storeConfig.Bucket, stagingObjKey)
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

	// Staging and storage share one bucket, distinguished by key prefix (see DESIGN §8.1), so
	// this is a server-side copy within the bucket that rewrites the MIME type to the sniffed
	// value.
	if err := m.s3.CopyObject(
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
	// staging objects (see DESIGN §6.1 step 7, §8.2.1).
	if err := m.s3.DeleteObject(ctx, m.storeConfig.Bucket, stagingObjKey); err != nil {
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
final entry update decides the winner; the loser's object is reclaimed the same way. There
is no optimistic locking (see DESIGN §7.5.2).

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

	// Measure what actually landed, before any copy, so an over-cap object costs nothing and
	// leaves the entry pointing at its existing content.
	stagingStat, err := m.s3.GetObjectStat(ctx, m.storeConfig.Bucket, stagingObjKey)
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

	if err := m.s3.CopyObject(
		ctx, m.storeConfig.Bucket, stagingObjKey, m.storeConfig.Bucket, storeObjKey, &mimeType,
	); err != nil {
		return failure(err)
	}

	var updated models.Artifact
	if err := db.ActiveSessionWrapper(
		ctx, activeSession, m.persistence, func(dbCtx context.Context, dbClient db.Database) error {
			if err := dbClient.UpdateArtifactObject(
				dbCtx, artifact.ID, storeObjKey, mimeType, stagingStat.Size,
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
	// reclaims aged staging objects (see DESIGN §8.2.1).
	if err := m.s3.DeleteObject(ctx, m.storeConfig.Bucket, stagingObjKey); err != nil {
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

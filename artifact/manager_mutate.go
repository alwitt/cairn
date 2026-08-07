package artifact

import (
	"context"
	"errors"
	"fmt"

	"github.com/alwitt/cairn/db"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
)

// ======================================================================================
// Artifact Metadata Mutations & Deletion
//
// None of these touch the object store. A rename or description change is metadata only,
// and a delete leaves the backing object for the object-reaping GC rather than removing
// it here (see DESIGN §2.2, §4.1, §8.2.1).

/*
RenameArtifact change an artifact's name.

A pure DB update - the backing object key carries a random suffix rather than the name, so
a rename never touches the object store (see DESIGN §2.2).

Names are unique within the parent workspace, and the unique index remains the real guard.
The collision is pre-checked here anyway, for the message: left to the index, a taken name
comes back as a raw constraint violation naming `artifact_workspace_name`, which reads as a
server fault rather than as the caller error it is. The check makes it a `BadInputError`
carrying the same wording the upload path's `verifyArtifactNameFree` produces.

	@param ctx context.Context - execution context
	@param artifactID string - ID of artifact to rename
	@param newName string - the new artifact name
	@param activeSession db.Database - if set, this is an existing open DB persistence
	    layer transaction, and function will perform additional persistence operations
	    within it.
	@returns the artifact entry with an updated name
*/
func (m *managerImpl) RenameArtifact(
	ctx context.Context, artifactID string, newName string, activeSession db.Database,
) (models.Artifact, error) {
	var updated models.Artifact

	if err := db.ActiveSessionWrapper(
		ctx, activeSession, m.persistence, func(dbCtx context.Context, dbClient db.Database) error {
			// Read first for the parent workspace ID: names are unique per workspace, so the
			// collision check cannot be posed without it.
			current, err := dbClient.GetArtifact(dbCtx, artifactID)
			if err != nil {
				return goutils.NewPersistenceError(
					fmt.Sprintf("failed to read artifact %s before renaming it", artifactID),
					err,
					true,
				)
			}

			if err := verifyRenameTargetFree(
				dbCtx, dbClient, artifactID, current.WorkspaceID, newName,
			); err != nil {
				return err
			}

			if err := dbClient.UpdateArtifactName(dbCtx, artifactID, newName); err != nil {
				return goutils.NewPersistenceError(
					fmt.Sprintf("failed to rename artifact %s to '%s'", artifactID, newName), err, true,
				)
			}
			// Re-read within the same transaction so the returned entry reflects the update.
			updated, err = dbClient.GetArtifact(dbCtx, artifactID)
			if err != nil {
				return goutils.NewPersistenceError(
					fmt.Sprintf("failed to read back renamed artifact %s", artifactID), err, true,
				)
			}
			return nil
		},
	); err != nil {
		return models.Artifact{}, models.NewArtifactMangerError(
			fmt.Sprintf("failed to update artifact %s name to '%s'", artifactID, newName), err, true,
		)
	}

	return updated, nil
}

/*
verifyRenameTargetFree confirm no other artifact in the workspace already holds the new name.

The same three-way branch the upload path's `verifyArtifactNameFree` uses, and for the same
reason: treating any error as "the name is free" would let a database outage fall through to
the raw constraint violation this exists to replace.

Runs inside the caller's transaction rather than taking its own, so the read and the update
that follows it see one consistent snapshot.

	@param ctx context.Context - execution context
	@param dbClient db.Database - the open transaction to check within
	@param artifactID string - ID of the artifact being renamed
	@param workspaceID string - ID of the parent workspace, which scopes name uniqueness
	@param newName string - the name being moved to
*/
func verifyRenameTargetFree(
	ctx context.Context, dbClient db.Database, artifactID string, workspaceID string, newName string,
) error {
	existing, err := dbClient.GetArtifactByName(ctx, workspaceID, newName)

	if err == nil {
		// Renaming an artifact to the name it already carries is a no-op rather than a
		// collision - the row the index would clash with is this same row. Left unhandled it
		// would be reported as the artifact colliding with itself.
		if existing.ID == artifactID {
			return nil
		}
		return goutils.NewBadInputError(
			fmt.Sprintf(
				"workspace %s already has an artifact named '%s'", workspaceID, newName,
			), nil, true,
		)
	}

	// Not-found is the success case here. It survives the persistence layer's wrapping
	// because the goutils error types unwrap, so matching on the type reaches it.
	var notFound goutils.NotFoundError
	if errors.As(err, &notFound) {
		return nil
	}

	return goutils.NewPersistenceError(
		fmt.Sprintf(
			"failed to check whether the name '%s' is free in workspace %s", newName, workspaceID,
		), err, true,
	)
}

/*
UpdateArtifactDescription change an artifact's description

	@param ctx context.Context - execution context
	@param artifactID string - ID of artifact to update
	@param newDescription *string - the new description, nil to clear it
	@param activeSession db.Database - if set, this is an existing open DB persistence
	    layer transaction, and function will perform additional persistence operations
	    within it.
	@returns the artifact entry with an updated description
*/
func (m *managerImpl) UpdateArtifactDescription(
	ctx context.Context,
	artifactID string,
	newDescription *string,
	activeSession db.Database,
) (models.Artifact, error) {
	var updated models.Artifact

	if err := db.ActiveSessionWrapper(
		ctx, activeSession, m.persistence, func(dbCtx context.Context, dbClient db.Database) error {
			if err := dbClient.UpdateArtifactDescription(
				dbCtx, artifactID, newDescription,
			); err != nil {
				return goutils.NewPersistenceError(
					fmt.Sprintf("failed to update artifact %s description", artifactID), err, true,
				)
			}
			// Re-read within the same transaction so the returned entry reflects the update.
			var err error
			updated, err = dbClient.GetArtifact(dbCtx, artifactID)
			if err != nil {
				return goutils.NewPersistenceError(
					fmt.Sprintf("failed to read back updated artifact %s", artifactID), err, true,
				)
			}
			return nil
		},
	); err != nil {
		return models.Artifact{}, models.NewArtifactMangerError(
			fmt.Sprintf("failed to update artifact %s description", artifactID), err, true,
		)
	}

	return updated, nil
}

/*
DeleteArtifact delete an artifact entry.

Idempotent - deleting an absent entry is a no-op.

No object-store interaction: the object the entry referenced is left in the store and
reclaimed later by the object-reaping GC. Deleting it here would place a second reclaimer
alongside the GC, and the DB is what is authoritative for what exists (see DESIGN §2.2.1,
§4.1, §8.2.1).

	@param ctx context.Context - execution context
	@param artifactID string - ID of artifact to delete
	@param activeSession db.Database - if set, this is an existing open DB persistence
	    layer transaction, and function will perform additional persistence operations
	    within it.
*/
func (m *managerImpl) DeleteArtifact(
	ctx context.Context, artifactID string, activeSession db.Database,
) error {
	if err := db.ActiveSessionWrapper(
		ctx, activeSession, m.persistence, func(dbCtx context.Context, dbClient db.Database) error {
			if err := dbClient.DeleteArtifact(dbCtx, artifactID); err != nil {
				return goutils.NewPersistenceError(
					fmt.Sprintf("failed to delete artifact %s", artifactID), err, true,
				)
			}
			return nil
		},
	); err != nil {
		return models.NewArtifactMangerError(
			fmt.Sprintf("failed to delete artifact %s", artifactID), err, true,
		)
	}

	return nil
}

// Package db - database controllers for system persistence
package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/oklog/ulid/v2"
)

// ======================================================================================
// Artifact

/*
DefineNewArtifact record a new artifact.

Called only after the backing object is in place at its final key (see DESIGN §6.1), so the
entry is committed directly as `RECORDED` — there is no pending state. Name uniqueness within
the parent workspace is enforced by the DB constraint.

	@param ctx context.Context - execution context
	@param params NewArtifactParameter - new artifact parameters
	@returns the new artifact entry
*/
func (c *databaseImpl) DefineNewArtifact(
	ctx context.Context, params NewArtifactParameter,
) (models.Artifact, error) {
	newEntry := ArtifactEntry{
		Artifact: models.Artifact{
			ID:          ulid.Make().String(),
			WorkspaceID: params.WorkspaceID,
			Name:        params.Name,
			Description: params.Description,
			ObjectKey:   params.ObjectKey,
			MIMEType:    params.MIMEType,
			Size:        params.Size,
			State:       models.ArtifactStateRecorded,
		},
	}

	if err := c.validator.Struct(&newEntry); err != nil {
		return models.Artifact{}, goutils.NewValidationError(
			fmt.Sprintf(
				"new artifact '%s' of workspace %s entry is not valid",
				params.Name, params.WorkspaceID,
			), err, true,
		)
	}

	if tmp := c.db.Create(&newEntry); tmp.Error != nil {
		return models.Artifact{}, goutils.NewSQLError(
			fmt.Sprintf(
				"new artifact '%s' of workspace %s insert failed", params.Name, params.WorkspaceID,
			), tmp.Error, true,
		)
	}

	// Record new artifact event
	if _, err := c.defineNewSystemEvent(
		ctx,
		models.SystemEventTypeNewArtifact,
		&models.SystemEventNewArtifact{
			WorkspaceID:  newEntry.WorkspaceID,
			ArtifactID:   newEntry.ID,
			ArtifactName: newEntry.Name,
			ObjectKey:    newEntry.ObjectKey,
			MIMEType:     newEntry.MIMEType,
			Size:         newEntry.Size,
		},
	); err != nil {
		return models.Artifact{}, goutils.NewSQLError(
			fmt.Sprintf(
				"failed to record new artifact '%s' of workspace %s system event",
				params.Name, params.WorkspaceID,
			), err, true,
		)
	}

	return newEntry.Artifact, nil
}

// getArtifactDBEntry helper function to fetch one artifact
func (c *databaseImpl) getArtifactDBEntry(artifactID string) (ArtifactEntry, error) {
	var entry ArtifactEntry
	tmp := c.db.Model(&ArtifactEntry{}).Where("id = ?", artifactID).First(&entry)
	return entry, notFoundOrError(tmp.Error, "artifact", artifactID)
}

/*
GetArtifact fetch an artifact by ID

	@param ctx context.Context - execution context
	@param artifactID string - artifact ID
	@returns the artifact entry
*/
func (c *databaseImpl) GetArtifact(
	_ context.Context, artifactID string,
) (models.Artifact, error) {
	entry, err := c.getArtifactDBEntry(artifactID)
	return entry.Artifact, err
}

/*
GetArtifactByName fetch an artifact by name within a workspace.

Artifact names are unique per workspace, so this resolves a (workspace, name) pair to exactly
one entry. It backs the MCP layer's name -> ID resolution (see DESIGN §3).

	@param ctx context.Context - execution context
	@param workspaceID string - ID of the parent workspace
	@param name string - artifact name
	@returns the artifact entry
*/
func (c *databaseImpl) GetArtifactByName(
	_ context.Context, workspaceID string, name string,
) (models.Artifact, error) {
	var entry ArtifactEntry
	tmp := c.db.
		Model(&ArtifactEntry{}).
		Where("workspace_id = ? and name = ?", workspaceID, name).
		First(&entry)
	return entry.Artifact, notFoundOrError(
		tmp.Error, "artifact", fmt.Sprintf("%s/%s", workspaceID, name),
	)
}

/*
ListArtifacts list artifacts

	@param ctx context.Context - execution context
	@param filters ArtifactQueryFilter - query filtering conditions
	@returns list of artifacts
*/
func (c *databaseImpl) ListArtifacts(
	_ context.Context, filters ArtifactQueryFilter,
) ([]models.Artifact, error) {
	if err := c.validator.Struct(&filters); err != nil {
		return nil, goutils.NewValidationError("artifact query filter is not valid", err, true)
	}

	query := c.db.Model(&ArtifactEntry{})

	if filters.WorkspaceID != nil {
		query = query.Where("workspace_id = ?", *filters.WorkspaceID)
	}

	if len(filters.TargetIDs) > 0 {
		query = query.Where("id in ?", filters.TargetIDs)
	}

	if len(filters.TargetNames) > 0 {
		query = query.Where("name in ?", filters.TargetNames)
	}

	if len(filters.ArtifactStates) > 0 {
		query = query.Where("state in ?", filters.ArtifactStates)
	}

	if len(filters.ObjectKeys) > 0 {
		query = query.Where("object_key in ?", filters.ObjectKeys)
	}

	if filters.Limit != nil {
		query = query.Limit(*filters.Limit)
	}
	if filters.Offset != nil {
		query = query.Offset(*filters.Offset)
	}

	// The ID is a ULID, so it is itself creation-ordered — a stable tie breaker for entries
	// sharing a `created_at` timestamp.
	query = query.Order("created_at").Order("id")

	var entries []ArtifactEntry
	if tmp := query.Find(&entries); tmp.Error != nil {
		return nil, goutils.NewSQLError("failed to list artifacts", tmp.Error, true)
	}

	result := []models.Artifact{}
	for _, entry := range entries {
		result = append(result, entry.Artifact)
	}

	return result, nil
}

/*
ListWorkspaceArtifacts list the artifacts belonging to a particular workspace

	@param ctx context.Context - execution context
	@param workspaceID string - ID of the parent workspace
	@param filters ArtifactQueryFilter - query filtering conditions
	@returns list of artifacts
*/
func (c *databaseImpl) ListWorkspaceArtifacts(
	ctx context.Context, workspaceID string, filters ArtifactQueryFilter,
) ([]models.Artifact, error) {
	filters.WorkspaceID = &workspaceID
	return c.ListArtifacts(ctx, filters)
}

/*
UpdateArtifactName change an artifact's name.

A pure DB update: the backing object key carries a random suffix rather than the name, so a
rename never touches the object store (see DESIGN §2.2). Uniqueness within the parent
workspace is enforced by the DB constraint.

	@param ctx context.Context - execution context
	@param artifactID string - artifact ID
	@param newName string - the new artifact name
*/
func (c *databaseImpl) UpdateArtifactName(
	ctx context.Context, artifactID string, newName string,
) error {
	entry, err := c.getArtifactDBEntry(artifactID)
	if err != nil {
		return err
	}

	// Capture the outgoing name for the audit record before overwriting it
	oldName := entry.Name

	entry.Name = newName
	if err := c.validator.Struct(&entry); err != nil {
		return goutils.NewValidationError(
			fmt.Sprintf("artifact %s new name '%s' is not valid", artifactID, newName), err, true,
		)
	}

	tmp := c.db.Model(&ArtifactEntry{}).Where("id = ?", entry.ID).UpdateColumn("name", newName)
	if tmp.Error != nil {
		return goutils.NewSQLError(
			fmt.Sprintf("failed to update artifact %s name", artifactID), tmp.Error, true,
		)
	}

	// Record artifact rename event
	if _, err := c.defineNewSystemEvent(
		ctx,
		models.SystemEventTypeRenameArtifact,
		&models.SystemEventRenameArtifact{
			WorkspaceID:     entry.WorkspaceID,
			ArtifactID:      entry.ID,
			OldArtifactName: oldName,
			NewArtifactName: newName,
		},
	); err != nil {
		return goutils.NewSQLError(
			fmt.Sprintf("failed to record artifact %s rename system event", artifactID), err, true,
		)
	}

	return nil
}

/*
UpdateArtifactDescription change an artifact's description

	@param ctx context.Context - execution context
	@param artifactID string - artifact ID
	@param newDescription *string - the new description, nil to clear it
*/
func (c *databaseImpl) UpdateArtifactDescription(
	_ context.Context, artifactID string, newDescription *string,
) error {
	entry, err := c.getArtifactDBEntry(artifactID)
	if err != nil {
		return err
	}

	tmp := c.db.
		Model(&ArtifactEntry{}).
		Where("id = ?", entry.ID).
		UpdateColumn("description", newDescription)
	if tmp.Error != nil {
		return goutils.NewSQLError(
			fmt.Sprintf("failed to update artifact %s description", artifactID), tmp.Error, true,
		)
	}

	return nil
}

/*
UpdateArtifactObject repoint an artifact at a newly written backing object.

The artifact's bytes are replaced by copying them to a NEW final key and flipping the row over
to it in one update, so readers always observe a complete object. The old object is orphaned
by design and reclaimed later by the object-reaping GC (see DESIGN §6.3).

Also restores the entry to `RECORDED`, so re-uploading the bytes of an artifact quarantined as
`MISSING_OBJECT` brings it back into service.

	@param ctx context.Context - execution context
	@param artifactID string - artifact ID
	@param objectKey string - the new backing object key
	@param mimeType string - server-sniffed content type of the new object
	@param size int64 - size of the new object in bytes
*/
func (c *databaseImpl) UpdateArtifactObject(
	ctx context.Context, artifactID string, objectKey string, mimeType string, size int64,
) error {
	entry, err := c.getArtifactDBEntry(artifactID)
	if err != nil {
		return err
	}

	if err := entry.ValidStateNextState(models.ArtifactStateRecorded); err != nil {
		return goutils.NewConsistencyError(
			fmt.Sprintf(
				"artifact %s can't transition to '%s'", artifactID, models.ArtifactStateRecorded,
			), err, true,
		)
	}

	// Capture the outgoing object for the audit record before overwriting it — the old key is
	// about to become an orphan, and this is the only record of what it was.
	oldObjectKey := entry.ObjectKey
	oldMIMEType := entry.MIMEType
	oldSize := entry.Size

	entry.ObjectKey = objectKey
	entry.MIMEType = mimeType
	entry.Size = size
	entry.State = models.ArtifactStateRecorded
	if err := c.validator.Struct(&entry); err != nil {
		return goutils.NewValidationError(
			fmt.Sprintf("artifact %s new backing object is not valid", artifactID), err, true,
		)
	}

	// One update flips the whole reference over, so a reader never observes the row pointing
	// at a half-written object.
	tmp := c.db.Model(&ArtifactEntry{}).Where("id = ?", entry.ID).Updates(map[string]any{
		"object_key": objectKey,
		"mime_type":  mimeType,
		"size":       size,
		"state":      models.ArtifactStateRecorded,
	})
	if tmp.Error != nil {
		return goutils.NewSQLError(
			fmt.Sprintf("failed to update artifact %s backing object", artifactID), tmp.Error, true,
		)
	}

	// Record artifact backing object update event
	if _, err := c.defineNewSystemEvent(
		ctx,
		models.SystemEventTypeUpdateArtifactObject,
		&models.SystemEventUpdateArtifactObject{
			WorkspaceID:  entry.WorkspaceID,
			ArtifactID:   entry.ID,
			ArtifactName: entry.Name,
			OldObjectKey: oldObjectKey,
			OldMIMEType:  oldMIMEType,
			OldSize:      oldSize,
			NewObjectKey: objectKey,
			NewMIMEType:  mimeType,
			NewSize:      size,
		},
	); err != nil {
		return goutils.NewSQLError(
			fmt.Sprintf(
				"failed to record artifact %s backing object update system event", artifactID,
			), err, true,
		)
	}

	return nil
}

/*
MarkArtifactMissingObject quarantine an artifact whose backing object is gone.

A data-loss signal rather than routine garbage, so it is not auto-remediated: the transition
preserves the row as evidence of the loss and surfaces the incident for an operator, who may
then delete the row (see DESIGN §8.2.1).

	@param ctx context.Context - execution context
	@param artifactID string - artifact ID
*/
func (c *databaseImpl) MarkArtifactMissingObject(ctx context.Context, artifactID string) error {
	entry, err := c.getArtifactDBEntry(artifactID)
	if err != nil {
		return err
	}

	if err := entry.ValidStateNextState(models.ArtifactStateMissingObject); err != nil {
		return goutils.NewConsistencyError(
			fmt.Sprintf(
				"artifact %s can't transition to '%s'", artifactID, models.ArtifactStateMissingObject,
			), err, true,
		)
	}

	tmp := c.db.
		Model(&ArtifactEntry{}).
		Where("id = ?", entry.ID).
		UpdateColumn("state", models.ArtifactStateMissingObject)
	if tmp.Error != nil {
		return goutils.NewSQLError(
			fmt.Sprintf(
				"failed to record artifact %s with new state '%s'",
				artifactID, models.ArtifactStateMissingObject,
			), tmp.Error, true,
		)
	}

	// Record artifact missing object event
	if _, err := c.defineNewSystemEvent(
		ctx,
		models.SystemEventTypeArtifactMissingObject,
		&models.SystemEventArtifactMissingObject{
			WorkspaceID:  entry.WorkspaceID,
			ArtifactID:   entry.ID,
			ArtifactName: entry.Name,
			ObjectKey:    entry.ObjectKey,
		},
	); err != nil {
		return goutils.NewSQLError(
			fmt.Sprintf(
				"failed to record artifact %s missing object system event", artifactID,
			), err, true,
		)
	}

	return nil
}

/*
DeleteArtifact delete an artifact entry.

Idempotent — deleting an absent entry is a no-op. No object-store interaction: the object the
row referenced is left in the store and reclaimed later by the object-reaping GC (see
DESIGN §4.1, §8.2.1).

	@param ctx context.Context - execution context
	@param artifactID string - artifact ID
*/
func (c *databaseImpl) DeleteArtifact(ctx context.Context, artifactID string) error {
	// Read the entry before removing it, both to keep the delete a no-op when it is already
	// gone and to capture its details while they still exist.
	entry, err := c.getArtifactDBEntry(artifactID)
	if err != nil {
		var notFound goutils.NotFoundError
		if errors.As(err, &notFound) {
			// Nothing was deleted, so there is nothing to record.
			return nil
		}
		return err
	}

	tmp := c.db.Where("id = ?", entry.ID).Delete(&ArtifactEntry{})
	if tmp.Error != nil {
		return goutils.NewSQLError(
			fmt.Sprintf("failed to delete artifact %s", artifactID), tmp.Error, true,
		)
	}

	// Record delete artifact event. The audit entry is independent of the artifact it
	// describes, so it outlives the deleted row.
	if _, err := c.defineNewSystemEvent(
		ctx,
		models.SystemEventTypeDeleteArtifact,
		&models.SystemEventDeleteArtifact{
			WorkspaceID:  entry.WorkspaceID,
			ArtifactID:   entry.ID,
			ArtifactName: entry.Name,
			ObjectKey:    entry.ObjectKey,
		},
	); err != nil {
		return goutils.NewSQLError(
			fmt.Sprintf("failed to record delete artifact %s system event", artifactID), err, true,
		)
	}

	return nil
}

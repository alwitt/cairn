// Package db - database controllers for system persistence
package db

import (
	"context"
	"fmt"

	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// ======================================================================================
// Workspace

/*
buildVolumeMetadataColumn validate volume metadata and wrap it for the `volume_metadata`
column, mapping nil onto a NULL column.

The struct is validated here rather than through the enclosing `WorkspaceEntry`: the column's
`datatypes.JSONType` wrapper is opaque to the validator, so a `validate` tag on the workspace
field can't reach the fields inside. Every write path to the column goes through here so the
check can't be skipped.

	@param metadata *models.WorkspaceVolumeMetadata - the metadata to store, nil for none
	@returns the column value to persist
*/
func (c *databaseImpl) buildVolumeMetadataColumn(
	metadata *models.WorkspaceVolumeMetadata,
) (*datatypes.JSONType[models.WorkspaceVolumeMetadata], error) {
	if metadata == nil {
		return nil, nil
	}

	if err := c.validator.Struct(metadata); err != nil {
		return nil, err
	}

	wrapped := datatypes.NewJSONType(*metadata)
	return &wrapped, nil
}

/*
DefineNewWorkspace define a new workspace.

The workspace's persistent volume name is derived here as `<app name>-<workspace ID>` and
persisted, so no client ever guesses or re-derives it (see DESIGN §2.1). Deriving it from the
immutable ID rather than the name keeps it stable across a workspace rename.

The new workspace starts with no persistent volume (`VolumeState = NONE`); the volume is
provisioned separately by the operator (see DESIGN §4.2). Any volume metadata given here is
recorded for that later provisioning to read.

	@param ctx context.Context - execution context
	@param params NewWorkspaceParameter - new workspace parameters
	@returns the new workspace entry
*/
func (c *databaseImpl) DefineNewWorkspace(
	ctx context.Context, params NewWorkspaceParameter,
) (models.Workspace, error) {
	workspaceID := uuid.NewString()

	volumeMetadata, err := c.buildVolumeMetadataColumn(params.VolumeMetadata)
	if err != nil {
		return models.Workspace{}, goutils.NewValidationError(
			fmt.Sprintf("new workspace '%s' volume metadata is not valid", params.Name), err, true,
		)
	}

	newEntry := WorkspaceEntry{
		Workspace: models.Workspace{
			ID:             workspaceID,
			Name:           params.Name,
			Description:    params.Description,
			VolumeName:     models.WorkspaceVolumeName(params.AppName, workspaceID),
			VolumeState:    models.WorkspaceVolumeStateNone,
			VolumeMetadata: volumeMetadata,
		},
	}

	if err := c.validator.Struct(&newEntry); err != nil {
		return models.Workspace{}, goutils.NewValidationError(
			fmt.Sprintf("new workspace '%s' entry is not valid", params.Name), err, true,
		)
	}

	if tmp := c.db.Create(&newEntry); tmp.Error != nil {
		return models.Workspace{}, goutils.NewSQLError(
			fmt.Sprintf("new workspace '%s' insert failed", params.Name), tmp.Error, true,
		)
	}

	// Record new workspace event
	if _, err := c.defineNewSystemEvent(
		ctx,
		models.SystemEventTypeNewWorkspace,
		&models.SystemEventNewWorkspace{
			WorkspaceID:   newEntry.ID,
			WorkspaceName: newEntry.Name,
			VolumeName:    newEntry.VolumeName,
		},
	); err != nil {
		return models.Workspace{}, goutils.NewSQLError(
			fmt.Sprintf("failed to record new workspace '%s' system event", params.Name), err, true,
		)
	}

	return newEntry.Workspace, nil
}

// getWorkspaceDBEntry helper function to fetch one workspace
func (c *databaseImpl) getWorkspaceDBEntry(workspaceID string) (WorkspaceEntry, error) {
	var entry WorkspaceEntry
	tmp := c.db.Model(&WorkspaceEntry{}).Where("id = ?", workspaceID).First(&entry)
	return entry, notFoundOrError(tmp.Error, "workspace", workspaceID)
}

/*
GetWorkspace fetch a workspace by ID

	@param ctx context.Context - execution context
	@param workspaceID string - workspace ID
	@returns the workspace entry
*/
func (c *databaseImpl) GetWorkspace(
	_ context.Context, workspaceID string,
) (models.Workspace, error) {
	entry, err := c.getWorkspaceDBEntry(workspaceID)
	return entry.Workspace, err
}

/*
GetWorkspaceByName fetch a workspace by name.

Workspace names are globally unique, so this resolves a name to exactly one entry. It backs
the MCP layer's name -> ID resolution (see DESIGN §3).

	@param ctx context.Context - execution context
	@param name string - workspace name
	@returns the workspace entry
*/
func (c *databaseImpl) GetWorkspaceByName(
	_ context.Context, name string,
) (models.Workspace, error) {
	var entry WorkspaceEntry
	tmp := c.db.Model(&WorkspaceEntry{}).Where("name = ?", name).First(&entry)
	return entry.Workspace, notFoundOrError(tmp.Error, "workspace", name)
}

/*
ListWorkspaces list workspaces

	@param ctx context.Context - execution context
	@param filters WorkspaceQueryFilter - query filtering conditions
	@returns list of workspaces
*/
func (c *databaseImpl) ListWorkspaces(
	_ context.Context, filters WorkspaceQueryFilter,
) ([]models.Workspace, error) {
	if err := c.validator.Struct(&filters); err != nil {
		return nil, goutils.NewValidationError("workspace query filter is not valid", err, true)
	}

	query := c.db.Model(&WorkspaceEntry{})

	if len(filters.TargetIDs) > 0 {
		query = query.Where("id in ?", filters.TargetIDs)
	}

	if len(filters.TargetNames) > 0 {
		query = query.Where("name in ?", filters.TargetNames)
	}

	if len(filters.VolumeStates) > 0 {
		query = query.Where("volume_state in ?", filters.VolumeStates)
	}

	if filters.Limit != nil {
		query = query.Limit(*filters.Limit)
	}
	if filters.Offset != nil {
		query = query.Offset(*filters.Offset)
	}

	query = query.Order("created_at")

	var entries []WorkspaceEntry
	if tmp := query.Find(&entries); tmp.Error != nil {
		return nil, goutils.NewSQLError("failed to list workspaces", tmp.Error, true)
	}

	result := []models.Workspace{}
	for _, entry := range entries {
		result = append(result, entry.Workspace)
	}

	return result, nil
}

/*
UpdateWorkspaceName change a workspace's name.

A pure DB update with no volume guard: the volume name is derived from the immutable
workspace ID, so a rename never affects the volume, even a live mounted one (see DESIGN §7.1).

	@param ctx context.Context - execution context
	@param workspaceID string - workspace ID
	@param newName string - the new workspace name
*/
func (c *databaseImpl) UpdateWorkspaceName(
	ctx context.Context, workspaceID string, newName string,
) error {
	entry, err := c.getWorkspaceDBEntry(workspaceID)
	if err != nil {
		return err
	}

	// Capture the outgoing name for the audit record before overwriting it
	oldName := entry.Name

	entry.Name = newName
	if err := c.validator.Struct(&entry); err != nil {
		return goutils.NewValidationError(
			fmt.Sprintf("workspace %s new name '%s' is not valid", workspaceID, newName), err, true,
		)
	}

	tmp := c.db.Model(&WorkspaceEntry{}).Where("id = ?", entry.ID).UpdateColumn("name", newName)
	if tmp.Error != nil {
		return goutils.NewSQLError(
			fmt.Sprintf("failed to update workspace %s name", workspaceID), tmp.Error, true,
		)
	}

	// Record workspace rename event
	if _, err := c.defineNewSystemEvent(
		ctx,
		models.SystemEventTypeRenameWorkspace,
		&models.SystemEventRenameWorkspace{
			WorkspaceID:      entry.ID,
			OldWorkspaceName: oldName,
			NewWorkspaceName: newName,
		},
	); err != nil {
		return goutils.NewSQLError(
			fmt.Sprintf("failed to record workspace %s rename system event", workspaceID), err, true,
		)
	}

	return nil
}

/*
UpdateWorkspaceDescription change a workspace's description

	@param ctx context.Context - execution context
	@param workspaceID string - workspace ID
	@param newDescription *string - the new description, nil to clear it
*/
func (c *databaseImpl) UpdateWorkspaceDescription(
	_ context.Context, workspaceID string, newDescription *string,
) error {
	entry, err := c.getWorkspaceDBEntry(workspaceID)
	if err != nil {
		return err
	}

	tmp := c.db.
		Model(&WorkspaceEntry{}).
		Where("id = ?", entry.ID).
		UpdateColumn("description", newDescription)
	if tmp.Error != nil {
		return goutils.NewSQLError(
			fmt.Sprintf("failed to update workspace %s description", workspaceID), tmp.Error, true,
		)
	}

	return nil
}

/*
UpdateWorkspaceVolumeMeta change a workspace's persistent volume provisioning metadata.

Refused unless the workspace has no volume (`VolumeState = NONE`). The metadata is only ever
read when the volume is provisioned (see DESIGN §4.2), so editing it while a volume is live
would leave the record describing provisioning parameters the existing volume was never
created with — and nothing re-provisions to reconcile the two.

	@param ctx context.Context - execution context
	@param workspaceID string - workspace ID
	@param newMetadata *models.WorkspaceVolumeMetadata - the new volume metadata, nil to clear
	    it and take the deployment's default provisioning parameters
*/
func (c *databaseImpl) UpdateWorkspaceVolumeMeta(
	_ context.Context, workspaceID string, newMetadata *models.WorkspaceVolumeMetadata,
) error {
	entry, err := c.getWorkspaceDBEntry(workspaceID)
	if err != nil {
		return err
	}

	// The metadata describes how to provision a volume, so it is only editable while there is
	// no volume to contradict it.
	if entry.VolumeState != models.WorkspaceVolumeStateNone {
		return goutils.NewConsistencyError(
			fmt.Sprintf(
				"workspace %s already has a persistent volume ('%s' is '%s'); "+
					"its volume metadata can no longer be changed",
				workspaceID, entry.VolumeName, entry.VolumeState,
			), nil, true,
		)
	}

	newColumn, err := c.buildVolumeMetadataColumn(newMetadata)
	if err != nil {
		return goutils.NewValidationError(
			fmt.Sprintf("workspace %s new volume metadata is not valid", workspaceID), err, true,
		)
	}

	tmp := c.db.
		Model(&WorkspaceEntry{}).
		Where("id = ?", entry.ID).
		UpdateColumn("volume_metadata", newColumn)
	if tmp.Error != nil {
		return goutils.NewSQLError(
			fmt.Sprintf("failed to update workspace %s volume metadata", workspaceID),
			tmp.Error,
			true,
		)
	}

	return nil
}

// updateWorkspaceVolumeState update a workspace's persistent volume state
func (c *databaseImpl) updateWorkspaceVolumeState(
	ctx context.Context, workspaceID string, nextState models.WorkspaceVolumeStateENUM,
) error {
	entry, err := c.getWorkspaceDBEntry(workspaceID)
	if err != nil {
		return err
	}

	if err := entry.ValidVolumeNextState(nextState); err != nil {
		return goutils.NewConsistencyError(
			fmt.Sprintf(
				"workspace %s volume can't transition to '%s'", workspaceID, nextState,
			), err, true,
		)
	}

	tmp := c.db.
		Model(&WorkspaceEntry{}).
		Where("id = ?", entry.ID).
		UpdateColumn("volume_state", nextState)
	if tmp.Error != nil {
		return goutils.NewSQLError(
			fmt.Sprintf(
				"failed to record workspace %s volume with new state '%s'", workspaceID, nextState,
			), tmp.Error, true,
		)
	}

	// Record workspace volume state change event
	if _, err := c.defineNewSystemEvent(
		ctx,
		models.SystemEventTypeWorkspaceVolumeState,
		&models.SystemEventWorkspaceVolumeState{
			WorkspaceID:   entry.ID,
			WorkspaceName: entry.Name,
			VolumeName:    entry.VolumeName,
			NewState:      nextState,
		},
	); err != nil {
		return goutils.NewSQLError(
			fmt.Sprintf(
				"failed to record workspace %s volume change state to '%s' system event",
				workspaceID, nextState,
			), err, true,
		)
	}

	return nil
}

/*
MarkWorkspaceVolumeReady record that the workspace's persistent volume exists and is
mountable.

Written only AFTER Docker has actually created the volume (see DESIGN §4.2), and by the
volume-state reconciliation when it adopts a volume that exists in Docker but is recorded as
`NONE` (see DESIGN §8.2.2).

	@param ctx context.Context - execution context
	@param workspaceID string - workspace ID
*/
func (c *databaseImpl) MarkWorkspaceVolumeReady(ctx context.Context, workspaceID string) error {
	return c.updateWorkspaceVolumeState(ctx, workspaceID, models.WorkspaceVolumeStateReady)
}

/*
MarkWorkspaceVolumeNone record that the workspace has no persistent volume.

Written only AFTER Docker has actually removed the volume (see DESIGN §4.2), and by the
volume-state reconciliation when a volume recorded as `READY` has vanished from Docker (see
DESIGN §8.2.2).

	@param ctx context.Context - execution context
	@param workspaceID string - workspace ID
*/
func (c *databaseImpl) MarkWorkspaceVolumeNone(ctx context.Context, workspaceID string) error {
	return c.updateWorkspaceVolumeState(ctx, workspaceID, models.WorkspaceVolumeStateNone)
}

/*
DeleteWorkspace delete a workspace entry, cascading to its artifact rows.

Refused unless the workspace's persistent volume is already gone (`VolumeState = NONE`) —
deleting the row otherwise would orphan the Docker volume, since the volume name is
ID-derived and reconciliation could never adopt it without a row (see DESIGN §4.3).

No object-store interaction: the objects the deleted artifact rows referenced are left in the
store and reclaimed later by the object-reaping GC (see DESIGN §4.1, §8.2.1).

	@param ctx context.Context - execution context
	@param workspaceID string - workspace ID
*/
func (c *databaseImpl) DeleteWorkspace(ctx context.Context, workspaceID string) error {
	entry, err := c.getWorkspaceDBEntry(workspaceID)
	if err != nil {
		return err
	}

	// Teardown is bottom-up: the volume must be gone before its workspace record can be. A
	// workspace row is the only thing that can identify (and later adopt) its ID-derived
	// volume, so removing the row while the volume lives would strand it in Docker.
	if entry.VolumeState != models.WorkspaceVolumeStateNone {
		return goutils.NewConsistencyError(
			fmt.Sprintf(
				"workspace %s still has a persistent volume ('%s' is '%s'); delete the volume first",
				workspaceID, entry.VolumeName, entry.VolumeState,
			), nil, true,
		)
	}

	// The artifact rows go with it through the ArtifactEntry ON DELETE CASCADE constraint.
	tmp := c.db.Where("id = ?", entry.ID).Delete(&WorkspaceEntry{})
	if tmp.Error != nil {
		return goutils.NewSQLError(
			fmt.Sprintf("failed to delete workspace %s", workspaceID), tmp.Error, true,
		)
	}

	// Record delete workspace event. The audit entry is independent of the workspace it
	// describes, so it outlives the deleted row.
	if _, err := c.defineNewSystemEvent(
		ctx,
		models.SystemEventTypeDeleteWorkspace,
		&models.SystemEventDeleteWorkspace{WorkspaceID: entry.ID, WorkspaceName: entry.Name},
	); err != nil {
		return goutils.NewSQLError(
			fmt.Sprintf("failed to record delete workspace %s system event", workspaceID), err, true,
		)
	}

	return nil
}

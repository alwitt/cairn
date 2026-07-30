// Package workspace - workspace management code that operates the persistence layer
// and external resources such as named volumes
package workspace

import (
	"context"
	"fmt"

	"github.com/alwitt/cairn/db"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/apex/log"
	"github.com/go-playground/validator/v10"
)

// Manager workspace manager
type Manager interface {
	/*
		DefineNewWorkspace define a new workspace

		The new workspace starts with no persistent volume (`VolumeState = NONE`); the volume
		is provisioned separately by the operator (see DESIGN §4.2).

			@param ctx context.Context - execution context
			@param name string - new workspace name
			@param description *string - optionally, a description of the workspace
			@param activeSession db.Database - if set, this is an existing open DB persistence
			    layer transaction, and function will perform additional persistence operations
			    within it.
			@returns the new workspace entry
	*/
	DefineNewWorkspace(
		ctx context.Context, name string, description *string, activeSession db.Database,
	) (models.Workspace, error)

	/*
		GetWorkspace fetch a particular workspace and an estimated number of entities
		mounting its associated persistent volume.

			@param ctx context.Context - execution context
			@param id string - the workspace ID
			@param activeSession db.Database - if set, this is an existing open DB persistence
			    layer transaction, and function will perform additional persistence operations
			    within it.
			@returns the workspace entry
			@returns an estimate number of entities mounting its associated persistence volume
	*/
	GetWorkspace(
		ctx context.Context, id string, activeSession db.Database,
	) (models.Workspace, int, error)

	/*
		GetWorkspaceByName fetch a particular workspace by name and an estimated number of entities
		mounting its associated persistent volume.

			@param ctx context.Context - execution context
			@param name string - the workspace name
			@param activeSession db.Database - if set, this is an existing open DB persistence
			    layer transaction, and function will perform additional persistence operations
			    within it.
			@returns the workspace entry
			@returns an estimate number of entities mounting its associated persistence volume
	*/
	GetWorkspaceByName(
		ctx context.Context, name string, activeSession db.Database,
	) (models.Workspace, int, error)

	/*
		ListWorkspaces list workspaces

			@param ctx context.Context - execution context
			@param filters WorkspaceQueryFilter - query filtering conditions
			@param activeSession db.Database - if set, this is an existing open DB persistence
			    layer transaction, and function will perform additional persistence operations
			    within it.
			@returns list of workspaces
	*/
	ListWorkspaces(
		ctx context.Context, filters db.WorkspaceQueryFilter, activeSession db.Database,
	) ([]models.Workspace, error)

	/*
		UpdateWorkspaceName change a workspace's name.

			@param ctx context.Context - execution context
			@param id string - workspace ID
			@param newName string - the new workspace name
			@param activeSession db.Database - if set, this is an existing open DB persistence
			    layer transaction, and function will perform additional persistence operations
			    within it.
			@returns the workspace entry with an updated name
	*/
	UpdateWorkspaceName(
		ctx context.Context, id string, newName string, activeSession db.Database,
	) (models.Workspace, error)

	/*
		UpdateWorkspaceDescription change a workspace's description

			@param ctx context.Context - execution context
			@param id string - workspace ID
			@param newDescription *string - the new description, nil to clear it
			@param activeSession db.Database - if set, this is an existing open DB persistence
			    layer transaction, and function will perform additional persistence operations
			    within it.
			@returns the workspace entry with an updated description
	*/
	UpdateWorkspaceDescription(
		ctx context.Context, id string, newDescription *string, activeSession db.Database,
	) (models.Workspace, error)

	/*
		DeleteWorkspace delete a workspace entry, cascading to its artifact rows.

		Refused unless the workspace's persistent volume is already gone
		(`VolumeState = NONE`) — deleting the row otherwise would orphan the Docker volume,
		since the volume name is ID-derived and reconciliation could never adopt it without a
		row (see DESIGN §4.3).

		No object-store interaction: the objects the deleted artifact rows referenced are left
		in the store and reclaimed later by the object-reaping GC (see DESIGN §4.1, §8.2.1).

			@param ctx context.Context - execution context
			@param workspaceID string - workspace ID
			@param activeSession db.Database - if set, this is an existing open DB persistence
			    layer transaction, and function will perform additional persistence operations
			    within it.
	*/
	DeleteWorkspace(ctx context.Context, workspaceID string, activeSession db.Database) error
}

// managerImpl implements Manager
type managerImpl struct {
	goutils.Component

	appName string

	validator *validator.Validate

	persistence db.Client
}

// unknownVolumeMountCount the mount-count estimate reported when the number of entities
// mounting a workspace's persistent volume can't be determined.
//
// Docker itself reports `-1` for an unavailable `RefCount` (see DESIGN §4.3), so the same
// sentinel carries through. Until the manager holds a `VolumeManager` to ask, every fetch
// reports it.
const unknownVolumeMountCount = -1

/*
NewManager define a new workspace manager

	@param appName string - the per-deployment application name which namespaces this
	    deployment's persistent volumes. A workspace's volume name is derived from it (see
	    DESIGN §2.1).
	@param persistence db.Client - persistence client
	@returns the new workspace manager
*/
func NewManager(appName string, persistence db.Client) (Manager, error) {
	logTags := log.Fields{
		"package": "cairn", "module": "workspace", "component": "manager", "instance": appName,
	}

	validate := validator.New()
	if err := models.RegisterWithValidator(validate); err != nil {
		return nil, goutils.NewRuntimeError(
			"failed to install custom validation macros", err, true,
		)
	}

	// The application name lands in every workspace's volume name, so hold it to the same
	// charset the volume name must satisfy.
	if err := validate.Var(appName, "required,valid_name"); err != nil {
		return nil, goutils.NewValidationError(
			fmt.Sprintf("application name '%s' is not valid", appName), err, true,
		)
	}

	if persistence == nil {
		return nil, goutils.NewValidationError("persistence client is required", nil, true)
	}

	instance := &managerImpl{
		Component: goutils.Component{
			LogTags: logTags,
			LogTagModifiers: []goutils.LogMetadataModifier{
				goutils.ModifyLogMetadataByRestRequestParam,
			},
		},
		appName:     appName,
		validator:   validate,
		persistence: persistence,
	}

	return instance, nil
}

/*
DefineNewWorkspace define a new workspace

The new workspace starts with no persistent volume (`VolumeState = NONE`); the volume is
provisioned separately by the operator (see DESIGN §4.2).

	@param ctx context.Context - execution context
	@param name string - new workspace name
	@param description *string - optionally, a description of the workspace
	@param activeSession db.Database - if set, this is an existing open DB persistence layer
	    transaction, and function will perform additional persistence operations within it.
	@returns the new workspace entry
*/
func (m *managerImpl) DefineNewWorkspace(
	ctx context.Context, name string, description *string, activeSession db.Database,
) (models.Workspace, error) {
	var newEntry models.Workspace

	if err := db.ActiveSessionWrapper(
		ctx, activeSession, m.persistence, func(dbCtx context.Context, dbClient db.Database) error {
			var err error
			newEntry, err = dbClient.DefineNewWorkspace(dbCtx, db.NewWorkspaceParameter{
				Name: name, Description: description, AppName: m.appName,
			})
			if err != nil {
				return goutils.NewPersistenceError(
					fmt.Sprintf("failed to define new workspace '%s'", name), err, true,
				)
			}
			return nil
		},
	); err != nil {
		return models.Workspace{}, models.NewWorkspaceMangerError(
			fmt.Sprintf("failed to define new workspace '%s'", name), err, true,
		)
	}

	return newEntry, nil
}

/*
GetWorkspace fetch a particular workspace and an estimated number of entities mounting its
associated persistent volume.

	@param ctx context.Context - execution context
	@param id string - the workspace ID
	@param activeSession db.Database - if set, this is an existing open DB persistence layer
	    transaction, and function will perform additional persistence operations within it.
	@returns the workspace entry
	@returns an estimate number of entities mounting its associated persistence volume
*/
func (m *managerImpl) GetWorkspace(
	ctx context.Context, id string, activeSession db.Database,
) (models.Workspace, int, error) {
	var entry models.Workspace

	if err := db.ActiveSessionWrapper(
		ctx, activeSession, m.persistence, func(dbCtx context.Context, dbClient db.Database) error {
			var err error
			entry, err = dbClient.GetWorkspace(dbCtx, id)
			if err != nil {
				return goutils.NewPersistenceError(
					fmt.Sprintf("failed to read workspace %s", id), err, true,
				)
			}
			return nil
		},
	); err != nil {
		return models.Workspace{}, unknownVolumeMountCount, models.NewWorkspaceMangerError(
			fmt.Sprintf("failed to fetch workspace %s", id), err, true,
		)
	}

	return entry, m.estimateVolumeMountCount(ctx, entry), nil
}

/*
GetWorkspaceByName fetch a particular workspace by name and an estimated number of entities
mounting its associated persistent volume.

	@param ctx context.Context - execution context
	@param name string - the workspace name
	@param activeSession db.Database - if set, this is an existing open DB persistence layer
	    transaction, and function will perform additional persistence operations within it.
	@returns the workspace entry
	@returns an estimate number of entities mounting its associated persistence volume
*/
func (m *managerImpl) GetWorkspaceByName(
	ctx context.Context, name string, activeSession db.Database,
) (models.Workspace, int, error) {
	var entry models.Workspace

	if err := db.ActiveSessionWrapper(
		ctx, activeSession, m.persistence, func(dbCtx context.Context, dbClient db.Database) error {
			var err error
			entry, err = dbClient.GetWorkspaceByName(dbCtx, name)
			if err != nil {
				return goutils.NewPersistenceError(
					fmt.Sprintf("failed to read workspace '%s'", name), err, true,
				)
			}
			return nil
		},
	); err != nil {
		return models.Workspace{}, unknownVolumeMountCount, models.NewWorkspaceMangerError(
			fmt.Sprintf("failed to fetch workspace '%s'", name), err, true,
		)
	}

	return entry, m.estimateVolumeMountCount(ctx, entry), nil
}

// estimateVolumeMountCount estimate the number of entities currently mounting a workspace's
// persistent volume.
//
// Only Docker can answer this - the volume is mounted by client services the manager never
// launched, so the DB has no record of it (see DESIGN §4.3). Until the manager embeds a
// `VolumeManager` to ask, it reports the unavailable sentinel rather than a count it can't
// substantiate.
func (m *managerImpl) estimateVolumeMountCount(_ context.Context, _ models.Workspace) int {
	return unknownVolumeMountCount
}

/*
ListWorkspaces list workspaces

	@param ctx context.Context - execution context
	@param filters WorkspaceQueryFilter - query filtering conditions
	@param activeSession db.Database - if set, this is an existing open DB persistence layer
	    transaction, and function will perform additional persistence operations within it.
	@returns list of workspaces
*/
func (m *managerImpl) ListWorkspaces(
	ctx context.Context, filters db.WorkspaceQueryFilter, activeSession db.Database,
) ([]models.Workspace, error) {
	var entries []models.Workspace

	if err := db.ActiveSessionWrapper(
		ctx, activeSession, m.persistence, func(dbCtx context.Context, dbClient db.Database) error {
			var err error
			entries, err = dbClient.ListWorkspaces(dbCtx, filters)
			if err != nil {
				return goutils.NewPersistenceError("failed to list workspaces", err, true)
			}
			return nil
		},
	); err != nil {
		return nil, models.NewWorkspaceMangerError("failed to list workspaces", err, true)
	}

	return entries, nil
}

/*
UpdateWorkspaceName change a workspace's name.

	@param ctx context.Context - execution context
	@param id string - workspace ID
	@param newName string - the new workspace name
	@param activeSession db.Database - if set, this is an existing open DB persistence layer
	    transaction, and function will perform additional persistence operations within it.
	@returns the workspace entry with an updated name
*/
func (m *managerImpl) UpdateWorkspaceName(
	ctx context.Context, id string, newName string, activeSession db.Database,
) (models.Workspace, error) {
	var updated models.Workspace

	if err := db.ActiveSessionWrapper(
		ctx, activeSession, m.persistence, func(dbCtx context.Context, dbClient db.Database) error {
			if err := dbClient.UpdateWorkspaceName(dbCtx, id, newName); err != nil {
				return goutils.NewPersistenceError(
					fmt.Sprintf("failed to rename workspace %s to '%s'", id, newName), err, true,
				)
			}
			// Re-read within the same transaction so the returned entry reflects the update.
			var err error
			updated, err = dbClient.GetWorkspace(dbCtx, id)
			if err != nil {
				return goutils.NewPersistenceError(
					fmt.Sprintf("failed to read back renamed workspace %s", id), err, true,
				)
			}
			return nil
		},
	); err != nil {
		return models.Workspace{}, models.NewWorkspaceMangerError(
			fmt.Sprintf("failed to update workspace %s name to '%s'", id, newName), err, true,
		)
	}

	return updated, nil
}

/*
UpdateWorkspaceDescription change a workspace's description

	@param ctx context.Context - execution context
	@param id string - workspace ID
	@param newDescription *string - the new description, nil to clear it
	@param activeSession db.Database - if set, this is an existing open DB persistence layer
	    transaction, and function will perform additional persistence operations within it.
	@returns the workspace entry with an updated description
*/
func (m *managerImpl) UpdateWorkspaceDescription(
	ctx context.Context, id string, newDescription *string, activeSession db.Database,
) (models.Workspace, error) {
	var updated models.Workspace

	if err := db.ActiveSessionWrapper(
		ctx, activeSession, m.persistence, func(dbCtx context.Context, dbClient db.Database) error {
			if err := dbClient.UpdateWorkspaceDescription(dbCtx, id, newDescription); err != nil {
				return goutils.NewPersistenceError(
					fmt.Sprintf("failed to update workspace %s description", id), err, true,
				)
			}
			// Re-read within the same transaction so the returned entry reflects the update.
			var err error
			updated, err = dbClient.GetWorkspace(dbCtx, id)
			if err != nil {
				return goutils.NewPersistenceError(
					fmt.Sprintf("failed to read back updated workspace %s", id), err, true,
				)
			}
			return nil
		},
	); err != nil {
		return models.Workspace{}, models.NewWorkspaceMangerError(
			fmt.Sprintf("failed to update workspace %s description", id), err, true,
		)
	}

	return updated, nil
}

/*
DeleteWorkspace delete a workspace entry, cascading to its artifact rows.

Refused unless the workspace's persistent volume is already gone (`VolumeState = NONE`) —
deleting the row otherwise would orphan the Docker volume, since the volume name is ID-derived
and reconciliation could never adopt it without a row (see DESIGN §4.3). The persistence layer
holds that guard, since `VolumeState` is the workspace's own column.

No object-store interaction: the objects the deleted artifact rows referenced are left in the
store and reclaimed later by the object-reaping GC (see DESIGN §4.1, §8.2.1).

	@param ctx context.Context - execution context
	@param workspaceID string - workspace ID
	@param activeSession db.Database - if set, this is an existing open DB persistence layer
	    transaction, and function will perform additional persistence operations within it.
*/
func (m *managerImpl) DeleteWorkspace(
	ctx context.Context, workspaceID string, activeSession db.Database,
) error {
	if err := db.ActiveSessionWrapper(
		ctx, activeSession, m.persistence, func(dbCtx context.Context, dbClient db.Database) error {
			if err := dbClient.DeleteWorkspace(dbCtx, workspaceID); err != nil {
				return goutils.NewPersistenceError(
					fmt.Sprintf("failed to delete workspace %s", workspaceID), err, true,
				)
			}
			return nil
		},
	); err != nil {
		return models.NewWorkspaceMangerError(
			fmt.Sprintf("failed to delete workspace %s", workspaceID), err, true,
		)
	}

	return nil
}

// Package workspace - workspace management code that operates the persistence layer
// and external resources such as named volumes
package workspace

import (
	"context"
	"fmt"

	"github.com/alwitt/cairn/db"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/alwitt/goutils/runtime"
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
			@param volumeMetadata *models.WorkspaceVolumeMetadata - optionally, provisioning
			    parameters for the workspace's persistent volume. Recorded now and only read
			    when the volume is actually provisioned; nil takes the deployment's defaults.
			@param activeSession db.Database - if set, this is an existing open DB persistence
			    layer transaction, and function will perform additional persistence operations
			    within it.
			@returns the new workspace entry
	*/
	DefineNewWorkspace(
		ctx context.Context,
		name string,
		description *string,
		volumeMetadata *models.WorkspaceVolumeMetadata,
		activeSession db.Database,
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
		UpdateWorkspaceVolumeMeta change a workspace's persistent volume provisioning metadata.

		Refused unless the workspace has no volume (`VolumeState = NONE`), which surfaces as a
		`goutils.ConsistencyError`. The metadata only describes how to provision a volume (see
		DESIGN §4.2), so editing it while one is live would leave the record describing
		parameters the existing volume was never created with.

			@param ctx context.Context - execution context
			@param id string - workspace ID
			@param newMetadata *models.WorkspaceVolumeMetadata - the new volume metadata, nil to
			    clear it and take the deployment's default provisioning parameters
			@param activeSession db.Database - if set, this is an existing open DB persistence
			    layer transaction, and function will perform additional persistence operations
			    within it.
			@returns the workspace entry with updated volume metadata
	*/
	UpdateWorkspaceVolumeMeta(
		ctx context.Context,
		id string,
		newMetadata *models.WorkspaceVolumeMetadata,
		activeSession db.Database,
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

	/*
		SetupWorkspaceVolume provision a workspace's persistent volume and record that it is
		ready.

		Idempotent (see DESIGN §4.2): a volume that already exists is adopted rather than
		re-created, so this also repairs a workspace whose `VolumeState` drifted to `NONE`
		while its volume lived on (see DESIGN §8.2.2).

		The volume is created before the state column is written, so the column is only ever
		set `READY` after Docker has actually provisioned it (see DESIGN §4.2). One
		consequence of that ordering: when run inside an `activeSession`, a later rollback of
		that transaction undoes the column write but not the volume - reconciliation is what
		settles the difference.

			@param ctx context.Context - execution context
			@param workspace models.Workspace - the workspace whose volume to provision
			@param activeSession db.Database - if set, this is an existing open DB persistence
			    layer transaction, and function will perform additional persistence operations
			    within it.
	*/
	SetupWorkspaceVolume(
		ctx context.Context, workspace models.Workspace, activeSession db.Database,
	) error

	/*
		ListWorkspaceVolumes return the names of the observed persistent volumes belonging to
		this deployment.

		Selected by the `<app name>-` prefix every workspace volume name carries (see DESIGN
		§2.1), so the result may include volumes this deployment never created - an orphan
		left by a prior incarnation still matches. That is the point: it backs the
		reconciliation that spots volumes Docker holds but the DB does not (see DESIGN §8.2.2).

			@param ctx context.Context - execution context
			@returns the names of the observed volumes
	*/
	ListWorkspaceVolumes(ctx context.Context) ([]string, error)

	/*
		TeardownWorkspaceVolume delete a workspace's persistent volume and record that it is
		gone.

		Refused while any entity still mounts the volume - the Docker daemon decides that
		atomically as part of the removal, so there is no TOCTOU window (see DESIGN §4.3). A
		refusal surfaces as a `goutils.ConsistencyError` and leaves `VolumeState` untouched.

		Idempotent (see DESIGN §4.2): a volume that is already gone is not an error, so this
		also repairs a workspace whose `VolumeState` drifted to `READY` after its volume
		vanished (see DESIGN §8.2.2).

		The volume is removed before the state column is written; see `SetupWorkspaceVolume`
		for what that ordering means inside an `activeSession`.

			@param ctx context.Context - execution context
			@param workspace models.Workspace - the workspace whose volume to remove
			@param activeSession db.Database - if set, this is an existing open DB persistence
			    layer transaction, and function will perform additional persistence operations
			    within it.
	*/
	TeardownWorkspaceVolume(
		ctx context.Context, workspace models.Workspace, activeSession db.Database,
	) error
}

// managerImpl implements Manager
type managerImpl struct {
	goutils.Component

	appName string

	validator *validator.Validate

	persistence db.Client

	// volumes manages the persistent volumes backing workspaces. The manager operates it but
	// does not own its lifecycle - the caller performs `Start` and `Cleanup`.
	volumes runtime.VolumeManager

	// sidecarConfig sidecar config. Shared with the artifact operator, though volume
	// preparation reads only `Image` and `TimeoutSecs` from it.
	sidecarConfig models.ArtifactSidecarConfig

	// defineRuntime defines the container runtime the volume preparation sidecar runs in
	defineRuntime models.SystemCallDockerRuntimeFactory
}

// unknownVolumeMountCount the mount-count estimate reported when the number of entities
// mounting a workspace's persistent volume can't be determined.
//
// Docker itself reports `-1` for an unavailable `RefCount` (see DESIGN §4.3), so the same
// sentinel carries through.
const unknownVolumeMountCount = -1

/*
NewManager define a new workspace manager

	@param appName string - the per-deployment application name which namespaces this
	    deployment's persistent volumes. A workspace's volume name is derived from it (see
	    DESIGN §2.1).
	@param persistence db.Client - persistence client
	@param volumes runtime.VolumeManager - manager for the persistent volumes backing
	    workspaces. Its lifecycle is the caller's responsibility; it must already be started,
	    and this manager never tears it down.
	@param sidecarConfig models.ArtifactSidecarConfig - sidecar config. Volume preparation
	    reads only `Image` and `TimeoutSecs`; the rest describes the transfer sidecars.
	@param defineRuntime models.SystemCallDockerRuntimeFactory - defines the container runtime
	    the volume preparation sidecar runs in. Pass `runtime.NewDockerSystemCallRuntime`
	    outside of tests.
	@returns the new workspace manager
*/
func NewManager(
	appName string,
	persistence db.Client,
	volumes runtime.VolumeManager,
	sidecarConfig models.ArtifactSidecarConfig,
	defineRuntime models.SystemCallDockerRuntimeFactory,
) (Manager, error) {
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

	if volumes == nil {
		return nil, goutils.NewValidationError("volume manager is required", nil, true)
	}

	// Required rather than defaulted to `runtime.NewDockerSystemCallRuntime`, so the choice
	// of runtime driver stays explicit at the wiring site.
	if defineRuntime == nil {
		return nil, goutils.NewValidationError("container runtime factory is required", nil, true)
	}

	// Validated up front so a missing sidecar image or timeout fails here rather than at the
	// first volume provisioning.
	if err := validate.Struct(&sidecarConfig); err != nil {
		return nil, goutils.NewValidationError("sidecar config is not valid", err, true)
	}

	instance := &managerImpl{
		Component: goutils.Component{
			LogTags: logTags,
			LogTagModifiers: []goutils.LogMetadataModifier{
				goutils.ModifyLogMetadataByRestRequestParam,
			},
		},
		appName:       appName,
		validator:     validate,
		persistence:   persistence,
		volumes:       volumes,
		sidecarConfig: sidecarConfig,
		defineRuntime: defineRuntime,
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
	@param volumeMetadata *models.WorkspaceVolumeMetadata - optionally, provisioning parameters
	    for the workspace's persistent volume. Recorded now and only read when the volume is
	    actually provisioned; nil takes the deployment's defaults.
	@param activeSession db.Database - if set, this is an existing open DB persistence layer
	    transaction, and function will perform additional persistence operations within it.
	@returns the new workspace entry
*/
func (m *managerImpl) DefineNewWorkspace(
	ctx context.Context,
	name string,
	description *string,
	volumeMetadata *models.WorkspaceVolumeMetadata,
	activeSession db.Database,
) (models.Workspace, error) {
	var newEntry models.Workspace

	if err := db.ActiveSessionWrapper(
		ctx, activeSession, m.persistence, func(dbCtx context.Context, dbClient db.Database) error {
			var err error
			newEntry, err = dbClient.DefineNewWorkspace(dbCtx, db.NewWorkspaceParameter{
				Name:           name,
				Description:    description,
				AppName:        m.appName,
				VolumeMetadata: volumeMetadata,
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
// launched, so the DB has no record of it (see DESIGN §4.3).
//
// Every failure, including the volume simply not existing, reports the unavailable sentinel
// instead of propagating: this only ever decorates a workspace fetch, and a fetch must still
// answer when Docker is unreachable (see DESIGN §7.1).
func (m *managerImpl) estimateVolumeMountCount(
	ctx context.Context, workspace models.Workspace,
) int {
	logTags := m.GetLogTagsForContext(ctx)

	_, mounters, err := m.volumes.GetVolume(ctx, workspace.VolumeName)
	if err != nil {
		log.
			WithError(err).
			WithFields(logTags).
			WithField("volume", workspace.VolumeName).
			Debug("Unable to estimate volume mount count")
		return unknownVolumeMountCount
	}

	return len(mounters)
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
UpdateWorkspaceVolumeMeta change a workspace's persistent volume provisioning metadata.

Refused unless the workspace has no volume (`VolumeState = NONE`), which surfaces as a
`goutils.ConsistencyError`. The metadata only describes how to provision a volume (see DESIGN
§4.2), so editing it while one is live would leave the record describing parameters the existing
volume was never created with. The persistence layer holds that guard, since `VolumeState` is the
workspace's own column.

	@param ctx context.Context - execution context
	@param id string - workspace ID
	@param newMetadata *models.WorkspaceVolumeMetadata - the new volume metadata, nil to clear it
	    and take the deployment's default provisioning parameters
	@param activeSession db.Database - if set, this is an existing open DB persistence layer
	    transaction, and function will perform additional persistence operations within it.
	@returns the workspace entry with updated volume metadata
*/
func (m *managerImpl) UpdateWorkspaceVolumeMeta(
	ctx context.Context,
	id string,
	newMetadata *models.WorkspaceVolumeMetadata,
	activeSession db.Database,
) (models.Workspace, error) {
	var updated models.Workspace

	if err := db.ActiveSessionWrapper(
		ctx, activeSession, m.persistence, func(dbCtx context.Context, dbClient db.Database) error {
			if err := dbClient.UpdateWorkspaceVolumeMeta(dbCtx, id, newMetadata); err != nil {
				return goutils.NewPersistenceError(
					fmt.Sprintf("failed to update workspace %s volume metadata", id), err, true,
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
			fmt.Sprintf("failed to update workspace %s volume metadata", id), err, true,
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

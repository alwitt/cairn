package workspace

import (
	"context"
	"errors"
	"fmt"

	"github.com/alwitt/cairn/db"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/alwitt/goutils/runtime"
	"github.com/apex/log"
)

// ======================================================================================
// Workspace Persistent Volume Management
//
// The operator drives volume lifecycle through these calls (see DESIGN §4.2); agents never
// do. Each one settles Docker first and writes `VolumeState` only after Docker has agreed,
// so the column never claims a state Docker has not reached.

/*
findWorkspaceVolume look up a workspace's persistent volume, reporting whether it exists.

Not-found is an ordinary answer here rather than an error - both volume lifecycle calls are
idempotent, so "no such volume" is what tells them there is nothing to do.

	@param ctx context.Context - execution context
	@param workspace models.Workspace - the workspace whose volume to look up
	@returns whether the volume currently exists
*/
func (m *managerImpl) findWorkspaceVolume(
	ctx context.Context, workspace models.Workspace,
) (bool, error) {
	if _, _, err := m.volumes.GetVolume(ctx, workspace.VolumeName); err != nil {
		var notFound goutils.NotFoundError
		if errors.As(err, &notFound) {
			return false, nil
		}
		return false, goutils.NewDockerError(
			fmt.Sprintf("failed to inspect volume '%s'", workspace.VolumeName), err, true,
		)
	}
	return true, nil
}

/*
requestedVolumeSize read the capacity a workspace asked for, if any.

Absent metadata - or metadata carrying no size - leaves the request at zero, which the
volume manager reads as "no explicit capacity" and provisions with the driver's default.

	@param workspace models.Workspace - the workspace to read
	@returns the requested capacity in bytes, 0 when unspecified
*/
func requestedVolumeSize(workspace models.Workspace) int64 {
	if workspace.VolumeMetadata == nil {
		return 0
	}
	if size := workspace.VolumeMetadata.Data().SizeBytes; size != nil {
		return *size
	}
	return 0
}

/*
SetupWorkspaceVolume provision a workspace's persistent volume and record that it is ready.

Idempotent (see DESIGN §4.2): a volume that already exists is adopted rather than
re-created, so this also repairs a workspace whose `VolumeState` drifted to `NONE` while its
volume lived on (see DESIGN §8.2.2).

The volume is created before the state column is written, so the column is only ever set
`READY` after Docker has actually provisioned it (see DESIGN §4.2). One consequence of that
ordering: when run inside an `activeSession`, a later rollback of that transaction undoes
the column write but not the volume - reconciliation is what settles the difference.

	@param ctx context.Context - execution context
	@param workspace models.Workspace - the workspace whose volume to provision
	@param activeSession db.Database - if set, this is an existing open DB persistence layer
	    transaction, and function will perform additional persistence operations within it.
*/
func (m *managerImpl) SetupWorkspaceVolume(
	ctx context.Context, workspace models.Workspace, activeSession db.Database,
) error {
	logTags := m.GetLogTagsForContext(ctx)

	exists, err := m.findWorkspaceVolume(ctx, workspace)
	if err != nil {
		return models.NewWorkspaceMangerError(
			fmt.Sprintf("failed to set up workspace %s volume", workspace.ID), err, true,
		)
	}

	if !exists {
		if _, err := m.volumes.DefineVolume(
			ctx,
			runtime.ContainerVolume{
				Name: workspace.VolumeName, Size: requestedVolumeSize(workspace),
			},
			nil,
		); err != nil {
			return models.NewWorkspaceMangerError(
				fmt.Sprintf("failed to set up workspace %s volume", workspace.ID),
				goutils.NewDockerError(
					fmt.Sprintf("failed to define volume '%s'", workspace.VolumeName), err, true,
				),
				true,
			)
		}
		log.
			WithFields(logTags).
			WithField("volume", workspace.VolumeName).
			Debug("Defined workspace persistent volume")
	} else {
		// The volume outlived its `NONE` record, so adopt it instead of failing. Deleting it
		// to start clean is exactly the auto-reap DESIGN §4.2 forbids - other systems may be
		// mounting it.
		log.
			WithFields(logTags).
			WithField("volume", workspace.VolumeName).
			Debug("Adopting existing workspace persistent volume")
	}

	if err := db.ActiveSessionWrapper(
		ctx, activeSession, m.persistence, func(dbCtx context.Context, dbClient db.Database) error {
			if err := dbClient.MarkWorkspaceVolumeReady(dbCtx, workspace.ID); err != nil {
				return goutils.NewPersistenceError(
					fmt.Sprintf("failed to record workspace %s volume as ready", workspace.ID),
					err,
					true,
				)
			}
			return nil
		},
	); err != nil {
		return models.NewWorkspaceMangerError(
			fmt.Sprintf("failed to set up workspace %s volume", workspace.ID), err, true,
		)
	}

	return nil
}

/*
ListWorkspaceVolumes return the names of the observed persistent volumes belonging to this
deployment.

Selected by the `<app name>-` prefix every workspace volume name carries (see DESIGN §2.1),
so the result may include volumes this deployment never created - an orphan left by a prior
incarnation still matches. That is the point: it backs the reconciliation that spots volumes
Docker holds but the DB does not (see DESIGN §8.2.2).

	@param ctx context.Context - execution context
	@returns the names of the observed volumes
*/
func (m *managerImpl) ListWorkspaceVolumes(ctx context.Context) ([]string, error) {
	prefix := models.WorkspaceVolumeNamePrefix(m.appName)

	observed, err := m.volumes.ListVolumes(ctx, &prefix)
	if err != nil {
		return nil, models.NewWorkspaceMangerError(
			"failed to list workspace volumes",
			goutils.NewDockerError(
				fmt.Sprintf("failed to list volumes with prefix '%s'", prefix), err, true,
			),
			true,
		)
	}

	names := make([]string, 0, len(observed))
	for _, volume := range observed {
		names = append(names, volume.Name)
	}

	return names, nil
}

/*
TeardownWorkspaceVolume delete a workspace's persistent volume and record that it is gone.

Refused while any entity still mounts the volume - the Docker daemon decides that atomically
as part of the removal, so there is no TOCTOU window (see DESIGN §4.3). A refusal surfaces as
a `goutils.ConsistencyError` and leaves `VolumeState` untouched.

Idempotent (see DESIGN §4.2): a volume that is already gone is not an error, so this also
repairs a workspace whose `VolumeState` drifted to `READY` after its volume vanished (see
DESIGN §8.2.2).

The volume is removed before the state column is written; see `SetupWorkspaceVolume` for
what that ordering means inside an `activeSession`.

	@param ctx context.Context - execution context
	@param workspace models.Workspace - the workspace whose volume to remove
	@param activeSession db.Database - if set, this is an existing open DB persistence layer
	    transaction, and function will perform additional persistence operations within it.
*/
func (m *managerImpl) TeardownWorkspaceVolume(
	ctx context.Context, workspace models.Workspace, activeSession db.Database,
) error {
	logTags := m.GetLogTagsForContext(ctx)

	exists, err := m.findWorkspaceVolume(ctx, workspace)
	if err != nil {
		return models.NewWorkspaceMangerError(
			fmt.Sprintf("failed to tear down workspace %s volume", workspace.ID), err, true,
		)
	}

	if exists {
		// Never force: the daemon's refusal is the authoritative in-use gate (see DESIGN
		// §4.3), and it arrives as a `ConsistencyError` the caller can distinguish. Passing
		// it through unwrapped keeps that distinction walkable.
		if err := m.volumes.DeleteVolume(ctx, workspace.VolumeName, false); err != nil {
			return models.NewWorkspaceMangerError(
				fmt.Sprintf("failed to tear down workspace %s volume", workspace.ID), err, true,
			)
		}
		log.
			WithFields(logTags).
			WithField("volume", workspace.VolumeName).
			Debug("Deleted workspace persistent volume")
	} else {
		// The volume was already gone; the record just has not caught up yet.
		log.
			WithFields(logTags).
			WithField("volume", workspace.VolumeName).
			Debug("Workspace persistent volume already absent")
	}

	if err := db.ActiveSessionWrapper(
		ctx, activeSession, m.persistence, func(dbCtx context.Context, dbClient db.Database) error {
			if err := dbClient.MarkWorkspaceVolumeNone(dbCtx, workspace.ID); err != nil {
				return goutils.NewPersistenceError(
					fmt.Sprintf("failed to record workspace %s volume as gone", workspace.ID),
					err,
					true,
				)
			}
			return nil
		},
	); err != nil {
		return models.NewWorkspaceMangerError(
			fmt.Sprintf("failed to tear down workspace %s volume", workspace.ID), err, true,
		)
	}

	return nil
}

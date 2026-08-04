package maintenance

import (
	"context"
	"errors"
	"fmt"

	"github.com/alwitt/cairn/db"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/apex/log"
)

// ======================================================================================
// Workspace Volume State Reconciliation
//
// `VolumeState` is a DB column describing a Docker object. The service's own transitions can't
// drift - both volume lifecycle calls write the column only after Docker has agreed (see DESIGN
// §4.2) - but something outside the service can: a human `docker volume rm`, host pruning, an
// orphan left by a prior incarnation. This is what settles the difference (see DESIGN §8.2.2).

/*
readAllWorkspaces read every workspace record the sweep will reconcile.

Unpaginated by design: a deployment holds a workspace per agent workspace, a population small
enough to compare in one pass. The object-store sweep that pages (see DESIGN §8.3.1) is walking
a fundamentally larger set.

	@param ctx context.Context - execution context
	@returns every workspace record
*/
func (m *managerImpl) readAllWorkspaces(ctx context.Context) ([]models.Workspace, error) {
	var entries []models.Workspace

	if err := m.persistence.UseDatabaseInTransaction(
		ctx, func(dbCtx context.Context, dbClient db.Database) error {
			var err error
			entries, err = dbClient.ListWorkspaces(dbCtx, db.WorkspaceQueryFilter{})
			if err != nil {
				return goutils.NewPersistenceError("failed to list workspaces", err, true)
			}
			return nil
		},
	); err != nil {
		return nil, err
	}

	return entries, nil
}

/*
observeWorkspaceVolumes snapshot the persistent volumes this deployment currently holds.

One listing per sweep rather than an inspect per workspace: it costs a single round trip
whatever the workspace count, and the names left unclaimed at the end of the comparison are
exactly the volumes no workspace record accounts for.

	@param ctx context.Context - execution context
	@returns the set of observed volume names
*/
func (m *managerImpl) observeWorkspaceVolumes(ctx context.Context) (map[string]bool, error) {
	prefix := models.WorkspaceVolumeNamePrefix(m.appName)

	observed, err := m.volumes.ListVolumes(ctx, &prefix)
	if err != nil {
		return nil, goutils.NewDockerError(
			fmt.Sprintf("failed to list volumes with prefix '%s'", prefix), err, true,
		)
	}

	names := make(map[string]bool, len(observed))
	for _, volume := range observed {
		names[volume.Name] = true
	}

	return names, nil
}

/*
correctWorkspaceVolumeState write a workspace's corrected persistent volume state.

Each correction is its own transaction so one workspace's failure can't roll back another's,
and so the column write stays atomic with the audit event persistence records alongside it.

	@param ctx context.Context - execution context
	@param workspace models.Workspace - the workspace whose column to correct
	@param corrected models.WorkspaceVolumeStateENUM - the state the volume was observed in
*/
func (m *managerImpl) correctWorkspaceVolumeState(
	ctx context.Context,
	workspace models.Workspace,
	corrected models.WorkspaceVolumeStateENUM,
) error {
	return m.persistence.UseDatabaseInTransaction(
		ctx, func(dbCtx context.Context, dbClient db.Database) error {
			var err error
			if corrected == models.WorkspaceVolumeStateReady {
				err = dbClient.MarkWorkspaceVolumeReady(dbCtx, workspace.ID)
			} else {
				err = dbClient.MarkWorkspaceVolumeNone(dbCtx, workspace.ID)
			}
			if err != nil {
				return goutils.NewPersistenceError(
					fmt.Sprintf(
						"failed to correct workspace %s volume state to '%s'",
						workspace.ID, corrected,
					), err, true,
				)
			}
			return nil
		},
	)
}

/*
SyncWorkspaceVolumeStates reconcile every workspace's `VolumeState` against the volumes that
actually exist, correcting the column where the two disagree (see DESIGN §8.2.2).

Only the column is ever written. No volume is created, deleted, or otherwise touched - this
settles a record against reality, it does not impose a record onto it.

Both drift directions self-heal silently rather than being flagged as incidents: a workspace
volume is disposable scratch, not irreplaceable data (see DESIGN §0, §8.2.2). A volume observed
with no workspace record to claim it is reported but left alone.

Corrections are applied independently, so one that fails does not withhold the others. The
returned report therefore describes what actually landed even when the error is non-nil, and a
caller that halts on the error can still say what changed.

	@param ctx context.Context - execution context
	@returns what the sweep observed and corrected
*/
func (m *managerImpl) SyncWorkspaceVolumeStates(
	ctx context.Context,
) (VolumeStateSyncReport, error) {
	logTags := m.GetLogTagsForContext(ctx)

	var report VolumeStateSyncReport

	// Read the records BEFORE observing the volumes. The ordering is load-bearing: volume
	// setup creates the volume and only then writes the column (see DESIGN §4.2), so a
	// workspace provisioned between the two reads is seen as a stale `NONE` against a volume
	// that is present - which adopts, the correct outcome. Observing the volumes first would
	// instead miss that volume entirely and flap a freshly-`READY` record back to `NONE`.
	workspaces, err := m.readAllWorkspaces(ctx)
	if err != nil {
		return report, models.NewMaintenanceError(
			"failed to reconcile workspace volume states", err, true,
		)
	}

	// A failure here aborts the sweep with nothing written. There is no safe way to proceed on
	// a partial view: an unreachable daemon read as "no volumes exist" would clear every
	// `READY` record in the deployment. The sweep is level-triggered, so the next one simply
	// re-derives all of this.
	observed, err := m.observeWorkspaceVolumes(ctx)
	if err != nil {
		return report, models.NewMaintenanceError(
			"failed to reconcile workspace volume states", err, true,
		)
	}

	report.Examined = len(workspaces)

	var failures []error
	for _, workspace := range workspaces {
		exists := observed[workspace.VolumeName]

		// Claimed by this record, so it is not an orphan whatever the column says.
		delete(observed, workspace.VolumeName)

		// The two agreeing cases are deliberately left alone rather than rewritten with the
		// state they already hold. Persistence records an audit event on every volume state
		// write, so a write-regardless sweep would bury the audit trail under one non-event
		// per workspace per sweep.
		if exists == (workspace.VolumeState == models.WorkspaceVolumeStateReady) {
			continue
		}

		corrected := models.WorkspaceVolumeStateNone
		if exists {
			corrected = models.WorkspaceVolumeStateReady
		}

		if err := m.correctWorkspaceVolumeState(ctx, workspace, corrected); err != nil {
			// The record went away between the listing and the write. That is an ordinary
			// outcome of reconciling against a live system, not a failure to report.
			var notFound goutils.NotFoundError
			if errors.As(err, &notFound) {
				log.
					WithFields(logTags).
					WithField("workspace", workspace.ID).
					Debug("Workspace deleted before its volume state could be corrected")
				continue
			}

			// Keep sweeping. The corrections that do land are durable and worth keeping, and
			// the caller still sees this once every workspace has had its turn.
			log.
				WithError(err).
				WithFields(logTags).
				WithField("workspace", workspace.ID).
				WithField("volume", workspace.VolumeName).
				Error("Failed to correct workspace volume state")
			failures = append(failures, err)
			continue
		}

		log.
			WithFields(logTags).
			WithField("workspace", workspace.ID).
			WithField("volume", workspace.VolumeName).
			WithField("recorded_state", workspace.VolumeState).
			WithField("corrected_state", corrected).
			Info("Corrected workspace volume state")

		if corrected == models.WorkspaceVolumeStateReady {
			report.AdoptedReady = append(report.AdoptedReady, workspace.ID)
		} else {
			report.ClearedNone = append(report.ClearedNone, workspace.ID)
		}
	}

	// Whatever no workspace claimed. Reported, never reaped: the service can't know what else
	// mounts a volume (see DESIGN §4.2), and with no record to adopt it there is nothing to
	// reconcile it toward either.
	for name := range observed {
		log.
			WithFields(logTags).
			WithField("volume", name).
			Warn("Observed persistent volume no workspace record claims")
		report.OrphanVolumes = append(report.OrphanVolumes, name)
	}

	if len(failures) > 0 {
		return report, models.NewMaintenanceError(
			fmt.Sprintf(
				"failed to correct the volume state of %d of %d workspaces",
				len(failures), report.Examined,
			), errors.Join(failures...), true,
		)
	}

	return report, nil
}

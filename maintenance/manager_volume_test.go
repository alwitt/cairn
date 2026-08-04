package maintenance_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/alwitt/cairn/db"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/alwitt/goutils/runtime"
	"github.com/apex/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// expectTransactions arrange the transaction wrapper every DB touch routes through, counting
// the transactions opened so a test can assert corrections are not batched into one.
func expectTransactions(mocks unitTestMocks) *int {
	opened := 0
	mocks.client.EXPECT().
		UseDatabaseInTransaction(mock.Anything, mock.Anything).
		RunAndReturn(
			func(ctx context.Context, core func(context.Context, db.Database) error) error {
				opened++
				return core(ctx, mocks.database)
			},
		)
	return &opened
}

// expectWorkspaceListing arrange the unfiltered workspace read the sweep opens with.
func expectWorkspaceListing(mocks unitTestMocks, entries []models.Workspace) {
	mocks.database.EXPECT().
		ListWorkspaces(mock.Anything, db.WorkspaceQueryFilter{}).
		Return(entries, nil).
		Once()
}

// expectVolumeListing arrange the single volume snapshot the sweep takes, reporting the given
// names as observed.
func expectVolumeListing(mocks unitTestMocks, names ...string) {
	observed := make([]runtime.ContainerVolume, 0, len(names))
	for _, name := range names {
		observed = append(observed, runtime.ContainerVolume{Name: name})
	}
	mocks.volumes.EXPECT().
		ListVolumes(mock.Anything, mock.Anything).
		Return(observed, nil).
		Once()
}

/*
TestSyncWorkspaceVolumeStates validates the DB-to-Docker volume state reconciliation of DESIGN
§8.2.2 - the sweep that corrects a `VolumeState` column something outside the service made
wrong.

Two properties carry most of the weight here. The sweep only ever writes the column, never
touching a volume; and it only writes when the column and reality actually disagree.
*/
func TestSyncWorkspaceVolumeStates(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	utCtx := context.Background()

	// Case 1: a healthy deployment is left completely alone. No `MarkWorkspaceVolume*` is
	// arranged, so any write at all fails the case.
	//
	// This is not merely an optimization. Persistence records an audit event on every volume
	// state write, so a sweep that rewrote agreeing records with the state they already hold
	// would emit one non-event per workspace per sweep and bury the audit trail. Note the
	// self-transitions such a sweep would attempt are permitted by `ValidVolumeNextState` -
	// nothing downstream would reject them, which is exactly why this has to be asserted here.
	t.Run("writes nothing when every record already agrees", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)

		ready := sampleWorkspace("ready-ws", models.WorkspaceVolumeStateReady)
		none := sampleWorkspace("none-ws", models.WorkspaceVolumeStateNone)

		transactions := expectTransactions(mocks)
		expectWorkspaceListing(mocks, []models.Workspace{ready, none})
		expectVolumeListing(mocks, ready.VolumeName)

		report, err := manager.SyncWorkspaceVolumeStates(utCtx)
		assert.Nil(err)
		assert.Equal(2, report.Examined)
		assert.Empty(report.AdoptedReady)
		assert.Empty(report.ClearedNone)
		assert.Empty(report.OrphanVolumes)

		// Only the workspace read opened a transaction.
		assert.Equal(1, *transactions)
	})

	// Case 2: the volume vanished from under a `READY` record - a human `docker volume rm`,
	// host pruning. The column is corrected down. A workspace volume is disposable scratch, so
	// this self-heals silently rather than being flagged as an incident.
	t.Run("clears a record whose volume vanished", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)

		entry := sampleWorkspace("stale-ready", models.WorkspaceVolumeStateReady)

		expectTransactions(mocks)
		expectWorkspaceListing(mocks, []models.Workspace{entry})
		expectVolumeListing(mocks)

		mocks.database.EXPECT().
			MarkWorkspaceVolumeNone(mock.Anything, entry.ID).
			Return(nil).
			Once()

		report, err := manager.SyncWorkspaceVolumeStates(utCtx)
		assert.Nil(err)
		assert.Equal(1, report.Examined)
		assert.Equal([]string{entry.ID}, report.ClearedNone)
		assert.Empty(report.AdoptedReady)
		assert.Empty(report.OrphanVolumes)
	})

	// Case 3: the volume outlived a `NONE` record, so it is adopted rather than deleted.
	// Leaving the record at `NONE` would permanently block volume creation for this workspace -
	// the derived name would always collide - and deleting the volume to start clean is the
	// auto-reap DESIGN §4.2 forbids.
	t.Run("adopts a volume its record does not know about", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)

		entry := sampleWorkspace("stale-none", models.WorkspaceVolumeStateNone)

		expectTransactions(mocks)
		expectWorkspaceListing(mocks, []models.Workspace{entry})
		expectVolumeListing(mocks, entry.VolumeName)

		mocks.database.EXPECT().
			MarkWorkspaceVolumeReady(mock.Anything, entry.ID).
			Return(nil).
			Once()

		report, err := manager.SyncWorkspaceVolumeStates(utCtx)
		assert.Nil(err)
		assert.Equal(1, report.Examined)
		assert.Equal([]string{entry.ID}, report.AdoptedReady)
		assert.Empty(report.ClearedNone)
		assert.Empty(report.OrphanVolumes)
	})

	// Case 4: both drift directions and both agreeing cases in one sweep, each landing in its
	// own bucket. This is what pins the correction to the right direction - a sweep that
	// corrected every disagreement the same way would still pass cases 2 and 3 individually.
	t.Run("corrects both drift directions in one sweep", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)

		staleReady := sampleWorkspace("stale-ready", models.WorkspaceVolumeStateReady)
		staleNone := sampleWorkspace("stale-none", models.WorkspaceVolumeStateNone)
		liveReady := sampleWorkspace("live-ready", models.WorkspaceVolumeStateReady)
		liveNone := sampleWorkspace("live-none", models.WorkspaceVolumeStateNone)

		transactions := expectTransactions(mocks)
		expectWorkspaceListing(
			mocks, []models.Workspace{staleReady, staleNone, liveReady, liveNone},
		)
		expectVolumeListing(mocks, staleNone.VolumeName, liveReady.VolumeName)

		mocks.database.EXPECT().
			MarkWorkspaceVolumeNone(mock.Anything, staleReady.ID).
			Return(nil).
			Once()
		mocks.database.EXPECT().
			MarkWorkspaceVolumeReady(mock.Anything, staleNone.ID).
			Return(nil).
			Once()

		report, err := manager.SyncWorkspaceVolumeStates(utCtx)
		assert.Nil(err)
		assert.Equal(4, report.Examined)
		assert.Equal([]string{staleNone.ID}, report.AdoptedReady)
		assert.Equal([]string{staleReady.ID}, report.ClearedNone)
		assert.Empty(report.OrphanVolumes)

		// One transaction for the listing plus one per correction - the two corrections are
		// not batched, so neither can roll the other back.
		assert.Equal(3, *transactions)
	})

	// Case 5: a volume no workspace record claims is reported and left in place. `DeleteVolume`
	// is not arranged, so reaping it fails the case - the service cannot know what else mounts
	// a volume (DESIGN §4.2), and with no record there is nothing to adopt it either.
	t.Run("reports an unclaimed volume without deleting it", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)

		entry := sampleWorkspace("live-ws", models.WorkspaceVolumeStateReady)
		orphan := models.WorkspaceVolumeName(unitTestAppName, "00000000-0000-0000-0000-00000000dead")

		expectTransactions(mocks)
		expectWorkspaceListing(mocks, []models.Workspace{entry})
		expectVolumeListing(mocks, entry.VolumeName, orphan)

		report, err := manager.SyncWorkspaceVolumeStates(utCtx)
		assert.Nil(err)
		assert.Equal(1, report.Examined)
		assert.Equal([]string{orphan}, report.OrphanVolumes)
		assert.Empty(report.AdoptedReady)
		assert.Empty(report.ClearedNone)
	})

	// Case 6: with no records at all, every observed volume is unclaimed. This also proves a
	// claimed volume is removed from the unclaimed set rather than merely skipped - case 5's
	// single orphan could otherwise be produced by never claiming anything.
	t.Run("reports every volume when no records exist", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)

		first := models.WorkspaceVolumeName(unitTestAppName, "00000000-0000-0000-0000-000000000001")
		second := models.WorkspaceVolumeName(unitTestAppName, "00000000-0000-0000-0000-000000000002")

		expectTransactions(mocks)
		expectWorkspaceListing(mocks, []models.Workspace{})
		expectVolumeListing(mocks, first, second)

		report, err := manager.SyncWorkspaceVolumeStates(utCtx)
		assert.Nil(err)
		assert.Equal(0, report.Examined)
		assert.ElementsMatch([]string{first, second}, report.OrphanVolumes)
	})

	// Case 7: an empty deployment is a clean, empty sweep rather than an error.
	t.Run("empty deployment reconciles to nothing", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)

		expectTransactions(mocks)
		expectWorkspaceListing(mocks, []models.Workspace{})
		expectVolumeListing(mocks)

		report, err := manager.SyncWorkspaceVolumeStates(utCtx)
		assert.Nil(err)
		assert.Equal(0, report.Examined)
		assert.Empty(report.AdoptedReady)
		assert.Empty(report.ClearedNone)
		assert.Empty(report.OrphanVolumes)
	})

	// Case 8: volumes are found by the deployment's `<app name>-` prefix. A wrong prefix would
	// observe nothing and read as "every volume has vanished", clearing the whole deployment.
	t.Run("lists volumes by the application name prefix", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)
		expectedPrefix := fmt.Sprintf("%s-", unitTestAppName)

		expectTransactions(mocks)
		expectWorkspaceListing(mocks, []models.Workspace{})

		mocks.volumes.EXPECT().
			ListVolumes(mock.Anything, &expectedPrefix).
			Return([]runtime.ContainerVolume{}, nil).
			Once()

		_, err := manager.SyncWorkspaceVolumeStates(utCtx)
		assert.Nil(err)
	})

	// Case 9: the records are read BEFORE the volumes are observed, and the ordering is
	// load-bearing. Volume setup creates the volume and only then writes the column, so a
	// workspace provisioned between the two reads must be seen as a stale `NONE` against a
	// present volume - which adopts. Observing volumes first would miss that volume and flap a
	// freshly-`READY` record straight back to `NONE`.
	t.Run("reads records before observing volumes", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)

		var order []string

		expectTransactions(mocks)
		mocks.database.EXPECT().
			ListWorkspaces(mock.Anything, mock.Anything).
			RunAndReturn(
				func(context.Context, db.WorkspaceQueryFilter) ([]models.Workspace, error) {
					order = append(order, "records")
					return []models.Workspace{}, nil
				},
			).
			Once()
		mocks.volumes.EXPECT().
			ListVolumes(mock.Anything, mock.Anything).
			RunAndReturn(
				func(context.Context, *string) ([]runtime.ContainerVolume, error) {
					order = append(order, "volumes")
					return []runtime.ContainerVolume{}, nil
				},
			).
			Once()

		_, err := manager.SyncWorkspaceVolumeStates(utCtx)
		assert.Nil(err)
		assert.Equal([]string{"records", "volumes"}, order)
	})

	// Case 10: an unreachable daemon aborts the sweep with nothing written. This is the most
	// dangerous failure in the file - reading a failed listing as "no volumes exist" would
	// clear every `READY` record in the deployment. No `MarkWorkspaceVolume*` is arranged, so
	// any write fails the case.
	t.Run("aborts without writing when the volume listing fails", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)
		dockerFailure := fmt.Errorf("docker is unreachable")

		expectTransactions(mocks)
		expectWorkspaceListing(
			mocks, []models.Workspace{sampleWorkspace("live-ws", models.WorkspaceVolumeStateReady)},
		)

		mocks.volumes.EXPECT().
			ListVolumes(mock.Anything, mock.Anything).
			Return(nil, dockerFailure).
			Once()

		report, err := manager.SyncWorkspaceVolumeStates(utCtx)
		assert.NotNil(err)
		assert.Equal(0, report.Examined)

		// The daemon failure stays walkable, so the maintenance loop can tell a transient
		// external fault from a DB one and decline to halt on it (see DESIGN §8.3.2).
		var maintenanceErr models.MaintenanceError
		assert.ErrorAs(err, &maintenanceErr)
		var dockerErr goutils.DockerError
		assert.ErrorAs(err, &dockerErr)
		assert.ErrorIs(err, dockerFailure)
	})

	// Case 11: a failed record read aborts before Docker is ever asked. `ListVolumes` is not
	// arranged, so touching Docker fails the case.
	t.Run("aborts before observing volumes when the record read fails", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)
		dbFailure := fmt.Errorf("database is unreachable")

		expectTransactions(mocks)
		mocks.database.EXPECT().
			ListWorkspaces(mock.Anything, mock.Anything).
			Return(nil, dbFailure).
			Once()

		report, err := manager.SyncWorkspaceVolumeStates(utCtx)
		assert.NotNil(err)
		assert.Equal(0, report.Examined)
		assert.ErrorIs(err, dbFailure)
	})

	// Case 12: one workspace failing to correct does not withhold the others. The sweep is
	// level-triggered so the failure is re-derived next run, but the corrections that did land
	// are durable and the report still names them.
	t.Run("keeps correcting after one workspace fails", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)

		doomed := sampleWorkspace("doomed-ws", models.WorkspaceVolumeStateReady)
		healthy := sampleWorkspace("healthy-ws", models.WorkspaceVolumeStateReady)
		writeFailure := fmt.Errorf("row is locked")

		expectTransactions(mocks)
		expectWorkspaceListing(mocks, []models.Workspace{doomed, healthy})
		expectVolumeListing(mocks)

		mocks.database.EXPECT().
			MarkWorkspaceVolumeNone(mock.Anything, doomed.ID).
			Return(writeFailure).
			Once()
		mocks.database.EXPECT().
			MarkWorkspaceVolumeNone(mock.Anything, healthy.ID).
			Return(nil).
			Once()

		report, err := manager.SyncWorkspaceVolumeStates(utCtx)
		assert.NotNil(err)
		assert.ErrorIs(err, writeFailure)

		// The report describes what actually landed, so a caller that halts on the error can
		// still say what changed.
		assert.Equal(2, report.Examined)
		assert.Equal([]string{healthy.ID}, report.ClearedNone)
	})

	// Case 13: a workspace deleted between the listing and its correction is an ordinary
	// outcome of reconciling against a live system, not a failure. Reporting it would make an
	// unattended sweep raise an alarm every time an operator deletes a workspace at the wrong
	// moment - and under DESIGN §8.3.2's failure policy, alarms halt the loop.
	t.Run("tolerates a record deleted mid-sweep", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)

		vanished := sampleWorkspace("vanished-ws", models.WorkspaceVolumeStateReady)
		healthy := sampleWorkspace("healthy-ws", models.WorkspaceVolumeStateReady)

		expectTransactions(mocks)
		expectWorkspaceListing(mocks, []models.Workspace{vanished, healthy})
		expectVolumeListing(mocks)

		mocks.database.EXPECT().
			MarkWorkspaceVolumeNone(mock.Anything, vanished.ID).
			Return(goutils.NewNotFoundError("workspace does not exist", nil, true)).
			Once()
		mocks.database.EXPECT().
			MarkWorkspaceVolumeNone(mock.Anything, healthy.ID).
			Return(nil).
			Once()

		report, err := manager.SyncWorkspaceVolumeStates(utCtx)
		assert.Nil(err)
		assert.Equal(2, report.Examined)

		// Not counted as a correction - nothing was corrected.
		assert.Equal([]string{healthy.ID}, report.ClearedNone)
	})
}

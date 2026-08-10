package maintenance_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/alwitt/cairn/maintenance"
	mockmaintenance "github.com/alwitt/cairn/mocks/maintenance"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	taskingModel "github.com/alwitt/tasking/models"
	"github.com/apex/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// ======================================================================================
// Operator test harness

// newUnitTestOperator build an Operator over a mocked maintenance manager. No expectations are
// set, so a sweep a case did not arrange for fails that case - which is how every halt case
// below asserts that the sweeps after the halt never ran.
func newUnitTestOperator(t *testing.T) (maintenance.Operator, *mockmaintenance.Manager) {
	assert := assert.New(t)

	manager := mockmaintenance.NewManager(t)

	operator, err := maintenance.NewOperator(unitTestAppName, manager)
	assert.Nil(err)
	assert.NotNil(operator)

	return operator, manager
}

// unitTestIterationTime the instant an iteration is driven at. Distinct from
// `unitTestSweepTime`, since what these cases check is that whatever the caller passed is what
// reaches the sweeps.
var unitTestIterationTime = time.Date(2026, time.July, 2, 4, 15, 0, 0, time.UTC)

// expectVolumeSweep arrange the volume state reconciliation, returning the given outcome.
func expectVolumeSweep(
	manager *mockmaintenance.Manager, report maintenance.VolumeStateSyncReport, err error,
) {
	manager.EXPECT().
		SyncWorkspaceVolumeStates(mock.Anything).
		Return(report, err).
		Once()
}

// expectStagingSweep arrange the staging reap over the whole bucket at the iteration's instant.
//
// The timestamp and the nil scope are matched exactly rather than waved through: an iteration
// that reached for its own clock, or that quietly narrowed to one workspace, would fail here.
func expectStagingSweep(
	manager *mockmaintenance.Manager, report maintenance.StagingReapReport, err error,
) {
	manager.EXPECT().
		DeleteOrphanedStagingObjects(mock.Anything, unitTestIterationTime, (*string)(nil)).
		Return(report, err).
		Once()
}

// expectStorageSweep arrange the storage reconciliation over the whole bucket at the iteration's
// instant, matched as exactly as the staging one.
func expectStorageSweep(
	manager *mockmaintenance.Manager, report maintenance.StorageReconcileReport, err error,
) {
	manager.EXPECT().
		ReconcileStorageObjects(mock.Anything, unitTestIterationTime, (*string)(nil)).
		Return(report, err).
		Once()
}

// transientFailure a sweep failure of the kind the next iteration is expected to shrug off - an
// object store that was briefly unreachable, wrapped the way a sweep reports it.
func transientFailure() error {
	return models.NewMaintenanceError(
		"failed to reclaim orphaned staging objects",
		goutils.NewObjectStoreError("failed to list s3://bucket/staging/", nil, true),
		true,
	)
}

// databaseFailure a sweep failure carrying a `SQLError`, wrapped exactly as a sweep reports one:
// the persistence error nested in the manager's own, reached through a plain unwrap chain.
func databaseFailure() error {
	return models.NewMaintenanceError(
		"failed to reconcile workspace volume states",
		goutils.NewPersistenceError(
			"failed to list workspaces",
			goutils.NewSQLError("no such table: workspaces", nil, true),
			true,
		),
		true,
	)
}

// aggregatedDatabaseFailure a sweep failure with a `SQLError` reachable only across an
// `errors.Join`, which is the shape a sweep that collected several per-item failures actually
// produces. Nothing may find the DB fault in it except a walker that crosses a joined tree.
func aggregatedDatabaseFailure() error {
	return models.NewMaintenanceError(
		"failed to fully reconcile the 12 storage objects examined",
		errors.Join(
			goutils.NewObjectStoreError("failed to delete s3://bucket/store/ws/a", nil, true),
			goutils.NewPersistenceError(
				"failed to quarantine artifact 01J",
				goutils.NewSQLError("database is locked", nil, true),
				true,
			),
		),
		true,
	)
}

/*
TestNewOperator validates that a maintenance operator refuses to be built over dependencies it
cannot work with. The operator runs unattended on a timer, so a wiring mistake caught here is one
that would otherwise surface as a failed iteration in the middle of the night.
*/
func TestNewOperator(t *testing.T) {
	// Case 1: the full valid set builds.
	t.Run("accepts a valid dependency set", func(t *testing.T) {
		newUnitTestOperator(t)
	})

	// Case 2: the application name is held to the same charset everywhere else in the service
	// holds it to, so a deployment cannot be named one thing here and another below.
	t.Run("rejects an invalid application name", func(t *testing.T) {
		assert := assert.New(t)

		for _, badName := range []string{"", "has spaces", "has/slash"} {
			operator, err := maintenance.NewOperator(badName, mockmaintenance.NewManager(t))
			assert.Nil(operator)
			assert.NotNil(err, "application name '%s' should be rejected", badName)
		}
	})

	// Case 3: the manager is the only thing an iteration does anything through. There is no
	// sensible zero value for it - an operator without one has no maintenance to perform.
	t.Run("rejects a missing manager", func(t *testing.T) {
		assert := assert.New(t)

		operator, err := maintenance.NewOperator(unitTestAppName, nil)
		assert.Nil(operator)
		assert.NotNil(err)
	})
}

/*
TestPerformMaintenance validates one iteration of the maintenance loop of DESIGN §8.3.2.

Two properties carry the weight. Every reconciliation runs, whatever the one before it did - they
answer to different external systems, so a fault in one must not withhold the others. And the
return is a halt signal rather than a result: non-nil only when cairn's own database is what
failed, since that is the one fault the next iteration cannot be expected to shrug off.
*/
func TestPerformMaintenance(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	utCtx := context.Background()

	// Case 1: the happy path. All three reconciliations run, and the iteration reports that the
	// loop should keep going.
	t.Run("runs every sweep and continues", func(t *testing.T) {
		assert := assert.New(t)

		operator, manager := newUnitTestOperator(t)

		expectVolumeSweep(manager, maintenance.VolumeStateSyncReport{Examined: 3}, nil)
		expectStagingSweep(manager, maintenance.StagingReapReport{Examined: 7, Deleted: 2}, nil)
		expectStorageSweep(manager, maintenance.StorageReconcileReport{Examined: 9}, nil)

		assert.Nil(operator.PerformMaintenance(utCtx, unitTestIterationTime))
	})

	// Case 2: the timestamp the caller supplied is the one both object sweeps are judged by -
	// asserted by the exact match the two helpers arrange, since an iteration reading its own
	// clock would not produce this value. One instant for the whole pass is what stops an
	// object's fate depending on how far into a long sweep it was reached.
	//
	// The scope is nil in both, too: the periodic iteration sweeps the whole bucket, and the
	// per-workspace form exists for the on-demand endpoint rather than for this.
	t.Run("judges both object sweeps by the timestamp it was given", func(t *testing.T) {
		assert := assert.New(t)

		operator, manager := newUnitTestOperator(t)

		var stagingAt, storageAt time.Time
		expectVolumeSweep(manager, maintenance.VolumeStateSyncReport{}, nil)
		manager.EXPECT().
			DeleteOrphanedStagingObjects(mock.Anything, unitTestIterationTime, (*string)(nil)).
			RunAndReturn(
				func(_ context.Context, ts time.Time, _ *string) (
					maintenance.StagingReapReport, error,
				) {
					stagingAt = ts
					return maintenance.StagingReapReport{}, nil
				},
			).
			Once()
		manager.EXPECT().
			ReconcileStorageObjects(mock.Anything, unitTestIterationTime, (*string)(nil)).
			RunAndReturn(
				func(_ context.Context, ts time.Time, _ *string) (
					maintenance.StorageReconcileReport, error,
				) {
					storageAt = ts
					return maintenance.StorageReconcileReport{}, nil
				},
			).
			Once()

		assert.Nil(operator.PerformMaintenance(utCtx, unitTestIterationTime))
		assert.Equal(unitTestIterationTime, stagingAt)
		assert.Equal(stagingAt, storageAt)
	})

	// Case 3: a zero timestamp is refused before anything runs. No sweep is arranged, so one
	// that ran would fail the case.
	//
	// Letting it through is the quiet failure this guard exists for: it puts every grace window
	// in year one, so nothing is ever old enough to reclaim, and the iteration would do nothing
	// while reporting success for as long as the deployment lives.
	t.Run("rejects a zero timestamp before sweeping", func(t *testing.T) {
		assert := assert.New(t)

		operator, _ := newUnitTestOperator(t)

		err := operator.PerformMaintenance(utCtx, time.Time{})
		assert.NotNil(err)

		var validationErr goutils.ValidationError
		assert.True(errors.As(err, &validationErr))
	})

	// Case 4: an object store fault in the first sweep withholds neither of the others, and the
	// loop is told to continue. The sweeps answer to different external systems - the volume
	// sweep never touches the store, and the staging sweep never touches the database - so
	// there is nothing about one failing that makes another's work unsafe or pointless.
	t.Run("continues past a transient failure in the volume sweep", func(t *testing.T) {
		assert := assert.New(t)

		operator, manager := newUnitTestOperator(t)

		expectVolumeSweep(
			manager, maintenance.VolumeStateSyncReport{}, models.NewMaintenanceError(
				"failed to reconcile workspace volume states",
				goutils.NewDockerError("failed to list volumes", nil, true),
				true,
			),
		)
		expectStagingSweep(manager, maintenance.StagingReapReport{}, nil)
		expectStorageSweep(manager, maintenance.StorageReconcileReport{}, nil)

		assert.Nil(operator.PerformMaintenance(utCtx, unitTestIterationTime))
	})

	// Case 5: the same, one sweep later. The storage reconciliation is the sweep that actually
	// reclaims and quarantines, so letting a staging fault withhold it would be the costliest
	// version of this mistake.
	t.Run("continues past a transient failure in the staging sweep", func(t *testing.T) {
		assert := assert.New(t)

		operator, manager := newUnitTestOperator(t)

		expectVolumeSweep(manager, maintenance.VolumeStateSyncReport{}, nil)
		expectStagingSweep(manager, maintenance.StagingReapReport{}, transientFailure())
		expectStorageSweep(manager, maintenance.StorageReconcileReport{}, nil)

		assert.Nil(operator.PerformMaintenance(utCtx, unitTestIterationTime))
	})

	// Case 6: a failure in the last sweep still leaves the loop running, since nothing about it
	// is more fatal for arriving last.
	t.Run("continues past a transient failure in the storage sweep", func(t *testing.T) {
		assert := assert.New(t)

		operator, manager := newUnitTestOperator(t)

		expectVolumeSweep(manager, maintenance.VolumeStateSyncReport{}, nil)
		expectStagingSweep(manager, maintenance.StagingReapReport{}, nil)
		expectStorageSweep(manager, maintenance.StorageReconcileReport{}, transientFailure())

		assert.Nil(operator.PerformMaintenance(utCtx, unitTestIterationTime))
	})

	// Case 7: cairn's own database failing halts the iteration where it stands. Neither object
	// sweep is arranged, so either one running fails the case.
	//
	// The error comes back as the sweep built it rather than re-wrapped, which is what lets the
	// loop above render the origin it halted on.
	t.Run("halts immediately on a database failure", func(t *testing.T) {
		assert := assert.New(t)

		operator, manager := newUnitTestOperator(t)

		failure := databaseFailure()
		expectVolumeSweep(manager, maintenance.VolumeStateSyncReport{}, failure)

		err := operator.PerformMaintenance(utCtx, unitTestIterationTime)
		assert.Equal(failure, err)

		var sqlErr goutils.SQLError
		assert.True(errors.As(err, &sqlErr))
		assert.NotEmpty(goutils.AllDeepestErrorsWithTrace(err))
	})

	// Case 8: the shape a sweep actually produces. A sweep collects its per-item failures and
	// joins them, so the `SQLError` is reachable only by crossing a node that unwraps to a list
	// rather than to a single error.
	//
	// This is the case that would fail silently if the classification ever went back to walking
	// with `errors.Unwrap`: the DB fault would be invisible, the loop would carry on against a
	// broken database, and nothing would say so.
	t.Run("halts on a database failure reached through an aggregate", func(t *testing.T) {
		assert := assert.New(t)

		operator, manager := newUnitTestOperator(t)

		expectVolumeSweep(manager, maintenance.VolumeStateSyncReport{}, nil)
		expectStagingSweep(manager, maintenance.StagingReapReport{}, nil)

		failure := aggregatedDatabaseFailure()
		expectStorageSweep(manager, maintenance.StorageReconcileReport{Examined: 12}, failure)

		err := operator.PerformMaintenance(utCtx, unitTestIterationTime)
		assert.Equal(failure, err)

		var sqlErr goutils.SQLError
		assert.True(errors.As(err, &sqlErr))

		// Both origins survive to be logged, not just whichever one the walker reached first.
		assert.Len(goutils.AllDeepestErrorsWithTrace(err), 2)
	})

	// Case 9: a database failure mid-iteration withholds the sweeps after it, not just the
	// return value. The storage reconciliation is the one that deletes objects and quarantines
	// entries - running it against a database that just failed to answer is exactly what
	// halting is meant to prevent.
	t.Run("halts before the sweeps that follow the failure", func(t *testing.T) {
		assert := assert.New(t)

		operator, manager := newUnitTestOperator(t)

		expectVolumeSweep(manager, maintenance.VolumeStateSyncReport{}, nil)

		failure := models.NewMaintenanceError(
			"failed to reclaim orphaned staging objects",
			fmt.Errorf("wrapped [%w]", goutils.NewSQLError("connection refused", nil, true)),
			true,
		)
		expectStagingSweep(manager, maintenance.StagingReapReport{}, failure)

		assert.Equal(failure, operator.PerformMaintenance(utCtx, unitTestIterationTime))
	})
}

/*
TestProcessTaskExecution validates the adapter that lets the Task Engine drive a maintenance
iteration.

It is a thin thing, but both halves of it are load bearing: where the iteration's timestamp comes
from, and how a failure is dispositioned. Getting either wrong produces no error and no log - just
a maintenance loop that reclaims nothing, or one that retries a broken database in a tight cycle.
*/
func TestProcessTaskExecution(t *testing.T) {
	utCtx := context.Background()

	// A task defined long before this execution, which is the state any retried or delayed
	// instance is in by the time a worker picks it up.
	staleTask := taskingModel.Task{
		ID:        "01JQTASK",
		TaskName:  maintenance.MaintenanceTaskName,
		CreatedAt: time.Date(2021, time.March, 4, 5, 6, 7, 0, time.UTC),
	}
	execution := taskingModel.TaskExecution{
		ID:        "01JQEXEC",
		TaskID:    staleTask.ID,
		CreatedAt: staleTask.CreatedAt,
	}

	// Case 1: a successful iteration reports success, and the instant it is judged at is derived
	// here rather than carried on the task. Every sweep's grace window is measured back from it,
	// so a stamp fixed at submission would have a long-queued instance reclaiming against a
	// cutoff that had gone stale - and would do so silently.
	t.Run("runs an iteration at the current instant", func(t *testing.T) {
		assert := assert.New(t)

		processor, operator := newUnitTestTaskProcessor(t)

		before := time.Now().UTC()
		var observed time.Time
		operator.EXPECT().
			PerformMaintenance(mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, timestamp time.Time) error {
				observed = timestamp
				return nil
			}).
			Once()

		assert.Nil(processor.ProcessTaskExecution(utCtx, staleTask, execution))

		assert.False(observed.Before(before), "the iteration must be judged at the current instant")
		assert.NotEqual(staleTask.CreatedAt, observed, "the task's own timestamp must not be reused")
	})

	// Case 2: a failure is dispositioned non-retryable. What reaches here is a failure of cairn's
	// own database - `PerformMaintenance` absorbs everything else - and retrying that against the
	// same database within the same window achieves nothing the next iteration would not.
	t.Run("reports a failure as non-recoverable", func(t *testing.T) {
		assert := assert.New(t)

		processor, operator := newUnitTestTaskProcessor(t)

		failure := databaseFailure()
		operator.EXPECT().
			PerformMaintenance(mock.Anything, mock.Anything).
			Return(failure).
			Once()

		err := processor.ProcessTaskExecution(utCtx, staleTask, execution)
		assert.NotNil(err)

		var nonRecoverable taskingModel.NonRecoverableError
		assert.True(
			errors.As(err, &nonRecoverable),
			"expected NonRecoverableError, got %T: %v", err, err,
		)

		// The disposition must not cost the diagnosis: an operator reading the recorded failure
		// still needs to see which database call went wrong.
		var sqlErr goutils.SQLError
		assert.True(errors.As(err, &sqlErr), "the underlying cause must survive the wrapping")
		assert.Contains(err.Error(), failure.Error())
	})
}

// Package maintenance - system maintenance package
package maintenance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	taskingModel "github.com/alwitt/tasking/models"
	"github.com/apex/log"
	"github.com/go-playground/validator/v10"
)

// The names one iteration's three reconciliations are reported under. A failure log carries the
// one it came from, so an operator reading it knows which external system to go look at without
// decoding the error.
const (
	sweepWorkspaceVolumeStates = "workspace-volume-states"
	sweepStagingObjects        = "staging-objects"
	sweepStorageObjects        = "storage-objects"
)

// Operator maintenance task operations runner
type Operator interface {
	/*
		PerformMaintenance run one iteration of the system maintenance loop (see DESIGN
		§8.3.2).

		Every reconciliation the loop owns runs here, in one pass: the workspace volume states
		against docker, then the staging and storage objects against the artifact entries. They
		are independent, so each runs whatever the one before it did - the staging sweep needs
		no database at all, and the volume sweep needs no object store, so a fault in one
		external system does not withhold the work that does not depend on it.

		A NIL RETURN DOES NOT MEAN EVERY SWEEP SUCCEEDED. The return is the loop's halt
		signal, not a result: it is non-nil only when continuing to loop would be the wrong
		thing to do. A failure of cairn's own database is that case and nothing else is - a
		loop spinning against a broken or inconsistent database is far more dangerous than a
		halted one, which the orchestration system restarts loudly. Everything else - an
		unreachable object store, a docker daemon that went away - is logged in full and left
		for the next iteration to re-derive, because every decision here is taken from durable
		state rather than from anything this iteration remembers.

			@param ctx context.Context - execution context
			@param timestamp time.Time - the current timestamp. One instant judges the whole
			    iteration: it is what each sweep's grace window is measured back from, so an
			    object's fate does not depend on how far into a long pass it was reached.
	*/
	PerformMaintenance(ctx context.Context, timestamp time.Time) error
}

// operatorImpl implements Operator
type operatorImpl struct {
	goutils.Component

	appName string

	validator *validator.Validate

	manager Manager
}

/*
NewOperator define a new system maintenance operator

	@param appName string - the per-deployment application name
	@param manager Manager - the core maintenance manager the reconciliations are built on
	@returns the new system maintenance operator
*/
func NewOperator(appName string, manager Manager) (Operator, error) {
	logTags := log.Fields{
		"package": "cairn", "module": "maintenance", "component": "operator", "instance": appName,
	}

	validate := validator.New()
	if err := models.RegisterWithValidator(validate); err != nil {
		return nil, goutils.NewRuntimeError(
			"failed to install custom validation macros", err, true,
		)
	}

	if err := validate.Var(appName, "required,valid_name"); err != nil {
		return nil, goutils.NewValidationError(
			fmt.Sprintf("application name '%s' is not valid", appName), err, true,
		)
	}

	if manager == nil {
		return nil, goutils.NewValidationError("maintenance manager is required", nil, true)
	}

	instance := &operatorImpl{
		Component: goutils.Component{
			LogTags: logTags,
			LogTagModifiers: []goutils.LogMetadataModifier{
				goutils.ModifyLogMetadataByRestRequestParam,
			},
		},
		appName:   appName,
		validator: validate,
		manager:   manager,
	}

	return instance, nil
}

/*
PerformMaintenance run one iteration of the system maintenance loop (see DESIGN §8.3.2).

Every reconciliation the loop owns runs here, in one pass. They are independent, so each runs
whatever the one before it did.

A nil return does not mean every sweep succeeded - it means the loop should keep going. See the
interface doc for why cairn's own database failing is the only thing that stops an iteration.

	@param ctx context.Context - execution context
	@param timestamp time.Time - the current timestamp, which every sweep's grace window is
	    measured back from
*/
func (o *operatorImpl) PerformMaintenance(ctx context.Context, timestamp time.Time) error {
	logTags := o.GetLogTagsForContext(ctx)

	// A zero timestamp fails silently rather than loudly if it is let through: it puts every
	// grace window's cutoff in year one, so nothing is ever old enough to reclaim and no entry
	// is ever settled enough to quarantine. The iteration would do nothing at all and report
	// success for as long as the deployment lives.
	if timestamp.IsZero() {
		return goutils.NewValidationError("maintenance timestamp is required", nil, true)
	}

	var volumes VolumeStateSyncReport
	var staging StagingReapReport
	var storage StorageReconcileReport

	// Deferred so it is emitted on the halt path too, describing what landed before the
	// iteration gave up. The manager logs each individual correction, orphan volume, and
	// quarantine; this is the one line that shows the loop is alive without turning on `Debug`.
	defer func() {
		log.
			WithFields(logTags).
			WithField("volumes_examined", volumes.Examined).
			WithField("volumes_adopted", len(volumes.AdoptedReady)).
			WithField("volumes_cleared", len(volumes.ClearedNone)).
			WithField("volumes_orphaned", len(volumes.OrphanVolumes)).
			WithField("staging_examined", staging.Examined).
			WithField("staging_deleted", staging.Deleted).
			WithField("storage_examined", storage.Examined).
			WithField("storage_deleted", storage.Deleted).
			WithField("artifacts_flagged", len(storage.FlaggedMissing)).
			Info("Completed maintenance iteration")
	}()

	// First because it is the only sweep needing neither the object store nor the artifact
	// entries. A deployment whose object store is unreachable still gets its volume states
	// settled.
	var err error
	volumes, err = o.manager.SyncWorkspaceVolumeStates(ctx)
	if err != nil && o.reportSweepFailure(ctx, sweepWorkspaceVolumeStates, err) {
		return err
	}

	// The whole bucket, not one workspace. The scoped form exists for the on-demand operator
	// endpoint (see DESIGN §7.1); the periodic iteration has no reason to leave anything out.
	staging, err = o.manager.DeleteOrphanedStagingObjects(ctx, timestamp, nil)
	if err != nil && o.reportSweepFailure(ctx, sweepStagingObjects, err) {
		return err
	}

	storage, err = o.manager.ReconcileStorageObjects(ctx, timestamp, nil)
	if err != nil && o.reportSweepFailure(ctx, sweepStorageObjects, err) {
		return err
	}

	return nil
}

/*
reportSweepFailure log a sweep's failure, and report whether it must stop the iteration.

	@param ctx context.Context - execution context
	@param sweep string - which reconciliation failed
	@param err error - what it failed with
	@returns whether the iteration must halt
*/
func (o *operatorImpl) reportSweepFailure(ctx context.Context, sweep string, err error) bool {
	logTags := o.GetLogTagsForContext(ctx)

	entry := log.WithError(err).WithFields(logTags).WithField("sweep", sweep)

	// A sweep aggregates its per-item failures, so there is rarely a single origin to point at.
	// Render every one of them: naming one arbitrary victim and dropping the rest is precisely
	// what made these unreadable before the error walker learned to cross a joined tree.
	if traced := goutils.AllDeepestErrorsWithTrace(err); len(traced) > 0 {
		entry.Errorf(
			"Maintenance sweep '%s' failed with:\n%s", sweep, goutils.PrintErrorsWithTrace(traced),
		)
	} else {
		entry.Errorf("Maintenance sweep '%s' failed", sweep)
	}

	// The one fault the next iteration cannot be expected to shrug off (see DESIGN §8.3.2).
	// Note this reads a `SQLError`, which the persistence layer raises on a failed statement -
	// a transaction that fails to COMMIT surfaces the driver's own error instead and would be
	// treated as transient here.
	var sqlErr goutils.SQLError
	return errors.As(err, &sqlErr)
}

// taskingWrapper a basic wrapper around the Operator which can be handed over to the
// tasking Task Engine for processing maintenance tasks
type taskingWrapper struct {
	core Operator
}

/*
ProcessTaskExecution process a task specific to this processor

Runs one iteration of the maintenance loop.

The timestamp is taken here rather than carried on the task. Every sweep's grace window is
measured back from it, and an execution instance can be enqueued well before a worker picks it
up - longer still when the instance is a retry - so a stamp fixed at submission would judge the
iteration against a cutoff that had since gone stale. Deriving it at execution is what keeps
the sweep level-triggered: what it reclaims follows from the state it finds, not from when it
was asked for.

A failure is reported as non-recoverable, which opts it out of the task's retry policy. That is
not a claim that maintenance cannot be retried - it follows from what `PerformMaintenance` returns.
Everything a later attempt could plausibly get past is already absorbed there and logged: an
unreachable object store, a docker daemon that went away. What reaches here is a failure of
cairn's own database, which is the one fault the loop is not meant to spin against (see DESIGN
§8.3.2). Note this is a weaker remedy than the halt that section describes - the failure is
recorded against the task rather than taking the process down, and the next scheduled iteration
still fires, re-deriving the whole sweep from durable state.

	@param ctx context.Context - execution context
	@param taskEntry Task - task entry
	@param executeEntry TaskExecution - task execution instance
*/
func (t taskingWrapper) ProcessTaskExecution(
	ctx context.Context, taskEntry taskingModel.Task, executeEntry taskingModel.TaskExecution,
) error {
	if err := t.core.PerformMaintenance(ctx, time.Now().UTC()); err != nil {
		return taskingModel.NewNonRecoverableError(
			fmt.Sprintf(
				"maintenance iteration failed during task %s execution %s",
				taskEntry.ID,
				executeEntry.ID,
			), err, true,
		)
	}

	return nil
}

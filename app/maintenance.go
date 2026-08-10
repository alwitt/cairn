// Package app - application entry points
package app //revive:disable-line:var-naming

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/alwitt/cairn/maintenance"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	goutilsRedis "github.com/alwitt/goutils/redis"
	"github.com/alwitt/goutils/runtime"
	taskingDB "github.com/alwitt/tasking/db"
	taskingModel "github.com/alwitt/tasking/models"
	"github.com/apex/log"
)

// Maintainer `cairn` data cleanup maintainer
type Maintainer interface {
	/*
		Start the maintainer and its components

			@param ctx context.Context - execution context
			@param serverErrors chan error - channel used to broadcast a fatal runtime
				failure back to the caller so it can trigger shutdown
	*/
	Start(ctx context.Context, serverErrors chan error) error

	/*
		Stop shutdown the maintainer and its components

			@param ctx context.Context - execution context
	*/
	Stop(ctx context.Context) error
}

// maintainerImpl implements Maintainer
type maintainerImpl struct {
	goutils.Component

	// parentCtx parent execution context for all running tasks
	parentCtx context.Context

	// appName the per-deployment application name
	appName string

	// taskEngineConfig the process wide Task Engine config
	taskEngineConfig models.TaskEngineConfig

	// maintenanceConfig the maintenance system config
	maintenanceConfig models.MaintenanceConfig

	// volume the persistent volume view a sweep reconciles against.
	//
	// The same object the maintenance manager was built over, held here because its lifecycle is
	// this component's to run: it holds no runtime client until Start, and the manager never
	// tears it down.
	volume runtime.VolumeManager

	// operator the maintenance operator one task execution runs an iteration of
	operator maintenance.Operator

	// taskPersistence the Task Engine's persistence client, addressing `tasking`'s own schema
	// rather than cairn's
	taskPersistence taskingDB.Client

	// redis the REDIS client backing the Task Engine's IPC queues
	redis goutilsRedis.Client

	// runner the maintenance runner; built by Start
	runner maintenance.Runner

	// trigger the maintenance execution trigger; built by Start
	trigger maintenance.Trigger
}

/*
BuildNewMaintainer build new cairn maintainer

Stands up everything a maintenance sweep is run out of except the two Task Engine components,
which Start builds - they report an unrecoverable fault through a channel that only reaches this
component at Start (see maintainerImpl.Start), so what they are built from is held on the instance
until then.

The infrastructure clients are this component's own rather than the application server's. It costs
a second connection to each, and buys a maintainer that can be lifted into its own process without
first being untangled from the server.

	@param parentCtx context.Context - parent execution context for all running tasks
	@param configs models.ApplicationConfig - server config
	@returns new maintainer
*/
func BuildNewMaintainer(
	parentCtx context.Context, configs models.ApplicationConfig,
) (Maintainer, error) {
	// ------------------------------------------------------------------------------------
	// Build infrastructure clients

	persistence, err := buildPersistenceClient(configs.Persistence.SQL.Application)
	if err != nil {
		return nil, err
	}

	s3Manager, err := buildS3ClientManager(configs.Artifact.ObjectStore)
	if err != nil {
		return nil, err
	}

	volume, err := buildVolumeManager(parentCtx, configs.Workspace.VolumeType)
	if err != nil {
		return nil, err
	}

	// The Task Engine's own persistence, which is a different database from the application's -
	// `tasking` defines and migrates that schema itself.
	taskPersistence, err := maintenance.NewTaskEnginePersistence(configs.Persistence.SQL.TaskEngine)
	if err != nil {
		return nil, models.NewBootStrapError(
			"Failed to prepare Task Engine DB persistence client", err, true,
		)
	}

	redisClient, err := goutilsRedis.NewClient(parentCtx, configs.Persistence.Redis.ToStandard())
	if err != nil {
		return nil, models.NewBootStrapError("Failed to prepare REDIS client", err, true)
	}

	// ------------------------------------------------------------------------------------
	// Build core

	// The volume manager is handed over unstarted; nothing reads through it until the first
	// sweep, which is long after Start has established its runtime client.
	maintenanceMgr, err := maintenance.NewManager(
		configs.AppName,
		persistence,
		s3Manager,
		configs.Artifact.Storage,
		configs.Maintenance,
		volume,
	)
	if err != nil {
		return nil, models.NewBootStrapError("Failed to prepare maintenance manager", err, true)
	}

	maintenanceOperator, err := maintenance.NewOperator(configs.AppName, maintenanceMgr)
	if err != nil {
		return nil, models.NewBootStrapError("Failed to prepare maintenance operator", err, true)
	}

	return &maintainerImpl{
		Component: goutils.Component{
			LogTags: log.Fields{"module": "main", "component": "maintainer"},
			LogTagModifiers: []goutils.LogMetadataModifier{
				goutils.ModifyLogMetadataByRestRequestParam,
			},
		},
		parentCtx:         parentCtx,
		appName:           configs.AppName,
		taskEngineConfig:  configs.TaskEngine,
		maintenanceConfig: configs.Maintenance,
		volume:            volume,
		operator:          maintenanceOperator,
		taskPersistence:   taskPersistence,
		redis:             redisClient,
	}, nil
}

/*
Start the maintainer and its components

The Task Engine components are built here rather than at construction. Both take the callback
invoked when one of their goroutines hits an unrecoverable fault, and that fault has to reach the
caller's shutdown path - which means the channel it is broadcast on, which arrives with this call.

The order is load bearing. The runner answers maintenance requests and the trigger raises them, so
the runner comes up first and there is somewhere for the first request to land.

	@param ctx context.Context - execution context
	@param serverErrors chan error - channel used to broadcast a fatal runtime
		failure back to the caller so it can trigger shutdown
*/
func (m *maintainerImpl) Start(ctx context.Context, serverErrors chan error) error {
	logTags := m.GetLogTagsForContext(ctx)

	// A failed Start returns no handle the caller is expected to Stop, so whatever came up
	// before the failure is brought back down here rather than left running for the life of the
	// process. The teardown is the one Stop uses, and only reaches what actually exists.
	started := false
	defer func() {
		if started {
			return
		}
		if err := m.stopComponents(ctx); err != nil {
			log.
				WithError(err).
				WithFields(goutils.UpdateCodePositionInTags(logTags)).
				Error("Failed to unwind partially started maintainer")
		}
	}()

	if err := m.volume.Start(ctx); err != nil {
		return models.NewBootStrapError(
			"Failed to start maintenance persistent volume manager", err, true,
		)
	}

	runner, err := maintenance.NewRunner(maintenance.NewRunnerParams{
		ParentCtx:   m.parentCtx,
		AppName:     m.appName,
		TaskEngine:  m.taskEngineConfig,
		Maintenance: m.maintenanceConfig,
		Operator:    m.operator,
		Persistence: m.taskPersistence,
		Redis:       m.redis,
		OnFatal:     m.reportTaskEngineFatal(serverErrors),
	})
	if err != nil {
		return models.NewBootStrapError("Failed to prepare maintenance runner", err, true)
	}
	// Held before it is started: stopping a runner that never started is a no-op, so the
	// teardown above needs no notion of how far Start got.
	m.runner = runner

	trigger, err := maintenance.NewTrigger(maintenance.NewTriggerParams{
		ParentCtx:   m.parentCtx,
		AppName:     m.appName,
		TaskEngine:  m.taskEngineConfig,
		Maintenance: m.maintenanceConfig,
		Persistence: m.taskPersistence,
		Redis:       m.redis,
	})
	if err != nil {
		return models.NewBootStrapError("Failed to prepare maintenance trigger", err, true)
	}
	// Held for the same reason, and additionally because stopping it is what releases the timer
	// context the constructor derived - whether or not the timer was ever started.
	m.trigger = trigger

	if err := m.runner.Start(ctx); err != nil {
		return models.NewBootStrapError("Failed to start maintenance runner", err, true)
	}

	if err := m.trigger.StartTimer(ctx); err != nil {
		return models.NewBootStrapError("Failed to start maintenance trigger", err, true)
	}

	started = true
	return nil
}

/*
reportTaskEngineFatal build the callback the Task Engine reports an unrecoverable fault through.

Supplied rather than left nil, which is not a neutral choice: `tasking`'s own default logs the
fault and terminates the process outright, taking down the API server mid-request and skipping
every component's shutdown. Handing the fault to the caller instead is what turns it into an
orderly shutdown.

	@param serverErrors chan error - the channel the fault is broadcast on
	@returns the fault callback
*/
func (m *maintainerImpl) reportTaskEngineFatal(serverErrors chan error) taskingModel.OnFatalCB {
	logTags := m.GetLogTagsForContext(m.parentCtx)

	return func(reporter string, err error, timestamp time.Time) {
		log.
			WithError(err).
			WithFields(goutils.UpdateCodePositionInTags(logTags)).
			WithField("reporter", reporter).
			WithField("timestamp", timestamp).
			Error("Task Engine reported an unrecoverable fault")

		/*
			Posted without blocking. The channel's buffer belongs to the caller, and an engine
			goroutine wedged on a full one would never return - it is reporting that it is
			already giving up. Nothing is lost by dropping it either: this says only that
			shutdown should begin, which whatever filled the channel has already asked for, and
			the fault itself has just been logged in full.
		*/
		select {
		case serverErrors <- models.NewMaintenanceError(
			fmt.Sprintf("Task Engine component '%s' hit an unrecoverable fault", reporter), err, true,
		):
		default:
		}
	}
}

/*
Stop shutdown the maintainer and its components

	@param ctx context.Context - execution context
*/
func (m *maintainerImpl) Stop(ctx context.Context) error {
	return m.stopComponents(ctx)
}

/*
stopComponents bring down whatever of the maintainer is running.

The reverse of Start - the trigger first, so nothing new is asked for while the runner that
answers it is going down. Every step is attempted whatever the one before it did, so a failure in
one never leaves another running, and all of them are reported rather than only the first.

Safe against a maintainer that was never started, or only partly started, which is what lets Start
use it to unwind.

	@param ctx context.Context - execution context
*/
func (m *maintainerImpl) stopComponents(ctx context.Context) error {
	var triggerErr, runnerErr error
	if m.trigger != nil {
		triggerErr = m.trigger.StopTimer(ctx)
	}
	if m.runner != nil {
		runnerErr = m.runner.Stop(ctx)
	}
	volumeErr := m.volume.Cleanup(ctx)

	// The trigger's and the runner's failures are already shutdown errors of their own; only the
	// volume manager's needs saying what it was.
	allErrors := []error{triggerErr, runnerErr}
	if volumeErr != nil {
		allErrors = append(allErrors, models.NewShutdownError(
			"Failed to stop maintenance volume manager", volumeErr, true,
		))
	}

	return errors.Join(allErrors...)
}

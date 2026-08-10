// Package maintenance - system maintenance package
package maintenance

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/alwitt/cairn/db"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	goutilsRedis "github.com/alwitt/goutils/redis"
	"github.com/alwitt/tasking"
	taskingDB "github.com/alwitt/tasking/db"
	taskingModel "github.com/alwitt/tasking/models"
	taskEngine "github.com/alwitt/tasking/task"
	"github.com/apex/log"
	"github.com/go-playground/validator/v10"
	"gorm.io/gorm/logger"
)

// MaintenanceTaskName the `tasking` task name one maintenance iteration runs under.
//
// A constant rather than config: task names must be unique across an entire engine config, and
// this one is the contract between whatever submits a maintenance request and the processor that
// answers it. A deployment has nothing to gain by renaming both halves in step.
const MaintenanceTaskName = "perform-maintenance"

// maintenanceTaskWorkers how many maintenance iterations the worker will run at once.
//
// One, and not a tunable. Every sweep is level-triggered - it re-derives what to do from durable
// state each run (see DESIGN §8.3.1) - so a second concurrent iteration would examine the same
// objects and reach the same conclusions as the first. Raising this buys parallelism over work
// that is already idempotent, at the cost of two passes racing to enact the same deletions.
const maintenanceTaskWorkers = 1

// maintenanceTaskBufferLen how many execution requests the worker buffers locally before it
// stops reading its queue. Small deliberately: a backlog of maintenance requests is redundant
// by the same level-triggered argument, so there is nothing to gain by holding a deep one.
const maintenanceTaskBufferLen = 2

/*
maintenanceTaskRetry the retry policy attached to the maintenance task.

Stated but inert, and worth saying so plainly rather than leaving the next reader to work out why
retry never appears to happen: `taskingWrapper.ProcessTaskExecution` wraps every failure in a
`NonRecoverableError`, which opts that execution out of retry entirely. What reaches it is a
failure of cairn's own database, which the following iteration is better placed to retry than
this one is.

It exists because `TaskDefinition.Retry` is `validate:"required"` - the engine insists a policy be
named whether or not the processor ever lets it apply.
*/
var maintenanceTaskRetry = taskingModel.RetryParam{InitialDelaySec: 1, MaxRetries: 0}

/*
maintenanceTaskSubmitRetry the retry policy stamped onto a submitted maintenance task.

The same policy as `maintenanceTaskRetry` in the shape a submission carries, derived from it
rather than restated so the two cannot drift. It has to be named at submission because the task
client is given no queue configs to read a policy out of - see NewTrigger for why - and what it
would otherwise fall back to is the library default of five retries, leaving every maintenance task
in the database claiming attempts `ProcessTaskExecution` can never let it have.

Factor is inert at zero retries but must still satisfy the `gte=1` the client validates it against.
*/
var maintenanceTaskSubmitRetry = taskingModel.TaskRetryParameters{
	InitialDelaySec: maintenanceTaskRetry.InitialDelaySec,
	MaxRetries:      maintenanceTaskRetry.MaxRetries,
	Factor:          1,
}

/*
terminalTaskStates the states a maintenance task is finished in.

Restated here because `tasking` keeps its own predicate unexported. These four are exactly what its
delete guard accepts, so a task in any of them can be cleaned up and a task in any other cannot.
CANCELLING is deliberately absent - a task awaiting cancellation has not stopped yet.
*/
var terminalTaskStates = []taskingModel.TaskStateENUM{
	taskingModel.TaskStateComplete,
	taskingModel.TaskStateFailed,
	taskingModel.TaskStateTimeout,
	taskingModel.TaskStateCancelled,
}

/*
terminalTaskCleanupBatch the most finished maintenance tasks one clean up pass will consider.

A cap so the first pass after a long outage reads a page rather than the whole table. Capping is
safe because the Task Engine returns these oldest first, which is the end that wants clearing, and
whatever a pass leaves behind the next one picks up.

Their order is by creation while the age out is measured on last update, so the two correspond
closely rather than exactly - a task created early but updated late could outlive its turn by a
pass. Nothing here needs to be exact, and for tasks that neither retry nor linger the two
timestamps sit close together anyway.
*/
const terminalTaskCleanupBatch = 100

// The values recorded on a maintenance task to say what asked for it. Metadata rather than
// parameters: the processor reads neither, so this exists for whoever is later reading the task
// entries and wondering why a sweep ran when it did.
const (
	// maintenanceSourceTimer the request came from the periodic sweep timer
	maintenanceSourceTimer = "timer"
	// maintenanceSourceOnDemand the request was asked for through RequestMaintenance
	maintenanceSourceOnDemand = "on-demand"
)

/*
maintenanceTaskMetadata what is recorded on a maintenance task to say what asked for it.

A struct rather than the map this shape reads like, and not by preference: the Task Engine
validates whatever metadata it is handed with go-playground, which accepts structs and nothing
else. A map is refused where it is stored rather than where it was built - at submission, on the
sweep timer's thread - so the struct is what the engine's contract actually asks for.

Serializes to the same object either way; the field tag is what the task entry carries.
*/
type maintenanceTaskMetadata struct {
	// Source what raised the request; one of the maintenanceSource values above
	Source string `json:"source" validate:"required"`
}

// Runner maintenance operations runner
type Runner interface {
	/*
		Start the maintenance runner and all its components

			@param ctx context.Context - execution context
	*/
	Start(ctx context.Context) error

	/*
		Stop the maintenance runner and all its components

			@param ctx context.Context - execution context
	*/
	Stop(ctx context.Context) error
}

// runnerImpl implements Runner
type runnerImpl struct {
	goutils.Component

	// taskScheduler task engine work scheduler
	taskScheduler taskEngine.Scheduler

	// taskReceiver task engine worker's work receiver
	taskReceiver taskEngine.Receiver
}

// ======================================================================================
// Task Engine Provisioning

/*
NewTaskEnginePersistence define the persistence client the `tasking` Task Engine runs on.

A thin wrapper around `tasking.NewPersistenceClient`, existing so the rest of cairn can stand the
engine up without importing `tasking` itself. Together with the optional factories on
NewRunnerParams, this keeps every `tasking` import inside this package.

The dialector is built with cairn's own `db.GetPostgresDialector`, the same function the
application's client is built from, so both connection strings are assembled identically and a
setting that works for one works for the other.

	@param config models.PostgresConfig - the Task Engine's SQL persistence config
	@returns the new Task Engine persistence client
*/
func NewTaskEnginePersistence(config models.PostgresConfig) (taskingDB.Client, error) {
	dialector, err := db.GetPostgresDialector(config)
	if err != nil {
		return nil, models.NewMaintenanceError(
			"failed to parse Task Engine DB persistence parameters", err, true,
		)
	}

	// Read off the config rather than pinned to one level, so turning `debugLog` on for this
	// database actually produces the ORM logs it promises.
	logLevel := logger.Error
	if config.DebugLog {
		logLevel = logger.Info
	}

	client, err := tasking.NewPersistenceClient(dialector, logLevel)
	if err != nil {
		return nil, models.NewMaintenanceError(
			"failed to define Task Engine DB persistence client", err, true,
		)
	}

	return client, nil
}

/*
TaskReceiverFactory defines the Task Engine worker's request receiver.

Injected rather than calling `tasking.NewTaskWorkerReceiver` directly, so a unit test can drive
the runner's lifecycle without a live Redis or Postgres behind it. Mirrors that function's
signature exactly, and defaults to it when left nil.

	@param parentCtx context.Context - the parent execution context of the receiver
	@param receiverConfig taskingModel.TaskReceiverConfig - task receiver config
	@param dbClient taskingDB.Client - Task Engine persistence client
	@param redisClient goutilsRedis.Client - REDIS client
	@param onFatalCB taskingModel.OnFatalCB - invoked when a queue processing goroutine hits an
	    unrecoverable fault; nil keeps the library's own log-and-terminate default
	@returns the new task receiver
*/
type TaskReceiverFactory func(
	parentCtx context.Context,
	receiverConfig taskingModel.TaskReceiverConfig,
	dbClient taskingDB.Client,
	redisClient goutilsRedis.Client,
	onFatalCB taskingModel.OnFatalCB,
) (taskEngine.Receiver, error)

/*
TaskSchedulerFactory defines the Task Engine work scheduler.

The scheduler counterpart to TaskReceiverFactory; see there for why this is injected. Mirrors
`tasking.NewTaskScheduler` exactly, and defaults to it when left nil.

	@param parentCtx context.Context - the parent execution context of the scheduler
	@param schedulerConfig taskingModel.TaskSchedulerConfig - task scheduler config
	@param dbClient taskingDB.Client - Task Engine persistence client
	@param redisClient goutilsRedis.Client - REDIS client
	@param onFatalCB taskingModel.OnFatalCB - invoked when a queue processing goroutine hits an
	    unrecoverable fault; nil keeps the library's own log-and-terminate default
	@returns the new task scheduler
*/
type TaskSchedulerFactory func(
	parentCtx context.Context,
	schedulerConfig taskingModel.TaskSchedulerConfig,
	dbClient taskingDB.Client,
	redisClient goutilsRedis.Client,
	onFatalCB taskingModel.OnFatalCB,
) (taskEngine.Scheduler, error)

/*
TaskClientFactory defines the Task Engine client maintenance requests are submitted through.

The submission counterpart to TaskReceiverFactory; see there for why this is injected. Mirrors
`tasking.NewTaskClient` exactly, and defaults to it when left nil.

	@param parentCtx context.Context - the parent execution context of the client
	@param clientName string - the client's name, which is the identity stamped on the IPC messages
	    it sends
	@param taskCreator string - the creator bound to tasks this client submits
	@param clientConfig taskingModel.TaskClientConfig - task client config
	@param dbClient taskingDB.Client - Task Engine persistence client
	@param redisClient goutilsRedis.Client - REDIS client
	@returns the new task client
*/
type TaskClientFactory func(
	parentCtx context.Context,
	clientName string,
	taskCreator string,
	clientConfig taskingModel.TaskClientConfig,
	dbClient taskingDB.Client,
	redisClient goutilsRedis.Client,
) (taskEngine.Client, error)

/*
IntervalTimerFactory defines the timer periodic maintenance requests are submitted on.

Injected for a different reason than the factories above, which stand in for live infrastructure:
`goutils.GetIntervalTimerInstance` needs none. It is a seam so a test can take hold of the interval
and the handler and run a tick when it chooses, rather than waiting out a real one. Mirrors that
function exactly, and defaults to it when left nil.

	@param rootCtx context.Context - the base context the timer derives each run's context from
	@param wg *sync.WaitGroup - the wait group the timer's goroutine is tracked by
	@param logTags log.Fields - log metadata fields
	@returns the new interval timer
*/
type IntervalTimerFactory func(
	rootCtx context.Context, wg *sync.WaitGroup, logTags log.Fields,
) (goutils.IntervalTimer, error)

// Compile time proof the injected seams still match what they default to. Without these, a
// signature change in `tasking` or `goutils` would surface as a confusing assignment error inside
// the constructor rather than here, against the type that got left behind.
var (
	_ TaskReceiverFactory  = tasking.NewTaskWorkerReceiver
	_ TaskSchedulerFactory = tasking.NewTaskScheduler
	_ TaskClientFactory    = tasking.NewTaskClient
	_ IntervalTimerFactory = goutils.GetIntervalTimerInstance
)

// NewRunnerParams the parameters a maintenance runner is built from.
//
// A struct rather than a positional argument list: at this arity, several of the entries are
// same-typed and a caller transposing two of them would build a working-looking runner pointed at
// the wrong queue.
type NewRunnerParams struct {
	// ParentCtx the parent execution context the Task Engine's worker threads derive from
	ParentCtx context.Context

	// AppName the per-deployment application name
	AppName string

	// TaskEngine the process wide Task Engine config
	TaskEngine models.TaskEngineConfig

	// Maintenance the maintenance system config.
	//
	// Carried and validated, but nothing here reads it yet: what needs it is the cadence at
	// which maintenance requests are submitted, which is not this component's job today.
	Maintenance models.MaintenanceConfig

	// Operator the maintenance operator one task execution runs an iteration of
	Operator Operator

	// Persistence the Task Engine's persistence client; see NewTaskEnginePersistence. This is
	// `tasking`'s client, not cairn's - the two address different schemas.
	Persistence taskingDB.Client

	// Redis the REDIS client backing the Task Engine's IPC queues
	Redis goutilsRedis.Client

	// OnFatal invoked when an engine goroutine hits an unrecoverable fault. Optional - nil keeps
	// `tasking`'s own default, which logs the fault and terminates the process.
	OnFatal taskingModel.OnFatalCB

	// ReceiverFactory optional; nil uses `tasking.NewTaskWorkerReceiver`
	ReceiverFactory TaskReceiverFactory

	// SchedulerFactory optional; nil uses `tasking.NewTaskScheduler`
	SchedulerFactory TaskSchedulerFactory
}

/*
NewRunner define a new system maintenance runner

Builds the Task Engine components that run maintenance iterations, but starts nothing - see
Start. Nothing here submits a maintenance request either; this side of the integration only
answers them.

	@param params NewRunnerParams - the runner parameters
	@returns the new system maintenance runner
*/
func NewRunner(params NewRunnerParams) (Runner, error) {
	logTags := log.Fields{
		"package": "cairn", "module": "maintenance", "component": "runner", "instance": params.AppName,
	}

	validate := validator.New()
	if err := models.RegisterWithValidator(validate); err != nil {
		return nil, goutils.NewRuntimeError(
			"failed to install custom validation macros", err, true,
		)
	}

	if err := validate.Var(params.AppName, "required,valid_name"); err != nil {
		return nil, goutils.NewValidationError(
			fmt.Sprintf("application name '%s' is not valid", params.AppName), err, true,
		)
	}

	if params.ParentCtx == nil {
		return nil, goutils.NewValidationError("parent execution context is required", nil, true)
	}

	if params.Operator == nil {
		return nil, goutils.NewValidationError("maintenance operator is required", nil, true)
	}

	if params.Persistence == nil {
		return nil, goutils.NewValidationError(
			"Task Engine persistence client is required", nil, true,
		)
	}

	if params.Redis == nil {
		return nil, goutils.NewValidationError("REDIS client is required", nil, true)
	}

	// Validated up front so a queue name the engine would refuse, or a scheduler interval below
	// its floor, fails here against the config field rather than inside a library constructor.
	if err := validate.Struct(&params.TaskEngine); err != nil {
		return nil, goutils.NewValidationError("Task Engine config is not valid", err, true)
	}

	if err := validate.Struct(&params.Maintenance); err != nil {
		return nil, goutils.NewValidationError("maintenance config is not valid", err, true)
	}

	// Defaulted rather than rejected. Requiring these would force every caller to name the two
	// `tasking` constructors, which is precisely the import this package exists to absorb.
	receiverFactory := params.ReceiverFactory
	if receiverFactory == nil {
		receiverFactory = tasking.NewTaskWorkerReceiver
	}
	schedulerFactory := params.SchedulerFactory
	if schedulerFactory == nil {
		schedulerFactory = tasking.NewTaskScheduler
	}

	/*
		The one queue this deployment's engine serves, and the one task on it.

		This same slice goes to both components below, which is the engine's routing contract
		rather than a convenience: the receiver reads it to learn which processor runs a task,
		and the scheduler reads it to learn which queue to dispatch that task to. Given to only
		one of them, a submitted maintenance request is either defined but never routed, or
		routed to a worker that has no processor for it.
	*/
	taskQueues := []taskingModel.TaskQueueConfig{
		{
			Name:           params.TaskEngine.TaskQueue,
			Workers:        maintenanceTaskWorkers,
			BufferRequests: maintenanceTaskBufferLen,
			SupportedTasks: []taskingModel.TaskDefinition{
				{
					TaskName:  MaintenanceTaskName,
					Retry:     maintenanceTaskRetry,
					Processor: taskingWrapper{core: params.Operator},
				},
			},
		},
	}

	taskReceiver, err := receiverFactory(
		params.ParentCtx,
		taskingModel.TaskReceiverConfig{
			Name:           params.TaskEngine.WorkerName,
			TaskQueues:     taskQueues,
			SchedulerQueue: params.TaskEngine.SchedulerQueue,
		},
		params.Persistence,
		params.Redis,
		params.OnFatal,
	)
	if err != nil {
		return nil, models.NewMaintenanceError(
			"failed to define maintenance task receiver", err, true,
		)
	}

	taskScheduler, err := schedulerFactory(
		params.ParentCtx,
		taskingModel.TaskSchedulerConfig{
			MaintenanceTimerIntSecs: params.TaskEngine.SchedulerMaintenanceIntSec,
			SchedulerQueue:          params.TaskEngine.SchedulerQueue,
			TaskQueues:              taskQueues,
		},
		params.Persistence,
		params.Redis,
		params.OnFatal,
	)
	if err != nil {
		return nil, models.NewMaintenanceError(
			"failed to define maintenance task scheduler", err, true,
		)
	}

	instance := &runnerImpl{
		Component: goutils.Component{
			LogTags: logTags,
			LogTagModifiers: []goutils.LogMetadataModifier{
				goutils.ModifyLogMetadataByRestRequestParam,
			},
		},
		taskScheduler: taskScheduler,
		taskReceiver:  taskReceiver,
	}

	return instance, nil
}

// ======================================================================================
// Lifecycle

/*
Start the maintenance runner and all its components

The order is load bearing. `Initialize` drains the receiver's own queue buffer and reclaims the
execution instances this worker name held before the last restart, so it must complete before the
scheduler is running and dispatching new work - otherwise crash recovery and live traffic contend
over the same instances.

Each step is fatal. A half started engine answers some requests and silently drops others, which
is worse than not starting: the caller aborts startup on an error here, and the orchestration
system restarting the process is the loud signal that something is wrong. Anything already
started is stopped before returning, since a failed Start leaves the caller with no handle it is
expected to Stop.

	@param ctx context.Context - execution context
*/
func (r *runnerImpl) Start(ctx context.Context) error {
	if err := r.taskReceiver.Initialize(ctx, nil); err != nil {
		return models.NewBootStrapError(
			"failed to initialize maintenance task receiver", err, true,
		)
	}

	if err := r.taskReceiver.Start(ctx); err != nil {
		return models.NewBootStrapError("failed to start maintenance task receiver", err, true)
	}

	if err := r.taskScheduler.Start(ctx); err != nil {
		// Unwind the receiver started a moment ago. Its own failure is folded in rather than
		// replacing the scheduler's, which is the one that explains why startup failed.
		unwindErr := r.taskReceiver.Stop(ctx)
		if unwindErr != nil {
			err = errors.Join(err, unwindErr)
		}
		return models.NewBootStrapError("failed to start maintenance task scheduler", err, true)
	}

	return nil
}

/*
Stop the maintenance runner and all its components

The reverse of Start - scheduler first, so nothing new is dispatched to a receiver on its way
down. Both are attempted whatever the other does, so a failure in one never leaves the other
running, and both failures are reported rather than only the first.

	@param ctx context.Context - execution context
*/
func (r *runnerImpl) Stop(ctx context.Context) error {
	schedulerErr := r.taskScheduler.Stop(ctx)
	receiverErr := r.taskReceiver.Stop(ctx)

	allErrors := []error{}
	if schedulerErr != nil {
		allErrors = append(allErrors, models.NewShutdownError(
			"failed to stop maintenance task scheduler", schedulerErr, true,
		))
	}
	if receiverErr != nil {
		allErrors = append(allErrors, models.NewShutdownError(
			"failed to stop maintenance task receiver", receiverErr, true,
		))
	}

	return errors.Join(allErrors...)
}

// ======================================================================================
// Maintenance Request Trigger

/*
Trigger asks the Task Engine to run maintenance.

The opposite half of Runner, and deliberately a separate component. Runner answers maintenance
requests; this raises them - periodically on its own timer, and on demand for a caller that wants
a sweep now rather than at the next interval. Nothing connects the two but the task name they
agree on, so either can run in a process without the other.
*/
type Trigger interface {
	/*
		StartTimer start the interval timer that raises a maintenance request every sweep
		interval.

		The timer cannot be started twice, and does not come back after StopTimer.

			@param ctx context.Context - execution context
	*/
	StartTimer(ctx context.Context) error

	/*
		RequestMaintenance ask the Task Engine to run maintenance now.

		Independent of the timer: it neither disturbs the periodic schedule nor stops working
		once the timer has been stopped.

		A NON-NIL ERROR DOES NOT MEAN NO TASK EXISTS. The request is recorded and then handed to
		the scheduler, so a failure at the second step leaves a real task behind that the
		engine's own maintenance later picks up. The ID is returned either way, and is empty
		only when nothing was recorded at all.

			@param ctx context.Context - execution context
			@returns the ID of the task the request was recorded as
	*/
	RequestMaintenance(ctx context.Context) (string, error)

	/*
		StopTimer stop the interval timer, and wait for it to come to rest.

		One way: the timer does not start again afterwards. RequestMaintenance is unaffected.

			@param ctx context.Context - execution context
	*/
	StopTimer(ctx context.Context) error
}

// triggerImpl implements Trigger
type triggerImpl struct {
	goutils.Component

	// sweepInterval how often the timer raises a maintenance request
	sweepInterval time.Duration

	// terminalTaskAgeOut how long a finished maintenance task is kept before it is cleaned up
	terminalTaskAgeOut time.Duration

	// taskClient the Task Engine client requests are submitted through
	taskClient taskEngine.Client

	// timer the periodic sweep timer
	timer goutils.IntervalTimer

	// wg tracks the timer's goroutine, so StopTimer can wait for it rather than assume it left
	wg *sync.WaitGroup

	// timerCtx, timerCtxCancel the timer's context, and what ends it.
	//
	// The timer's alone, and not the client's, which runs on the parent context instead. That is
	// what lets StopTimer end the periodic schedule - including a submission already in flight -
	// without also taking RequestMaintenance down with it.
	timerCtx       context.Context
	timerCtxCancel context.CancelFunc
}

// NewTriggerParams the parameters a maintenance trigger is built from. A struct for the same
// reason as NewRunnerParams.
type NewTriggerParams struct {
	// ParentCtx the parent execution context the timer and the task client derive from
	ParentCtx context.Context

	// AppName the per-deployment application name. Also the creator every maintenance task is
	// bound to; see NewTrigger.
	AppName string

	// TaskEngine the process wide Task Engine config
	TaskEngine models.TaskEngineConfig

	// Maintenance the maintenance system config, which sets the sweep cadence
	Maintenance models.MaintenanceConfig

	// Persistence the Task Engine's persistence client; see NewTaskEnginePersistence
	Persistence taskingDB.Client

	// Redis the REDIS client backing the Task Engine's IPC queues
	Redis goutilsRedis.Client

	// ClientFactory optional; nil uses `tasking.NewTaskClient`
	ClientFactory TaskClientFactory

	// TimerFactory optional; nil uses `goutils.GetIntervalTimerInstance`
	TimerFactory IntervalTimerFactory
}

/*
NewTrigger define a new system maintenance trigger

Builds the task client and the sweep timer, but starts nothing - see StartTimer.

	@param params NewTriggerParams - the trigger parameters
	@returns the new system maintenance trigger
*/
func NewTrigger(params NewTriggerParams) (Trigger, error) {
	logTags := log.Fields{
		"package": "cairn", "module": "maintenance", "component": "trigger", "instance": params.AppName,
	}

	validate := validator.New()
	if err := models.RegisterWithValidator(validate); err != nil {
		return nil, goutils.NewRuntimeError(
			"failed to install custom validation macros", err, true,
		)
	}

	if err := validate.Var(params.AppName, "required,valid_name"); err != nil {
		return nil, goutils.NewValidationError(
			fmt.Sprintf("application name '%s' is not valid", params.AppName), err, true,
		)
	}

	if params.ParentCtx == nil {
		return nil, goutils.NewValidationError("parent execution context is required", nil, true)
	}

	if params.Persistence == nil {
		return nil, goutils.NewValidationError(
			"Task Engine persistence client is required", nil, true,
		)
	}

	if params.Redis == nil {
		return nil, goutils.NewValidationError("REDIS client is required", nil, true)
	}

	if err := validate.Struct(&params.TaskEngine); err != nil {
		return nil, goutils.NewValidationError("Task Engine config is not valid", err, true)
	}

	if err := validate.Struct(&params.Maintenance); err != nil {
		return nil, goutils.NewValidationError("maintenance config is not valid", err, true)
	}

	// Defaulted rather than rejected, for the reason given on NewRunnerParams.
	clientFactory := params.ClientFactory
	if clientFactory == nil {
		clientFactory = tasking.NewTaskClient
	}
	timerFactory := params.TimerFactory
	if timerFactory == nil {
		timerFactory = goutils.GetIntervalTimerInstance
	}

	/*
		The client is bound to the parent context, not the timer's - see triggerImpl.

		Its creator is the application name, which scopes every read and delete the client can
		perform to this deployment's own tasks. Two cairn deployments pointed at one Task Engine
		database therefore stay invisible to each other, which is what makes reaping this
		deployment's finished maintenance tasks safe to do by creator.

		No task queues are named in the config. Doing so would mean naming an execution processor
		for each task - the queue config requires one - and this side of the integration has no
		operator to build one from. Nothing is lost: what the queues would supply the client is a
		retry policy, and each submission states its own.
	*/
	taskClient, err := clientFactory(
		params.ParentCtx,
		fmt.Sprintf("%s-maintenance-trigger", params.AppName),
		params.AppName,
		taskingModel.TaskClientConfig{SchedulerQueue: params.TaskEngine.SchedulerQueue},
		params.Persistence,
		params.Redis,
	)
	if err != nil {
		return nil, models.NewMaintenanceError(
			"failed to define maintenance task client", err, true,
		)
	}

	timerCtx, timerCtxCancel := context.WithCancel(params.ParentCtx)

	// The same wait group the timer is handed is the one StopTimer waits on; two would leave the
	// wait watching nothing and returning immediately.
	timerWG := &sync.WaitGroup{}

	timer, err := timerFactory(timerCtx, timerWG, log.Fields{
		"package":       "cairn",
		"module":        "maintenance",
		"component":     "trigger",
		"sub-component": "sweep-timer",
		"instance":      params.AppName,
	})
	if err != nil {
		timerCtxCancel()
		return nil, models.NewMaintenanceError(
			"failed to define maintenance sweep timer", err, true,
		)
	}

	instance := &triggerImpl{
		Component: goutils.Component{
			LogTags: logTags,
			LogTagModifiers: []goutils.LogMetadataModifier{
				goutils.ModifyLogMetadataByRestRequestParam,
			},
		},
		sweepInterval:      params.Maintenance.MaintenanceSweepInt(),
		terminalTaskAgeOut: params.Maintenance.TerminalTaskAgeOut(),
		taskClient:         taskClient,
		timer:              timer,
		wg:                 timerWG,
		timerCtx:           timerCtx,
		timerCtxCancel:     timerCtxCancel,
	}

	return instance, nil
}

/*
taskDeadline the instant a maintenance task submitted now must complete by.

One and a half sweep intervals, derived rather than configured because the sweep cadence is the
only thing the loop knows enough to state a bound from. Its job is to keep a backlog from
outliving its usefulness: an iteration is level-triggered, so a request still waiting when the
next one is already due has nothing left to contribute, and the engine ages it out rather than
running a redundant sweep behind the current one. The half interval of margin is what separates a
sweep that merely ran long from one that piled up.

It follows that the sweep interval has to comfortably exceed how long a sweep actually takes. Where
it does not, iterations are cancelled rather than merely overlapping, and the interval is what
wants raising.

	@param now time.Time - when the request is being submitted
	@returns the deadline to record on the task
*/
func (t *triggerImpl) taskDeadline(now time.Time) time.Time {
	return now.Add(t.sweepInterval + t.sweepInterval/2)
}

/*
submitMaintenanceRequest record a maintenance request and hand it to the scheduler.

No task parameters are carried. `ProcessTaskExecution` reads none: it takes the sweep's timestamp
at execution rather than off the task, so an iteration is judged against the instant it actually
ran instead of the instant it was asked for.

	@param ctx context.Context - execution context
	@param source string - what raised the request, recorded on the task
	@returns the ID of the task the request was recorded as
*/
func (t *triggerImpl) submitMaintenanceRequest(ctx context.Context, source string) (string, error) {
	// Copies, so nothing reachable from a caller points at the package level policy.
	retry := maintenanceTaskSubmitRetry
	deadline := t.taskDeadline(time.Now().UTC())

	taskEntry, err := t.taskClient.DefineAndRunImmediateOneShotTask(
		ctx,
		taskEngine.DefineTaskParams{
			Name:     MaintenanceTaskName,
			Metadata: maintenanceTaskMetadata{Source: source},
			Deadline: &deadline,
			Retry:    &retry,
		},
		// No transaction of our own to continue in; the client opens its own.
		nil,
	)
	if err != nil {
		return taskEntry.ID, models.NewMaintenanceError(
			fmt.Sprintf("failed to submit %s maintenance request", source), err, true,
		)
	}

	return taskEntry.ID, nil
}

/*
RequestMaintenance ask the Task Engine to run maintenance now. See the Trigger interface for
what a non-nil error does and does not say about whether a task exists.

	@param ctx context.Context - execution context
	@returns the ID of the task the request was recorded as
*/
func (t *triggerImpl) RequestMaintenance(ctx context.Context) (string, error) {
	return t.submitMaintenanceRequest(ctx, maintenanceSourceOnDemand)
}

/*
cleanupTerminalTasks delete this deployment's finished maintenance tasks once they have been
finished for longer than the configured age out.

Every sweep leaves a task entry behind, so something has to clear them or the Task Engine's
database grows for the life of the deployment (DESIGN §8.3.2). The window is what keeps the entries
around long enough to be read first - a failed sweep is worth looking at before its record goes.

The age out is applied here rather than in the query because the Task Engine offers no filter on
when an entry was last updated. What the query does narrow is which entries are this deployment's
to delete at all.

	@param ctx context.Context - execution context
	@param timestamp time.Time - the current timestamp, which the age out is measured back from
*/
func (t *triggerImpl) cleanupTerminalTasks(ctx context.Context, timestamp time.Time) error {
	logTags := t.GetLogTagsForContext(ctx)

	/*
		Creators is deliberately left unset. The client fills in its own - this deployment's
		application name - so naming one here could only be a chance to name the wrong one, and
		reach tasks belonging to a deployment sharing the database.

		The task name narrows it further, so a task type cairn submits later under the same
		creator is not cleared away by a pass that knows nothing about it.
	*/
	limit := terminalTaskCleanupBatch
	candidates, err := t.taskClient.ListTasks(ctx, taskingDB.TaskQueryFilter{
		CommonListEntryQueryFilter: taskingDB.CommonListEntryQueryFilter{Limit: &limit},
		TaskNames:                  []string{MaintenanceTaskName},
		TaskStates:                 terminalTaskStates,
	}, nil)
	if err != nil {
		return models.NewMaintenanceError(
			"failed to list finished maintenance tasks", err, true,
		)
	}

	cutOff := timestamp.Add(-t.terminalTaskAgeOut)

	deleted := 0
	allErrors := []error{}
	for _, oneTask := range candidates {
		if !oneTask.UpdatedAt.Before(cutOff) {
			continue
		}

		// Each delete stands on its own, rather than sharing one transaction with the rest of
		// the batch. One entry that refuses to go should not take the others with it.
		if err := t.taskClient.DeleteTask(ctx, oneTask.ID, nil); err != nil {
			allErrors = append(allErrors, models.NewMaintenanceError(
				fmt.Sprintf("failed to delete finished maintenance task %s", oneTask.ID), err, true,
			))
			continue
		}
		deleted++
	}

	if deleted > 0 {
		log.
			WithFields(logTags).
			WithField("deleted", deleted).
			Debug("Cleaned up finished maintenance tasks")
	}

	return errors.Join(allErrors...)
}

/*
StartTimer start the interval timer that raises a maintenance request every sweep interval.

A tick does two things: it asks for a sweep, and then it clears away the finished tasks earlier
sweeps left behind. The second happens whatever the first did - an engine that cannot take new work
is exactly when its old entries least deserve to be kept - so the two failures are reported
together rather than the first hiding the second.

The handler hands those failures back to the timer rather than absorbing them, which has the timer
log them in full and carry on ticking. That is the wanted behavior and not an oversight: a failed
submission costs one sweep, and the next tick re-derives everything the lost one would have done
from durable state.

Starting twice needs no guard here - the timer refuses it.

	@param ctx context.Context - execution context
*/
func (t *triggerImpl) StartTimer(_ context.Context) error {
	if err := t.timer.Start(t.sweepInterval, func() error {
		_, submitErr := t.submitMaintenanceRequest(t.timerCtx, maintenanceSourceTimer)
		cleanupErr := t.cleanupTerminalTasks(t.timerCtx, time.Now().UTC())
		return errors.Join(submitErr, cleanupErr)
	}, false); err != nil {
		return models.NewBootStrapError("failed to start maintenance sweep timer", err, true)
	}

	return nil
}

/*
StopTimer stop the interval timer, and wait for it to come to rest.

Stopping the timer ends its loop; cancelling the context is what reaches a submission already
under way, which would otherwise hold the wait open for as long as the Task Engine took to answer
it. Both are done whatever either reports, so a failure in one never leaves the other undone.

The timer does not start again afterwards, its context having been spent. This matches how the
Task Engine's own components end, and RequestMaintenance - which runs on the parent context - is
unaffected.

	@param ctx context.Context - execution context
*/
func (t *triggerImpl) StopTimer(ctx context.Context) error {
	timerErr := t.timer.Stop()
	t.timerCtxCancel()
	waitErr := goutils.TimeBoundedWaitGroupWait(ctx, t.wg, time.Second*5)

	allErrors := []error{}
	if timerErr != nil {
		allErrors = append(allErrors, models.NewShutdownError(
			"failed to stop maintenance sweep timer", timerErr, true,
		))
	}
	if waitErr != nil {
		allErrors = append(allErrors, models.NewShutdownError(
			"maintenance sweep timer did not come to rest", waitErr, true,
		))
	}

	return errors.Join(allErrors...)
}

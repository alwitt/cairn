package maintenance_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alwitt/cairn/maintenance"
	mockmaintenance "github.com/alwitt/cairn/mocks/maintenance"
	mocktest "github.com/alwitt/cairn/mocks/test"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	mockgoutils "github.com/alwitt/goutils/mocks/goutils"
	mockgoutilsRedis "github.com/alwitt/goutils/mocks/redis"
	goutilsRedis "github.com/alwitt/goutils/redis"
	taskingDB "github.com/alwitt/tasking/db"
	mocktaskingDB "github.com/alwitt/tasking/mocks/db"
	mocktask "github.com/alwitt/tasking/mocks/task"
	taskingModel "github.com/alwitt/tasking/models"
	taskEngine "github.com/alwitt/tasking/task"
	"github.com/apex/log"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// ======================================================================================
// Runner test harness

// unitTestTaskEngineConfig a valid Task Engine config, the shape NewRunner must accept. The
// values are distinct strings rather than plausible ones, so a case asserting what reached the
// engine cannot pass by coincidence.
func unitTestTaskEngineConfig() models.TaskEngineConfig {
	return models.TaskEngineConfig{
		WorkerName:                 "unit-test-worker",
		SchedulerQueue:             "unit-test-scheduler-queue",
		TaskQueue:                  "unit-test-task-queue",
		SchedulerMaintenanceIntSec: 30,
	}
}

// unitTestRunnerMocks the mocked dependencies a harness runner is built over.
type unitTestRunnerMocks struct {
	operator    *mockmaintenance.Operator
	persistence *mocktaskingDB.Client
	redis       *mockgoutilsRedis.Client
	callbacks   *mocktest.UnitTestCallbackCollector
	receiver    *mocktask.Receiver
	scheduler   *mocktask.Scheduler

	// receiverConfig, schedulerConfig what the two factories were actually handed. Captured
	// rather than asserted inline so a case can examine the routing after construction.
	receiverConfig  taskingModel.TaskReceiverConfig
	schedulerConfig taskingModel.TaskSchedulerConfig
}

// newUnitTestRunnerParams assemble a complete, valid set of runner parameters over mocked
// dependencies, with both factories routed through the callback collector.
//
// The factories are arranged as `Maybe`, not `Once`: several cases below reject the parameters
// before either is reached, and those cases are asserting exactly that.
func newUnitTestRunnerParams(t *testing.T) (maintenance.NewRunnerParams, *unitTestRunnerMocks) {
	mocks := &unitTestRunnerMocks{
		operator:    mockmaintenance.NewOperator(t),
		persistence: mocktaskingDB.NewClient(t),
		redis:       mockgoutilsRedis.NewClient(t),
		callbacks:   mocktest.NewUnitTestCallbackCollector(t),
		receiver:    mocktask.NewReceiver(t),
		scheduler:   mocktask.NewScheduler(t),
	}

	mocks.callbacks.EXPECT().
		DefineTaskReceiver(
			mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
		).
		RunAndReturn(func(
			_ context.Context,
			receiverConfig taskingModel.TaskReceiverConfig,
			_ taskingDB.Client,
			_ goutilsRedis.Client,
			_ taskingModel.OnFatalCB,
		) (taskEngine.Receiver, error) {
			mocks.receiverConfig = receiverConfig
			return mocks.receiver, nil
		}).
		Maybe()

	mocks.callbacks.EXPECT().
		DefineTaskScheduler(
			mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
		).
		RunAndReturn(func(
			_ context.Context,
			schedulerConfig taskingModel.TaskSchedulerConfig,
			_ taskingDB.Client,
			_ goutilsRedis.Client,
			_ taskingModel.OnFatalCB,
		) (taskEngine.Scheduler, error) {
			mocks.schedulerConfig = schedulerConfig
			return mocks.scheduler, nil
		}).
		Maybe()

	params := maintenance.NewRunnerParams{
		ParentCtx:        context.Background(),
		AppName:          unitTestAppName,
		TaskEngine:       unitTestTaskEngineConfig(),
		Maintenance:      unitTestMaintenanceConfig(),
		Operator:         mocks.operator,
		Persistence:      mocks.persistence,
		Redis:            mocks.redis,
		ReceiverFactory:  mocks.callbacks.DefineTaskReceiver,
		SchedulerFactory: mocks.callbacks.DefineTaskScheduler,
	}

	return params, mocks
}

// newUnitTestRunner build a Runner over the harness parameters, asserting it was accepted.
func newUnitTestRunner(t *testing.T) (maintenance.Runner, *unitTestRunnerMocks) {
	assert := assert.New(t)

	params, mocks := newUnitTestRunnerParams(t)

	runner, err := maintenance.NewRunner(params)
	assert.Nil(err)
	assert.NotNil(runner)

	return runner, mocks
}

// newUnitTestTaskProcessor reach the maintenance task's execution processor, along with the
// operator it was built over.
//
// Through the runner rather than directly: the processor type is unexported, so the queue config
// the runner handed its receiver is the only way in from outside the package. Which is fitting -
// it is also the only way the Task Engine reaches it in production.
func newUnitTestTaskProcessor(
	t *testing.T,
) (taskingModel.TaskExecutionProcessor, *mockmaintenance.Operator) {
	assert := assert.New(t)

	_, mocks := newUnitTestRunner(t)

	assert.Len(mocks.receiverConfig.TaskQueues, 1)
	assert.Len(mocks.receiverConfig.TaskQueues[0].SupportedTasks, 1)

	processor := mocks.receiverConfig.TaskQueues[0].SupportedTasks[0].Processor
	assert.NotNil(processor)

	return processor, mocks.operator
}

/*
TestNewRunner validates that a maintenance runner refuses to be built over parameters it cannot
work with.

The runner answers maintenance requests unattended, so a wiring mistake caught here is one that
would otherwise present as a deployment whose maintenance silently never runs.
*/
func TestNewRunner(t *testing.T) {
	// Case 1: the ordinary path, which the remaining cases are variations on.
	t.Run("builds over complete parameters", func(t *testing.T) {
		assert := assert.New(t)

		params, _ := newUnitTestRunnerParams(t)

		runner, err := maintenance.NewRunner(params)
		assert.Nil(err)
		assert.NotNil(runner)
	})

	// Case 2: every dependency the runner cannot substitute for. Each is dropped in turn from an
	// otherwise complete set, so the case that fails names the one field that was missing.
	t.Run("rejects a missing dependency", func(t *testing.T) {
		assert := assert.New(t)

		for name, drop := range map[string]func(*maintenance.NewRunnerParams){
			"parent context": func(p *maintenance.NewRunnerParams) { p.ParentCtx = nil },
			"operator":       func(p *maintenance.NewRunnerParams) { p.Operator = nil },
			"persistence":    func(p *maintenance.NewRunnerParams) { p.Persistence = nil },
			"redis":          func(p *maintenance.NewRunnerParams) { p.Redis = nil },
		} {
			params, _ := newUnitTestRunnerParams(t)
			drop(&params)

			runner, err := maintenance.NewRunner(params)
			assert.Nil(runner, "a runner missing its %s should not be built", name)
			assert.NotNil(err, "a missing %s should be rejected", name)
		}
	})

	// Case 3: the factories are the one pair of parameters that may be absent. Leaving them out
	// is the production path - it is what lets a caller stand the engine up without naming
	// `tasking`'s own constructors - so a runner has to be buildable without them.
	//
	// This is the only case that runs the real `tasking` constructors, which is the point: it is
	// what proves the defaults are wired to something that actually builds, rather than merely
	// that a nil check exists. They reach straight for REDIS queue handles, hence the arrangement
	// below; `Maybe` rather than a count because how many handles the engine takes is `tasking`'s
	// business, not something to pin down from here.
	t.Run("accepts absent factories", func(t *testing.T) {
		assert := assert.New(t)

		params, mocks := newUnitTestRunnerParams(t)
		params.ReceiverFactory = nil
		params.SchedulerFactory = nil

		mocks.redis.EXPECT().
			GetQueueHandle(mock.Anything, mock.Anything).
			Return(mockgoutilsRedis.NewQueue(t), nil).
			Maybe()

		runner, err := maintenance.NewRunner(params)
		assert.Nil(err)
		assert.NotNil(runner)
	})

	// Case 4: the application name namespaces this deployment, and is held to the same charset
	// as everywhere else it appears.
	t.Run("rejects an invalid application name", func(t *testing.T) {
		assert := assert.New(t)

		for _, broken := range []string{"", "has spaces", "has/slash"} {
			params, _ := newUnitTestRunnerParams(t)
			params.AppName = broken

			runner, err := maintenance.NewRunner(params)
			assert.Nil(runner)
			assert.NotNil(err, "application name '%s' should be rejected", broken)
		}
	})

	// Case 5: config the Task Engine itself would refuse, caught here instead so the complaint
	// names cairn's own config field. The scheduler interval is the interesting one - `tasking`
	// enforces a floor of 10 seconds, and a config that undercuts it should not reach the
	// library at all.
	t.Run("rejects an invalid Task Engine config", func(t *testing.T) {
		assert := assert.New(t)

		for name, corrupt := range map[string]func(*models.TaskEngineConfig){
			"empty worker name":     func(c *models.TaskEngineConfig) { c.WorkerName = "" },
			"empty scheduler queue": func(c *models.TaskEngineConfig) { c.SchedulerQueue = "" },
			"empty task queue":      func(c *models.TaskEngineConfig) { c.TaskQueue = "" },
			"queue name charset":    func(c *models.TaskEngineConfig) { c.TaskQueue = "not a name" },
			"interval below floor": func(c *models.TaskEngineConfig) {
				c.SchedulerMaintenanceIntSec = 9
			},
			"interval unset": func(c *models.TaskEngineConfig) {
				c.SchedulerMaintenanceIntSec = 0
			},
		} {
			params, _ := newUnitTestRunnerParams(t)
			broken := unitTestTaskEngineConfig()
			corrupt(&broken)
			params.TaskEngine = broken

			runner, err := maintenance.NewRunner(params)
			assert.Nil(runner, "a runner with %s should not be built", name)
			assert.NotNil(err, "%s should be rejected", name)
		}
	})

	// Case 6: the maintenance config is not read by anything here yet, but it is validated, so a
	// deployment learns about a zero grace window at startup rather than at the first sweep.
	t.Run("rejects an invalid maintenance config", func(t *testing.T) {
		assert := assert.New(t)

		for name, corrupt := range map[string]func(*models.MaintenanceConfig){
			"sweep interval":        func(c *models.MaintenanceConfig) { c.MaintenanceSweepIntSec = 0 },
			"object grace window":   func(c *models.MaintenanceConfig) { c.OrphanedObjectAgeOutSec = 0 },
			"task retention window": func(c *models.MaintenanceConfig) { c.TerminalTaskAgeOutSec = 0 },
		} {
			broken := unitTestMaintenanceConfig()
			corrupt(&broken)

			params, _ := newUnitTestRunnerParams(t)
			params.Maintenance = broken

			runner, err := maintenance.NewRunner(params)
			assert.Nil(runner)
			assert.NotNil(err, "a zero %s should be rejected", name)
		}
	})

	// Case 7: a factory that fails takes the whole construction down. A runner holding one live
	// component and one nil would panic on Start rather than report anything.
	t.Run("surfaces a component that fails to build", func(t *testing.T) {
		assert := assert.New(t)

		buildFailure := errors.New("redis is unreachable")

		t.Run("receiver", func(t *testing.T) {
			params, _ := newUnitTestRunnerParams(t)
			params.ReceiverFactory = func(
				context.Context,
				taskingModel.TaskReceiverConfig,
				taskingDB.Client,
				goutilsRedis.Client,
				taskingModel.OnFatalCB,
			) (taskEngine.Receiver, error) {
				return nil, buildFailure
			}

			runner, err := maintenance.NewRunner(params)
			assert.Nil(runner)
			assert.NotNil(err)
			assert.Contains(err.Error(), buildFailure.Error())
		})

		t.Run("scheduler", func(t *testing.T) {
			params, _ := newUnitTestRunnerParams(t)
			params.SchedulerFactory = func(
				context.Context,
				taskingModel.TaskSchedulerConfig,
				taskingDB.Client,
				goutilsRedis.Client,
				taskingModel.OnFatalCB,
			) (taskEngine.Scheduler, error) {
				return nil, buildFailure
			}

			runner, err := maintenance.NewRunner(params)
			assert.Nil(runner)
			assert.NotNil(err)
			assert.Contains(err.Error(), buildFailure.Error())
		})
	})
}

/*
TestNewRunnerTaskWiring validates the routing the two Task Engine components are given.

This is the wiring with no runtime symptom worth the name: give the queue set to only one of the
two and a submitted maintenance request is either defined but never routed, or routed to a worker
with no processor for it. Either way the request simply never runs, and nothing reports why.
*/
func TestNewRunnerTaskWiring(t *testing.T) {
	// Case 1: the receiver and the scheduler must be describing the same fabric - the same
	// queue, carrying the same task, reachable through the same scheduler queue.
	t.Run("hands both components the same queue set", func(t *testing.T) {
		assert := assert.New(t)

		_, mocks := newUnitTestRunner(t)
		config := unitTestTaskEngineConfig()

		assert.Equal(config.WorkerName, mocks.receiverConfig.Name)
		assert.Equal(config.SchedulerQueue, mocks.receiverConfig.SchedulerQueue)
		assert.Equal(config.SchedulerQueue, mocks.schedulerConfig.SchedulerQueue)
		assert.Equal(
			config.SchedulerMaintenanceIntSec, mocks.schedulerConfig.MaintenanceTimerIntSecs,
		)

		assert.Equal(
			mocks.receiverConfig.TaskQueues,
			mocks.schedulerConfig.TaskQueues,
			"the receiver and the scheduler must be given identical task routing",
		)
	})

	// Case 2: the queue itself, named from config and carrying exactly one task - the
	// maintenance task, with a processor attached. A definition without a processor is refused
	// by the engine; one with the wrong name is never dispatched to.
	t.Run("defines the maintenance task on the configured queue", func(t *testing.T) {
		assert := assert.New(t)

		_, mocks := newUnitTestRunner(t)

		assert.Len(mocks.receiverConfig.TaskQueues, 1)
		queue := mocks.receiverConfig.TaskQueues[0]

		assert.Equal(unitTestTaskEngineConfig().TaskQueue, queue.Name)
		assert.GreaterOrEqual(queue.Workers, 1)
		assert.GreaterOrEqual(queue.BufferRequests, 1)

		assert.Len(queue.SupportedTasks, 1)
		assert.Equal(maintenance.MaintenanceTaskName, queue.SupportedTasks[0].TaskName)
		assert.NotNil(queue.SupportedTasks[0].Processor)
	})
}

/*
TestRunnerStart validates the Task Engine startup sequence.

The order is what is being asserted, not merely that everything started. `Initialize` reclaims the
execution instances this worker name held before the last restart, so it has to finish before the
scheduler is dispatching anything new - otherwise crash recovery contends with live traffic over
the same instances.
*/
func TestRunnerStart(t *testing.T) {
	// Case 1: the full sequence, with the ordering asserted by a counter each expectation stamps
	// rather than by the mere fact that all three were called.
	t.Run("initializes and starts the receiver before the scheduler", func(t *testing.T) {
		assert := assert.New(t)

		runner, mocks := newUnitTestRunner(t)

		step := 0
		mocks.receiver.EXPECT().
			Initialize(mock.Anything, nil).
			RunAndReturn(func(context.Context, taskingDB.Database) error {
				step++
				assert.Equal(1, step, "Initialize must run first")
				return nil
			}).
			Once()
		mocks.receiver.EXPECT().
			Start(mock.Anything).
			RunAndReturn(func(context.Context) error {
				step++
				assert.Equal(2, step, "the receiver must start after it is initialized")
				return nil
			}).
			Once()
		mocks.scheduler.EXPECT().
			Start(mock.Anything).
			RunAndReturn(func(context.Context) error {
				step++
				assert.Equal(3, step, "the scheduler must start last")
				return nil
			}).
			Once()

		assert.Nil(runner.Start(context.Background()))
	})

	// Case 2: a failed initialization stops the sequence there. The receiver's own Start is
	// never arranged, so reaching it fails this case - which is the point: starting a worker
	// whose crash recovery did not complete would have it competing for instances it may already
	// hold.
	t.Run("halts on a failed initialization", func(t *testing.T) {
		assert := assert.New(t)

		runner, mocks := newUnitTestRunner(t)

		initFailure := errors.New("queue buffer is unreadable")
		mocks.receiver.EXPECT().Initialize(mock.Anything, nil).Return(initFailure).Once()

		err := runner.Start(context.Background())
		assert.NotNil(err)
		assert.Contains(err.Error(), initFailure.Error())
	})

	// Case 3: a failed receiver start likewise never reaches the scheduler.
	t.Run("halts on a failed receiver start", func(t *testing.T) {
		assert := assert.New(t)

		runner, mocks := newUnitTestRunner(t)

		startFailure := errors.New("queue threads did not start")
		mocks.receiver.EXPECT().Initialize(mock.Anything, nil).Return(nil).Once()
		mocks.receiver.EXPECT().Start(mock.Anything).Return(startFailure).Once()

		err := runner.Start(context.Background())
		assert.NotNil(err)
		assert.Contains(err.Error(), startFailure.Error())
	})

	// Case 4: when the scheduler fails, the receiver started a moment earlier is stopped again.
	// A failed Start leaves the caller aborting rather than calling Stop, so anything left
	// running here is left running for the life of the process.
	t.Run("unwinds the receiver when the scheduler fails", func(t *testing.T) {
		assert := assert.New(t)

		runner, mocks := newUnitTestRunner(t)

		schedulerFailure := errors.New("scheduler queue is unreachable")
		mocks.receiver.EXPECT().Initialize(mock.Anything, nil).Return(nil).Once()
		mocks.receiver.EXPECT().Start(mock.Anything).Return(nil).Once()
		mocks.scheduler.EXPECT().Start(mock.Anything).Return(schedulerFailure).Once()
		mocks.receiver.EXPECT().Stop(mock.Anything).Return(nil).Once()

		err := runner.Start(context.Background())
		assert.NotNil(err)
		assert.Contains(err.Error(), schedulerFailure.Error())
	})

	// Case 5: and if that unwind itself fails, both failures are reported. The scheduler's is
	// what explains the abort; the receiver's says a thread was left behind.
	t.Run("reports a failed unwind alongside the cause", func(t *testing.T) {
		assert := assert.New(t)

		runner, mocks := newUnitTestRunner(t)

		schedulerFailure := errors.New("scheduler queue is unreachable")
		unwindFailure := errors.New("receiver threads did not stop")
		mocks.receiver.EXPECT().Initialize(mock.Anything, nil).Return(nil).Once()
		mocks.receiver.EXPECT().Start(mock.Anything).Return(nil).Once()
		mocks.scheduler.EXPECT().Start(mock.Anything).Return(schedulerFailure).Once()
		mocks.receiver.EXPECT().Stop(mock.Anything).Return(unwindFailure).Once()

		err := runner.Start(context.Background())
		assert.NotNil(err)
		assert.Contains(err.Error(), schedulerFailure.Error())
		assert.Contains(err.Error(), unwindFailure.Error())
	})
}

/*
TestRunnerStop validates the Task Engine shutdown sequence.

Unlike Start, no step here is allowed to abandon the next: a component left running past shutdown
keeps claiming work from a process that is going away.
*/
func TestRunnerStop(t *testing.T) {
	// Case 1: the ordinary path, scheduler first so nothing new is dispatched at a receiver on
	// its way down.
	t.Run("stops the scheduler before the receiver", func(t *testing.T) {
		assert := assert.New(t)

		runner, mocks := newUnitTestRunner(t)

		step := 0
		mocks.scheduler.EXPECT().
			Stop(mock.Anything).
			RunAndReturn(func(context.Context) error {
				step++
				assert.Equal(1, step, "the scheduler must stop first")
				return nil
			}).
			Once()
		mocks.receiver.EXPECT().
			Stop(mock.Anything).
			RunAndReturn(func(context.Context) error {
				step++
				assert.Equal(2, step, "the receiver must stop after the scheduler")
				return nil
			}).
			Once()

		assert.Nil(runner.Stop(context.Background()))
	})

	// Case 2: the receiver is stopped even when the scheduler could not be. Arranged as `Once`,
	// so a Stop that returned early on the scheduler's failure fails this case.
	t.Run("stops the receiver despite a failed scheduler stop", func(t *testing.T) {
		assert := assert.New(t)

		runner, mocks := newUnitTestRunner(t)

		schedulerFailure := errors.New("scheduler threads did not stop")
		mocks.scheduler.EXPECT().Stop(mock.Anything).Return(schedulerFailure).Once()
		mocks.receiver.EXPECT().Stop(mock.Anything).Return(nil).Once()

		err := runner.Stop(context.Background())
		assert.NotNil(err)
		assert.Contains(err.Error(), schedulerFailure.Error())
	})

	// Case 3: both failing reports both. Naming one and dropping the other would leave an
	// operator chasing half of what went wrong.
	t.Run("reports both failures", func(t *testing.T) {
		assert := assert.New(t)

		runner, mocks := newUnitTestRunner(t)

		schedulerFailure := errors.New("scheduler threads did not stop")
		receiverFailure := errors.New("receiver threads did not stop")
		mocks.scheduler.EXPECT().Stop(mock.Anything).Return(schedulerFailure).Once()
		mocks.receiver.EXPECT().Stop(mock.Anything).Return(receiverFailure).Once()

		err := runner.Stop(context.Background())
		assert.NotNil(err)
		assert.Contains(err.Error(), schedulerFailure.Error())
		assert.Contains(err.Error(), receiverFailure.Error())
	})
}

// ======================================================================================
// Trigger test harness

// unitTestTriggerMocks the mocked dependencies a harness trigger is built over, along with what
// the trigger handed each of them. Captured rather than asserted inline so a case can examine the
// wiring after construction, and so the timer's handler can be run when a case chooses rather
// than when a real interval elapses.
type unitTestTriggerMocks struct {
	persistence *mocktaskingDB.Client
	redis       *mockgoutilsRedis.Client
	callbacks   *mocktest.UnitTestCallbackCollector
	taskClient  *mocktask.Client
	timer       *mockgoutils.IntervalTimer

	// clientName, taskCreator, clientConfig what the task client was defined with
	clientName   string
	taskCreator  string
	clientConfig taskingModel.TaskClientConfig

	// timerInterval, timerOneShot, timerHandler what StartTimer handed the timer. Only set once
	// arrangeUnitTestTimerStart has been called and StartTimer has run.
	timerInterval time.Duration
	timerOneShot  bool
	timerHandler  goutils.TimeoutHandler
}

// newUnitTestTriggerParams assemble a complete, valid set of trigger parameters over mocked
// dependencies, with both factories routed through the callback collector.
//
// `Maybe` for the same reason as the runner's: several cases below reject the parameters before
// either factory is reached, and those cases are asserting exactly that.
func newUnitTestTriggerParams(t *testing.T) (maintenance.NewTriggerParams, *unitTestTriggerMocks) {
	mocks := &unitTestTriggerMocks{
		persistence: mocktaskingDB.NewClient(t),
		redis:       mockgoutilsRedis.NewClient(t),
		callbacks:   mocktest.NewUnitTestCallbackCollector(t),
		taskClient:  mocktask.NewClient(t),
		timer:       mockgoutils.NewIntervalTimer(t),
	}

	mocks.callbacks.EXPECT().
		DefineTaskClient(
			mock.Anything,
			mock.Anything,
			mock.Anything,
			mock.Anything,
			mock.Anything,
			mock.Anything,
		).
		RunAndReturn(func(
			_ context.Context,
			clientName string,
			taskCreator string,
			clientConfig taskingModel.TaskClientConfig,
			_ taskingDB.Client,
			_ goutilsRedis.Client,
		) (taskEngine.Client, error) {
			mocks.clientName = clientName
			mocks.taskCreator = taskCreator
			mocks.clientConfig = clientConfig
			return mocks.taskClient, nil
		}).
		Maybe()

	mocks.callbacks.EXPECT().
		DefineIntervalTimer(mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(
			_ context.Context, _ *sync.WaitGroup, _ log.Fields,
		) (goutils.IntervalTimer, error) {
			return mocks.timer, nil
		}).
		Maybe()

	params := maintenance.NewTriggerParams{
		ParentCtx:     context.Background(),
		AppName:       unitTestAppName,
		TaskEngine:    unitTestTaskEngineConfig(),
		Maintenance:   unitTestMaintenanceConfig(),
		Persistence:   mocks.persistence,
		Redis:         mocks.redis,
		ClientFactory: mocks.callbacks.DefineTaskClient,
		TimerFactory:  mocks.callbacks.DefineIntervalTimer,
	}

	return params, mocks
}

// newUnitTestTrigger build a Trigger over the harness parameters, asserting it was accepted.
func newUnitTestTrigger(t *testing.T) (maintenance.Trigger, *unitTestTriggerMocks) {
	assert := assert.New(t)

	params, mocks := newUnitTestTriggerParams(t)

	trigger, err := maintenance.NewTrigger(params)
	assert.Nil(err)
	assert.NotNil(trigger)

	return trigger, mocks
}

// arrangeUnitTestTimerStart capture what StartTimer hands the timer, and choose what the timer
// reports back to it.
func arrangeUnitTestTimerStart(mocks *unitTestTriggerMocks, result error) {
	mocks.timer.EXPECT().
		Start(mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(
			interval time.Duration, handler goutils.TimeoutHandler, oneShot bool,
		) error {
			mocks.timerInterval = interval
			mocks.timerHandler = handler
			mocks.timerOneShot = oneShot
			return result
		}).
		Once()
}

// arrangeUnitTestSubmit capture the submission a maintenance request is made as, and choose what
// the task client reports back. The task entry is returned regardless of `result`, since a client
// that fails at the submit step still has a real task to name.
func arrangeUnitTestSubmit(
	mocks *unitTestTriggerMocks, taskID string, result error,
) *taskEngine.DefineTaskParams {
	captured := &taskEngine.DefineTaskParams{}

	mocks.taskClient.EXPECT().
		DefineAndRunImmediateOneShotTask(mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(
			_ context.Context, params taskEngine.DefineTaskParams, _ taskingDB.Database,
		) (taskingModel.Task, error) {
			*captured = params
			return taskingModel.Task{ID: taskID}, result
		}).
		Once()

	return captured
}

// arrangeUnitTestListTerminal capture the query the clean up pass looks for finished tasks with,
// and choose what it finds. Every tick makes this call, so a case that runs a tick has to arrange
// it whether or not the clean up is what the case is about.
func arrangeUnitTestListTerminal(
	mocks *unitTestTriggerMocks, found []taskingModel.Task, result error,
) *taskingDB.TaskQueryFilter {
	captured := &taskingDB.TaskQueryFilter{}

	mocks.taskClient.EXPECT().
		ListTasks(mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(
			_ context.Context, filters taskingDB.TaskQueryFilter, _ taskingDB.Database,
		) ([]taskingModel.Task, error) {
			*captured = filters
			return found, result
		}).
		Once()

	return captured
}

// unitTestTerminalTask a finished maintenance task last updated `age` ago.
func unitTestTerminalTask(age time.Duration) taskingModel.Task {
	return taskingModel.Task{
		ID:        uuid.NewString(),
		TaskName:  maintenance.MaintenanceTaskName,
		TaskState: taskingModel.TaskStateComplete,
		UpdatedAt: time.Now().UTC().Add(-age),
	}
}

/*
TestNewTrigger validates that a maintenance trigger refuses to be built over parameters it cannot
work with.

Same stake as the runner's equivalent, from the other side: a trigger built over the wrong queue
raises requests nothing is listening for, and a deployment whose maintenance never runs reports
nothing about why.
*/
func TestNewTrigger(t *testing.T) {
	// Case 1: the ordinary path, which the remaining cases are variations on.
	t.Run("builds over complete parameters", func(t *testing.T) {
		assert := assert.New(t)

		params, _ := newUnitTestTriggerParams(t)

		trigger, err := maintenance.NewTrigger(params)
		assert.Nil(err)
		assert.NotNil(trigger)
	})

	// Case 2: every dependency the trigger cannot substitute for. Note the absence of an
	// operator - the trigger asks for sweeps, it never runs one.
	t.Run("rejects a missing dependency", func(t *testing.T) {
		assert := assert.New(t)

		for name, drop := range map[string]func(*maintenance.NewTriggerParams){
			"parent context": func(p *maintenance.NewTriggerParams) { p.ParentCtx = nil },
			"persistence":    func(p *maintenance.NewTriggerParams) { p.Persistence = nil },
			"redis":          func(p *maintenance.NewTriggerParams) { p.Redis = nil },
		} {
			params, _ := newUnitTestTriggerParams(t)
			drop(&params)

			trigger, err := maintenance.NewTrigger(params)
			assert.Nil(trigger, "a trigger missing its %s should not be built", name)
			assert.NotNil(err, "a missing %s should be rejected", name)
		}
	})

	// Case 3: absent factories are the production path, as they are for the runner. This is the
	// only case that runs the real `tasking.NewTaskClient` and the real interval timer, which is
	// what proves the defaults point at something that builds. The client reaches for a REDIS
	// queue handle on the way; `Maybe` because how many it takes is `tasking`'s business.
	t.Run("accepts absent factories", func(t *testing.T) {
		assert := assert.New(t)

		params, mocks := newUnitTestTriggerParams(t)
		params.ClientFactory = nil
		params.TimerFactory = nil

		mocks.redis.EXPECT().
			GetQueueHandle(mock.Anything, mock.Anything).
			Return(mockgoutilsRedis.NewQueue(t), nil).
			Maybe()

		trigger, err := maintenance.NewTrigger(params)
		assert.Nil(err)
		assert.NotNil(trigger)
	})

	// Case 4: the application name is the creator every maintenance task is bound to, so it is
	// held to the same charset as everywhere else it appears.
	t.Run("rejects an invalid application name", func(t *testing.T) {
		assert := assert.New(t)

		for _, broken := range []string{"", "has spaces", "has/slash"} {
			params, _ := newUnitTestTriggerParams(t)
			params.AppName = broken

			trigger, err := maintenance.NewTrigger(params)
			assert.Nil(trigger)
			assert.NotNil(err, "application name '%s' should be rejected", broken)
		}
	})

	// Case 5: config the Task Engine itself would refuse, caught here so the complaint names
	// cairn's own config field.
	t.Run("rejects an invalid Task Engine config", func(t *testing.T) {
		assert := assert.New(t)

		for name, corrupt := range map[string]func(*models.TaskEngineConfig){
			"empty scheduler queue": func(c *models.TaskEngineConfig) { c.SchedulerQueue = "" },
			"queue name charset": func(c *models.TaskEngineConfig) {
				c.SchedulerQueue = "not a name"
			},
			"interval below floor": func(c *models.TaskEngineConfig) {
				c.SchedulerMaintenanceIntSec = 9
			},
		} {
			params, _ := newUnitTestTriggerParams(t)
			broken := unitTestTaskEngineConfig()
			corrupt(&broken)
			params.TaskEngine = broken

			trigger, err := maintenance.NewTrigger(params)
			assert.Nil(trigger, "a trigger with %s should not be built", name)
			assert.NotNil(err, "%s should be rejected", name)
		}
	})

	// Case 6: the maintenance config carries both windows this component keeps - the cadence it
	// raises requests on and how long it keeps their records. A zero interval would be a timer
	// firing continuously, and a zero retention window would clear a sweep's record the moment it
	// finished; neither is a value to default around.
	t.Run("rejects an invalid maintenance config", func(t *testing.T) {
		assert := assert.New(t)

		for name, corrupt := range map[string]func(*models.MaintenanceConfig){
			"sweep interval":        func(c *models.MaintenanceConfig) { c.MaintenanceSweepIntSec = 0 },
			"object grace window":   func(c *models.MaintenanceConfig) { c.OrphanedObjectAgeOutSec = 0 },
			"task retention window": func(c *models.MaintenanceConfig) { c.TerminalTaskAgeOutSec = 0 },
		} {
			broken := unitTestMaintenanceConfig()
			corrupt(&broken)

			params, _ := newUnitTestTriggerParams(t)
			params.Maintenance = broken

			trigger, err := maintenance.NewTrigger(params)
			assert.Nil(trigger)
			assert.NotNil(err, "a zero %s should be rejected", name)
		}
	})

	// Case 7: a factory that fails takes the whole construction down, rather than leaving a
	// trigger holding one live component and one nil.
	t.Run("surfaces a component that fails to build", func(t *testing.T) {
		assert := assert.New(t)

		buildFailure := errors.New("redis is unreachable")

		t.Run("task client", func(t *testing.T) {
			params, _ := newUnitTestTriggerParams(t)
			params.ClientFactory = func(
				context.Context,
				string,
				string,
				taskingModel.TaskClientConfig,
				taskingDB.Client,
				goutilsRedis.Client,
			) (taskEngine.Client, error) {
				return nil, buildFailure
			}

			trigger, err := maintenance.NewTrigger(params)
			assert.Nil(trigger)
			assert.NotNil(err)
			assert.Contains(err.Error(), buildFailure.Error())
		})

		t.Run("timer", func(t *testing.T) {
			params, _ := newUnitTestTriggerParams(t)
			params.TimerFactory = func(
				context.Context, *sync.WaitGroup, log.Fields,
			) (goutils.IntervalTimer, error) {
				return nil, buildFailure
			}

			trigger, err := maintenance.NewTrigger(params)
			assert.Nil(trigger)
			assert.NotNil(err)
			assert.Contains(err.Error(), buildFailure.Error())
		})
	})
}

/*
TestNewTriggerClientWiring validates what the maintenance task client is defined with.

Two things with no visible symptom until it is too late to notice. A client pointed at the wrong
scheduler queue posts requests where nothing is reading, and every submission succeeds. A client
bound to the wrong creator submits fine too, but scopes every read and delete it can perform to a
name this deployment does not own - which is what a later reaper would work through.
*/
func TestNewTriggerClientWiring(t *testing.T) {
	// Case 1: the queue and the creator, both taken from what already identifies this deployment.
	t.Run("binds the client to this deployment's queue and creator", func(t *testing.T) {
		assert := assert.New(t)

		_, mocks := newUnitTestTrigger(t)

		assert.Equal(
			unitTestTaskEngineConfig().SchedulerQueue, mocks.clientConfig.SchedulerQueue,
		)
		assert.Equal(unitTestAppName, mocks.taskCreator)
		assert.Contains(
			mocks.clientName,
			unitTestAppName,
			"the client's name is what an operator sees in the engine's logs",
		)
	})

	// Case 2: no task queues. The client has no execution processor to name in one, and needs
	// none - each submission states its own retry policy instead, which is what the queue
	// definitions would otherwise have supplied.
	t.Run("names no task queues", func(t *testing.T) {
		assert := assert.New(t)

		_, mocks := newUnitTestTrigger(t)

		assert.Empty(mocks.clientConfig.TaskQueues)
	})
}

/*
TestTriggerRequestMaintenance validates a single maintenance request.

The request is the whole of the contract between the two halves of the integration: the task name
is what routes it to the processor the runner registered, and the deadline is what keeps a
request that has been overtaken from running a sweep nobody is waiting on any more.
*/
func TestTriggerRequestMaintenance(t *testing.T) {
	// Case 1: the submission itself.
	t.Run("submits the maintenance task", func(t *testing.T) {
		assert := assert.New(t)

		trigger, mocks := newUnitTestTrigger(t)

		taskID := uuid.NewString()
		submitted := arrangeUnitTestSubmit(mocks, taskID, nil)

		before := time.Now().UTC()
		observedID, err := trigger.RequestMaintenance(context.Background())
		after := time.Now().UTC()

		assert.Nil(err)
		assert.Equal(taskID, observedID)

		// The name the runner's processor is registered under. Anything else is accepted by the
		// engine and then never routed anywhere.
		assert.Equal(maintenance.MaintenanceTaskName, submitted.Name)

		// The retry policy is stated per submission, since the client was given no queue
		// definitions to read one from. Zero retries, matching what the task definition declares
		// and what the processor - which reports every failure as non-recoverable - can honor.
		assert.NotNil(submitted.Retry)
		assert.Equal(0, submitted.Retry.MaxRetries)
		assert.GreaterOrEqual(submitted.Retry.Factor, float64(1))

		// One and a half sweep intervals out, bracketed by the instants either side of the call
		// rather than compared against a clock read here.
		margin := unitTestMaintenanceConfig().MaintenanceSweepInt() * 3 / 2
		assert.NotNil(submitted.Deadline)
		assert.False(
			submitted.Deadline.Before(before.Add(margin)),
			"the deadline must be at least one and a half sweep intervals out",
		)
		assert.False(
			submitted.Deadline.After(after.Add(margin)),
			"the deadline must be no more than one and a half sweep intervals out",
		)

		// Nothing is carried for the processor to read - it takes the sweep's timestamp at
		// execution instead, so an iteration is judged against when it ran.
		assert.Nil(submitted.Parameters)
	})

	// Case 2: a failed submission still names the task it failed on. The client defines the task
	// and then hands it to the scheduler, so a failure at the second step leaves a real entry
	// behind - reporting only an error would strand it unnamed.
	t.Run("names the task even when the submission fails", func(t *testing.T) {
		assert := assert.New(t)

		trigger, mocks := newUnitTestTrigger(t)

		taskID := uuid.NewString()
		submitFailure := errors.New("scheduler queue is unreachable")
		arrangeUnitTestSubmit(mocks, taskID, submitFailure)

		observedID, err := trigger.RequestMaintenance(context.Background())
		assert.NotNil(err)
		assert.Contains(err.Error(), submitFailure.Error())
		assert.Equal(taskID, observedID)
	})
}

/*
TestTriggerStartTimer validates the periodic half of the trigger.

The timer is the only thing that makes maintenance happen unattended, so what is asserted here is
that it repeats, that it repeats at the configured cadence, and that a tick does what a tick is
for.
*/
func TestTriggerStartTimer(t *testing.T) {
	// Case 1: the cadence, and that it recurs. A one-shot timer would run maintenance once at
	// startup and then never again, which no error anywhere would report.
	t.Run("runs at the sweep interval, repeatedly", func(t *testing.T) {
		assert := assert.New(t)

		trigger, mocks := newUnitTestTrigger(t)
		arrangeUnitTestTimerStart(mocks, nil)

		assert.Nil(trigger.StartTimer(context.Background()))

		assert.Equal(unitTestMaintenanceConfig().MaintenanceSweepInt(), mocks.timerInterval)
		assert.False(mocks.timerOneShot, "the sweep timer must keep firing")
		assert.NotNil(mocks.timerHandler)
	})

	// Case 2: one tick, one request - run here rather than waited for, which is what the
	// injected timer is for.
	t.Run("submits one maintenance request per tick", func(t *testing.T) {
		assert := assert.New(t)

		trigger, mocks := newUnitTestTrigger(t)
		arrangeUnitTestTimerStart(mocks, nil)
		assert.Nil(trigger.StartTimer(context.Background()))

		submitted := arrangeUnitTestSubmit(mocks, uuid.NewString(), nil)
		arrangeUnitTestListTerminal(mocks, nil, nil)

		assert.Nil(mocks.timerHandler())
		assert.Equal(maintenance.MaintenanceTaskName, submitted.Name)
		assert.NotNil(submitted.Deadline)
	})

	// Case 3: a failed submission is handed back to the timer rather than absorbed, which is
	// what has the timer log it in full. The timer keeps ticking either way - one lost sweep is
	// re-derived by the next, since every iteration works from durable state.
	t.Run("hands a failed submission back to the timer", func(t *testing.T) {
		assert := assert.New(t)

		trigger, mocks := newUnitTestTrigger(t)
		arrangeUnitTestTimerStart(mocks, nil)
		assert.Nil(trigger.StartTimer(context.Background()))

		submitFailure := errors.New("scheduler queue is unreachable")
		arrangeUnitTestSubmit(mocks, uuid.NewString(), submitFailure)
		arrangeUnitTestListTerminal(mocks, nil, nil)

		err := mocks.timerHandler()
		assert.NotNil(err)
		assert.Contains(err.Error(), submitFailure.Error())
	})

	// Case 4: a timer that will not start is a startup failure. Reporting nothing would leave a
	// process serving requests with maintenance silently switched off.
	t.Run("reports a timer that will not start", func(t *testing.T) {
		assert := assert.New(t)

		trigger, mocks := newUnitTestTrigger(t)

		startFailure := errors.New("timer is already running")
		arrangeUnitTestTimerStart(mocks, startFailure)

		err := trigger.StartTimer(context.Background())
		assert.NotNil(err)
		assert.Contains(err.Error(), startFailure.Error())
	})
}

/*
TestTriggerStopTimer validates the periodic half coming to rest.

Stopping has two halves to it: the timer stops firing, and a submission already under way is cut
short rather than held for however long the Task Engine takes to answer it. What is deliberately
not stopped is the on-demand path.
*/
func TestTriggerStopTimer(t *testing.T) {
	// Case 1: the ordinary path.
	t.Run("stops the timer", func(t *testing.T) {
		assert := assert.New(t)

		trigger, mocks := newUnitTestTrigger(t)
		mocks.timer.EXPECT().Stop().Return(nil).Once()

		assert.Nil(trigger.StopTimer(context.Background()))
	})

	// Case 2: a timer that fails to stop says so, rather than shutdown reporting a clean stop it
	// did not get.
	t.Run("reports a timer that will not stop", func(t *testing.T) {
		assert := assert.New(t)

		trigger, mocks := newUnitTestTrigger(t)

		stopFailure := errors.New("timer loop did not exit")
		mocks.timer.EXPECT().Stop().Return(stopFailure).Once()

		err := trigger.StopTimer(context.Background())
		assert.NotNil(err)
		assert.Contains(err.Error(), stopFailure.Error())
	})

	// Case 3: the two contexts, which is the whole reason the trigger holds one of its own. A
	// tick's context ends with the timer, so a submission that outlives the stop is cut short -
	// while an on-demand request, which runs on its caller's context, still goes through.
	t.Run("ends the timer's work without ending on-demand requests", func(t *testing.T) {
		assert := assert.New(t)

		trigger, mocks := newUnitTestTrigger(t)
		arrangeUnitTestTimerStart(mocks, nil)
		assert.Nil(trigger.StartTimer(context.Background()))

		// Take hold of the context a tick submits under, by running one.
		var tickCtx context.Context
		mocks.taskClient.EXPECT().
			DefineAndRunImmediateOneShotTask(mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(
				ctx context.Context, _ taskEngine.DefineTaskParams, _ taskingDB.Database,
			) (taskingModel.Task, error) {
				tickCtx = ctx
				return taskingModel.Task{ID: uuid.NewString()}, nil
			}).
			Once()
		arrangeUnitTestListTerminal(mocks, nil, nil)

		assert.Nil(mocks.timerHandler())
		assert.NotNil(tickCtx)
		assert.Nil(tickCtx.Err(), "a tick's context is live while the timer is running")

		mocks.timer.EXPECT().Stop().Return(nil).Once()
		assert.Nil(trigger.StopTimer(context.Background()))

		assert.NotNil(tickCtx.Err(), "stopping the timer must end the work it started")

		// The on-demand path is untouched by any of that.
		taskID := uuid.NewString()
		arrangeUnitTestSubmit(mocks, taskID, nil)

		observedID, err := trigger.RequestMaintenance(context.Background())
		assert.Nil(err)
		assert.Equal(taskID, observedID)
	})
}

/*
TestTriggerCleanupTerminalTasks validates the second thing a tick does.

Every sweep leaves a task entry behind, so this is what keeps the Task Engine's database from
growing for the life of the deployment (DESIGN §8.3.2). Two halves matter equally: that what has
aged out goes, and that what has not is left where an operator can still read it.
*/
func TestTriggerCleanupTerminalTasks(t *testing.T) {
	// startTickedTrigger a trigger with its timer running and the tick's submission already
	// arranged, leaving a case free to say only what the clean up should find and do.
	startTickedTrigger := func(t *testing.T) (*unitTestTriggerMocks, goutils.TimeoutHandler) {
		assert := assert.New(t)

		trigger, mocks := newUnitTestTrigger(t)
		arrangeUnitTestTimerStart(mocks, nil)
		assert.Nil(trigger.StartTimer(context.Background()))
		arrangeUnitTestSubmit(mocks, uuid.NewString(), nil)

		return mocks, mocks.timerHandler
	}

	// Case 1: the query. Its job is to reach every finished maintenance task of this
	// deployment's and nothing else - a filter that is too wide would have the pass deleting
	// entries that are not its to delete.
	t.Run("queries for this deployment's finished maintenance tasks", func(t *testing.T) {
		assert := assert.New(t)

		mocks, tick := startTickedTrigger(t)
		filter := arrangeUnitTestListTerminal(mocks, nil, nil)

		assert.Nil(tick())

		assert.Equal([]string{maintenance.MaintenanceTaskName}, filter.TaskNames)
		assert.ElementsMatch(
			[]taskingModel.TaskStateENUM{
				taskingModel.TaskStateComplete,
				taskingModel.TaskStateFailed,
				taskingModel.TaskStateTimeout,
				taskingModel.TaskStateCancelled,
			},
			filter.TaskStates,
			"only these four states may be deleted, and all four should be reached",
		)

		// Left to the client to fill in with its own creator. Naming one here could only be a
		// chance to name a deployment's whose tasks are not ours to delete.
		assert.Empty(filter.Creators)

		// Bounded, so the first pass after a long outage reads a page rather than the table.
		assert.NotNil(filter.Limit)
		assert.Greater(*filter.Limit, 0)
	})

	// Case 2: the age out itself. The mock fails on an unarranged delete, so the half of this
	// that says the young entry is left alone is asserted by the absence of an arrangement for
	// it rather than by anything written below.
	t.Run("deletes only what has aged out", func(t *testing.T) {
		assert := assert.New(t)

		mocks, tick := startTickedTrigger(t)

		ageOut := unitTestMaintenanceConfig().TerminalTaskAgeOut()
		aged := unitTestTerminalTask(ageOut * 2)
		atTheEdge := unitTestTerminalTask(ageOut + time.Minute)
		young := unitTestTerminalTask(ageOut / 2)

		arrangeUnitTestListTerminal(
			mocks, []taskingModel.Task{aged, young, atTheEdge}, nil,
		)

		deleted := []string{}
		for _, expected := range []taskingModel.Task{aged, atTheEdge} {
			mocks.taskClient.EXPECT().
				DeleteTask(mock.Anything, expected.ID, mock.Anything).
				RunAndReturn(func(_ context.Context, taskID string, _ taskingDB.Database) error {
					deleted = append(deleted, taskID)
					return nil
				}).
				Once()
		}

		assert.Nil(tick())
		assert.ElementsMatch([]string{aged.ID, atTheEdge.ID}, deleted)
	})

	// Case 3: a failed query ends the pass. With nothing listed there is nothing to act on, and
	// deleting on a guess is not an option - no delete is arranged, so an attempt fails here.
	t.Run("deletes nothing when the query fails", func(t *testing.T) {
		assert := assert.New(t)

		mocks, tick := startTickedTrigger(t)

		listFailure := errors.New("task engine database is unreachable")
		arrangeUnitTestListTerminal(mocks, nil, listFailure)

		err := tick()
		assert.NotNil(err)
		assert.Contains(err.Error(), listFailure.Error())
	})

	// Case 4: one entry that refuses to go must not take the rest of the batch with it, which is
	// why each delete stands on its own. The failure is still reported - an entry that cannot be
	// deleted is a real fault, and a pass that kept quiet would let the table grow with nothing
	// saying so.
	t.Run("carries on past a delete that fails", func(t *testing.T) {
		assert := assert.New(t)

		mocks, tick := startTickedTrigger(t)

		ageOut := unitTestMaintenanceConfig().TerminalTaskAgeOut()
		stuck := unitTestTerminalTask(ageOut * 3)
		following := unitTestTerminalTask(ageOut * 2)

		arrangeUnitTestListTerminal(mocks, []taskingModel.Task{stuck, following}, nil)

		deleteFailure := errors.New("entry is referenced elsewhere")
		mocks.taskClient.EXPECT().
			DeleteTask(mock.Anything, stuck.ID, mock.Anything).
			Return(deleteFailure).
			Once()
		mocks.taskClient.EXPECT().
			DeleteTask(mock.Anything, following.ID, mock.Anything).
			Return(nil).
			Once()

		err := tick()
		assert.NotNil(err)
		assert.Contains(err.Error(), deleteFailure.Error())
		assert.Contains(err.Error(), stuck.ID)
	})

	// Case 5: the ordering requirement, stated as a test. An engine that will not take new work
	// is exactly when its old entries least deserve to be kept, so a failed submission must not
	// carry the clean up down with it - and neither failure may hide the other.
	t.Run("cleans up even when the submission failed", func(t *testing.T) {
		assert := assert.New(t)

		trigger, mocks := newUnitTestTrigger(t)
		arrangeUnitTestTimerStart(mocks, nil)
		assert.Nil(trigger.StartTimer(context.Background()))

		submitFailure := errors.New("scheduler queue is unreachable")
		arrangeUnitTestSubmit(mocks, uuid.NewString(), submitFailure)

		aged := unitTestTerminalTask(unitTestMaintenanceConfig().TerminalTaskAgeOut() * 2)
		arrangeUnitTestListTerminal(mocks, []taskingModel.Task{aged}, nil)

		deleteFailure := errors.New("entry is referenced elsewhere")
		mocks.taskClient.EXPECT().
			DeleteTask(mock.Anything, aged.ID, mock.Anything).
			Return(deleteFailure).
			Once()

		err := mocks.timerHandler()
		assert.NotNil(err)
		assert.Contains(err.Error(), submitFailure.Error())
		assert.Contains(err.Error(), deleteFailure.Error())
	})
}

// Package test - various support components used in unit-testing.
package test

import (
	"context"
	"sync"

	"github.com/alwitt/goutils"
	goutilsRedis "github.com/alwitt/goutils/redis"
	"github.com/alwitt/goutils/runtime"
	taskingDB "github.com/alwitt/tasking/db"
	taskingModel "github.com/alwitt/tasking/models"
	taskEngine "github.com/alwitt/tasking/task"
	"github.com/apex/log"
)

// UnitTestCallbackCollector unit-testing interface for collecting callbacks
type UnitTestCallbackCollector interface {
	// EstimateMIMEType called when artifact manager to need to estimate data MIME type
	EstimateMIMEType(data []byte) string

	// DefineSystemCallDockerRuntime called when the artifact operator needs to define a
	// docker container runtime to run a sidecar with
	DefineSystemCallDockerRuntime(
		ctx context.Context,
		name string,
		command runtime.ContainerCommand,
		params runtime.DockerRuntimeParams,
		clearANSIFromOutput bool,
	) (runtime.SystemCallRuntime, error)

	// DefineTaskReceiver called when the maintenance runner needs to define the `tasking` Task
	// Engine worker's request receiver
	DefineTaskReceiver(
		parentCtx context.Context,
		receiverConfig taskingModel.TaskReceiverConfig,
		dbClient taskingDB.Client,
		redisClient goutilsRedis.Client,
		onFatalCB taskingModel.OnFatalCB,
	) (taskEngine.Receiver, error)

	// DefineTaskScheduler called when the maintenance runner needs to define the `tasking` Task
	// Engine work scheduler
	DefineTaskScheduler(
		parentCtx context.Context,
		schedulerConfig taskingModel.TaskSchedulerConfig,
		dbClient taskingDB.Client,
		redisClient goutilsRedis.Client,
		onFatalCB taskingModel.OnFatalCB,
	) (taskEngine.Scheduler, error)

	// DefineTaskClient called when the maintenance trigger needs to define the `tasking` Task
	// Engine client it submits maintenance requests through
	DefineTaskClient(
		parentCtx context.Context,
		clientName string,
		taskCreator string,
		clientConfig taskingModel.TaskClientConfig,
		dbClient taskingDB.Client,
		redisClient goutilsRedis.Client,
	) (taskEngine.Client, error)

	// DefineIntervalTimer called when the maintenance trigger needs to define the timer its
	// periodic maintenance requests are submitted on
	DefineIntervalTimer(
		rootCtx context.Context, wg *sync.WaitGroup, logTags log.Fields,
	) (goutils.IntervalTimer, error)
}

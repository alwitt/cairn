// Package app - application entry points
package app //revive:disable-line:var-naming

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/alwitt/cairn/api"
	"github.com/alwitt/cairn/artifact"
	"github.com/alwitt/cairn/db"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/cairn/workspace"
	"github.com/alwitt/goutils"
	"github.com/alwitt/goutils/runtime"
	"github.com/apex/log"
	"gorm.io/gorm/logger"
)

// Server `cairn` server
type Server interface {
	/*
		Start the server and its components

			@param ctx context.Context - execution context
			@param serverErrors chan error - channel used to broadcast a fatal runtime
				failure (e.g. one of the HTTP servers failing on ListenAndServe) back to
				the caller so it can trigger shutdown
	*/
	Start(ctx context.Context, serverErrors chan error) error

	/*
		Stop shutdown the server and its components

			@param ctx context.Context - execution context
	*/
	Stop(ctx context.Context) error
}

// serverImpl implements `Server`
type serverImpl struct {
	goutils.Component

	// parentCtx parent execution context for all running tasks
	parentCtx context.Context

	// persistence application persistence
	persistence db.Client

	// workspaceMgr workspace manager
	workspaceMgr workspace.Manager

	// artifactMgr artifact manager
	artifactMgr artifact.Manager

	// artifactOperator artifact operator
	artifactOperator artifact.Operator

	// mainServer primary API server
	mainServer *http.Server

	// metricsServer metrics server
	metricsServer *http.Server

	wg sync.WaitGroup
}

/*
Start the server and its components

	@param ctx context.Context - execution context
	@param serverErrors chan error - channel used to broadcast a fatal runtime
		failure (e.g. one of the HTTP servers failing on ListenAndServe) back to
		the caller so it can trigger shutdown
*/
func (s *serverImpl) Start(_ context.Context, serverErrors chan error) error {
	// Start API server
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		logTags := s.GetLogTagsForContext(s.parentCtx)
		if err := s.mainServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.
				WithError(err).
				WithFields(goutils.UpdateCodePositionInTags(logTags)).
				Error("REST API Server failure")
			serverErrors <- goutils.NewRuntimeError("REST API Server failure", err, true)
		}
	}()

	// Start Metrics server
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		logTags := s.GetLogTagsForContext(s.parentCtx)
		if err := s.metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.
				WithError(err).
				WithFields(goutils.UpdateCodePositionInTags(logTags)).
				Error("Metrics API Server failure")
			serverErrors <- goutils.NewRuntimeError("Metrics API Server failure", err, true)
		}
	}()

	return nil
}

/*
Stop shutdown the server and its components

	@param ctx context.Context - execution context
*/
func (s *serverImpl) Stop(ctx context.Context) error {
	lclCtx, lclCtxCancel := context.WithTimeout(ctx, time.Second*15)
	defer lclCtxCancel()

	// Gracefully stop the HTTP servers so their ListenAndServe goroutines return. Attempt
	// both even if the first fails so neither server is left running.
	mainErr := s.mainServer.Shutdown(lclCtx)
	metricsErr := s.metricsServer.Shutdown(lclCtx)
	// Wait for all threads to stop
	waitErr := goutils.TimeBoundedWaitGroupWait(lclCtx, &s.wg, time.Second*5)

	allErrors := []error{}
	if mainErr != nil {
		allErrors = append(
			allErrors, models.NewShutdownError("Failed to stop REST API server", mainErr, true),
		)
	}
	if metricsErr != nil {
		allErrors = append(
			allErrors, models.NewShutdownError("Failed to stop metrics server", metricsErr, true),
		)
	}
	if waitErr != nil {
		allErrors = append(
			allErrors, models.NewShutdownError("Daemon tasks did not stop in time", waitErr, true),
		)
	}

	return errors.Join(allErrors...)
}

/*
BuildNewServer build new cairn server

	@param parentCtx context.Context - parent execution context for all running tasks
	@param configs models.ApplicationConfig - server config
	@returns new server
*/
func BuildNewServer(parentCtx context.Context, configs models.ApplicationConfig) (Server, error) {
	// ------------------------------------------------------------------------------------
	// Build persistence client

	// Prepare persistence
	psqlDial, err := db.GetPostgresDialector(configs.Persistence.SQL.Application)
	if err != nil {
		return nil, models.NewBootStrapError("Failed to parse DB persistence parameters", err, true)
	}
	persistence, err := db.NewConnection(psqlDial, logger.Error)
	if err != nil {
		return nil, models.NewBootStrapError("Failed to prepare DB persistence client", err, true)
	}

	// Prepare S3 client
	s3Manager, err := goutils.NewS3ClientManager(
		configs.Artifact.ObjectStore.ToStandard(), configs.Artifact.ObjectStore.ClientTTL(),
	)
	if err != nil {
		return nil, models.NewBootStrapError("Failed to prepare S3 client manager", err, true)
	}

	// Prepare volume manager
	var volume runtime.VolumeManager
	switch configs.Workspace.VolumeType {
	case models.WorkspaceVolumeTypeDocker:
		volume, err = runtime.NewDockerVolumeManager(parentCtx)
		if err != nil {
			return nil, models.NewBootStrapError("Failed to prepare docker volume manager", err, true)
		}

	default:
		return nil, models.NewBootStrapError(
			"Supported persistence volume type '"+string(configs.Workspace.VolumeType)+"'", nil, true,
		)
	}

	// ------------------------------------------------------------------------------------
	// Build core

	// Build workspace manager
	workspaceMgr, err := workspace.NewManager(configs.AppName, persistence, volume)
	if err != nil {
		return nil, models.NewBootStrapError("Failed to prepare workspace manager", err, true)
	}

	// Build artifact manager
	artifactMgr, err := artifact.NewManager(
		configs.AppName,
		persistence,
		s3Manager,
		configs.Artifact.Storage,
		artifact.DefaultMIMETypeDetector,
	)
	if err != nil {
		return nil, models.NewBootStrapError("Failed to prepare artifact manager", err, true)
	}

	// Build artifact operator in support of the artifact manager
	var artifactOperator artifact.Operator
	switch configs.Workspace.VolumeType {
	case models.WorkspaceVolumeTypeDocker:
		artifactOperator, err = artifact.NewDockerOperator(
			configs.AppName, artifactMgr, configs.Artifact.Sidecar, runtime.NewDockerSystemCallRuntime,
		)
		if err != nil {
			return nil, models.NewBootStrapError("Failed to prepare Docker artifact operator", err, true)
		}

	default:
		return nil, models.NewBootStrapError(
			"Supported persistence volume type '"+string(configs.Workspace.VolumeType)+"'", nil, true,
		)
	}

	// ------------------------------------------------------------------------------------
	// Build metrics server

	metricsCollector, err := goutils.GetNewMetricsCollector(
		log.Fields{"package": "goutils", "module": "utils", "component": "metrics-core"},
		[]goutils.LogMetadataModifier{},
	)
	if err != nil {
		return nil, models.NewBootStrapError("Failed to create metrics collector", err, true)
	}

	if configs.Metrics.Features.EnableAppMetrics {
		metricsCollector.InstallApplicationMetrics()
	}

	var httpMetricsAgent goutils.HTTPRequestMetricHelper
	if configs.Metrics.Features.EnableHTTPMetrics {
		httpMetricsAgent = metricsCollector.InstallHTTPMetrics()
	}

	// Build metrics hosting server
	metricsServer := api.BuildMetricsCollectionServer(
		configs.Metrics.Server,
		metricsCollector,
		configs.Metrics.MetricsEndpoint,
		configs.Metrics.MaxRequests,
	)

	// ------------------------------------------------------------------------------------
	// Build API server

	apiServer, err := api.BuildHTTPServer(
		configs.AppName,
		configs.API,
		configs.Artifact.Storage,
		persistence,
		workspaceMgr,
		artifactMgr,
		artifactOperator,
		httpMetricsAgent,
	)
	if err != nil {
		return nil, models.NewBootStrapError("Failed to create API server", err, true)
	}

	return &serverImpl{
		Component: goutils.Component{
			LogTags: log.Fields{"module": "main", "component": "server"},
			LogTagModifiers: []goutils.LogMetadataModifier{
				goutils.ModifyLogMetadataByRestRequestParam,
			},
		},
		parentCtx:        parentCtx,
		persistence:      persistence,
		workspaceMgr:     workspaceMgr,
		artifactMgr:      artifactMgr,
		artifactOperator: artifactOperator,
		mainServer:       apiServer,
		metricsServer:    metricsServer,
		wg:               sync.WaitGroup{},
	}, nil
}

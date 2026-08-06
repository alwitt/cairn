// Package api - application REST API
package api //revive:disable-line:var-naming

import (
	"fmt"
	"net/http"
	"time"

	"github.com/alwitt/cairn/artifact"
	"github.com/alwitt/cairn/db"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/cairn/workspace"
	"github.com/alwitt/goutils"
	"github.com/gorilla/mux"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/cors"
)

/*
BuildMetricsCollectionServer create server to host metrics collection endpoint

	@param httpCfg common.HTTPServerConfig - HTTP server configuration
	@param metricsCollector goutils.MetricsCollector - metrics collector
	@param collectionEndpoint string - endpoint to expose the metrics on
	@param maxRESTRequests int - max number fo parallel requests to support
	@returns HTTP server instance
*/
func BuildMetricsCollectionServer(
	httpCfg models.HTTPServerConfig,
	metricsCollector goutils.MetricsCollector,
	collectionEndpoint string,
	maxRESTRequests int,
) *http.Server {
	router := mux.NewRouter()
	metricsCollector.ExposeCollectionEndpoint(router, collectionEndpoint, maxRESTRequests)

	serverListen := fmt.Sprintf(
		"%s:%d", httpCfg.ListenOn, httpCfg.Port,
	)
	httpSrv := &http.Server{
		Addr:         serverListen,
		WriteTimeout: time.Second * time.Duration(httpCfg.Timeouts.WriteTimeout),
		ReadTimeout:  time.Second * time.Duration(httpCfg.Timeouts.ReadTimeout),
		IdleTimeout:  time.Second * time.Duration(httpCfg.Timeouts.IdleTimeout),
		Handler:      router,
	}
	httpSrv.Protocols = new(http.Protocols)
	httpSrv.Protocols.SetHTTP1(true)
	httpSrv.Protocols.SetUnencryptedHTTP2(true)

	return httpSrv
}

/*
BuildHTTPServer create the application's REST API server

	@param appName string - the per-deployment application name which namespaces this
	    deployment's persistent volumes
	@param httpCfg common.APIServerConfig - HTTP API / server configuration
	@param storeConfig common.ArtifactStorageConfig - artifact storage config
	@param persistence db.Client - DB persistence layer
	@param workspaces workspace.Manager - workspace manager
	@param artifacts artifact.Manager - artifact manager
	@param artifactOps artifact.Operator - artifact operator
	@param metrics goutils.HTTPRequestMetricHelper - metric collection agent
	@returns HTTP server instance
*/
func BuildHTTPServer(
	appName string,
	httpCfg models.APIServerConfig,
	storeConfig models.ArtifactStorageConfig,
	persistence db.Client,
	workspaces workspace.Manager,
	artifacts artifact.Manager,
	artifactOps artifact.Operator,
	metrics goutils.HTTPRequestMetricHelper,
) (*http.Server, error) {
	livenessAPI := NewLivenessHandler(persistence, httpCfg.APIs.RequestLogging, metrics)

	workspaceAPI, err := NewWorkspaceAPIHandler(
		appName, workspaces, httpCfg.APIs.RequestLogging, metrics,
	)
	if err != nil {
		return nil, goutils.NewRuntimeError(
			"failed to define workspace management API handler", err, true,
		)
	}

	artifactAPI, err := NewArtifactAPIHandler(
		appName, workspaces, artifacts, artifactOps, storeConfig,
		httpCfg.APIs.RequestLogging, metrics,
	)
	if err != nil {
		return nil, goutils.NewRuntimeError(
			"failed to define artifact management API handler", err, true,
		)
	}

	router := mux.NewRouter()
	mainRouter := registerPathPrefix(router, httpCfg.APIs.Endpoint.PathPrefix, nil)
	livenessRouter := registerPathPrefix(mainRouter, "/liveness", nil)
	v1Router := registerPathPrefix(mainRouter, "/v1", nil)

	// --------------------------------------------------------------------------------
	// Health check

	_ = registerPathPrefix(livenessRouter, "/alive", map[string]http.HandlerFunc{
		"get": livenessAPI.Alive,
	})
	_ = registerPathPrefix(livenessRouter, "/ready", map[string]http.HandlerFunc{
		"get": livenessAPI.Ready,
	})

	// --------------------------------------------------------------------------------
	// Workspace
	//
	// Each method map is wrapped in the middleware of the API handler it belongs to. The `/v1`
	// tree is shared by two handlers, and the request logging middleware records each request
	// under the log tags of the handler it was built from, so a router level `Use` here would
	// attribute every request beneath it to whichever handler installed it.

	workspacesRouter := registerPathPrefix(
		v1Router, "/workspaces", withMiddleware(workspaceAPI.RestAPIHandler, methodHandlers{
			"post": workspaceAPI.DefineNewWorkspace,
			"get":  workspaceAPI.ListWorkspaces,
		}),
	)

	perWorkspaceRouter := registerPathPrefix(
		workspacesRouter, "/{workspaceID}", withMiddleware(
			workspaceAPI.RestAPIHandler, methodHandlers{
				"get":    workspaceAPI.GetWorkspace,
				"delete": workspaceAPI.DeleteWorkspace,
			},
		),
	)

	// Basic attribute update
	_ = registerPathPrefix(
		perWorkspaceRouter, "/name", withMiddleware(
			workspaceAPI.RestAPIHandler, methodHandlers{
				"put": workspaceAPI.UpdateWorkspaceName,
			},
		),
	)
	_ = registerPathPrefix(
		perWorkspaceRouter, "/description", withMiddleware(
			workspaceAPI.RestAPIHandler, methodHandlers{
				"put": workspaceAPI.UpdateWorkspaceDescription,
			},
		),
	)
	_ = registerPathPrefix(
		perWorkspaceRouter, "/volume-metadata", withMiddleware(
			workspaceAPI.RestAPIHandler, methodHandlers{
				"put": workspaceAPI.UpdateWorkspaceVolumeMeta,
			},
		),
	)

	// Persistent volume life cycle management
	_ = registerPathPrefix(
		perWorkspaceRouter, "/volume", withMiddleware(
			workspaceAPI.RestAPIHandler, methodHandlers{
				"post":   workspaceAPI.SetupWorkspaceVolume,
				"delete": workspaceAPI.TeardownWorkspaceVolume,
			},
		),
	)

	// --------------------------------------------------------------------------------
	// Artifact - workspace scoped operations
	//
	// These hang off the workspace they belong to: a staging key is namespaced by workspace, a
	// listing is workspace scoped, and both creation paths need the parent to write into.

	_ = registerPathPrefix(
		perWorkspaceRouter, "/new-staging", withMiddleware(
			artifactAPI.RestAPIHandler, methodHandlers{
				"post": artifactAPI.NewStagingUpload,
			},
		),
	)
	_ = registerPathPrefix(
		perWorkspaceRouter, "/artifacts", withMiddleware(
			artifactAPI.RestAPIHandler, methodHandlers{
				"post": artifactAPI.RegisterArtifact,
				"get":  artifactAPI.ListArtifacts,
			},
		),
	)
	_ = registerPathPrefix(
		perWorkspaceRouter, "/artifact-from-volume", withMiddleware(
			artifactAPI.RestAPIHandler, methodHandlers{
				"post": artifactAPI.SaveArtifactFromVolume,
			},
		),
	)

	// --------------------------------------------------------------------------------
	// Artifact - per artifact operations
	//
	// Addressed by artifact ID alone, with no workspace in the path: an artifact ID is unique
	// across the deployment, and the operations that do need a parent workspace resolve it from
	// the entry rather than from the caller (see DESIGN §7.1).

	artifactsRouter := registerPathPrefix(v1Router, "/artifacts", nil)

	perArtifactRouter := registerPathPrefix(
		artifactsRouter, "/{artifactID}", withMiddleware(
			artifactAPI.RestAPIHandler, methodHandlers{
				"get":    artifactAPI.GetArtifact,
				"delete": artifactAPI.DeleteArtifact,
			},
		),
	)

	// Basic attribute update
	_ = registerPathPrefix(
		perArtifactRouter, "/name", withMiddleware(
			artifactAPI.RestAPIHandler, methodHandlers{
				"put": artifactAPI.UpdateArtifactName,
			},
		),
	)
	_ = registerPathPrefix(
		perArtifactRouter, "/description", withMiddleware(
			artifactAPI.RestAPIHandler, methodHandlers{
				"put": artifactAPI.UpdateArtifactDescription,
			},
		),
	)

	// Content transfer
	_ = registerPathPrefix(
		perArtifactRouter, "/content", withMiddleware(
			artifactAPI.RestAPIHandler, methodHandlers{
				"put": artifactAPI.UpdateArtifactContent,
			},
		),
	)
	_ = registerPathPrefix(
		perArtifactRouter, "/load-in-volume", withMiddleware(
			artifactAPI.RestAPIHandler, methodHandlers{
				"post": artifactAPI.LoadArtifactInVolume,
			},
		),
	)
	_ = registerPathPrefix(
		perArtifactRouter, "/update-from-volume", withMiddleware(
			artifactAPI.RestAPIHandler, methodHandlers{
				"post": artifactAPI.UpdateArtifactFromVolume,
			},
		),
	)

	// --------------------------------------------------------------------------------
	// MCP endpoint
	//
	// The agent facing surface (see DESIGN §7.2). It carries its own receiving middleware
	// rather than being wrapped by the REST handlers' - the two log under different tags, and
	// `/v1` installs no router level `Use` precisely so this route picks up only its own.

	if httpCfg.APIs.MCP.Enable {
		mcpAPI, err := NewMCPHandler(
			appName, workspaces, artifacts, artifactOps, httpCfg.APIs.RequestLogging,
		)
		if err != nil {
			return nil, goutils.NewRuntimeError("failed to define MCP API handler", err, true)
		}

		mcpServer := mcp.NewServer(
			&mcp.Implementation{Name: mcpServerName, Version: mcpServerVersion},
			&mcp.ServerOptions{Instructions: mcpServerInstructions},
		)
		if err := mcpAPI.RegisterTools(mcpServer); err != nil {
			return nil, goutils.NewRuntimeError("failed to register MCP tools", err, true)
		}

		mcpServer.AddReceivingMiddleware(mcpAPI.LoggingMiddleware)

		_ = v1Router.Path("/mcp").Handler(mcp.NewStreamableHTTPHandler(
			func(_ *http.Request) *mcp.Server { return mcpServer },
			&mcp.StreamableHTTPOptions{
				// Every tool is synchronous request/response and none needs to ask the client
				// anything, which is all a stateless session gives up. In exchange the endpoint
				// holds no per client state, so any replica behind the proxy can serve any
				// request.
				Stateless: true,
				// The SDK spells this one as the negative, so the config's positive inverts.
				DisableLocalhostProtection: !httpCfg.APIs.MCP.EnableDNSRebindGuard,
			},
		))
	}

	// --------------------------------------------------------------------------------
	// Middleware

	livenessRouter.Use(func(next http.Handler) http.Handler {
		return livenessAPI.LoggingMiddleware(next.ServeHTTP)
	})

	// CORS middleware
	corsWrapper := cors.New(cors.Options{
		AllowedOrigins:      []string{"*"},
		AllowedMethods:      []string{"POST", "GET", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:      []string{"*"},
		ExposedHeaders:      []string{httpCfg.APIs.RequestLogging.RequestIDHeader},
		AllowPrivateNetwork: true,
	})

	// --------------------------------------------------------------------------------
	// HTTP Server

	serverListen := fmt.Sprintf(
		"%s:%d", httpCfg.Server.ListenOn, httpCfg.Server.Port,
	)
	httpSrv := &http.Server{
		Addr:         serverListen,
		WriteTimeout: time.Second * time.Duration(httpCfg.Server.Timeouts.WriteTimeout),
		ReadTimeout:  time.Second * time.Duration(httpCfg.Server.Timeouts.ReadTimeout),
		IdleTimeout:  time.Second * time.Duration(httpCfg.Server.Timeouts.IdleTimeout),
		Handler:      corsWrapper.Handler(router),
	}
	httpSrv.Protocols = new(http.Protocols)
	httpSrv.Protocols.SetHTTP1(true)
	httpSrv.Protocols.SetUnencryptedHTTP2(true)

	return httpSrv, nil
}

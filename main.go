// Package main - application entry point
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alwitt/cairn/app"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/apex/log"
	apexJSON "github.com/apex/log/handlers/json"
	"github.com/go-playground/validator/v10"
	"github.com/spf13/viper"
	"github.com/urfave/cli/v2"
)

// serverArgs the arguments the API server sub-command is run with
type serverArgs struct {
	ConfigFile string `validate:"required,file"`
}

// maintainerArgs the arguments the maintainer sub-command is run with
type maintainerArgs struct {
	ConfigFile string `validate:"required,file"`

	// TaskingSQLPassword the Task Engine database's user password.
	//
	// Its own credential rather than a reuse of the application's: the Task Engine's schema is a
	// separate database (see models.SQLPersistenceConfig.TaskEngine), which a deployment is free
	// to place on a different server under a different user.
	TaskingSQLPassword string `validate:"required"`
}

type cliArgs struct {
	JSONLog     bool
	LogLevel    string `validate:"required,oneof=debug info warn error"`
	Hostname    string
	SQLPassword string `validate:"required"`
}

/*
runnable the lifecycle every application entry point exposes.

Both `app.Server` and `app.Maintainer` satisfy it as written, which is what lets one startup and
shutdown routine serve either of them.
*/
type runnable interface {
	// Start the component, broadcasting a fatal runtime failure back on fatalErrors
	Start(ctx context.Context, fatalErrors chan error) error

	// Stop shutdown the component
	Stop(ctx context.Context) error
}

var s3Creds goutils.S3Credentials

var cmdArgs cliArgs

var svrArgs serverArgs

var mtnArgs maintainerArgs

var logTags log.Fields

// @title cairn
// @version v0.1.0
// @description Artifacts Management For Agent Environments
// @host localhost:44123
// @BasePath /
// @query.collection.format multi
func main() {
	hostname, err := os.Hostname()
	if err != nil {
		log.WithError(err).Fatal("Unable to read hostname")
	}
	cmdArgs.Hostname = hostname
	logTags = log.Fields{
		"package":   "cairn",
		"module":    "main",
		"component": "main",
		"instance":  hostname,
	}

	app := &cli.App{
		Version:     "v0.1.0",
		Usage:       "application entrypoint",
		Description: "Artifacts Management For Agent Environments",
		Flags: []cli.Flag{
			// LOGGING
			&cli.BoolFlag{
				Name:        "json-log",
				Usage:       "Whether to log in JSON format",
				Aliases:     []string{"j"},
				EnvVars:     []string{"LOG_AS_JSON"},
				Value:       false,
				DefaultText: "false",
				Destination: &cmdArgs.JSONLog,
				Required:    false,
			},
			&cli.StringFlag{
				Name:        "log-level",
				Usage:       "Logging level: [debug info warn error]",
				Aliases:     []string{"l"},
				EnvVars:     []string{"LOG_LEVEL"},
				Value:       "warn",
				DefaultText: "warn",
				Destination: &cmdArgs.LogLevel,
				Required:    false,
			},
			// SQL Persistence
			&cli.StringFlag{
				Name:        "sql-pw",
				Usage:       "SQL DB User Password",
				EnvVars:     []string{"CAIRN_APP_SQL_PASSWORD"},
				Destination: &cmdArgs.SQLPassword,
				Required:    true,
			},
			// Object Store
			&cli.StringFlag{
				Name:        "s3-access-key",
				Usage:       "Object Store Access Key",
				EnvVars:     []string{"CAIRN_S3_ACCESS_KEY"},
				Destination: &s3Creds.AccessKey,
				Required:    true,
			},
			&cli.StringFlag{
				Name:        "s3-secret-key",
				Usage:       "Object Store Secret Key",
				EnvVars:     []string{"CAIRN_S3_SECRET_KEY"},
				Destination: &s3Creds.SecretAccessKey,
				Required:    true,
			},
		},
		Commands: []*cli.Command{
			{
				Name:        "server",
				Aliases:     []string{"svr"},
				Usage:       "Run application server",
				Description: "Start the REST API server",
				Flags: []cli.Flag{
					// Config file
					configFileFlag(&svrArgs.ConfigFile),
				},
				Action: runApplicationServer,
			},
			{
				Name:    "maintainer",
				Aliases: []string{"mtn"},
				Usage:   "Run data maintenance daemon",
				Description: "Start the periodic data maintenance loop. MUST run as a single " +
					"instance - unlike the API server, this daemon does not scale horizontally",
				Flags: []cli.Flag{
					// Config file
					configFileFlag(&mtnArgs.ConfigFile),
					// Task Engine SQL Persistence
					&cli.StringFlag{
						Name:        "tasking-sql-pw",
						Usage:       "Task Engine SQL DB User Password",
						EnvVars:     []string{"CAIRN_TASKING_SQL_PASSWORD"},
						Destination: &mtnArgs.TaskingSQLPassword,
						Required:    true,
					},
				},
				Action: runMaintainer,
			},
		},
	}

	err = app.Run(os.Args)
	if err != nil {
		deepestErrorsWithStack := goutils.AllDeepestErrorsWithTrace(err)
		logEntry := log.
			WithError(err).
			WithFields(goutils.UpdateCodePositionInTags(logTags))
		if deepestErrorsWithStack != nil {
			logEntry.Fatalf(
				"Program encountered error:\n%s", goutils.PrintErrorsWithTrace(deepestErrorsWithStack),
			)
		} else {
			logEntry.Fatal("Program encountered error")
		}
	}
}

/*
configFileFlag define the server config file flag.

Shared by every sub-command rather than restated by each, so the flag name, alias, and environment
variable an operator reaches the config through cannot come to differ between them.

	@param dest *string - where the flag's value is stored
	@returns the config file flag
*/
func configFileFlag(dest *string) cli.Flag {
	return &cli.StringFlag{
		Name:        "config-file",
		Usage:       "Server config file",
		Aliases:     []string{"c"},
		EnvVars:     []string{"CAIRN_CONFIG_FILE"},
		Destination: dest,
		Required:    true,
	}
}

// setupLogging helper function to prepare the app logging
func setupLogging() {
	if cmdArgs.JSONLog {
		log.SetHandler(apexJSON.New(os.Stderr))
	}
	switch cmdArgs.LogLevel {
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "info":
		log.SetLevel(log.InfoLevel)
	case "warn":
		log.SetLevel(log.WarnLevel)
	case "error":
		log.SetLevel(log.ErrorLevel)
	default:
		log.SetLevel(log.ErrorLevel)
	}
}

/*
prepareRuntime process the arguments every sub-command is run with, and prepare the process.

The general arguments are validated before logging is configured, so an unusable `--log-level` is
reported rather than acted on.

	@returns the validator the remainder of the startup is checked with
*/
func prepareRuntime() (*validator.Validate, error) {
	validate := validator.New()

	// Validate general config
	if err := validate.Struct(&cmdArgs); err != nil {
		log.
			WithError(err).
			WithFields(goutils.UpdateCodePositionInTags(logTags)).
			Error("Invalid application args")
		return nil, err
	}

	setupLogging()

	if err := models.RegisterWithValidator(validate); err != nil {
		log.
			WithError(err).
			WithFields(goutils.UpdateCodePositionInTags(logTags)).
			Error("Failed to register config validators")
		return nil, err
	}

	// Process S3 config
	if err := validate.Struct(&s3Creds); err != nil {
		log.
			WithError(err).
			WithFields(goutils.UpdateCodePositionInTags(logTags)).
			Error("Invalid object store credentials")
		return nil, err
	}

	return validate, nil
}

/*
loadApplicationConfig read the server config file, install the credentials onto it, and validate
the result.

	@param validate *validator.Validate - the validator to check the config with
	@param configFile string - the server config file to read
	@param taskingSQLPassword *string - the Task Engine database's user password. Optional, and
	    left nil by an entry point that opens no Task Engine database - an empty password installed
	    for the sake of installing one would only put an unwanted `password=` into a connection
	    string that never asked for it.
	@returns the application config
*/
func loadApplicationConfig(
	validate *validator.Validate, configFile string, taskingSQLPassword *string,
) (models.ApplicationConfig, error) {
	var configs models.ApplicationConfig

	// Process the config file
	models.InstallDefaultServerConfigValues()
	viper.SetConfigFile(configFile)
	if err := viper.ReadInConfig(); err != nil {
		log.
			WithError(err).
			WithFields(goutils.UpdateCodePositionInTags(logTags)).
			WithField("file", configFile).
			Error("Failed to read server config file")
		return configs, err
	}
	if err := viper.Unmarshal(&configs); err != nil {
		log.
			WithError(err).
			WithFields(goutils.UpdateCodePositionInTags(logTags)).
			WithField("file", configFile).
			Error("Server config content not valid")
		return configs, err
	}

	// Install credentials - SQL
	configs.Persistence.SQL.Application.Password = &cmdArgs.SQLPassword
	if taskingSQLPassword != nil {
		configs.Persistence.SQL.TaskEngine.Password = taskingSQLPassword
	}
	// Install credentials - Object Store
	configs.Artifact.ObjectStore.Creds = s3Creds

	if err := validate.Struct(&configs); err != nil {
		log.
			WithError(err).
			WithFields(goutils.UpdateCodePositionInTags(logTags)).
			WithField("file", configFile).
			Error("Server config failed validation")
		return configs, err
	}

	return configs, nil
}

/*
runUntilShutdown start an application entry point and run it until it is asked to stop.

Termination comes from one of two places: a shutdown signal, or a fatal runtime failure the
component reported for itself. Either way the component is stopped before returning.

	@param runtimeCtx context.Context - the process's execution context
	@param name string - the entry point's name, recorded on anything logged here
	@param component runnable - the entry point to run
	@param fatalBufferLen int - how many fatal failures the component can report without blocking
	    on the send. Sized by the caller, which is what knows how many independent things inside
	    the component could fail at once.
*/
func runUntilShutdown(
	runtimeCtx context.Context, name string, component runnable, fatalBufferLen int,
) error {
	entryTags := goutils.UpdateCodePositionInTags(logTags)
	entryTags["entrypoint"] = name

	fatalErrors := make(chan error, fatalBufferLen)
	if err := component.Start(runtimeCtx, fatalErrors); err != nil {
		log.WithError(err).WithFields(entryTags).Error("Initialization failed")
		return err
	}

	// ------------------------------------------------------------------------------------
	// Wait for termination: either a shutdown signal (runCtx cancelled) or a fatal
	// runtime failure reported by the component.

	// Derive a context that is cancelled on SIGINT (Ctrl+C) or SIGTERM (the signal
	// orchestrators such as Docker / Kubernetes / systemd send for graceful shutdown).
	runCtx, stop := signal.NotifyContext(runtimeCtx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case <-runCtx.Done():
	case err := <-fatalErrors:
		log.WithError(err).WithFields(entryTags).Error("Runtime failure; initiating shutdown")
	}

	// Restore default signal handling so a second SIGINT/SIGTERM force-quits the process
	// instead of being swallowed while shutdown is in progress.
	stop()

	// Stop the component using a fresh, short-lived context: runCtx is already cancelled,
	// which would otherwise blow through the shutdown timeouts immediately.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second*30)
	defer stopCancel()
	if err := component.Stop(stopCtx); err != nil {
		log.WithError(err).WithFields(entryTags).Error("Shutdown failed")
		return err
	}

	return nil
}

// runApplicationServer run the cairn application server
func runApplicationServer(ctx *cli.Context) error {
	validate, err := prepareRuntime()
	if err != nil {
		return err
	}

	// Process server config
	if err := validate.Struct(&svrArgs); err != nil {
		log.
			WithError(err).
			WithFields(goutils.UpdateCodePositionInTags(logTags)).
			Error("Invalid server args")
		return err
	}

	// The server opens no Task Engine database, so it is not asked for that credential.
	configs, err := loadApplicationConfig(validate, svrArgs.ConfigFile, nil)
	if err != nil {
		return err
	}

	// ------------------------------------------------------------------------------------
	// Build and start server

	server, err := app.BuildNewServer(ctx.Context, configs)
	if err != nil {
		log.
			WithError(err).
			WithFields(goutils.UpdateCodePositionInTags(logTags)).
			Error("Server construction failed")
		return err
	}

	// Buffered so a failing server goroutine never blocks on the send; sized for both
	// the API and metrics servers in case they fail concurrently.
	return runUntilShutdown(ctx.Context, "server", server, 2)
}

/*
runMaintainer run the cairn data maintenance daemon.

A separate entry point from the API server rather than a component of it, because the two do not
scale alike. The API server's replicas are interchangeable; this daemon's are not - the Task Engine
worker name it runs under has to be unique per replica (see models.TaskEngineConfig.WorkerName),
and a second replica's sweep timer would only raise requests for work the first one's already
covers. So it is deployed as a singleton, on its own.
*/
func runMaintainer(ctx *cli.Context) error {
	validate, err := prepareRuntime()
	if err != nil {
		return err
	}

	// Process maintainer config
	if err := validate.Struct(&mtnArgs); err != nil {
		log.
			WithError(err).
			WithFields(goutils.UpdateCodePositionInTags(logTags)).
			Error("Invalid maintainer args")
		return err
	}

	configs, err := loadApplicationConfig(validate, mtnArgs.ConfigFile, &mtnArgs.TaskingSQLPassword)
	if err != nil {
		return err
	}

	// ------------------------------------------------------------------------------------
	// Build and start maintainer

	maintainer, err := app.BuildNewMaintainer(ctx.Context, configs)
	if err != nil {
		log.
			WithError(err).
			WithFields(goutils.UpdateCodePositionInTags(logTags)).
			Error("Maintainer construction failed")
		return err
	}

	// Buffered so a failing Task Engine goroutine never blocks on the send; sized for the
	// engine's receiver and scheduler, each of which reports at most one fault.
	return runUntilShutdown(ctx.Context, "maintainer", maintainer, 2)
}

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

type serverArgs struct {
	ConfigFile string `validate:"required,file"`
}

type cliArgs struct {
	JSONLog     bool
	LogLevel    string `validate:"required,oneof=debug info warn error"`
	Hostname    string
	SQLPassword string `validate:"required"`
}

var s3Creds goutils.S3Credentials

var cmdArgs cliArgs

var svrArgs serverArgs

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
					&cli.StringFlag{
						Name:        "config-file",
						Usage:       "Server config file",
						Aliases:     []string{"c"},
						EnvVars:     []string{"CAIRN_CONFIG_FILE"},
						Destination: &svrArgs.ConfigFile,
						Required:    true,
					},
				},
				Action: runApplicationServer,
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

// runApplicationServer run the cairn application server
func runApplicationServer(ctx *cli.Context) error {
	validate := validator.New()

	// Validate general config
	if err := validate.Struct(&cmdArgs); err != nil {
		log.
			WithError(err).
			WithFields(goutils.UpdateCodePositionInTags(logTags)).
			Error("Invalid application args")
		return err
	}

	setupLogging()

	if err := models.RegisterWithValidator(validate); err != nil {
		log.
			WithError(err).
			WithFields(goutils.UpdateCodePositionInTags(logTags)).
			Error("Failed to register config validators")
		return err
	}

	// Process S3 config
	if err := validate.Struct(&s3Creds); err != nil {
		log.
			WithError(err).
			WithFields(goutils.UpdateCodePositionInTags(logTags)).
			Error("Invalid object store credentials")
		return err
	}

	// Process server config
	var configs models.ApplicationConfig
	{
		if err := validate.Struct(&svrArgs); err != nil {
			log.
				WithError(err).
				WithFields(goutils.UpdateCodePositionInTags(logTags)).
				Error("Invalid server args")
			return err
		}
		// Process the config file
		models.InstallDefaultServerConfigValues()
		viper.SetConfigFile(svrArgs.ConfigFile)
		if err := viper.ReadInConfig(); err != nil {
			log.
				WithError(err).
				WithFields(goutils.UpdateCodePositionInTags(logTags)).
				WithField("file", svrArgs.ConfigFile).
				Error("Failed to read server config file")
			return err
		}
		if err := viper.Unmarshal(&configs); err != nil {
			log.
				WithError(err).
				WithFields(goutils.UpdateCodePositionInTags(logTags)).
				WithField("file", svrArgs.ConfigFile).
				Error("Server config content not valid")
			return err
		}

		// Install credentials - SQL
		configs.Persistence.SQL.Application.Password = &cmdArgs.SQLPassword
		// Install credentials - Object Store
		configs.Artifact.ObjectStore.Creds = s3Creds

		if err := validate.Struct(&configs); err != nil {
			log.
				WithError(err).
				WithFields(goutils.UpdateCodePositionInTags(logTags)).
				WithField("file", svrArgs.ConfigFile).
				Error("Server config failed validation")
			return err
		}
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
	serverErrors := make(chan error, 2)
	if err := server.Start(ctx.Context, serverErrors); err != nil {
		log.
			WithError(err).
			WithFields(goutils.UpdateCodePositionInTags(logTags)).
			Error("Server initialization failed")
		return err
	}
	// ------------------------------------------------------------------------------------
	// Wait for termination: either a shutdown signal (runCtx cancelled) or a fatal
	// runtime failure from one of the servers.

	// Derive a context that is cancelled on SIGINT (Ctrl+C) or SIGTERM (the signal
	// orchestrators such as Docker / Kubernetes / systemd send for graceful shutdown).
	runCtx, stop := signal.NotifyContext(ctx.Context, os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case <-runCtx.Done():
	case err := <-serverErrors:
		log.
			WithError(err).
			WithFields(goutils.UpdateCodePositionInTags(logTags)).
			Error("Server runtime failure; initiating shutdown")
	}

	// Restore default signal handling so a second SIGINT/SIGTERM force-quits the process
	// instead of being swallowed while shutdown is in progress.
	stop()

	// Stop the server using a fresh, short-lived context: runCtx is already cancelled,
	// which would otherwise blow through the shutdown timeouts immediately.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second*30)
	defer stopCancel()
	if err := server.Stop(stopCtx); err != nil {
		log.
			WithError(err).
			WithFields(goutils.UpdateCodePositionInTags(logTags)).
			Error("Server shutdown failed")
		return err
	}

	return nil
}

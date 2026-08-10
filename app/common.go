// Package app - application entry points
package app //revive:disable-line:var-naming

import (
	"context"

	"github.com/alwitt/cairn/db"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/alwitt/goutils/runtime"
	"gorm.io/gorm/logger"
)

/*
The infrastructure clients every application entry point stands up for itself.

Each entry point - see BuildNewServer and BuildNewMaintainer - builds its own instances rather
than sharing one set, which is what keeps either of them liftable into its own process without
untangling it from the other. Sharing the construction here is what keeps them consistent: a
setting that works for one entry point works for the other, and a failure is reported the same
way from both.
*/

/*
buildPersistenceClient define a SQL persistence client against one Postgres database.

Takes a single database's config rather than the aggregate, so it serves whichever of them is
named - the application's own and the Task Engine's are separate entries addressing separate
schemas (see models.SQLPersistenceConfig).

	@param config models.PostgresConfig - the SQL persistence config
	@returns the new persistence client
*/
func buildPersistenceClient(config models.PostgresConfig) (db.Client, error) {
	dialector, err := db.GetPostgresDialector(config)
	if err != nil {
		return nil, models.NewBootStrapError("Failed to parse DB persistence parameters", err, true)
	}

	// Read off the config rather than pinned to one level, so turning `debugLog` on for this
	// database actually produces the ORM logs it promises.
	logLevel := logger.Error
	if config.DebugLog {
		logLevel = logger.Info
	}

	client, err := db.NewConnection(dialector, logLevel)
	if err != nil {
		return nil, models.NewBootStrapError("Failed to prepare DB persistence client", err, true)
	}

	return client, nil
}

/*
buildS3ClientManager define the object store client manager.

The manager, rather than a client: it owns the client's lifecycle and replaces it once it has
aged past the configured TTL, which is how a rotated credential is picked up without a restart.

	@param config models.ObjectStoreConfig - the object store config, credentials included
	@returns the new object store client manager
*/
func buildS3ClientManager(config models.ObjectStoreConfig) (goutils.S3ClientManager, error) {
	s3Manager, err := goutils.NewS3ClientManager(config.ToStandard(), config.ClientTTL())
	if err != nil {
		return nil, models.NewBootStrapError("Failed to prepare S3 client manager", err, true)
	}

	return s3Manager, nil
}

/*
buildVolumeManager define the manager of the persistent volumes backing workspaces.

Returned unstarted - it holds no runtime client until Start, and the caller is responsible for
that and for the matching Cleanup.

	@param parentCtx context.Context - parent execution context
	@param volumeType models.WorkspaceVolumeTypeENUM - which volume management driver to build
	@returns the new volume manager
*/
func buildVolumeManager(
	parentCtx context.Context, volumeType models.WorkspaceVolumeTypeENUM,
) (runtime.VolumeManager, error) {
	switch volumeType {
	case models.WorkspaceVolumeTypeDocker:
		volume, err := runtime.NewDockerVolumeManager(parentCtx)
		if err != nil {
			return nil, models.NewBootStrapError("Failed to prepare docker volume manager", err, true)
		}
		return volume, nil

	default:
		return nil, models.NewBootStrapError(
			"Unsupported persistence volume type '"+string(volumeType)+"'", nil, true,
		)
	}
}

// Package maintenance - system maintenance package
package maintenance

import (
	"context"
	"fmt"

	"github.com/alwitt/cairn/db"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/alwitt/goutils/runtime"
	"github.com/apex/log"
	"github.com/go-playground/validator/v10"
)

/*
VolumeStateSyncReport what one workspace volume-state reconciliation sweep observed and
corrected (see DESIGN §8.2.2).

Both drift directions are recorded rather than merely counted, so a caller can name the
workspaces it moved. The two consistent cases are not recorded at all - a sweep over a healthy
deployment reports nothing but its `Examined` count.
*/
type VolumeStateSyncReport struct {
	// Examined number of workspace records compared against the observed volumes
	Examined int

	// AdoptedReady IDs of the workspaces corrected `NONE` -> `READY`, whose volume was found
	// to exist after all
	AdoptedReady []string

	// ClearedNone IDs of the workspaces corrected `READY` -> `NONE`, whose volume has vanished
	ClearedNone []string

	// OrphanVolumes names of the observed volumes no workspace record claims.
	//
	// Reported only, never deleted. The service can't know every entity mounting a volume, so
	// reaping one is exactly the action DESIGN §4.2 forbids; an operator decides.
	OrphanVolumes []string
}

// Manager system maintenance manager
type Manager interface {
	/*
		SyncWorkspaceVolumeStates reconcile every workspace's `VolumeState` against the volumes
		that actually exist, correcting the column where the two disagree (see DESIGN §8.2.2).

		Only the column is ever written. No volume is created, deleted, or otherwise touched -
		this settles a record against reality, it does not impose a record onto it.

		Both drift directions self-heal silently rather than being flagged as incidents: a
		workspace volume is disposable scratch, not irreplaceable data (see DESIGN §0, §8.2.2).
		A volume observed with no workspace record to claim it is reported but left alone.

		Corrections are applied independently, so one that fails does not withhold the others.
		The returned report therefore describes what actually landed even when the error is
		non-nil, and a caller that halts on the error can still say what changed.

			@param ctx context.Context - execution context
			@returns what the sweep observed and corrected
	*/
	SyncWorkspaceVolumeStates(ctx context.Context) (VolumeStateSyncReport, error)
}

// managerImpl implements Manager
type managerImpl struct {
	goutils.Component

	appName string

	validator *validator.Validate

	persistence db.Client

	s3Manager goutils.S3ClientManager

	// storeConfig artifact storage config
	storeConfig models.ArtifactStorageConfig

	// maintenanceConfig maintenance system config
	maintenanceConfig models.MaintenanceConfig

	// volumes provides the view of the persistent volumes backing workspaces. The manager
	// reads it but does not own its lifecycle - the caller performs `Start` and `Cleanup`.
	volumes runtime.VolumeManager
}

/*
NewManager define a new system maintenance manager

	@param appName string - the per-deployment application name which namespaces this
	    deployment's persistent volumes. Reconciliation finds them by the prefix it forms (see
	    DESIGN §2.1).
	@param persistence db.Client - persistence client
	@param s3Manager goutils.S3ClientManager - provides the object store client holding this
	    deployment's credentials. The manager owns the client's lifecycle, replacing it once it
	    has aged past its TTL, so a client is acquired per object store call rather than held
	    on to.
	@param storeConfig models.ArtifactStorageConfig - artifact storage config
	@param maintenanceConfig models.MaintenanceConfig - maintenance system config
	@param volumes runtime.VolumeManager - the view of the persistent volumes backing
	    workspaces. Its lifecycle is the caller's responsibility; it must already be started,
	    and this manager never tears it down.
	@returns the new system maintenance manager
*/
func NewManager(
	appName string,
	persistence db.Client,
	s3Manager goutils.S3ClientManager,
	storeConfig models.ArtifactStorageConfig,
	maintenanceConfig models.MaintenanceConfig,
	volumes runtime.VolumeManager,
) (Manager, error) {
	logTags := log.Fields{
		"package": "cairn", "module": "maintenance", "component": "manager", "instance": appName,
	}

	validate := validator.New()
	if err := models.RegisterWithValidator(validate); err != nil {
		return nil, goutils.NewRuntimeError(
			"failed to install custom validation macros", err, true,
		)
	}

	// The application name forms the prefix reconciliation lists volumes on, so hold it to the
	// same charset the volume names it must match were built under.
	if err := validate.Var(appName, "required,valid_name"); err != nil {
		return nil, goutils.NewValidationError(
			fmt.Sprintf("application name '%s' is not valid", appName), err, true,
		)
	}

	if persistence == nil {
		return nil, goutils.NewValidationError("persistence client is required", nil, true)
	}

	if s3Manager == nil {
		return nil, goutils.NewValidationError(
			"object store client manager is required", nil, true,
		)
	}

	if volumes == nil {
		return nil, goutils.NewValidationError("volume manager is required", nil, true)
	}

	// Validate both configs up front so a missing bucket, prefix, or grace window fails here
	// rather than partway through the first sweep.
	if err := validate.Struct(&storeConfig); err != nil {
		return nil, goutils.NewValidationError("artifact storage config is not valid", err, true)
	}

	if err := validate.Struct(&maintenanceConfig); err != nil {
		return nil, goutils.NewValidationError("maintenance config is not valid", err, true)
	}

	instance := &managerImpl{
		Component: goutils.Component{
			LogTags: logTags,
			LogTagModifiers: []goutils.LogMetadataModifier{
				goutils.ModifyLogMetadataByRestRequestParam,
			},
		},
		appName:           appName,
		validator:         validate,
		persistence:       persistence,
		s3Manager:         s3Manager,
		storeConfig:       storeConfig,
		maintenanceConfig: maintenanceConfig,
		volumes:           volumes,
	}

	return instance, nil
}

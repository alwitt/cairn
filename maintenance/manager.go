// Package maintenance - system maintenance package
package maintenance

import (
	"context"
	"fmt"
	"time"

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

/*
StagingReapReport what one staging-object sweep observed and reclaimed (see DESIGN §8.2.1
item 1).

Objects are counted rather than named. The population is unbounded and an individual staging
key means nothing to anyone - it was never addressable by a user.
*/
type StagingReapReport struct {
	// Examined number of staging objects listed
	Examined int

	// Deleted number of objects reclaimed
	Deleted int

	// Retained number of objects left in place because they are still inside the grace window
	Retained int
}

/*
StorageReconcileReport what one storage-object reconciliation observed, reclaimed, and flagged
(see DESIGN §8.2.1 items 2 and 3).

Objects are counted; quarantined artifacts are named. Each of the latter is a distinct
data-loss incident an operator has to act on, and the set is bounded by the row count rather
than by the size of the bucket.
*/
type StorageReconcileReport struct {
	// Examined number of storage objects listed
	Examined int

	// Deleted number of unassociated objects reclaimed
	Deleted int

	// Retained number of unassociated objects left in place because they are still inside the
	// grace window, or because their age could not be established
	Retained int

	// FlaggedMissing IDs of the artifacts quarantined as `MISSING_OBJECT`, whose backing object
	// the store did not have
	FlaggedMissing []string
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

	/*
		DeleteOrphanedStagingObjects reclaim the staging objects left behind by uploads that
		aborted before their best-effort cleanup (see DESIGN §8.2.1 item 1).

		A staging key never maps to an artifact entry by construction (see DESIGN §8.1), so
		there is nothing to join against - every staging object is unassociated, and age alone
		decides whether it is still in flight or abandoned. Nothing here reads the persistence
		layer.

		The grace window must therefore outlast a WHOLE upload rather than just the
		copy -> insert gap the storage sweep contends with: a staging object is live from the
		moment its PUT URL is minted until registration copies it away. Configure
		`objAgeOutSec` above `putUrlTTL` plus the time registration takes, or a slow upload is
		reclaimed out from under itself. The two settings are independent and nothing checks
		the relationship between them.

			@param ctx context.Context - execution context
			@param workspaceID *string - optionally, confine the sweep to one workspace's
			    staging key namespace; nil sweeps every workspace's
			@returns what the sweep observed and reclaimed
	*/
	DeleteOrphanedStagingObjects(
		ctx context.Context, workspaceID *string,
	) (StagingReapReport, error)

	/*
		ReconcileStorageObjects settle the artifact entries against the objects backing them,
		in both directions (see DESIGN §8.2.1 items 2 and 3).

		This is the sole object-deleter in the system. Deleting an artifact or a workspace is a
		plain row delete that leaves the object untouched (see DESIGN §4.1), so a freed object
		only ever reaches reclamation through here.

		Both directions fall out of one pass over the entries, which is why they are not
		separate calls - asking the same question twice would scan the store twice to learn the
		same thing:

		  - An object no entry references, aged past the grace window, is reclaimed. The window
		    is load-bearing: because a purge removes the row first, every freed object transits
		    through unassociated, and so does every in-flight upload during the copy -> insert
		    gap. The window is the only thing separating the two.
		  - A `RECORDED` entry the store had no object for is quarantined as `MISSING_OBJECT`.
		    An entry is only ever inserted after its object copy completes, so this can only
		    mean the object vanished afterward - a data-loss signal rather than routine garbage.
		    It is deliberately not remediated: the row is preserved as evidence and an operator
		    decides what to do with it (see DESIGN §2.2.1).

		Reclamations and quarantines are applied independently, so one that fails does not
		withhold the others. The returned report therefore describes what actually landed even
		when the error is non-nil.

			@param ctx context.Context - execution context
			@param workspaceID *string - optionally, confine the reconciliation to one
			    workspace's entries and storage key namespace; nil reconciles every workspace's
			@returns what the reconciliation observed, reclaimed, and flagged
	*/
	ReconcileStorageObjects(
		ctx context.Context, workspaceID *string,
	) (StorageReconcileReport, error)
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

// s3Client acquire the object store client to use for a single call.
//
// The client manager hands back a shared client and rebuilds it once it has aged past its TTL,
// which is how a client that has quietly lost its connection to the object store gets replaced.
// A call site therefore asks for one each time rather than holding on to it. A sweep is
// long-running by nature, so this matters more here than anywhere else - a client acquired at
// the start of a full-bucket pass could easily outlive its TTL before the pass ends.
//
// This is the one place the wall clock is read for the client's sake; the TTL the client
// manager enforces is wall-clock based.
func (m *managerImpl) s3Client(ctx context.Context) (goutils.S3Client, error) {
	return m.s3Manager.GetClient(ctx, time.Now().UTC())
}

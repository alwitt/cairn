// Package db - database controllers for system persistence
package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/apex/log"
	"github.com/go-playground/validator/v10"
	"gorm.io/gorm"
)

// CommonListEntryQueryFilter common query filter when listing data entries
type CommonListEntryQueryFilter struct {
	Limit  *int `validate:"omitempty,gt=0"`
	Offset *int `validate:"omitempty,gte=0"`
}

// SystemEventQueryFilter audit event query filter conditions
type SystemEventQueryFilter struct {
	CommonListEntryQueryFilter
	// EventTypes the specific event types to query for
	EventTypes []models.SystemEventTypeENUM `validate:"omitempty,dive,system_event_type"`
	// EventsAfter filter for events at or after this timestamp
	EventsAfter *time.Time
	// EventsBefore filter for events at or before this timestamp
	EventsBefore *time.Time
}

// WorkspaceQueryFilter query filter conditions to list workspaces
type WorkspaceQueryFilter struct {
	CommonListEntryQueryFilter
	// TargetIDs the specific workspace ID set to query for
	TargetIDs []string
	// TargetNames the specific workspace names to query for
	TargetNames []string
	// VolumeStates the specific persistent volume states to query for
	VolumeStates []models.WorkspaceVolumeStateENUM `validate:"omitempty,dive,volume_state"`
}

// ArtifactQueryFilter query filter conditions to list artifacts
type ArtifactQueryFilter struct {
	CommonListEntryQueryFilter
	// WorkspaceID fetch artifacts belonging to this parent workspace
	WorkspaceID *string
	// TargetIDs the specific artifact ID set to query for
	TargetIDs []string
	// TargetNames the specific artifact names to query for
	TargetNames []string
	// ArtifactStates the specific artifact states to query for.
	//
	// This is a listing option, not a hardcoded filter: leaving it empty returns artifacts in
	// every state. The caller decides what to surface — the API layer defaults it to
	// `RECORDED`, and asks for `MISSING_OBJECT` when triaging (see DESIGN §7.1).
	ArtifactStates []models.ArtifactStateENUM `validate:"omitempty,dive,artifact_state"`
	// ObjectKeys the specific backing object keys to query for
	ObjectKeys []string
}

// NewWorkspaceParameter new workspace parameters
type NewWorkspaceParameter struct {
	// Name of the workspace, unique across the deployment
	Name string
	// Description an optional description for the workspace
	Description *string
	// AppName the per-deployment application name which namespaces this deployment's volumes.
	// The workspace's persistent volume name is derived from it (see DESIGN §2.1).
	AppName string
}

// NewArtifactParameter new artifact parameters
type NewArtifactParameter struct {
	// WorkspaceID ID of the parent workspace
	WorkspaceID string
	// Name of the artifact, unique within the parent workspace
	Name string
	// Description an optional description for the artifact
	Description *string
	// ObjectKey the complete object key backing this artifact in the object store
	ObjectKey string
	// MIMEType server-sniffed content type of the backing object
	MIMEType string
	// Size size of the backing object in bytes
	Size int64
}

// Database the database handle to interacting with the data base.
//
// All methods must be invoked within a transaction. A `Database` instance can only be
// obtained via `Client.UseDatabaseInTransaction`, which runs the caller's logic inside a
// transaction; there is no way to construct one outside of that scope. Consequently each
// method may assume it is already running in a transaction, and multi-statement operations
// (e.g. a state change plus its audit event) are committed or rolled back atomically.
type Database interface {
	// ------------------------------------------------------------------------------------
	// Audit

	/*
		ListSystemEvents list captured system events

			@param ctx context.Context - execution context
			@param filters SystemEventQueryFilter - entry listing filter
			@return list of system events
	*/
	ListSystemEvents(
		ctx context.Context, filters SystemEventQueryFilter,
	) ([]models.SystemEventAudit, error)

	// ------------------------------------------------------------------------------------
	// Workspace

	/*
		DefineNewWorkspace define a new workspace.

		The workspace's persistent volume name is derived here as
		`<app name>-<workspace ID>` and persisted, so no client ever guesses or re-derives it
		(see DESIGN §2.1). Deriving it from the immutable ID rather than the name keeps it
		stable across a workspace rename.

		The new workspace starts with no persistent volume (`VolumeState = NONE`); the volume
		is provisioned separately by the operator (see DESIGN §4.2).

			@param ctx context.Context - execution context
			@param params NewWorkspaceParameter - new workspace parameters
			@returns the new workspace entry
	*/
	DefineNewWorkspace(
		ctx context.Context, params NewWorkspaceParameter,
	) (models.Workspace, error)

	/*
		GetWorkspace fetch a workspace by ID

			@param ctx context.Context - execution context
			@param workspaceID string - workspace ID
			@returns the workspace entry
	*/
	GetWorkspace(ctx context.Context, workspaceID string) (models.Workspace, error)

	/*
		GetWorkspaceByName fetch a workspace by name.

		Workspace names are globally unique, so this resolves a name to exactly one entry. It
		backs the MCP layer's name -> ID resolution (see DESIGN §3).

			@param ctx context.Context - execution context
			@param name string - workspace name
			@returns the workspace entry
	*/
	GetWorkspaceByName(ctx context.Context, name string) (models.Workspace, error)

	/*
		ListWorkspaces list workspaces

			@param ctx context.Context - execution context
			@param filters WorkspaceQueryFilter - query filtering conditions
			@returns list of workspaces
	*/
	ListWorkspaces(
		ctx context.Context, filters WorkspaceQueryFilter,
	) ([]models.Workspace, error)

	/*
		UpdateWorkspaceName change a workspace's name.

		A pure DB update with no volume guard: the volume name is derived from the immutable
		workspace ID, so a rename never affects the volume, even a live mounted one (see
		DESIGN §7.1).

			@param ctx context.Context - execution context
			@param workspaceID string - workspace ID
			@param newName string - the new workspace name
	*/
	UpdateWorkspaceName(ctx context.Context, workspaceID string, newName string) error

	/*
		UpdateWorkspaceDescription change a workspace's description

			@param ctx context.Context - execution context
			@param workspaceID string - workspace ID
			@param newDescription *string - the new description, nil to clear it
	*/
	UpdateWorkspaceDescription(
		ctx context.Context, workspaceID string, newDescription *string,
	) error

	/*
		MarkWorkspaceVolumeReady record that the workspace's persistent volume exists and is
		mountable.

		Written only AFTER Docker has actually created the volume (see DESIGN §4.2), and by
		the volume-state reconciliation when it adopts a volume that exists in Docker but is
		recorded as `NONE` (see DESIGN §8.2.2).

			@param ctx context.Context - execution context
			@param workspaceID string - workspace ID
	*/
	MarkWorkspaceVolumeReady(ctx context.Context, workspaceID string) error

	/*
		MarkWorkspaceVolumeNone record that the workspace has no persistent volume.

		Written only AFTER Docker has actually removed the volume (see DESIGN §4.2), and by
		the volume-state reconciliation when a volume recorded as `READY` has vanished from
		Docker (see DESIGN §8.2.2).

			@param ctx context.Context - execution context
			@param workspaceID string - workspace ID
	*/
	MarkWorkspaceVolumeNone(ctx context.Context, workspaceID string) error

	/*
		DeleteWorkspace delete a workspace entry, cascading to its artifact rows.

		Refused unless the workspace's persistent volume is already gone
		(`VolumeState = NONE`) — deleting the row otherwise would orphan the Docker volume,
		since the volume name is ID-derived and reconciliation could never adopt it without a
		row (see DESIGN §4.3).

		No object-store interaction: the objects the deleted artifact rows referenced are left
		in the store and reclaimed later by the object-reaping GC (see DESIGN §4.1, §8.2.1).

			@param ctx context.Context - execution context
			@param workspaceID string - workspace ID
	*/
	DeleteWorkspace(ctx context.Context, workspaceID string) error

	// ------------------------------------------------------------------------------------
	// Artifact

	/*
		DefineNewArtifact record a new artifact.

		Called only after the backing object is in place at its final key (see DESIGN §6.1),
		so the entry is committed directly as `RECORDED` — there is no pending state. Name
		uniqueness within the parent workspace is enforced by the DB constraint.

			@param ctx context.Context - execution context
			@param params NewArtifactParameter - new artifact parameters
			@returns the new artifact entry
	*/
	DefineNewArtifact(ctx context.Context, params NewArtifactParameter) (models.Artifact, error)

	/*
		GetArtifact fetch an artifact by ID

			@param ctx context.Context - execution context
			@param artifactID string - artifact ID
			@returns the artifact entry
	*/
	GetArtifact(ctx context.Context, artifactID string) (models.Artifact, error)

	/*
		GetArtifactByName fetch an artifact by name within a workspace.

		Artifact names are unique per workspace, so this resolves a
		(workspace, name) pair to exactly one entry. It backs the MCP layer's name -> ID
		resolution (see DESIGN §3).

			@param ctx context.Context - execution context
			@param workspaceID string - ID of the parent workspace
			@param name string - artifact name
			@returns the artifact entry
	*/
	GetArtifactByName(
		ctx context.Context, workspaceID string, name string,
	) (models.Artifact, error)

	/*
		ListArtifacts list artifacts

			@param ctx context.Context - execution context
			@param filters ArtifactQueryFilter - query filtering conditions
			@returns list of artifacts
	*/
	ListArtifacts(ctx context.Context, filters ArtifactQueryFilter) ([]models.Artifact, error)

	/*
		ListWorkspaceArtifacts list the artifacts belonging to a particular workspace

			@param ctx context.Context - execution context
			@param workspaceID string - ID of the parent workspace
			@param filters ArtifactQueryFilter - query filtering conditions
			@returns list of artifacts
	*/
	ListWorkspaceArtifacts(
		ctx context.Context, workspaceID string, filters ArtifactQueryFilter,
	) ([]models.Artifact, error)

	/*
		UpdateArtifactName change an artifact's name.

		A pure DB update: the backing object key carries a random suffix rather than the name,
		so a rename never touches the object store (see DESIGN §2.2). Uniqueness within the
		parent workspace is enforced by the DB constraint.

			@param ctx context.Context - execution context
			@param artifactID string - artifact ID
			@param newName string - the new artifact name
	*/
	UpdateArtifactName(ctx context.Context, artifactID string, newName string) error

	/*
		UpdateArtifactDescription change an artifact's description

			@param ctx context.Context - execution context
			@param artifactID string - artifact ID
			@param newDescription *string - the new description, nil to clear it
	*/
	UpdateArtifactDescription(
		ctx context.Context, artifactID string, newDescription *string,
	) error

	/*
		UpdateArtifactObject repoint an artifact at a newly written backing object.

		The artifact's bytes are replaced by copying them to a NEW final key and flipping the
		row over to it in one update, so readers always observe a complete object. The old
		object is orphaned by design and reclaimed later by the object-reaping GC (see
		DESIGN §6.3).

		Also restores the entry to `RECORDED`, so re-uploading the bytes of an artifact
		quarantined as `MISSING_OBJECT` brings it back into service.

			@param ctx context.Context - execution context
			@param artifactID string - artifact ID
			@param objectKey string - the new backing object key
			@param mimeType string - server-sniffed content type of the new object
			@param size int64 - size of the new object in bytes
	*/
	UpdateArtifactObject(
		ctx context.Context, artifactID string, objectKey string, mimeType string, size int64,
	) error

	/*
		MarkArtifactMissingObject quarantine an artifact whose backing object is gone.

		A data-loss signal rather than routine garbage, so it is not auto-remediated: the
		transition preserves the row as evidence of the loss and surfaces the incident for an
		operator, who may then delete the row (see DESIGN §8.2.1).

			@param ctx context.Context - execution context
			@param artifactID string - artifact ID
	*/
	MarkArtifactMissingObject(ctx context.Context, artifactID string) error

	/*
		DeleteArtifact delete an artifact entry.

		Idempotent — deleting an absent entry is a no-op. No object-store interaction: the
		object the row referenced is left in the store and reclaimed later by the
		object-reaping GC (see DESIGN §4.1, §8.2.1).

			@param ctx context.Context - execution context
			@param artifactID string - artifact ID
	*/
	DeleteArtifact(ctx context.Context, artifactID string) error
}

// databaseImpl implements Database
type databaseImpl struct {
	goutils.Component
	db        *gorm.DB
	validator *validator.Validate
}

// newDatabase define a new database client
func newDatabase(_ context.Context, sqlClient *gorm.DB) (Database, error) {
	logTags := log.Fields{"package": "cairn", "module": "db", "component": "db-client"}

	instance := &databaseImpl{
		Component: goutils.Component{
			LogTags: logTags,
			LogTagModifiers: []goutils.LogMetadataModifier{
				goutils.ModifyLogMetadataByRestRequestParam,
			},
		},
		db:        sqlClient,
		validator: validator.New(),
	}

	if err := models.RegisterWithValidator(instance.validator); err != nil {
		return nil, goutils.NewRuntimeError("failed to install custom validation macros", err, true)
	}

	return instance, nil
}

// notFoundOrError translates the error returned by a single-entry fetch into a
// goutils.NotFoundError when GORM reports that the record does not exist. Any other
// error is returned unchanged, and a nil error stays nil.
func notFoundOrError(err error, entity, id string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return goutils.NewNotFoundError(
			fmt.Sprintf("%s '%s' does not exist", entity, id), err, true,
		)
	}
	return goutils.NewSQLError(fmt.Sprintf("failed to fetch %s '%s'", entity, id), err, true)
}

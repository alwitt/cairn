// Package models - application data models
package models

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/alwitt/goutils"
	"github.com/go-playground/validator/v10"
	"gorm.io/datatypes"
)

// SystemEventTypeENUM system event type ENUM value type
type SystemEventTypeENUM string

// Various system event types which will be captured
const (
	// --- Workspace events ---

	// SystemEventTypeNewWorkspace defined a new workspace
	SystemEventTypeNewWorkspace SystemEventTypeENUM = "NEW_WORKSPACE"
	// SystemEventTypeRenameWorkspace renamed a workspace
	SystemEventTypeRenameWorkspace SystemEventTypeENUM = "RENAME_WORKSPACE"
	// SystemEventTypeWorkspaceVolumeState a workspace's persistent volume state changed
	SystemEventTypeWorkspaceVolumeState SystemEventTypeENUM = "WORKSPACE_VOLUME_STATE"
	// SystemEventTypeDeleteWorkspace deleted a workspace
	SystemEventTypeDeleteWorkspace SystemEventTypeENUM = "DELETE_WORKSPACE"

	// --- Artifact events ---

	// SystemEventTypeNewArtifact defined a new artifact
	SystemEventTypeNewArtifact SystemEventTypeENUM = "NEW_ARTIFACT"
	// SystemEventTypeRenameArtifact renamed an artifact
	SystemEventTypeRenameArtifact SystemEventTypeENUM = "RENAME_ARTIFACT"
	// SystemEventTypeUpdateArtifactObject an artifact was repointed at a new backing object
	SystemEventTypeUpdateArtifactObject SystemEventTypeENUM = "UPDATE_ARTIFACT_OBJECT"
	// SystemEventTypeArtifactMissingObject an artifact was quarantined as missing its
	// backing object
	SystemEventTypeArtifactMissingObject SystemEventTypeENUM = "ARTIFACT_MISSING_OBJECT"
	// SystemEventTypeDeleteArtifact deleted an artifact
	SystemEventTypeDeleteArtifact SystemEventTypeENUM = "DELETE_ARTIFACT"
)

// Values all valid SystemEventTypeENUM values
func (SystemEventTypeENUM) Values() []SystemEventTypeENUM {
	return []SystemEventTypeENUM{
		SystemEventTypeNewWorkspace,
		SystemEventTypeRenameWorkspace,
		SystemEventTypeWorkspaceVolumeState,
		SystemEventTypeDeleteWorkspace,
		SystemEventTypeNewArtifact,
		SystemEventTypeRenameArtifact,
		SystemEventTypeUpdateArtifactObject,
		SystemEventTypeArtifactMissingObject,
		SystemEventTypeDeleteArtifact,
	}
}

// SystemEventAudit recording of events occurring at the system level
type SystemEventAudit struct {
	// ID audit entry ID
	ID string `json:"id" gorm:"column:id;primaryKey;unique" validate:"required"`
	// EventType system event type
	EventType SystemEventTypeENUM `json:"type" gorm:"column:type;not null" validate:"required,system_event_type"`
	// Metadata a metadata relating to the event
	Metadata datatypes.JSON `json:"metadata,omitempty" gorm:"column:metadata;default:null"`
	// CreatedAt entry creation timestamp
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt entry update timestamp
	UpdatedAt time.Time `json:"updated_at"`
}

/*
ParseMetadata parse the metadata based on the event type

	@param validator *validator.Validate - validator to verify the parsed metadata against
	@returns the typed metadata entry
*/
func (a SystemEventAudit) ParseMetadata(validator *validator.Validate) (interface{}, error) {
	return parseSystemEventMetadata(a.EventType, a.Metadata, validator)
}

// parseSystemEventMetadata parse raw system-event metadata into its typed struct based on the
// event type, then validate it.
func parseSystemEventMetadata(
	eventType SystemEventTypeENUM, metadata []byte, validator *validator.Validate,
) (interface{}, error) {
	// unmarshalAndValidate decode the raw metadata into the event type's structure, then
	// verify it. A structure which does not decode, or does not validate, indicates the
	// recorded metadata does not match its event type.
	unmarshalAndValidate := func(parsed interface{}) error {
		if err := json.Unmarshal(metadata, parsed); err != nil {
			return goutils.NewConsistencyError(
				fmt.Sprintf("system event '%s' metadata parse failed", eventType), err, true,
			)
		}
		if err := validator.Struct(parsed); err != nil {
			return goutils.NewValidationError(
				fmt.Sprintf("system event '%s' metadata validation failed", eventType), err, true,
			)
		}
		return nil
	}

	switch eventType {
	case SystemEventTypeNewWorkspace:
		var parsed SystemEventNewWorkspace
		return parsed, unmarshalAndValidate(&parsed)

	case SystemEventTypeRenameWorkspace:
		var parsed SystemEventRenameWorkspace
		return parsed, unmarshalAndValidate(&parsed)

	case SystemEventTypeWorkspaceVolumeState:
		var parsed SystemEventWorkspaceVolumeState
		return parsed, unmarshalAndValidate(&parsed)

	case SystemEventTypeDeleteWorkspace:
		var parsed SystemEventDeleteWorkspace
		return parsed, unmarshalAndValidate(&parsed)

	case SystemEventTypeNewArtifact:
		var parsed SystemEventNewArtifact
		return parsed, unmarshalAndValidate(&parsed)

	case SystemEventTypeRenameArtifact:
		var parsed SystemEventRenameArtifact
		return parsed, unmarshalAndValidate(&parsed)

	case SystemEventTypeUpdateArtifactObject:
		var parsed SystemEventUpdateArtifactObject
		return parsed, unmarshalAndValidate(&parsed)

	case SystemEventTypeArtifactMissingObject:
		var parsed SystemEventArtifactMissingObject
		return parsed, unmarshalAndValidate(&parsed)

	case SystemEventTypeDeleteArtifact:
		var parsed SystemEventDeleteArtifact
		return parsed, unmarshalAndValidate(&parsed)

	default:
		return nil, goutils.NewConsistencyError(
			fmt.Sprintf("unsupported system event type '%s'", eventType), nil, true,
		)
	}
}

// --------------------------------------------------------------------------------------
// Workspace event metadata

// SystemEventNewWorkspace records the definition of a new workspace
type SystemEventNewWorkspace struct {
	// WorkspaceID ID of the new workspace
	WorkspaceID string `json:"workspace_id" validate:"required,uuid"`
	// WorkspaceName name of the new workspace
	WorkspaceName string `json:"workspace_name" validate:"required,valid_name"`
	// VolumeName name of the persistent volume associated with the workspace
	VolumeName string `json:"volume_name" validate:"required"`
}

// SystemEventRenameWorkspace records the rename of a workspace.
//
// The persistent volume name is derived from the immutable workspace ID, so a rename never
// affects the volume; it is not recorded here.
type SystemEventRenameWorkspace struct {
	// WorkspaceID ID of the renamed workspace
	WorkspaceID string `json:"workspace_id" validate:"required,uuid"`
	// OldWorkspaceName the workspace's name prior to the rename
	OldWorkspaceName string `json:"old_workspace_name" validate:"required,valid_name"`
	// NewWorkspaceName the workspace's name after the rename
	NewWorkspaceName string `json:"new_workspace_name" validate:"required,valid_name"`
}

// SystemEventWorkspaceVolumeState records a change to a workspace's persistent volume state
type SystemEventWorkspaceVolumeState struct {
	// WorkspaceID ID of the workspace whose volume state changed
	WorkspaceID string `json:"workspace_id" validate:"required,uuid"`
	// WorkspaceName name of the workspace
	WorkspaceName string `json:"workspace_name" validate:"required,valid_name"`
	// VolumeName name of the persistent volume associated with the workspace
	VolumeName string `json:"volume_name" validate:"required"`
	// NewState the persistent volume state the workspace transitioned to
	NewState WorkspaceVolumeStateENUM `json:"new_state" validate:"required,volume_state"`
}

// SystemEventDeleteWorkspace records the deletion of a workspace
type SystemEventDeleteWorkspace struct {
	// WorkspaceID ID of the deleted workspace
	WorkspaceID string `json:"workspace_id" validate:"required,uuid"`
	// WorkspaceName name of the deleted workspace
	WorkspaceName string `json:"workspace_name" validate:"required,valid_name"`
}

// --------------------------------------------------------------------------------------
// Artifact event metadata
//
// Every artifact event records its parent `WorkspaceID`. An artifact row is the only other
// place that association lives, so denormalizing it here keeps the audit entry self-contained
// once the row is gone — whether deleted directly or cascaded away with its workspace.

// SystemEventNewArtifact records the definition of a new artifact
type SystemEventNewArtifact struct {
	// WorkspaceID ID of the parent workspace
	WorkspaceID string `json:"workspace_id" validate:"required,uuid"`
	// ArtifactID ID of the new artifact
	ArtifactID string `json:"artifact_id" validate:"required"`
	// ArtifactName name of the new artifact
	ArtifactName string `json:"artifact_name" validate:"required,valid_name"`
	// ObjectKey the complete object key backing the artifact
	ObjectKey string `json:"object_key" validate:"required"`
	// MIMEType server-sniffed content type of the backing object
	MIMEType string `json:"mime_type" validate:"required"`
	// Size size of the backing object in bytes
	Size int64 `json:"size" validate:"gte=0"`
}

// SystemEventRenameArtifact records the rename of an artifact.
//
// The backing object key carries a random suffix rather than the name, so a rename never
// touches the object store; no object key is recorded.
type SystemEventRenameArtifact struct {
	// WorkspaceID ID of the parent workspace
	WorkspaceID string `json:"workspace_id" validate:"required,uuid"`
	// ArtifactID ID of the renamed artifact
	ArtifactID string `json:"artifact_id" validate:"required"`
	// OldArtifactName the artifact's name prior to the rename
	OldArtifactName string `json:"old_artifact_name" validate:"required,valid_name"`
	// NewArtifactName the artifact's name after the rename
	NewArtifactName string `json:"new_artifact_name" validate:"required,valid_name"`
}

// SystemEventUpdateArtifactObject records an artifact being repointed at a new backing object.
//
// Both the outgoing and incoming object are captured: the update writes a NEW key and flips
// the row over to it, leaving the old object orphaned for the object-reaping GC. The old key
// recorded here is the audit trail's only record of what that orphan was.
type SystemEventUpdateArtifactObject struct {
	// WorkspaceID ID of the parent workspace
	WorkspaceID string `json:"workspace_id" validate:"required,uuid"`
	// ArtifactID ID of the updated artifact
	ArtifactID string `json:"artifact_id" validate:"required"`
	// ArtifactName name of the updated artifact
	ArtifactName string `json:"artifact_name" validate:"required,valid_name"`
	// OldObjectKey the object key backing the artifact prior to the update
	OldObjectKey string `json:"old_object_key" validate:"required"`
	// OldMIMEType content type of the backing object prior to the update
	OldMIMEType string `json:"old_mime_type" validate:"required"`
	// OldSize size in bytes of the backing object prior to the update
	OldSize int64 `json:"old_size" validate:"gte=0"`
	// NewObjectKey the object key backing the artifact after the update
	NewObjectKey string `json:"new_object_key" validate:"required"`
	// NewMIMEType content type of the backing object after the update
	NewMIMEType string `json:"new_mime_type" validate:"required"`
	// NewSize size in bytes of the backing object after the update
	NewSize int64 `json:"new_size" validate:"gte=0"`
}

// SystemEventArtifactMissingObject records an artifact quarantined because its backing object
// is gone. A data-loss signal, so the key that resolved to nothing is preserved as evidence.
type SystemEventArtifactMissingObject struct {
	// WorkspaceID ID of the parent workspace
	WorkspaceID string `json:"workspace_id" validate:"required,uuid"`
	// ArtifactID ID of the quarantined artifact
	ArtifactID string `json:"artifact_id" validate:"required"`
	// ArtifactName name of the quarantined artifact
	ArtifactName string `json:"artifact_name" validate:"required,valid_name"`
	// ObjectKey the object key which resolved to no object
	ObjectKey string `json:"object_key" validate:"required"`
}

// SystemEventDeleteArtifact records the deletion of an artifact. The object the row referenced
// is left in the store for the object-reaping GC, so its key is preserved here.
type SystemEventDeleteArtifact struct {
	// WorkspaceID ID of the parent workspace
	WorkspaceID string `json:"workspace_id" validate:"required,uuid"`
	// ArtifactID ID of the deleted artifact
	ArtifactID string `json:"artifact_id" validate:"required"`
	// ArtifactName name of the deleted artifact
	ArtifactName string `json:"artifact_name" validate:"required,valid_name"`
	// ObjectKey the object key which backed the deleted artifact
	ObjectKey string `json:"object_key" validate:"required"`
}

// Package models - application data models
package models

import (
	"fmt"
	"time"

	"github.com/alwitt/goutils"
)

// ArtifactStateENUM artifact state ENUM
type ArtifactStateENUM string

const (
	// ArtifactStateRecorded artifact is registered and its backing object is stored
	ArtifactStateRecorded ArtifactStateENUM = "RECORDED"

	// ArtifactStateMissingObject artifact's backing object is gone; a quarantine state
	// preserving the metadata as evidence of the loss
	ArtifactStateMissingObject ArtifactStateENUM = "MISSING_OBJECT"
)

// Values return list of valid ArtifactStateENUM values
func (ArtifactStateENUM) Values() []ArtifactStateENUM {
	return []ArtifactStateENUM{
		ArtifactStateRecorded,
		ArtifactStateMissingObject,
	}
}

// Artifact is a single durable, agent facing artifact within a workspace
type Artifact struct {
	// ID artifact ID
	ID string `json:"id" gorm:"column:id;primaryKey" validate:"required"`

	// WorkspaceID ID of the parent workspace
	WorkspaceID string `json:"workspace_id" gorm:"column:workspace_id;not null;uniqueIndex:artifact_workspace_name" validate:"required,uuid"`

	// Workspace parent workspace association; carries the ON DELETE CASCADE constraint so an
	// artifact's row is removed with its workspace. Not serialized, not validated.
	Workspace *Workspace `json:"-" gorm:"constraint:OnDelete:CASCADE;foreignKey:WorkspaceID" validate:"-"`

	// Name of the artifact, unique within the workspace, can only contain alphanumeric
	// characters, `-`, and `_`
	Name string `json:"name" gorm:"column:name;not null;uniqueIndex:artifact_workspace_name" validate:"required,valid_name" jsonschema:"name of the artifact, can only contain alphanumeric characters, -, and _"`

	// Description an optional description for the artifact
	Description *string `json:"description" gorm:"column:description;default:null" jsonschema:"an optional description for the artifact"`

	// ObjectKey the complete object key backing this artifact in the object store
	ObjectKey string `json:"object_key" gorm:"column:object_key;not null" validate:"required"`

	// MIMEType server-sniffed content type; advisory metadata only, not a security boundary
	MIMEType string `json:"mime_type" gorm:"column:mime_type;not null" validate:"required" jsonschema:"content type of the artifact"`

	// Size size of the artifact in bytes
	Size int64 `json:"size" gorm:"column:size;not null" validate:"gte=0" jsonschema:"size of the artifact in bytes"`

	// State artifact state [RECORDED, MISSING_OBJECT]
	State ArtifactStateENUM `json:"state" gorm:"column:state;not null;default:RECORDED" validate:"required,artifact_state" jsonschema:"artifact state. RECORDED: registered and stored, the normal live state. MISSING_OBJECT: the backing object is gone, a quarantine state preserving the metadata as evidence of the loss."`

	// CreatedAt entry creation timestamp
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt entry update timestamp
	UpdatedAt time.Time `json:"updated_at"`
}

// ValidStateNextState verify the artifact next state
func (a Artifact) ValidStateNextState(newState ArtifactStateENUM) error {
	statesWithTransitions := map[ArtifactStateENUM]map[ArtifactStateENUM]bool{
		ArtifactStateRecorded: {
			ArtifactStateRecorded:      true,
			ArtifactStateMissingObject: true,
		},
		ArtifactStateMissingObject: {
			ArtifactStateRecorded:      true,
			ArtifactStateMissingObject: true,
		},
	}

	availableNextStates, ok := statesWithTransitions[a.State]
	if !ok {
		return goutils.NewConsistencyError(fmt.Sprintf(
			"artifact %s can't transition out of state '%s'", a.ID, a.State,
		), nil, true)
	}

	if _, ok := availableNextStates[newState]; !ok {
		return goutils.NewConsistencyError(fmt.Sprintf(
			"artifact %s can't transition from '%s' to '%s'",
			a.ID,
			a.State,
			newState,
		), nil, true)
	}

	return nil
}

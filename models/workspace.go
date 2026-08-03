// Package models - application data models
package models

import (
	"fmt"
	"time"

	"github.com/alwitt/goutils"
	"gorm.io/datatypes"
)

// WorkspaceVolumeStateENUM workspace persistent volume state ENUM
type WorkspaceVolumeStateENUM string

const (
	// WorkspaceVolumeStateNone workspace persistent volume does not exist
	WorkspaceVolumeStateNone WorkspaceVolumeStateENUM = "NONE"

	// WorkspaceVolumeStateReady workspace persistent volume is defined and ready
	WorkspaceVolumeStateReady WorkspaceVolumeStateENUM = "READY"
)

// Values return list of valid WorkspaceVolumeStateENUM values
func (WorkspaceVolumeStateENUM) Values() []WorkspaceVolumeStateENUM {
	return []WorkspaceVolumeStateENUM{
		WorkspaceVolumeStateNone,
		WorkspaceVolumeStateReady,
	}
}

// WorkspaceMountPath the canonical path every container mounts a workspace volume at.
//
// Artifact paths only round-trip if every container agrees on it - the tool container where
// the agent's tool writes a file, and cairn's own sidecars that read it back. A file written
// at this path must be visible to the upload sidecar at the same path, or no artifact
// operation works.
//
// A named constant rather than scattered literals, so it can become configurable later; it is
// fixed for the first cut (see DESIGN §4.4).
const WorkspaceMountPath = "/mnt/cairn/ws"

// WorkspaceVolumeTypeENUM workspace persistence volume type
type WorkspaceVolumeTypeENUM string

const (
	// WorkspaceVolumeTypeDocker workspace persistence volumes are docker volumes
	WorkspaceVolumeTypeDocker WorkspaceVolumeTypeENUM = "docker"
)

// Values return all valid WorkspaceVolumeTypeENUM values
func (WorkspaceVolumeTypeENUM) Values() []WorkspaceVolumeTypeENUM {
	return []WorkspaceVolumeTypeENUM{
		WorkspaceVolumeTypeDocker,
	}
}

// WorkspaceVolumeMetadata per-workspace provisioning parameters for the persistent volume.
//
// Runtime-neutral by construction: it carries only what a caller may legitimately vary
// between workspaces. Everything else needed to provision a volume - the Docker volume
// driver and its options, or the Kubernetes storage class and access modes - is deployment
// policy that is identical for every workspace, so it lives in the application config
// rather than being copied onto every row.
//
// The volume's name is deliberately absent; it lives in `Workspace.VolumeName`, derived
// from the immutable workspace ID (DESIGN §2.1), and must stay single-sourced.
type WorkspaceVolumeMetadata struct {
	// SizeBytes requested capacity for the volume. Honored as a storage request by
	// Kubernetes and by capacity-aware Docker volume drivers; the default Docker `local`
	// driver treats it as advisory.
	SizeBytes *int64 `json:"size_bytes,omitempty" validate:"omitempty,gt=0" jsonschema:"requested capacity for the volume in bytes; must be > 0 when set. Advisory for the default docker volume driver."`
}

// Workspace is a collection of agent facing artifacts
type Workspace struct {
	// ID workspace ID
	ID string `json:"id" gorm:"column:id;primaryKey" validate:"required,uuid"`

	// Name of the workspace, can only contain alphanumeric characters, `-`, and `_`
	Name string `json:"name" gorm:"column:name;not null;unique" validate:"required,valid_name" jsonschema:"name of the workspace, can only contain alphanumeric characters, -, and _"`

	// Description an optional description for the workspace
	Description *string `json:"description,omitempty" gorm:"column:description;default:null" jsonschema:"an optional description for the workspace"`

	// VolumeName name of the persistent volume associated with workspace. It is not tied to
	// the name of the workspace.
	VolumeName string `json:"volume_name" gorm:"column:volume_name;not null;unique" validate:"required" jsonschema:"Name of the persistent volume associated with workspace. It is not tied to the name of the workspace."`

	// VolumeState workspace persistent volume state [NONE, READY]
	VolumeState WorkspaceVolumeStateENUM `json:"volume_state" gorm:"column:volume_state;not null;default:NONE" validate:"required,volume_state" jsonschema:"workspace persistent volume state. NONE: no volume exists. READY: volume exists and is mountable."`

	// VolumeMetadata persistence volume metadata for configuring the persistent volume.
	// Nil when the workspace takes the deployment's default provisioning parameters.
	VolumeMetadata *datatypes.JSONType[WorkspaceVolumeMetadata] `json:"volume_metadata,omitempty" gorm:"column:volume_metadata;default:null" validate:"omitempty" jsonschema:"persistence volume metadata for configuring the persistent volume; omitted when the workspace takes the deployment defaults"`

	// CreatedAt entry creation timestamp
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt entry update timestamp
	UpdatedAt time.Time `json:"updated_at"`
}

// ValidVolumeNextState verify the workspace persistent volume next state
func (w Workspace) ValidVolumeNextState(newState WorkspaceVolumeStateENUM) error {
	statesWithTransitions := map[WorkspaceVolumeStateENUM]map[WorkspaceVolumeStateENUM]bool{
		WorkspaceVolumeStateNone: {
			WorkspaceVolumeStateNone:  true,
			WorkspaceVolumeStateReady: true,
		},
		WorkspaceVolumeStateReady: {
			WorkspaceVolumeStateNone:  true,
			WorkspaceVolumeStateReady: true,
		},
	}

	availableNextStates, ok := statesWithTransitions[w.VolumeState]
	if !ok {
		return goutils.NewConsistencyError(fmt.Sprintf(
			"workspace volume %s can't transition out of state '%s'", w.VolumeName, w.VolumeState,
		), nil, true)
	}

	if _, ok := availableNextStates[newState]; !ok {
		return goutils.NewConsistencyError(fmt.Sprintf(
			"workspace volume %s can't transition from '%s' to '%s'",
			w.VolumeName,
			w.VolumeState,
			newState,
		), nil, true)
	}

	return nil
}

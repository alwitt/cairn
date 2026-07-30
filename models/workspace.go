// Package models - application data models
package models

import (
	"fmt"
	"time"

	"github.com/alwitt/goutils"
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

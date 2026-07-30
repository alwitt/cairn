// Package db - database controllers for system persistence
package db

import (
	"github.com/alwitt/cairn/models"
)

// --------------------------------------------------------------------------------------
// Audit

// SystemEventAuditEntry system event audit entry
type SystemEventAuditEntry struct {
	models.SystemEventAudit
}

// TableName hard code table name
func (SystemEventAuditEntry) TableName() string {
	return "audit_system_events"
}

// --------------------------------------------------------------------------------------
// Workspace

// WorkspaceEntry workspace DB entry
type WorkspaceEntry struct {
	models.Workspace
}

// TableName hard code table name
func (WorkspaceEntry) TableName() string {
	return "workspaces"
}

// --------------------------------------------------------------------------------------
// Artifact

// ArtifactEntry artifact DB entry.
//
// The parent workspace association carries the ON DELETE CASCADE constraint, so deleting a
// workspace row reaps its artifact rows in the same statement (see DESIGN §4.1).
type ArtifactEntry struct {
	models.Artifact
	Workspace WorkspaceEntry `gorm:"constraint:OnDelete:CASCADE;foreignKey:WorkspaceID" validate:"-"`
}

// TableName hard code table name
func (ArtifactEntry) TableName() string {
	return "artifacts"
}

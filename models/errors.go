package models

import "github.com/alwitt/goutils"

// ======================================================================================
// Workspace Module Error

// WorkspaceMangerError encountered error operating the workspace manager
type WorkspaceMangerError struct{ goutils.BaseError }

// NewWorkspaceMangerError build a WorkspaceMangerError, optionally capturing the call stack.
func NewWorkspaceMangerError(message string, core error, getCallStack bool) WorkspaceMangerError {
	base := goutils.BaseError{Name: "WorkspaceMangerError", Message: message, Core: core}
	if getCallStack {
		base.Stack = goutils.GetCallStack(1)
	}
	return WorkspaceMangerError{BaseError: base}
}

// ======================================================================================
// Artifact Module Error

// ArtifactMangerError encountered error operating the artifact manager
type ArtifactMangerError struct{ goutils.BaseError }

// NewArtifactMangerError build a ArtifactMangerError, optionally capturing the call stack.
func NewArtifactMangerError(message string, core error, getCallStack bool) ArtifactMangerError {
	base := goutils.BaseError{Name: "ArtifactMangerError", Message: message, Core: core}
	if getCallStack {
		base.Stack = goutils.GetCallStack(1)
	}
	return ArtifactMangerError{BaseError: base}
}
